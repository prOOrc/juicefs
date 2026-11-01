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

// Writer fail-closed guard (stage 11, decision B4): an encrypted-flagged file
// without a usable FEK must never produce plaintext blocks — writes fail with
// EIO and plaintext slices are rejected at commit, instead of the legacy
// silent plaintext commit.

import (
	"bytes"
	crand "crypto/rand"
	"syscall"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/stretchr/testify/require"
)

// TestEncryptedWriteWithoutFekFails (scenario "Missing FEK stops the write
// instead of leaking plaintext"): writing to an encrypted-flagged file whose
// open carried no FEK fails with EIO and stores nothing.
func TestEncryptedWriteWithoutFekFails(t *testing.T) {
	m := &encMetaStub{}
	store := &recordingStore{}
	conf := newEncTestConf()
	writer := NewDataWriter(conf, m, store, NewDataReader(conf, m, store))

	attr := &meta.Attr{Encrypted: true, FekVersion: 1, DriveFileID: "df-1"} // Fek missing
	fw := writer.Open(1, 0, 0, attr)
	defer fw.Close(meta.Background())

	data := bytes.Repeat([]byte{0xAB}, 4096)
	st := fw.Write(meta.Background(), 0, data)
	require.Equal(t, syscall.EIO, st, "a write without a FEK must fail closed")

	m.mu.Lock()
	written := append([]meta.Slice(nil), m.written...)
	m.mu.Unlock()
	store.mu.Lock()
	writeKey := append([]byte(nil), store.writeKey...)
	store.mu.Unlock()
	require.Empty(t, written, "nothing may be committed")
	require.Empty(t, writeKey, "no slice writer may be created")
}

// TestEncryptedCommitWithoutCEKRejected (decision B4, commit side): a slice
// without a CEK reaching the commit thread of an encrypted-flagged file is
// rejected with EIO instead of being committed as a plaintext record.
func TestEncryptedCommitWithoutCEKRejected(t *testing.T) {
	m := &encMetaStub{}
	store := &recordingStore{}
	conf := newEncTestConf()
	w := NewDataWriter(conf, m, store, NewDataReader(conf, m, store)).(*dataWriter)

	fek := make([]byte, 32)
	if _, err := crand.Read(fek); err != nil {
		t.Fatalf("generate FEK: %s", err)
	}
	attr := &meta.Attr{Encrypted: true, Fek: fek, FekVersion: 1, DriveFileID: "df-1"}
	fw := w.Open(1, 0, 0, attr).(*fileWriter)

	// Inject a plaintext (CEK-less) slice as the commit thread would see it —
	// defense in depth behind the writeChunk guard.
	fw.Lock()
	c := fw.findChunk(0)
	s := &sliceWriter{
		chunk:   c,
		id:      123,
		length:  4096,
		soff:    0,
		slen:    4096,
		writer:  store.NewWriter(0, 0),
		cek:     nil, // plaintext slice
		done:    true,
		notify:  utils.NewCond(&fw.Mutex),
		started: time.Now(),
	}
	c.slices = append(c.slices, s)
	fw.Unlock()

	go c.commitThread()
	require.Eventually(t, func() bool {
		fw.Lock()
		defer fw.Unlock()
		return fw.err == syscall.EIO
	}, 5*time.Second, 10*time.Millisecond, "the plaintext commit must be rejected with EIO")

	m.mu.Lock()
	written := append([]meta.Slice(nil), m.written...)
	m.mu.Unlock()
	require.Empty(t, written, "the plaintext slice must not reach the engine")
}
