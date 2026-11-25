// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package frame

import (
	"bytes"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Batch constants from the contract.
const (
	DictID         = 0x4E465231
	DefaultWindow  = 262144
	DefaultMaxBody = 1 << 20
)

// WindowOf returns the dictionary window for a batch at offset: the last
// min(window, offset) bytes of the raw stream before it. stream is the raw
// stream from byte 0 (or any suffix that holds those bytes ending at offset).
func WindowOf(prefix []byte, window int) []byte {
	if len(prefix) > window {
		prefix = prefix[len(prefix)-window:]
	}
	return prefix
}

// EncodeBatch compresses raw as one zstd frame with the raw dictionary window.
// An empty window means offset 0: no dictionary.
func EncodeBatch(raw, window []byte) ([]byte, error) {
	opts := []zstd.EOption{zstd.WithEncoderConcurrency(1), zstd.WithZeroFrames(true)}
	if len(window) > 0 {
		opts = append(opts, zstd.WithEncoderDictRaw(DictID, window))
	}
	enc, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(raw, nil), nil
}

// DecodeBatch is the server's half, used by tests: it decompresses a batch
// with the window, refusing more than max bytes.
func DecodeBatch(body, window []byte, max int) ([]byte, error) {
	opts := []zstd.DOption{zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(max))}
	if len(window) > 0 {
		opts = append(opts, zstd.WithDecoderDictRaw(DictID, window))
	}
	dec, err := zstd.NewReader(nil, opts...)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	out, err := dec.DecodeAll(body, nil)
	if err != nil {
		return nil, fmt.Errorf("decode batch: %w", err)
	}
	if bytes.Equal(out, nil) && len(body) == 0 {
		return nil, fmt.Errorf("decode batch: empty body")
	}
	return out, nil
}
