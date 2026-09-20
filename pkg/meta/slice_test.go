/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package meta

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestMarshalSliceCEK_LegacyByteIdentical(t *testing.T) {
	// A slice without a wrapped CEK must marshal to exactly the legacy 24-byte
	// record (NFR-COMPAT-1: old readers keep working).
	pos, id, size, off, length := uint32(7), uint64(0x1122334455667788), uint32(1<<20), uint32(4096), uint32(1<<20)
	legacy := marshalSlice(pos, id, size, off, length)
	if len(legacy) != sliceBytes {
		t.Fatalf("legacy record len = %d, want %d", len(legacy), sliceBytes)
	}
	got := marshalSliceCEK(pos, id, size, off, length, nil)
	if !bytes.Equal(got, legacy) {
		t.Fatalf("marshalSliceCEK(nil CEK) != marshalSlice:\n got %x\nwant %x", got, legacy)
	}
}

func TestSliceRoundTripWithWrappedCEK(t *testing.T) {
	wrappedCEK := make([]byte, 65) // AGCK blob size
	for i := range wrappedCEK {
		wrappedCEK[i] = byte(i * 7)
	}
	pos, id, size, off, length := uint32(1), uint64(42), uint32(2<<20), uint32(0), uint32(1<<20)
	rec := marshalSliceCEK(pos, id, size, off, length, wrappedCEK)
	if len(rec) != sliceBytes+4+len(wrappedCEK) {
		t.Fatalf("record len = %d, want %d", len(rec), sliceBytes+4+len(wrappedCEK))
	}

	ss := readSlices([]string{string(rec)})
	if ss == nil {
		t.Fatal("readSlices returned nil for valid record")
	}
	if len(ss) != 1 {
		t.Fatalf("slices = %d, want 1", len(ss))
	}
	s := ss[0]
	if s.pos != pos || s.id != id || s.size != size || s.off != off || s.len != length {
		t.Fatalf("parsed slice = %+v, want pos=%d id=%d size=%d off=%d len=%d", s, pos, id, size, off, length)
	}
	if !bytes.Equal(s.wrappedCEK, wrappedCEK) {
		t.Fatalf("wrappedCEK mismatch:\n got %x\nwant %x", s.wrappedCEK, wrappedCEK)
	}
}

func TestSliceRoundTripMixedLegacyAndEncrypted(t *testing.T) {
	wrappedCEK := bytes.Repeat([]byte{0xAB}, 65)
	rec1 := marshalSlice(0, 1, 100, 0, 100) // legacy
	rec2 := marshalSliceCEK(100, 2, 200, 0, 100, wrappedCEK)

	ss := readSlices([]string{string(rec1), string(rec2)})
	if ss == nil {
		t.Fatal("readSlices returned nil")
	}
	if len(ss) != 2 {
		t.Fatalf("slices = %d, want 2", len(ss))
	}
	if ss[0].wrappedCEK != nil {
		t.Fatalf("legacy slice must have nil wrappedCEK, got %x", ss[0].wrappedCEK)
	}
	if !bytes.Equal(ss[1].wrappedCEK, wrappedCEK) {
		t.Fatal("encrypted slice lost its wrappedCEK")
	}

	// buildSlice must carry the CEK into the exported Slice records.
	exported := buildSlice(ss)
	var found *Slice
	for i := range exported {
		if exported[i].Id == 2 {
			found = &exported[i]
		}
	}
	if found == nil {
		t.Fatalf("slice id=2 missing from buildSlice output: %+v", exported)
	}
	if !bytes.Equal(found.WrappedCEK, wrappedCEK) {
		t.Fatal("buildSlice dropped WrappedCEK")
	}
}

func TestReadSlicesCorrupt(t *testing.T) {
	valid := marshalSlice(0, 1, 100, 0, 100)
	cases := []struct {
		name string
		rec  []byte
	}{
		{"too short", valid[:sliceBytes-1]},
		{"tail shorter than u32", append(append([]byte(nil), valid...), 0x01, 0x02, 0x03)},
		{"blobLen larger than tail", func() []byte {
			rec := append([]byte(nil), valid...)
			var blobLen [4]byte
			binary.BigEndian.PutUint32(blobLen[:], uint32(len(valid))) // claims more bytes than present
			return append(rec, blobLen[:]...)
		}()},
		{"blobLen smaller than tail", func() []byte {
			rec := append([]byte(nil), valid...)
			var blobLen [4]byte
			binary.BigEndian.PutUint32(blobLen[:], 1) // claims 1 byte, but 65 follow
			rec = append(rec, blobLen[:]...)
			return append(rec, bytes.Repeat([]byte{0xFF}, 65)...)
		}()},
	}
	for _, c := range cases {
		if ss := readSlices([]string{string(c.rec)}); ss != nil {
			t.Errorf("%s: expected nil, got %d slices", c.name, len(ss))
		}
	}
}

func TestSliceCutSharesWrappedCEK(t *testing.T) {
	wrappedCEK := bytes.Repeat([]byte{0xCD}, 65)
	s := newSlice(0, 9, 1<<20, 0, 1<<20)
	s.wrappedCEK = wrappedCEK

	left, right := s.cut(1 << 19)
	if left == nil || right == nil {
		t.Fatalf("cut returned nil halves: %v / %v", left, right)
	}
	if !bytes.Equal(left.wrappedCEK, wrappedCEK) {
		t.Fatal("left half lost wrappedCEK")
	}
	if !bytes.Equal(right.wrappedCEK, wrappedCEK) {
		t.Fatal("right half lost wrappedCEK")
	}
	if left.len != 1<<19 || right.len != 1<<19 {
		t.Fatalf("bad split: left.len=%d right.len=%d", left.len, right.len)
	}
}

func TestReadSliceBufFixedRecords(t *testing.T) {
	// readSliceBuf (sql/tkv path) parses concatenated fixed 24-byte records;
	// the bytes of record N+1 must not be mistaken for record N's CEK tail.
	buf := append(marshalSlice(0, 1, 100, 0, 100), marshalSlice(100, 2, 200, 50, 150)...)
	ss := readSliceBuf(buf)
	if ss == nil {
		t.Fatal("readSliceBuf returned nil for valid buffer")
	}
	if len(ss) != 2 {
		t.Fatalf("slices = %d, want 2", len(ss))
	}
	if ss[0].id != 1 || ss[0].size != 100 || ss[0].off != 0 || ss[0].len != 100 {
		t.Fatalf("slice 0 = %+v", ss[0])
	}
	if ss[1].pos != 100 || ss[1].id != 2 || ss[1].size != 200 || ss[1].off != 50 || ss[1].len != 150 {
		t.Fatalf("slice 1 = %+v", ss[1])
	}
	if ss[0].wrappedCEK != nil || ss[1].wrappedCEK != nil {
		t.Fatal("fixed records must not produce a wrappedCEK")
	}
	if got := readSliceBuf(marshalSlice(0, 1, 100, 0, 99)[:23]); got != nil {
		t.Fatal("readSliceBuf must reject non-24B buffers")
	}
}

func TestSliceEqualsIgnoresWrappedCEK(t *testing.T) {
	a := newSlice(0, 5, 100, 0, 100)
	b := newSlice(0, 5, 100, 0, 100)
	b.wrappedCEK = bytes.Repeat([]byte{1}, 65)
	if !a.equals(b) {
		t.Fatal("equals must compare data fields only")
	}
	b.len = 99
	if a.equals(b) {
		t.Fatal("equals must detect len difference")
	}
}
