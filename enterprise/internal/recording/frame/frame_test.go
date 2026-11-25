// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package frame

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

const goldenHex = "00" + "6f0468690d0a" + // dt 0, o, "hi\r\n"
	"dc0b" + "72025018" + // dt 1500, r, 80x24
	"0a" + "6d06" + "6465706c6f79" + // dt 10, m, "deploy"
	"fa01" + "780100" // dt 250, x, 0

func TestGoldenVector(t *testing.T) {
	var b []byte
	b = Append(b, 0, Output, []byte("hi\r\n"))
	b = Append(b, 1500, Resize, ResizePayload(80, 24))
	b = Append(b, 10, Marker, []byte("deploy"))
	b = Append(b, 250, Exit, ExitPayload(0))
	if got := hex.EncodeToString(b); got != goldenHex {
		t.Fatalf("got  %s\nwant %s", got, goldenHex)
	}
	fs, err := Decode(b)
	if err != nil || len(fs) != 4 || fs[1].DtMs != 1500 || fs[3].Type != Exit {
		t.Fatalf("decode: %+v %v", fs, err)
	}
}

func TestWriterReproducesGolden(t *testing.T) {
	var w Writer
	var b []byte
	b = w.Output(b, 0, []byte("hi\r\n"))
	b = w.Resize(b, 1500*time.Millisecond, 80, 24)
	b = w.Marker(b, 1510*time.Millisecond, "deploy")
	b = w.Exit(b, 1760*time.Millisecond, 0)
	if got := hex.EncodeToString(b); got != goldenHex {
		t.Fatalf("got  %s\nwant %s", got, goldenHex)
	}
}

func TestDecoderRefusals(t *testing.T) {
	cases := map[string]struct {
		in   string
		want error
	}{
		"unknown type":    {"0069" + "00", ErrType},
		"truncated":       {"006f0468", ErrTruncated},
		"nonminimal dt":   {"8000" + "6f00", ErrVarint},
		"nonminimal len":  {"006f" + "8000", ErrVarint},
		"payload too big": {"006f" + "808041", ErrPayloadSize},
		"dt only":         {"00", ErrTruncated},
	}
	for name, c := range cases {
		b, _ := hex.DecodeString(c.in)
		if _, err := Decode(b); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func TestWholePrefix(t *testing.T) {
	b := Append(nil, 0, Output, []byte("abc"))
	whole := len(b)
	b = Append(b, 5, Output, []byte("defgh"))
	for cut := whole; cut < len(b); cut++ {
		if n, err := WholePrefix(b[:cut]); err != nil || n != whole {
			t.Errorf("cut %d: n=%d err=%v", cut, n, err)
		}
	}
	if n, _ := WholePrefix(b); n != len(b) {
		t.Errorf("full: %d", n)
	}
}

func TestCoalescerMergesWithin10msAndCaps(t *testing.T) {
	var w Writer
	var b []byte
	b = w.Output(b, 0, []byte("a"))
	b = w.Output(b, 9*time.Millisecond, []byte("b"))
	if len(b) != 0 {
		t.Fatal("emitted early")
	}
	b = w.Output(b, 10*time.Millisecond, []byte("c")) // 10ms after first byte: new frame
	b = w.Flush(b)
	fs, err := Decode(b)
	if err != nil || len(fs) != 2 || string(fs[0].Payload) != "ab" || string(fs[1].Payload) != "c" || fs[1].DtMs != 10 {
		t.Fatalf("frames %+v err %v", fs, err)
	}

	w = Writer{}
	b = w.Output(nil, 0, bytes.Repeat([]byte("x"), 3*MaxOutput/2))
	b = w.Flush(b)
	fs, err = Decode(b)
	if err != nil || len(fs) != 2 || len(fs[0].Payload) != MaxOutput || len(fs[1].Payload) != MaxOutput/2 {
		t.Fatalf("cap: %d frames err %v", len(fs), err)
	}
}

func TestCoalescerTickAndDeterminism(t *testing.T) {
	run := func() []byte {
		var w Writer
		var b []byte
		b = w.Output(b, 3*time.Millisecond, []byte("q"))
		b = w.Tick(b, 12*time.Millisecond)
		if len(b) != 0 {
			t.Fatal("ticked early")
		}
		b = w.Tick(b, 13*time.Millisecond)
		b = w.Output(b, 40*time.Millisecond, []byte("z"))
		return w.Exit(b, 41*time.Millisecond, 7)
	}
	if !bytes.Equal(run(), run()) {
		t.Fatal("not deterministic")
	}
}

func TestBatchRoundTripWithWindow(t *testing.T) {
	var w Writer
	stream := w.Output(nil, 0, bytes.Repeat([]byte("the quick brown fox\r\n"), 40000))
	stream = w.Flush(stream)
	half := len(stream) / 2
	first, second := stream[:half], stream[half:]

	enc0, err := EncodeBatch(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	got0, err := DecodeBatch(enc0, nil, 1<<22)
	if err != nil || !bytes.Equal(got0, first) {
		t.Fatalf("offset 0 round trip: %v", err)
	}
	win := WindowOf(stream[:half], DefaultWindow)
	if len(win) != DefaultWindow {
		t.Fatalf("window %d", len(win))
	}
	enc1, err := EncodeBatch(second, win)
	if err != nil {
		t.Fatal(err)
	}
	got1, err := DecodeBatch(enc1, win, 1<<22)
	if err != nil || !bytes.Equal(got1, second) {
		t.Fatalf("windowed round trip: %v", err)
	}
	if _, err := DecodeBatch(enc1, nil, 1<<22); err == nil {
		t.Error("a windowed batch decoded without its window")
	}
	short := WindowOf([]byte("abc"), DefaultWindow)
	if string(short) != "abc" {
		t.Error("short prefix is the whole window")
	}
}

// A varint that is already ten continuation bytes long can never become valid,
// so it is malformed, not "need more data". Fewer continuation bytes at the end
// of the input still mean need more data.
func TestTenContinuationBytesAtTheEndAreMalformed(t *testing.T) {
	ten := strings.Repeat("ff", 10)
	for name, c := range map[string]struct {
		in   string
		want error
	}{
		"ten in the dt":        {ten, ErrVarint},
		"ten in the length":    {"006f" + ten, ErrVarint},
		"nine in the dt":       {strings.Repeat("ff", 9), ErrTruncated},
		"nine in the length":   {"006f" + strings.Repeat("ff", 9), ErrTruncated},
		"ten then a last byte": {strings.Repeat("ff", 9) + "02", ErrVarint},
	} {
		b, _ := hex.DecodeString(c.in)
		if _, _, err := Next(b); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
		if n, err := WholePrefix(b); n != 0 || (c.want == ErrTruncated) != (err == nil) {
			t.Errorf("%s: WholePrefix = %d, %v", name, n, err)
		}
	}
}
