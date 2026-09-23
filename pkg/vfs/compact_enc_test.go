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

package vfs

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"io"
	"sync"
	"testing"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
)

func randomKey32(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := crand.Read(key); err != nil {
		t.Fatalf("generate key: %s", err)
	}
	return key
}

// compactEncStore serves per-slice plaintext to readers and records the merged
// output plus the CEK handed to the writer.
type compactEncStore struct {
	mu       sync.Mutex
	data     map[uint64][]byte // source slice id -> plaintext
	written  map[uint64][]byte // new slice id -> merged plaintext
	writeKey []byte
}

func (s *compactEncStore) NewReader(id uint64, length int) chunk.Reader {
	return s.NewReaderWithKey(id, length, nil)
}

func (s *compactEncStore) NewWriter(id uint64, tierID uint8) chunk.Writer {
	return s.NewWriterWithKey(id, tierID, nil)
}

func (s *compactEncStore) NewReaderWithKey(id uint64, length int, key []byte) chunk.Reader {
	s.mu.Lock()
	data := append([]byte(nil), s.data[id]...)
	s.mu.Unlock()
	return &compactEncReader{data: data}
}

func (s *compactEncStore) NewWriterWithKey(id uint64, tierID uint8, key []byte) chunk.Writer {
	s.mu.Lock()
	s.writeKey = append([]byte(nil), key...)
	s.mu.Unlock()
	return &compactEncWriter{store: s, id: id}
}

func (s *compactEncStore) Remove(id uint64, length int) error            { return nil }
func (s *compactEncStore) FillCache(id uint64, length uint32) error      { return nil }
func (s *compactEncStore) EvictCache(id uint64, length uint32) error     { return nil }
func (s *compactEncStore) CheckCache(id uint64, length uint32, handler func(bool, string, int)) error {
	return nil
}
func (s *compactEncStore) UsedMemory() int64                  { return 0 }
func (s *compactEncStore) UpdateLimit(upload, download int64) {}
func (s *compactEncStore) BlobStorage() object.ObjectStorage  { return nil }

type compactEncReader struct{ data []byte }

func (r *compactEncReader) ReadAt(ctx context.Context, p *chunk.Page, off int) (int, error) {
	if off >= len(r.data) {
		return 0, io.EOF
	}
	return copy(p.Data, r.data[off:]), nil
}

type compactEncWriter struct {
	store *compactEncStore
	id    uint64
	data  []byte
}

func (w *compactEncWriter) WriteAt(p []byte, off int64) (int, error) {
	if int64(len(w.data)) < off+int64(len(p)) {
		b := make([]byte, off+int64(len(p)))
		copy(b, w.data)
		w.data = b
	}
	copy(w.data[off:], p)
	return len(p), nil
}

func (w *compactEncWriter) ID() uint64        { return w.id }
func (w *compactEncWriter) SetID(id uint64)   { w.id = id }
func (w *compactEncWriter) SetWriteback(bool) {}
func (w *compactEncWriter) FlushTo(int) error { return nil }
func (w *compactEncWriter) Finish(total int) error {
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	w.store.written[w.id] = append([]byte(nil), w.data...)
	return nil
}
func (w *compactEncWriter) Abort() {}

// TestCompact_WithCEK (task 5.3): compaction of encrypted slices unwraps each
// source CEK under the file FEK, merges the plaintext and returns a fresh CEK
// wrapped under the same FEK/AAD; the merged data must equal the concatenation
// of the source plaintexts (FR-OP-7).
func TestCompact_WithCEK(t *testing.T) {
	cconf := chunk.Config{
		BlockSize:   256 * 1024,
		MaxUpload:   2,
		MaxDownload: 200,
		BufferSize:  30 << 20,
		CacheSize:   10 << 20,
		CacheDir:    "memory",
	}
	fek := randomKey32(t)
	dfid := "df-compact"
	const ver = uint32(3)

	cek1, err := chunk.NewCEK()
	if err != nil {
		t.Fatalf("new CEK: %s", err)
	}
	cek2, err := chunk.NewCEK()
	if err != nil {
		t.Fatalf("new CEK: %s", err)
	}
	plain1 := bytes.Repeat([]byte{0x11}, 3000)
	plain2 := bytes.Repeat([]byte{0x22}, 5000)

	wrapped1, err := chunk.WrapCEK(fek, cek1, dfid, 11, ver)
	if err != nil {
		t.Fatalf("wrap CEK 1: %s", err)
	}
	wrapped2, err := chunk.WrapCEK(fek, cek2, dfid, 12, ver)
	if err != nil {
		t.Fatalf("wrap CEK 2: %s", err)
	}

	store := &compactEncStore{
		data:    map[uint64][]byte{11: plain1, 12: plain2},
		written: make(map[uint64][]byte),
	}
	slices := []meta.Slice{
		{Id: 11, Size: uint32(len(plain1)), Len: uint32(len(plain1)), WrappedCEK: wrapped1},
		{Id: 12, Size: uint32(len(plain2)), Len: uint32(len(plain2)), WrappedCEK: wrapped2},
	}

	const newID = uint64(900)
	wrapped, err := Compact(cconf, store, slices, newID, 0, fek, dfid, ver)
	if err != nil {
		t.Fatalf("compact: %s", err)
	}
	if len(wrapped) == 0 {
		t.Fatal("encrypted compaction must return a wrapped CEK")
	}

	store.mu.Lock()
	got := store.written[newID]
	writeKey := append([]byte(nil), store.writeKey...)
	store.mu.Unlock()

	want := append(append([]byte{}, plain1...), plain2...)
	if !bytes.Equal(got, want) {
		t.Fatalf("merged data = %d bytes (first=%x), want %d bytes", len(got), got[:min(8, len(got))], len(want))
	}
	if len(writeKey) != chunk.CEKSize {
		t.Fatalf("writer key = %d bytes, want %d", len(writeKey), chunk.CEKSize)
	}
	newCEK, err := chunk.UnwrapCEK(fek, wrapped, dfid, newID, ver)
	if err != nil {
		t.Fatalf("unwrap returned CEK: %s", err)
	}
	if !bytes.Equal(newCEK, writeKey) {
		t.Fatal("returned wrapped CEK does not match the CEK used by the writer")
	}
}

// TestCompact_WithCEK_FailClosed (task 5.3): encrypted slices without a FEK must
// fail closed — no data is read or written.
func TestCompact_WithCEK_FailClosed(t *testing.T) {
	cconf := chunk.Config{
		BlockSize:   256 * 1024,
		BufferSize:  30 << 20,
		CacheSize:   10 << 20,
		CacheDir:    "memory",
	}
	fek := randomKey32(t)
	wrapped, err := chunk.WrapCEK(fek, randomKey32(t), "df-x", 5, 1)
	if err != nil {
		t.Fatalf("wrap CEK: %s", err)
	}
	store := &compactEncStore{
		data:    map[uint64][]byte{5: bytes.Repeat([]byte{0x33}, 1024)},
		written: make(map[uint64][]byte),
	}
	slices := []meta.Slice{{Id: 5, Size: 1024, Len: 1024, WrappedCEK: wrapped}}

	_, err = Compact(cconf, store, slices, 901, 0, nil, "df-x", 1)
	if err == nil {
		t.Fatal("compact of encrypted slices without FEK must fail")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.written) != 0 {
		t.Fatal("no data may be written when compaction fails closed")
	}
}
