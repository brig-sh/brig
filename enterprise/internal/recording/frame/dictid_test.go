// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package frame

import (
	"encoding/binary"
	"testing"
)

// The raw dictionary id is part of the wire contract: a deployment decodes a
// batch with a raw dictionary registered under exactly this id, so a changed
// value makes every batch after the first undecodable. A round trip through
// this package would not notice, since both halves read the same constant, so
// this reads the id out of the zstd frame header of a batch.
func TestDictIDIsTheContractsAndIsInTheFrameHeader(t *testing.T) {
	if DictID != 0x4E465231 {
		t.Fatalf("DictID = %#x, want 0x4E465231", uint32(DictID))
	}
	window := []byte("the window of what came before, as the dictionary")
	enc, err := EncodeBatch([]byte("the next batch of terminal output"), window)
	if err != nil {
		t.Fatal(err)
	}
	// A zstd frame: magic 0xFD2FB528, then the frame header descriptor. Its low
	// two bits give the size of the dictionary id field: 0, 1, 2 or 4 bytes. A
	// window descriptor follows the descriptor unless the single-segment bit
	// (0x20) is set.
	if len(enc) < 10 || binary.LittleEndian.Uint32(enc) != 0xFD2FB528 {
		t.Fatalf("not a zstd frame: % x", enc[:min(len(enc), 8)])
	}
	fhd := enc[4]
	size := [4]int{0, 1, 2, 4}[fhd&3]
	if size != 4 {
		t.Fatalf("the frame header carries a %d byte dictionary id, want 4", size)
	}
	at := 5
	if fhd&0x20 == 0 {
		at++
	}
	if got := binary.LittleEndian.Uint32(enc[at:]); got != 0x4E465231 {
		t.Errorf("the frame names dictionary %#x, want 0x4E465231", got)
	}

	// No window means offset 0: no dictionary at all.
	plain, err := EncodeBatch([]byte("the first batch"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain[4]&3 != 0 {
		t.Errorf("a batch at offset 0 names a dictionary: % x", plain[:8])
	}
}
