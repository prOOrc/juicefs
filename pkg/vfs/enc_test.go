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
	"syscall"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
)

// encMetaStub implements only the meta calls made by the data reader/writer;
// any other call panics (nil embedded interface).
type encMetaStub struct {
	meta.Meta
	mu      sync.Mutex
	nextID  uint64
	slices  []meta.Slice // returned by Read
	written []meta.Slice // captured from Write
}

func (m *encMetaStub) NewSlice(ctx meta.Context, id *uint64) syscall.Errno {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	*id = m.nextID
	return 0
}

func (m *encMetaStub) Write(ctx meta.Context, inode Ino, indx, off uint32, slice meta.Slice, mtime time.Time) syscall.Errno {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.written = append(m.written, slice)
	return 0
}

func (m *encMetaStub) Read(ctx meta.Context, inode Ino, indx uint32, slices *[]meta.Slice) syscall.Errno {
	m.mu.Lock()
	defer m.mu.Unlock()
	*slices = append([]meta.Slice(nil), m.slices...)
	return 0
}

// recordingStore captures the keys passed to NewWriterWithKey/NewReaderWithKey.
type recordingStore struct {
	mu       sync.Mutex
	writeKey []byte
	readKeys [][]byte
	data     []byte // plaintext served by readers
}

func (s *recordingStore) NewReader(id uint64, length int) chunk.Reader {
	return s.NewReaderWithKey(id, length, nil)
}

func (s *recordingStore) NewWriter(id uint64, tierID uint8) chunk.Writer {
	return s.NewWriterWithKey(id, tierID, nil)
}

func (s *recordingStore) NewReaderWithKey(id uint64, length int, key []byte) chunk.Reader {
	s.mu.Lock()
	s.readKeys = append(s.readKeys, append([]byte(nil), key...))
	s.mu.Unlock()
	return &encChunkReader{store: s}
}

func (s *recordingStore) NewWriterWithKey(id uint64, tierID uint8, key []byte) chunk.Writer {
	s.mu.Lock()
	s.writeKey = append([]byte(nil), key...)
	s.mu.Unlock()
	return &encChunkWriter{}
}

func (s *recordingStore) Remove(id uint64, length int) error { return nil }
func (s *recordingStore) FillCache(id uint64, length uint32) error {
	return nil
}
func (s *recordingStore) EvictCache(id uint64, length uint32) error { return nil }
func (s *recordingStore) CheckCache(id uint64, length uint32, handler func(bool, string, int)) error {
	return nil
}
func (s *recordingStore) UsedMemory() int64                  { return 0 }
func (s *recordingStore) UpdateLimit(upload, download int64) {}
func (s *recordingStore) BlobStorage() object.ObjectStorage  { return nil }

type encChunkReader struct{ store *recordingStore }

func (r *encChunkReader) ReadAt(ctx context.Context, p *chunk.Page, off int) (int, error) {
	if off >= len(r.store.data) {
		return 0, io.EOF
	}
	return copy(p.Data, r.store.data[off:]), nil
}

type encChunkWriter struct {
	id   uint64
	data []byte
}

func (w *encChunkWriter) WriteAt(p []byte, off int64) (int, error) {
	if int64(len(w.data)) < off+int64(len(p)) {
		b := make([]byte, off+int64(len(p)))
		copy(b, w.data)
		w.data = b
	}
	copy(w.data[off:], p)
	return len(p), nil
}
func (w *encChunkWriter) ID() uint64        { return w.id }
func (w *encChunkWriter) SetID(id uint64)   { w.id = id }
func (w *encChunkWriter) SetWriteback(bool) {}
func (w *encChunkWriter) FlushTo(int) error { return nil }
func (w *encChunkWriter) Finish(int) error  { return nil }
func (w *encChunkWriter) Abort()            {}

func newEncTestConf() *Config {
	return &Config{
		Meta: meta.DefaultConf(),
		Chunk: &chunk.Config{
			BlockSize:  4 << 20,
			BufferSize: 64 << 20,
			Readahead:  1 << 20,
		},
		FuseOpts: &FuseOptions{},
	}
}

func TestFileWriter_WrapsCEKUnderFEK(t *testing.T) {
	m := &encMetaStub{}
	store := &recordingStore{}
	conf := newEncTestConf()
	reader := NewDataReader(conf, m, store)
	writer := NewDataWriter(conf, m, store, reader)

	fek := make([]byte, 32)
	if _, err := crand.Read(fek); err != nil {
		t.Fatalf("generate FEK: %s", err)
	}
	attr := &meta.Attr{Encrypted: true, Fek: fek, FekVersion: 1, DriveFileID: "df-1"}

	fw := writer.Open(1, 0, 0, attr)
	data := bytes.Repeat([]byte{0xAB}, 4096)
	if st := fw.Write(meta.Background(), 0, data); st != 0 {
		t.Fatalf("write: %s", st)
	}
	if st := fw.Flush(meta.Background()); st != 0 {
		t.Fatalf("flush: %s", st)
	}

	store.mu.Lock()
	writeKey := store.writeKey
	store.mu.Unlock()
	if len(writeKey) != 32 {
		t.Fatalf("writer key = %d bytes, want a 32-byte CEK", len(writeKey))
	}
	if bytes.Equal(writeKey, fek) {
		t.Fatal("store received the FEK instead of a per-slice CEK")
	}

	m.mu.Lock()
	written := m.written
	m.mu.Unlock()
	if len(written) != 1 {
		t.Fatalf("committed slices = %d, want 1", len(written))
	}
	ss := written[0]
	if len(ss.WrappedCEK) == 0 {
		t.Fatal("committed slice has no WrappedCEK")
	}
	cek, err := chunk.UnwrapCEK(fek, ss.WrappedCEK, "df-1", ss.Id, 1)
	if err != nil {
		t.Fatalf("unwrap CEK: %s", err)
	}
	if !bytes.Equal(cek, writeKey) {
		t.Fatal("unwrapped CEK does not match the CEK used by the store")
	}
}

func TestFileReader_UnwrapsCEKOnce(t *testing.T) {
	fek := make([]byte, 32)
	cek := make([]byte, 32)
	if _, err := crand.Read(fek); err != nil {
		t.Fatalf("generate FEK: %s", err)
	}
	if _, err := crand.Read(cek); err != nil {
		t.Fatalf("generate CEK: %s", err)
	}

	const size = 4096
	wrapped, err := chunk.WrapCEK(fek, cek, "df-1", 77, 1)
	if err != nil {
		t.Fatalf("wrap CEK: %s", err)
	}
	m := &encMetaStub{slices: []meta.Slice{{Id: 77, Size: size, Len: size, WrappedCEK: wrapped}}}
	store := &recordingStore{data: bytes.Repeat([]byte{0xCD}, size)}
	conf := newEncTestConf()
	reader := NewDataReader(conf, m, store).(*dataReader)

	attr := &meta.Attr{Encrypted: true, Fek: fek, FekVersion: 1, DriveFileID: "df-1"}
	fr := reader.Open(1, size, attr).(*fileReader)

	buf := make([]byte, size)
	if n, eno := fr.Read(meta.Background(), 0, buf); eno != 0 || n != size {
		t.Fatalf("first read: %d %s", n, eno)
	}
	if !bytes.Equal(buf, store.data) {
		t.Fatal("first read mismatch")
	}

	fr.cekMu.Lock()
	cached, ok := fr.cekCache[77]
	fr.cekMu.Unlock()
	if !ok || !bytes.Equal(cached, cek) {
		t.Fatal("CEK was not cached after the first read")
	}

	// Corrupt the wrapped CEK and drop the cached slice page: a second read
	// must still succeed from the CEK cache without unwrapping again.
	m.mu.Lock()
	m.slices[0].WrappedCEK = []byte("corrupted-wrapped-cek")
	m.mu.Unlock()
	reader.Invalidate(1, 0, size)

	if n, eno := fr.Read(meta.Background(), 0, buf); eno != 0 || n != size {
		t.Fatalf("second read (from CEK cache): %d %s", n, eno)
	}
	if !bytes.Equal(buf, store.data) {
		t.Fatal("second read mismatch")
	}

	store.mu.Lock()
	readKeys := store.readKeys
	store.mu.Unlock()
	if len(readKeys) < 2 {
		t.Fatalf("reader keys = %d, want at least 2", len(readKeys))
	}
	for _, k := range readKeys {
		if !bytes.Equal(k, cek) {
			t.Fatal("store did not receive the unwrapped CEK")
		}
	}
}
