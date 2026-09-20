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

package chunk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/object"
)

func testPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/256)
	}
	return b
}

func findCacheFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && info.Mode().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	return files
}

func getRawObject(t *testing.T, storage object.ObjectStorage, key string) []byte {
	t.Helper()
	in, err := storage.Get(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatalf("get %s: %s", key, err)
	}
	raw, err := io.ReadAll(in)
	_ = in.Close()
	if err != nil {
		t.Fatalf("read %s: %s", key, err)
	}
	return raw
}

func TestEncryptedStore_CiphertextInS3AndCache(t *testing.T) {
	mem, _ := object.CreateStorage("mem", "", "", "", "")
	conf := defaultConf
	conf.BlockSize = 4 << 10
	conf.CacheFullBlock = true
	conf.CacheDir = filepath.Join(os.TempDir(), fmt.Sprintf("diskCache-enc-%d", os.Getpid()))
	_ = os.RemoveAll(conf.CacheDir)
	store := NewCachedStore(mem, conf, nil)

	cek := make([]byte, 32)
	for i := range cek {
		cek[i] = byte(i*3 + 1)
	}
	id := uint64(9001)
	data := testPattern(2*conf.BlockSize + 100)

	w := store.NewWriterWithKey(id, 0, cek)
	if _, err := w.WriteAt(data, 0); err != nil {
		t.Fatalf("write: %s", err)
	}
	if err := w.Finish(len(data)); err != nil {
		t.Fatalf("finish: %s", err)
	}
	defer store.Remove(id, len(data))

	// Object storage must contain AGDF ciphertext only (NFR-SEC-1).
	keys := sliceForRead(id, len(data), store.(*cachedStore)).keys()
	if len(keys) != 3 {
		t.Fatalf("blocks = %d, want 3", len(keys))
	}
	for _, key := range keys {
		raw := getRawObject(t, mem, key)
		if IsLegacyBlock(raw) {
			t.Fatalf("object %s is plaintext, want AGDF ciphertext", key)
		}
		if bytes.Contains(raw, data[:32]) {
			t.Fatalf("object %s contains a plaintext fragment", key)
		}
	}

	// Read back the full content with the CEK.
	r := store.NewReaderWithKey(id, len(data), cek)
	got := make([]byte, len(data))
	if n, err := r.ReadAt(context.Background(), NewPage(got), 0); err != nil || n != len(data) {
		t.Fatalf("read all: %d %s", n, err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("full read mismatch")
	}
	// Partial read with a non-zero block offset.
	p := NewPage(make([]byte, 100))
	n, err := r.ReadAt(context.Background(), p, conf.BlockSize/2)
	if err != nil || n != 100 {
		t.Fatalf("read partial: %d %s", n, err)
	}
	if !bytes.Equal(p.Data[:n], data[conf.BlockSize/2:conf.BlockSize/2+100]) {
		t.Fatal("partial read mismatch")
	}

	// A wrong CEK must fail closed (NFR-SEC-11).
	badCek := make([]byte, 32)
	rBad := store.NewReaderWithKey(id, len(data), badCek)
	if _, err := rBad.ReadAt(context.Background(), NewPage(make([]byte, 100)), 0); err == nil {
		t.Fatal("read with a wrong CEK must fail")
	}

	// The disk cache must hold ciphertext only (NFR-SEC-1). The flush is
	// asynchronous, so poll for the cache files.
	var files []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files = findCacheFiles(t, conf.CacheDir)
		blocks := 0
		for _, f := range files {
			if filepath.Base(f) != ".lock" {
				blocks++
			}
		}
		if blocks >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(files) == 0 {
		t.Fatal("no cache files found")
	}
	for _, f := range files {
		if filepath.Base(f) == ".lock" {
			continue // cache dir lock file, not a cached block
		}
		raw, err := os.ReadFile(f)
		if err != nil || len(raw) < 5 {
			continue
		}
		if IsLegacyBlock(raw) {
			t.Fatalf("cache file %s holds plaintext", f)
		}
		if bytes.Contains(raw, data[:32]) {
			t.Fatalf("cache file %s contains a plaintext fragment", f)
		}
	}
}

func TestLegacyStore_Unchanged(t *testing.T) {
	mem, _ := object.CreateStorage("mem", "", "", "", "")
	conf := defaultConf
	conf.BlockSize = 4 << 10
	conf.CacheDir = filepath.Join(os.TempDir(), fmt.Sprintf("diskCache-legacy-%d", os.Getpid()))
	_ = os.RemoveAll(conf.CacheDir)
	store := NewCachedStore(mem, conf, nil)

	id := uint64(9002)
	data := testPattern(2*conf.BlockSize + 100)
	w := store.NewWriter(id, 0) // nil key → legacy plaintext
	if _, err := w.WriteAt(data, 0); err != nil {
		t.Fatalf("write: %s", err)
	}
	if err := w.Finish(len(data)); err != nil {
		t.Fatalf("finish: %s", err)
	}
	defer store.Remove(id, len(data))

	keys := sliceForRead(id, len(data), store.(*cachedStore)).keys()
	for _, key := range keys {
		raw := getRawObject(t, mem, key)
		if !IsLegacyBlock(raw) {
			t.Fatalf("legacy object %s must not carry the AGDF magic", key)
		}
	}

	r := store.NewReader(id, len(data))
	got := make([]byte, len(data))
	if n, err := r.ReadAt(context.Background(), NewPage(got), 0); err != nil || n != len(data) {
		t.Fatalf("read all: %d %s", n, err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("legacy read mismatch")
	}
}
