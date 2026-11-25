// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package recording

import (
	"bytes"
	"testing"

	"github.com/brig-sh/brig/enterprise/internal/recording/frame"
	"github.com/brig-sh/brig/enterprise/internal/recording/recordingtest"
)

func newFake(t *testing.T) *recordingtest.Server { return recordingtest.New(t, "tok-secret") }

func fakeClient(f *recordingtest.Server) *Client {
	return &Client{Base: f.URL(), Token: "tok-secret", HTTP: f.Client(), UserAgent: "brig-test"}
}

func decodeAll(t *testing.T, raw []byte) []frame.Frame {
	t.Helper()
	fs, err := frame.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return fs
}

func outputOf(fs []frame.Frame) []byte {
	var b bytes.Buffer
	for _, f := range fs {
		if f.Type == frame.Output {
			b.Write(f.Payload)
		}
	}
	return b.Bytes()
}
