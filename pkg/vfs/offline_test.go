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
type offlineMeta struct {
	meta.Meta
	mu      sync.Mutex
	offline bool
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
	return m.Meta.Write(ctx, inode, indx, off, slice, mtime)
}

// Open mirrors grpcMeta.Open's fast-fail (NFR-OFF-2): a new open needs the hub
// for the permission check, so it fails closed while offline.
func (m *offlineMeta) Open(ctx meta.Context, inode Ino, flags uint32, attr *meta.Attr) syscall.Errno {
	if m.isOffline() {
		return syscall.EIO
	}
	return m.Meta.Open(ctx, inode, flags, attr)
}

// Read fails offline: slice metadata lives on the hub. A read that succeeds
// while offline is therefore served from local caches only (AC-9).
func (m *offlineMeta) Read(ctx meta.Context, inode Ino, indx uint32, slices *[]meta.Slice) syscall.Errno {
	if m.isOffline() {
		return syscall.EIO
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
