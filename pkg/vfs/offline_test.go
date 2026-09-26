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
	crand "crypto/rand"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// offlineMeta wraps a real meta client and simulates hub unavailability:
// while offline, NewSlice/Write fail with EIO (as they would against an
// unreachable hub) and HubState reports offline-connected. All other calls
// pass through to the wrapped client.
//
// It can also simulate an encrypted volume (task 6.8): enableEncryption makes
// Open return a fabricated encrypted attr carrying the current simulated FEK
// instead of passing through, rotateFek bumps the version with a fresh key
// (as if the hub rotated the file's FEK while the client was offline), and
// setOpenErr makes Open fail for every inode (authz deny on the re-Open).
// Every successful Open/Write is recorded so tests can assert ordering.
type offlineMeta struct {
	meta.Meta
	mu      sync.Mutex
	offline bool

	// encryption simulation (task 6.8)
	encrypted bool
	fek       []byte
	fekVer    uint32
	openErr   syscall.Errno // non-zero → Open fails with this errno
	// slice store for the encryption simulation: memkv (TKV) does not persist
	// WrappedCEK/FekVersion, so when encryption is enabled the stub keeps the
	// committed slices itself and serves Read from them — mirroring what the
	// proxy persists.
	slices map[Ino][]meta.Slice

	// recorded events, in call order
	openInos  []Ino
	writeInos []Ino
	writeVers []uint32 // Slice.FekVersion of committed slices (5.8 plumbing)
}

func (m *offlineMeta) isOffline() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.offline
}

func (m *offlineMeta) setOffline(b bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.offline = b
}

// enableEncryption turns on the encrypted-volume simulation with a fresh FEK
// at version 1.
func (m *offlineMeta) enableEncryption() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.encrypted = true
	m.fek = make([]byte, 32)
	_, _ = crand.Read(m.fek)
	m.fekVer = 1
	m.slices = make(map[Ino][]meta.Slice)
}

// rotateFek simulates a hub-side FEK rotation (FR-REV-6 offboarding): the
// version is bumped and a fresh key issued; open handles still hold the old
// (now destroyed) key until they re-Open.
func (m *offlineMeta) rotateFek() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fek = make([]byte, 32)
	_, _ = crand.Read(m.fek)
	m.fekVer++
}

// setOpenErr makes Open fail with e for every inode (authz deny).
func (m *offlineMeta) setOpenErr(e syscall.Errno) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openErr = e
}

func (m *offlineMeta) openInodes() []Ino {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Ino(nil), m.openInos...)
}

// writeEvents returns the inodes and FEK versions of the committed slices, in
// call order.
func (m *offlineMeta) writeEvents() ([]Ino, []uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Ino(nil), m.writeInos...), append([]uint32(nil), m.writeVers...)
}

func (m *offlineMeta) HubState() meta.HubState {
	if m.isOffline() {
		return meta.HubOfflineConnected
	}
	return meta.HubOnline
}

func (m *offlineMeta) NewSlice(ctx meta.Context, id *uint64) syscall.Errno {
	if m.isOffline() {
		return syscall.EIO
	}
	return m.Meta.NewSlice(ctx, id)
}

func (m *offlineMeta) Write(ctx meta.Context, inode Ino, indx, off uint32, slice meta.Slice, mtime time.Time) syscall.Errno {
	if m.isOffline() {
		return syscall.EIO
	}
	m.mu.Lock()
	enc := m.encrypted
	m.mu.Unlock()
	if enc {
		// Keep the slice (WrappedCEK/FekVersion intact) in the stub's own
		// store: memkv would drop the encryption tail (see the field comment).
		m.mu.Lock()
		m.slices[inode] = append(m.slices[inode], slice)
		m.writeInos = append(m.writeInos, inode)
		m.writeVers = append(m.writeVers, slice.FekVersion)
		m.mu.Unlock()
		return 0
	}
	st := m.Meta.Write(ctx, inode, indx, off, slice, mtime)
	if st == 0 {
		m.mu.Lock()
		m.writeInos = append(m.writeInos, inode)
		m.writeVers = append(m.writeVers, slice.FekVersion)
		m.mu.Unlock()
	}
	return st
}

// Open mirrors grpcMeta.Open's fast-fail (NFR-OFF-2): a new open needs the hub
// for the permission check, so it fails closed while offline. With the
// encryption simulation enabled it returns a fabricated encrypted attr
// carrying the current simulated FEK (the memkv volume behind the stub is not
// encrypted).
func (m *offlineMeta) Open(ctx meta.Context, inode Ino, flags uint32, attr *meta.Attr) syscall.Errno {
	if m.isOffline() {
		return syscall.EIO
	}
	m.mu.Lock()
	err := m.openErr
	enc := m.encrypted
	fek := m.fek
	ver := m.fekVer
	m.mu.Unlock()
	if err != 0 {
		return err
	}
	if enc {
		attr.Encrypted = true
		attr.DriveFileID = "test-drive"
		attr.Fek = append([]byte(nil), fek...) // fresh copy per open, as grpcMeta issues
		attr.FekVersion = ver
		m.mu.Lock()
		m.openInos = append(m.openInos, inode)
		m.mu.Unlock()
		return 0
	}
	st := m.Meta.Open(ctx, inode, flags, attr)
	if st == 0 {
		m.mu.Lock()
		m.openInos = append(m.openInos, inode)
		m.mu.Unlock()
	}
	return st
}

// Read fails offline: slice metadata lives on the hub. A read that succeeds
// while offline is therefore served from local caches only (AC-9). With the
// encryption simulation enabled it serves the stub's own slice store (the
// test files live in a single chunk, so every index returns the whole list).
func (m *offlineMeta) Read(ctx meta.Context, inode Ino, indx uint32, slices *[]meta.Slice) syscall.Errno {
	if m.isOffline() {
		return syscall.EIO
	}
	m.mu.Lock()
	enc := m.encrypted
	var stored []meta.Slice
	if enc {
		stored = append([]meta.Slice(nil), m.slices[inode]...)
	}
	m.mu.Unlock()
	if enc {
		*slices = stored
		return 0
	}
	return m.Meta.Read(ctx, inode, indx, slices)
}

// TestOfflineWrites_ReplayOnReconnect verifies NFR-OFF-1/3 at the VFS level:
// a write made while the hub is unreachable succeeds and is journaled; after
// the hub recovers, ReplayWriteJournal re-applies it through the normal write
// path and the data reads back byte-identical. The original offline slice may
// also commit on its own (prepareID retries until the hub is back) — both
// commits carry identical content, so the read result is the same either way
// (buildSlice resolves overlaps last-write-wins).
func TestOfflineWrites_ReplayOnReconnect(t *testing.T) {
	mp := t.TempDir()
	metaConf := meta.DefaultConf()
	metaConf.MountPoint = mp
	m := meta.NewClient("memkv://", metaConf)
	format := &meta.Format{
		Name:        "test",
		UUID:        uuid.New().String(),
		Storage:     "mem",
		BlockSize:   4096,
		Compression: "lz4",
		DirStats:    true,
	}
	require.NoError(t, m.Init(format, true))

	off := &offlineMeta{Meta: m}
	conf := &Config{
		Meta:    metaConf,
		Format:  *format,
		Version: "Juicefs",
		Chunk: &chunk.Config{
			BlockSize:   format.BlockSize * 1024,
			Compress:    format.Compression,
			MaxUpload:   2,
			MaxDownload: 200,
			BufferSize:  30 << 20,
			CacheSize:   10 << 20,
			CacheDir:    "memory",
		},
		FuseOpts:         &FuseOptions{},
		WriteJournalPath: filepath.Join(t.TempDir(), "write_journal.bin"),
		HubState:         off.HubState,
	}
	blob, err := object.CreateStorage("mem", "", "", "", "")
	require.NoError(t, err)
	registry := prometheus.NewRegistry()
	registerer := prometheus.WrapRegistererWithPrefix("juicefs_", registry)
	store := chunk.NewCachedStore(blob, *conf.Chunk, registerer)
	v := NewVFS(conf, off, store, registerer, registry)

	ctx := NewLogContext(meta.NewContext(10, 1, []uint32{2, 3}))
	fe, e := v.Mknod(ctx, 1, "f", 0644|syscall.S_IFREG, 0, 0)
	require.Zero(t, e)
	_, fh, e := v.Open(ctx, fe.Inode, syscall.O_RDWR)
	require.Zero(t, e)

	data := make([]byte, 8<<10)
	_, err = crand.Read(data)
	require.NoError(t, err)

	// Hub goes down: the write must still succeed (NFR-OFF-1) and be journaled.
	off.setOffline(true)
	require.Equal(t, meta.HubOfflineConnected, off.HubState())
	e = v.Write(ctx, fe.Inode, data, 0, fh)
	require.Zero(t, e, "offline write must not fail (NFR-OFF-1)")
	require.NotNil(t, v.writeJournal)
	require.False(t, v.writeJournal.Empty(), "offline write must be journaled")

	// Hub recovers: replay re-applies the journaled write (NFR-OFF-3).
	off.setOffline(false)
	v.ReplayWriteJournal()
	require.True(t, v.writeJournal.Empty(), "journal must be truncated after a full replay")

	require.Zero(t, v.Flush(ctx, fe.Inode, fh, 0))
	buf := make([]byte, len(data))
	n, e := v.Read(ctx, fe.Inode, buf, 0, fh)
	require.Zero(t, e)
	require.Equal(t, len(data), n)
	require.True(t, bytes.Equal(data, buf), "read-back after replay must match the offline write")

	// A second replay on the empty journal is a no-op.
	v.ReplayWriteJournal()
	require.True(t, v.writeJournal.Empty())
}

// TestReplayAfterRotation_UnderNewFEK verifies design A2 (task 6.8): if the
// file's FEK is rotated while the client is offline (typical offboarding,
// FR-REV-6), the handle holds a destroyed key — replay must re-Open the file
// first and commit the journaled records under the CURRENT FEK. The journal is
// plaintext, so no re-wrap of journaled data is needed: it is re-encrypted at
// commit under the new key and reads back byte-identical.
func TestReplayAfterRotation_UnderNewFEK(t *testing.T) {
	mp := t.TempDir()
	metaConf := meta.DefaultConf()
	metaConf.MountPoint = mp
	m := meta.NewClient("memkv://", metaConf)
	format := &meta.Format{
		Name:        "test",
		UUID:        uuid.New().String(),
		Storage:     "mem",
		BlockSize:   4096,
		Compression: "lz4",
		DirStats:    true,
	}
	require.NoError(t, m.Init(format, true))

	off := &offlineMeta{Meta: m}
	off.enableEncryption()
	conf := &Config{
		Meta:    metaConf,
		Format:  *format,
		Version: "Juicefs",
		Chunk: &chunk.Config{
			BlockSize:   format.BlockSize * 1024,
			Compress:    format.Compression,
			MaxUpload:   2,
			MaxDownload: 200,
			BufferSize:  30 << 20,
			CacheSize:   10 << 20,
			CacheDir:    "memory",
		},
		FuseOpts:         &FuseOptions{},
		WriteJournalPath: filepath.Join(t.TempDir(), "write_journal.bin"),
		HubState:         off.HubState,
	}
	blob, err := object.CreateStorage("mem", "", "", "", "")
	require.NoError(t, err)
	registry := prometheus.NewRegistry()
	registerer := prometheus.WrapRegistererWithPrefix("juicefs_", registry)
	store := chunk.NewCachedStore(blob, *conf.Chunk, registerer)
	v := NewVFS(conf, off, store, registerer, registry)

	ctx := NewLogContext(meta.NewContext(10, 1, []uint32{2, 3}))
	fe, e := v.Mknod(ctx, 1, "f", 0644|syscall.S_IFREG, 0, 0)
	require.Zero(t, e)
	_, fh, e := v.Open(ctx, fe.Inode, syscall.O_RDWR)
	require.Zero(t, e)
	require.Equal(t, []Ino{fe.Inode}, off.openInodes(), "the initial open must be recorded")

	data := make([]byte, 8<<10)
	_, err = crand.Read(data)
	require.NoError(t, err)

	// Hub goes down: the write must still succeed (NFR-OFF-1) and be journaled.
	off.setOffline(true)
	e = v.Write(ctx, fe.Inode, data, 0, fh)
	require.Zero(t, e, "offline write must not fail (NFR-OFF-1)")
	require.False(t, v.writeJournal.Empty(), "offline write must be journaled")

	// The file's FEK is rotated while the client is offline (FR-REV-6): the
	// handle now holds a destroyed key.
	off.rotateFek()

	// Hub recovers: replay re-Opens the file first (fresh FEK) and then
	// applies the records under the new key.
	off.setOffline(false)
	v.ReplayWriteJournal()

	// Re-Open-first: Open was called for the journaled inode again, before any
	// record reached the write path.
	require.Equal(t, []Ino{fe.Inode, fe.Inode}, off.openInodes(), "re-Open must happen during replay")
	preInos, _ := off.writeEvents()
	require.Empty(t, preInos, "no record may be committed before the re-Open")
	require.True(t, v.writeJournal.Empty(), "journal must be truncated after a full replay")

	require.Zero(t, v.Flush(ctx, fe.Inode, fh, 0))
	// Every committed slice carries the NEW FEK version (5.8 plumbing).
	inos, vers := off.writeEvents()
	require.NotEmpty(t, vers)
	for i, ver := range vers {
		require.Equal(t, fe.Inode, inos[i])
		require.EqualValues(t, 2, ver, "slice %d must be committed under the rotated FEK", i)
	}

	// The content reads back byte-identical: re-encrypted at commit under the
	// new key; the overlapping identical slices resolve last-write-wins.
	buf := make([]byte, len(data))
	n, e := v.Read(ctx, fe.Inode, buf, 0, fh)
	require.Zero(t, e)
	require.Equal(t, len(data), n)
	require.True(t, bytes.Equal(data, buf), "read-back after rotation replay must match the offline write")
}

// TestReplay_Denied_KeepsJournal verifies design A2 (task 6.8): if the
// re-Open during replay is denied (authz / EACCES), the journal must be kept
// (not truncated), the file's records must not reach the write path with a
// stale key, and the conflict is surfaced to the user via a prominent log
// line (not silently). The next mount retries the replay after loadAllHandles.
func TestReplay_Denied_KeepsJournal(t *testing.T) {
	mp := t.TempDir()
	metaConf := meta.DefaultConf()
	metaConf.MountPoint = mp
	m := meta.NewClient("memkv://", metaConf)
	format := &meta.Format{
		Name:        "test",
		UUID:        uuid.New().String(),
		Storage:     "mem",
		BlockSize:   4096,
		Compression: "lz4",
		DirStats:    true,
	}
	require.NoError(t, m.Init(format, true))

	off := &offlineMeta{Meta: m}
	off.enableEncryption()
	conf := &Config{
		Meta:    metaConf,
		Format:  *format,
		Version: "Juicefs",
		Chunk: &chunk.Config{
			BlockSize:   format.BlockSize * 1024,
			Compress:    format.Compression,
			MaxUpload:   2,
			MaxDownload: 200,
			BufferSize:  30 << 20,
			CacheSize:   10 << 20,
			CacheDir:    "memory",
		},
		FuseOpts:         &FuseOptions{},
		WriteJournalPath: filepath.Join(t.TempDir(), "write_journal.bin"),
		HubState:         off.HubState,
	}
	blob, err := object.CreateStorage("mem", "", "", "", "")
	require.NoError(t, err)
	registry := prometheus.NewRegistry()
	registerer := prometheus.WrapRegistererWithPrefix("juicefs_", registry)
	store := chunk.NewCachedStore(blob, *conf.Chunk, registerer)
	v := NewVFS(conf, off, store, registerer, registry)

	ctx := NewLogContext(meta.NewContext(10, 1, []uint32{2, 3}))
	fe, e := v.Mknod(ctx, 1, "f", 0644|syscall.S_IFREG, 0, 0)
	require.Zero(t, e)
	_, fh, e := v.Open(ctx, fe.Inode, syscall.O_RDWR)
	require.Zero(t, e)

	data := make([]byte, 4<<10)
	_, err = crand.Read(data)
	require.NoError(t, err)

	// Hub goes down: the write succeeds and is journaled (NFR-OFF-1).
	off.setOffline(true)
	e = v.Write(ctx, fe.Inode, data, 0, fh)
	require.Zero(t, e)
	require.False(t, v.writeJournal.Empty())

	// The hub now denies access to the file (revocation / offboarding).
	off.setOpenErr(syscall.EACCES)
	off.setOffline(false)

	// Replay must not panic: the re-Open is denied, the conflict is logged
	// prominently, and the journal is kept.
	v.ReplayWriteJournal()

	require.False(t, v.writeJournal.Empty(), "journal must be kept when the re-Open is denied")
	inos, _ := off.writeEvents()
	for _, ino := range inos {
		require.NotEqual(t, fe.Inode, ino, "no record of a denied file may reach the write path")
	}
}

// TestOfflineConnected_ReadFromCache verifies AC-9 / NFR-OFF-1 at the VFS
// level: after the hub goes down, reads of an already-open file are served
// from local caches (slice metadata in RAM + data in the disk cache) without
// any hub round-trip, while a new Open fails closed (NFR-OFF-2).
func TestOfflineConnected_ReadFromCache(t *testing.T) {
	mp := t.TempDir()
	metaConf := meta.DefaultConf()
	metaConf.MountPoint = mp
	m := meta.NewClient("memkv://", metaConf)
	format := &meta.Format{
		Name:        "test",
		UUID:        uuid.New().String(),
		Storage:     "mem",
		BlockSize:   4096,
		Compression: "lz4",
		DirStats:    true,
	}
	require.NoError(t, m.Init(format, true))

	off := &offlineMeta{Meta: m}
	conf := &Config{
		Meta:    metaConf,
		Format:  *format,
		Version: "Juicefs",
		Chunk: &chunk.Config{
			BlockSize:   format.BlockSize * 1024,
			Compress:    format.Compression,
			MaxUpload:   2,
			MaxDownload: 200,
			BufferSize:  30 << 20,
			CacheSize:   10 << 20,
			CacheDir:    t.TempDir(), // real disk cache (AC-9: read from local cache)
		},
		FuseOpts: &FuseOptions{},
	}
	blob, err := object.CreateStorage("mem", "", "", "", "")
	require.NoError(t, err)
	registry := prometheus.NewRegistry()
	registerer := prometheus.WrapRegistererWithPrefix("juicefs_", registry)
	store := chunk.NewCachedStore(blob, *conf.Chunk, registerer)
	v := NewVFS(conf, off, store, registerer, registry)

	ctx := NewLogContext(meta.NewContext(10, 1, []uint32{2, 3}))
	fe, e := v.Mknod(ctx, 1, "f", 0644|syscall.S_IFREG, 0, 0)
	require.Zero(t, e)
	_, fh, e := v.Open(ctx, fe.Inode, syscall.O_RDWR)
	require.Zero(t, e)

	data := make([]byte, 256<<10)
	_, err = crand.Read(data)
	require.NoError(t, err)
	require.Zero(t, v.Write(ctx, fe.Inode, data, 0, fh))
	require.Zero(t, v.Flush(ctx, fe.Inode, fh, 0))

	// Prime the local caches: slice metadata in RAM, block data on disk.
	buf := make([]byte, len(data))
	n, e := v.Read(ctx, fe.Inode, buf, 0, fh)
	require.Zero(t, e)
	require.Equal(t, len(data), n)
	require.True(t, bytes.Equal(data, buf))

	// Hub goes down: re-reading the open file must work from local caches
	// (offlineMeta.Read fails offline, so any hub round-trip would break this).
	off.setOffline(true)
	n, e = v.Read(ctx, fe.Inode, buf, 0, fh)
	require.Zero(t, e, "offline read of an open file must be served from local caches")
	require.Equal(t, len(data), n)
	require.True(t, bytes.Equal(data, buf))

	// A new Open fails closed while offline (NFR-OFF-2).
	fe2, e := v.Mknod(ctx, 1, "g", 0644|syscall.S_IFREG, 0, 0)
	require.Zero(t, e) // Mknod goes through the passthrough (memkv) in this stub
	_, _, e = v.Open(ctx, fe2.Inode, syscall.O_RDONLY)
	require.Equal(t, syscall.EIO, e, "new Open must fail closed while offline")
}
