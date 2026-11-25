// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

// Package frame is the nofire.recording/v1 stream: the frame encoding, the
// output coalescer and the upload batch compression. The wire contract is the
// one the backend and the player are written against; this package is brig's
// half of it.
package frame

import (
	"errors"
	"fmt"
	"time"
)

// Frame types.
const (
	Output byte = 'o'
	Resize byte = 'r'
	Marker byte = 'm'
	Exit   byte = 'x'
)

// Limits.
const (
	MaxOutput  = 64 << 10 // output payload per frame the recorder writes
	MaxMarker  = 256
	MaxPayload = 1 << 20 // a decoder refuses more
	// MergeWindow is how long after the first byte of a pending output frame
	// later output is still merged into it.
	MergeWindow = 10 * time.Millisecond
)

// Frame is one decoded frame. DtMs is milliseconds since the previous frame.
type Frame struct {
	DtMs    uint64
	Type    byte
	Payload []byte
}

// Append appends the frame to dst.
func Append(dst []byte, dtMs uint64, typ byte, payload []byte) []byte {
	dst = AppendUvarint(dst, dtMs)
	dst = append(dst, typ)
	dst = AppendUvarint(dst, uint64(len(payload)))
	return append(dst, payload...)
}

// AppendUvarint appends the minimal unsigned varint of v.
func AppendUvarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// ResizePayload is the payload of a resize frame.
func ResizePayload(cols, rows uint16) []byte {
	return AppendUvarint(AppendUvarint(nil, uint64(cols)), uint64(rows))
}

// ExitPayload is the payload of an exit frame.
func ExitPayload(status int) []byte {
	if status < 0 {
		status = 0
	}
	return AppendUvarint(nil, uint64(status))
}

// Decoding errors.
var (
	ErrTruncated   = errors.New("frame: truncated")
	ErrVarint      = errors.New("frame: non-minimal or oversize varint")
	ErrType        = errors.New("frame: unknown type")
	ErrPayloadSize = errors.New("frame: payload over 1 MiB")
)

// uvarint reads a minimal varint. n is 0 when b ends inside it.
func uvarint(b []byte) (v uint64, n int, err error) {
	var shift uint
	for i, c := range b {
		if i == 10 {
			return 0, 0, ErrVarint
		}
		if c < 0x80 {
			if i > 0 && c == 0 {
				return 0, 0, ErrVarint // a trailing zero group is padding
			}
			if i == 9 && c > 1 {
				return 0, 0, ErrVarint
			}
			return v | uint64(c)<<shift, i + 1, nil
		}
		v |= uint64(c&0x7f) << shift
		shift += 7
	}
	if len(b) >= 10 {
		return 0, 0, ErrVarint // ten continuation bytes is already too long
	}
	return 0, 0, nil
}

// Next decodes the first frame of b and returns its length. It returns
// ErrTruncated when b holds only part of a frame, and the other errors for a
// frame that is invalid whatever follows it.
func Next(b []byte) (Frame, int, error) {
	dt, n1, err := uvarint(b)
	if err != nil {
		return Frame{}, 0, err
	}
	if n1 == 0 || n1 >= len(b) {
		return Frame{}, 0, ErrTruncated
	}
	typ := b[n1]
	switch typ {
	case Output, Resize, Marker, Exit:
	default:
		return Frame{}, 0, fmt.Errorf("%w 0x%02x", ErrType, typ)
	}
	ln, n2, err := uvarint(b[n1+1:])
	if err != nil {
		return Frame{}, 0, err
	}
	if n2 == 0 {
		return Frame{}, 0, ErrTruncated
	}
	if ln > MaxPayload {
		return Frame{}, 0, ErrPayloadSize
	}
	start := n1 + 1 + n2
	if uint64(len(b)-start) < ln {
		return Frame{}, 0, ErrTruncated
	}
	end := start + int(ln)
	return Frame{DtMs: dt, Type: typ, Payload: b[start:end]}, end, nil
}

// Decode decodes b, which must be whole frames.
func Decode(b []byte) ([]Frame, error) {
	var out []Frame
	for len(b) > 0 {
		f, n, err := Next(b)
		if err != nil {
			return out, err
		}
		out = append(out, f)
		b = b[n:]
	}
	return out, nil
}

// WholePrefix returns the length of the longest prefix of b made of whole
// valid frames, and the error that ended it unless b ended on a frame
// boundary or inside a frame (ErrTruncated).
func WholePrefix(b []byte) (int, error) {
	pos := 0
	for pos < len(b) {
		_, n, err := Next(b[pos:])
		if err == ErrTruncated {
			return pos, nil
		}
		if err != nil {
			return pos, err
		}
		pos += n
	}
	return pos, nil
}
