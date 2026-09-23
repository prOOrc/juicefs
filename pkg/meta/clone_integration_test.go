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

// Stage 5 integration suite (tasks.md 5.4/5.5): Clone and CopyFileRange of
// encrypted files through the real proxy stack (Redis + MetaProxyServer + fake
// KeyManager + gRPC client). The data path is served by an in-memory chunk store
// standing in for S3+AGDF, so both files can be read back after the operation;
// zero-copy (AC-7) is proven at the metadata level — identical slice IDs and
// incremented refcounts mean the S3 objects are shared, never rewritten.

import (
	"bytes"
	"context"
	"io"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	chunkenc "github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/stretchr/testify/require"
)

// sliceIDSet returns the set of slice IDs referenced by all chunk lists of inode.
func sliceIDSet(t *testing.T, m Meta, inode Ino) map[uint64]bool {
	t.Helper()
	var attr Attr
	require.Equal(t, syscall.Errno(0), m.GetAttr(Background(), inode, &attr))
	ids := make(map[uint64]bool)
	for indx := uint32(0); indx <= uint32(attr.Length/ChunkSize); indx++ {
		var cs []Slice
		require.Equal(t, syscall.Errno(0), m.Read(Background(), inode, indx, &cs))
		for _, s := range cs {
			ids[s.Id] = true
		}
	}
	return ids
}

// TestClone_Rewrap_NoS3Rewrite (task 5.4, FR-TEST-14, AC-7): cloning an encrypted
// file shares the slice objects verbatim (zero-copy — S3 objects are never
// rewritten) and gives the target its own FEK: the shared CEKs are re-wrapped
// under it. Both files stay readable with their own keys.
func TestClone_Rewrap_NoS3Rewrite(t *testing.T) {
	env := newEncryptTestEnv(t, 2, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	var srcIno Ino
	var srcAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "src.exr", 0644, 022, 0, &srcIno, &srcAttr))
	require.True(t, srcAttr.Encrypted)

	gm, ok := env.client.(*grpcMeta)
	require.True(t, ok)
	srcFek, srcDFID, srcVer, err := gm.ResolveFileKey(cctx, srcIno)
	require.NoError(t, err)
	require.Equal(t, srcAttr.DriveFileID, srcDFID)

	// Two chunks of encrypted slices: the slice records carry AGCK tails under
	// the source FEK (the data itself lives in S3 — not needed for AC-7).
	const size = ChunkSize
	cek1, err := chunkenc.NewCEK()
	require.NoError(t, err)
	cek2, err := chunkenc.NewCEK()
	require.NoError(t, err)
	wrapSrc := func(cek []byte, id uint64) []byte {
		blob, err := chunkenc.WrapCEK(srcFek, cek, srcDFID, id, srcVer)
		require.NoError(t, err)
		return blob
	}
	require.Equal(t, syscall.Errno(0), env.client.Write(cctx, srcIno, 0, 0, Slice{Id: 7001, Size: size, Len: size, WrappedCEK: wrapSrc(cek1, 7001)}, time.Now()))
	require.Equal(t, syscall.Errno(0), env.client.Write(cctx, srcIno, 1, 0, Slice{Id: 7002, Size: size, Len: size, WrappedCEK: wrapSrc(cek2, 7002)}, time.Now()))

	srcIDs := sliceIDSet(t, env.meta, srcIno)
	require.Equal(t, map[uint64]bool{7001: true, 7002: true}, srcIDs)

	// Clone through the proxy (FR-OP-1..3).
	var count, total uint64
	var dstIno Ino
	st := env.client.Clone(cctx, RootInode, srcIno, RootInode, "dst.exr", 0, 022, 1, &count, &total, &dstIno)
	require.Equal(t, syscall.Errno(0), st)
	require.NotZero(t, dstIno)
	require.NotEqual(t, srcIno, dstIno)

	// AC-7: zero-copy — the target references exactly the same slice objects and
	// their refcounts went up (shared, not copied). The refcount field stores only
	// the EXTRA references beyond the original writer (absent == 1), so "1" means
	// two owners.
	require.Equal(t, srcIDs, sliceIDSet(t, env.meta, dstIno))
	for _, id := range []uint64{7001, 7002} {
		val, err := env.meta.rdb.HGet(ctx, env.meta.sliceRefs(), env.meta.sliceKey(id, size)).Result()
		require.NoError(t, err)
		require.Equal(t, "1", val, "slice %d must be shared (one extra reference)", id)
	}

	// The target got its own FEK (FR-OP-1).
	var dstAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.GetAttr(cctx, dstIno, &dstAttr))
	require.True(t, dstAttr.Encrypted)
	require.NotEqual(t, srcDFID, dstAttr.DriveFileID)
	require.NotEqual(t, srcAttr.WrappedFek, dstAttr.WrappedFek)

	// The shared CEKs now unwrap under the TARGET FEK to the original keys.
	dstFek, dstDFID, dstVer, err := gm.ResolveFileKey(cctx, dstIno)
	require.NoError(t, err)
	wantCEKs := map[uint64][]byte{7001: cek1, 7002: cek2}
	for indx := uint32(0); indx < 2; indx++ {
		var cs []Slice
		require.Equal(t, syscall.Errno(0), env.meta.Read(ctx, dstIno, indx, &cs))
		require.Len(t, cs, 1)
		s := cs[0]
		cek, err := chunkenc.UnwrapCEK(dstFek, s.WrappedCEK, dstDFID, s.Id, dstVer)
		require.NoError(t, err, "dst slice %d must unwrap under the target FEK", s.Id)
		require.Equal(t, wantCEKs[s.Id], cek)
	}

	// The source is untouched: still readable under its own FEK.
	for indx := uint32(0); indx < 2; indx++ {
		var cs []Slice
		require.Equal(t, syscall.Errno(0), env.meta.Read(ctx, srcIno, indx, &cs))
		s := cs[0]
		cek, err := chunkenc.UnwrapCEK(srcFek, s.WrappedCEK, srcDFID, s.Id, srcVer)
		require.NoError(t, err, "src slice %d must still unwrap under the source FEK", s.Id)
		require.Equal(t, wantCEKs[s.Id], cek)
	}

	// KeyManager audit: Create(src)+Create(dst); GetFileFEK for the two
	// ResolveFileKey calls plus the clone's src (Read) and dst (Write).
	require.Equal(t, 2, env.keyMgr.createCalls)
	require.Equal(t, 4, env.keyMgr.getFekCalls)
}

// TestClone_LegacyFile (task 5.4): cloning a plaintext file on an unencrypted
// volume behaves exactly as before the stage — no re-wrap, KeyManager untouched.
func TestClone_LegacyFile(t *testing.T) {
	env := newEncryptTestEnv(t, 3, false)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	var srcIno Ino
	var srcAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "plain.txt", 0644, 022, 0, &srcIno, &srcAttr))
	require.False(t, srcAttr.Encrypted)

	const size = ChunkSize
	require.Equal(t, syscall.Errno(0), env.client.Write(cctx, srcIno, 0, 0, Slice{Id: 9001, Size: size, Len: size}, time.Now()))
	require.Equal(t, syscall.Errno(0), env.client.Write(cctx, srcIno, 1, 0, Slice{Id: 9002, Size: size, Len: size}, time.Now()))

	srcIDs := sliceIDSet(t, env.meta, srcIno)

	var count, total uint64
	var dstIno Ino
	st := env.client.Clone(cctx, RootInode, srcIno, RootInode, "plain-copy.txt", 0, 022, 1, &count, &total, &dstIno)
	require.Equal(t, syscall.Errno(0), st)
	require.NotZero(t, dstIno)

	var dstAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.GetAttr(cctx, dstIno, &dstAttr))
	require.False(t, dstAttr.Encrypted)
	require.Empty(t, dstAttr.WrappedFek)

	// Zero-copy as before; the records stay legacy (no AGCK tails).
	require.Equal(t, srcIDs, sliceIDSet(t, env.meta, dstIno))
	var cs []Slice
	require.Equal(t, syscall.Errno(0), env.meta.Read(ctx, dstIno, 0, &cs))
	require.Len(t, cs, 1)
	require.Empty(t, cs[0].WrappedCEK)

	require.Equal(t, 0, env.keyMgr.createCalls, "legacy clone must not call CreateFileKey")
	require.Equal(t, 0, env.keyMgr.getFekCalls, "legacy clone must not call GetFileFEK")
}

// cfrStore is an in-memory chunk store serving per-slice plaintext to readers —
// it stands in for S3+AGDF so the test can read both files after CopyFileRange.
type cfrStore struct {
	mu   sync.Mutex
	data map[uint64][]byte // slice id -> plaintext
}

func (s *cfrStore) NewReader(id uint64, length int) chunkenc.Reader {
	return s.NewReaderWithKey(id, length, nil)
}

func (s *cfrStore) NewWriter(id uint64, tierID uint8) chunkenc.Writer {
	return s.NewWriterWithKey(id, tierID, nil)
}

func (s *cfrStore) NewReaderWithKey(id uint64, length int, key []byte) chunkenc.Reader {
	s.mu.Lock()
	data := append([]byte(nil), s.data[id]...)
	s.mu.Unlock()
	return &cfrReader{data: data}
}

func (s *cfrStore) NewWriterWithKey(id uint64, tierID uint8, key []byte) chunkenc.Writer {
	return &cfrWriter{id: id} // writes are not exercised in this test
}

func (s *cfrStore) Remove(id uint64, length int) error { return nil }
func (s *cfrStore) FillCache(id uint64, length uint32) error {
	return nil
}
func (s *cfrStore) EvictCache(id uint64, length uint32) error { return nil }
func (s *cfrStore) CheckCache(id uint64, length uint32, handler func(bool, string, int)) error {
	return nil
}
func (s *cfrStore) UsedMemory() int64                  { return 0 }
func (s *cfrStore) UpdateLimit(upload, download int64) {}
func (s *cfrStore) BlobStorage() object.ObjectStorage  { return nil }

type cfrReader struct{ data []byte }

func (r *cfrReader) ReadAt(ctx context.Context, p *chunkenc.Page, off int) (int, error) {
	if off >= len(r.data) {
		return 0, io.EOF
	}
	return copy(p.Data, r.data[off:]), nil
}

type cfrWriter struct{ id uint64 }

func (w *cfrWriter) WriteAt(p []byte, off int64) (int, error) { return len(p), nil }
func (w *cfrWriter) ID() uint64                               { return w.id }
func (w *cfrWriter) SetID(id uint64)                          { w.id = id }
func (w *cfrWriter) SetWriteback(enabled bool)                {}
func (w *cfrWriter) FlushTo(offset int) error                 { return nil }
func (w *cfrWriter) Finish(length int) error                  { return nil }
func (w *cfrWriter) Abort()                                   {}

// readSliceData reads the s.Len bytes of slice record s from the store using cek.
func readSliceData(t *testing.T, store *cfrStore, s Slice, cek []byte) []byte {
	t.Helper()
	r := store.NewReaderWithKey(s.Id, int(s.Size), cek)
	buf := make([]byte, s.Len)
	page := &chunkenc.Page{Data: buf}
	n, err := r.ReadAt(context.Background(), page, int(s.Off))
	require.NoError(t, err)
	require.Equal(t, int(s.Len), n)
	return buf
}

// TestCopyFileRange_Encrypted (task 5.5, FR-OP-4/5, FR-TEST-15): CopyFileRange of
// an encrypted range shares the source slices and re-wraps their CEKs under the
// target FEK; the target sees exactly the copied bytes (subset — the rest of the
// source is not reachable through it), and the source stays intact under its own
// FEK. Both files are read back through a chunk store after the operation.
func TestCopyFileRange_Encrypted(t *testing.T) {
	env := newEncryptTestEnv(t, 8, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	const size = ChunkSize // 1 MiB
	plainA := bytes.Repeat([]byte{0xA1}, size)
	plainB := bytes.Repeat([]byte{0xB2}, size)
	plainC := bytes.Repeat([]byte{0xC3}, size)
	srcPlain := append(append(append([]byte{}, plainA...), plainB...), plainC...)

	store := &cfrStore{data: map[uint64][]byte{8001: plainA, 8002: plainB, 8003: plainC}}

	var srcIno Ino
	var srcAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "cfr-src.exr", 0644, 022, 0, &srcIno, &srcAttr))
	require.True(t, srcAttr.Encrypted)

	gm, ok := env.client.(*grpcMeta)
	require.True(t, ok)
	srcFek, srcDFID, srcVer, err := gm.ResolveFileKey(cctx, srcIno)
	require.NoError(t, err)

	cekA, err := chunkenc.NewCEK()
	require.NoError(t, err)
	cekB, err := chunkenc.NewCEK()
	require.NoError(t, err)
	cekC, err := chunkenc.NewCEK()
	require.NoError(t, err)
	wrapSrc := func(cek []byte, id uint64) []byte {
		blob, err := chunkenc.WrapCEK(srcFek, cek, srcDFID, id, srcVer)
		require.NoError(t, err)
		return blob
	}
	// Three full chunks.
	require.Equal(t, syscall.Errno(0), env.client.Write(cctx, srcIno, 0, 0, Slice{Id: 8001, Size: size, Len: size, WrappedCEK: wrapSrc(cekA, 8001)}, time.Now()))
	require.Equal(t, syscall.Errno(0), env.client.Write(cctx, srcIno, 1, 0, Slice{Id: 8002, Size: size, Len: size, WrappedCEK: wrapSrc(cekB, 8002)}, time.Now()))
	require.Equal(t, syscall.Errno(0), env.client.Write(cctx, srcIno, 2, 0, Slice{Id: 8003, Size: size, Len: size, WrappedCEK: wrapSrc(cekC, 8003)}, time.Now()))

	var dstIno Ino
	var dstAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "cfr-dst.exr", 0644, 022, 0, &dstIno, &dstAttr))
	require.True(t, dstAttr.Encrypted)

	// Copy [512KiB, 512KiB+2MiB) of the source to offset 0 of the target: spans
	// three source chunks and lands in two target chunks (FR-TEST-15 subset).
	const offIn = size / 2
	const copySize = 2 * size
	var copied, outLength uint64
	st := env.client.CopyFileRange(cctx, srcIno, offIn, dstIno, 0, copySize, 0, &copied, &outLength)
	require.Equal(t, syscall.Errno(0), st)
	require.EqualValues(t, copySize, copied)
	require.EqualValues(t, copySize, outLength)

	dstFek, dstDFID, dstVer, err := gm.ResolveFileKey(cctx, dstIno)
	require.NoError(t, err)
	require.NotEqual(t, srcDFID, dstAttr.DriveFileID)

	// Target layout: exactly the copied range, spread over two chunk lists.
	var cs0, cs1 []Slice
	require.Equal(t, syscall.Errno(0), env.meta.Read(ctx, dstIno, 0, &cs0))
	require.Equal(t, syscall.Errno(0), env.meta.Read(ctx, dstIno, 1, &cs1))
	require.Len(t, cs0, 2)
	require.Len(t, cs1, 2)

	// Every target slice unwraps under the TARGET FEK to the source's original CEK.
	wantCEK := map[uint64][]byte{8001: cekA, 8002: cekB, 8003: cekC}
	for _, cs := range [][]Slice{cs0, cs1} {
		for _, s := range cs {
			require.NotEmpty(t, s.WrappedCEK)
			cek, err := chunkenc.UnwrapCEK(dstFek, s.WrappedCEK, dstDFID, s.Id, dstVer)
			require.NoError(t, err, "dst slice %d must unwrap under the target FEK", s.Id)
			require.Equal(t, wantCEK[s.Id], cek)
		}
	}

	// Read both files back (tasks.md 5.5): the target sees exactly the copied
	// bytes; the source is intact under its own FEK.
	readFile := func(ino Ino, chunks uint32, fek []byte, dfid string, ver uint32) []byte {
		var out []byte
		for indx := uint32(0); indx < chunks; indx++ {
			var cs []Slice
			require.Equal(t, syscall.Errno(0), env.meta.Read(ctx, ino, indx, &cs))
			for _, s := range cs {
				cek, err := chunkenc.UnwrapCEK(fek, s.WrappedCEK, dfid, s.Id, ver)
				require.NoError(t, err)
				out = append(out, readSliceData(t, store, s, cek)...)
			}
		}
		return out
	}
	require.Equal(t, srcPlain[offIn:offIn+copySize], readFile(dstIno, 2, dstFek, dstDFID, dstVer))
	require.Equal(t, srcPlain, readFile(srcIno, 3, srcFek, srcDFID, srcVer))

	// KeyManager audit: only the two Create calls; GetFileFEK for the two
	// ResolveFileKey calls plus the re-wrap's src (Read) and dst (Write).
	require.Equal(t, 2, env.keyMgr.createCalls)
	require.Equal(t, 4, env.keyMgr.getFekCalls)
}

// TestClone_Subset (FR-TEST-15, FR-OP-3): copying a subset of an encrypted file
// (a range inside a single chunk) gives the target only the copied slice, re-wrapped
// under its own FEK. The CEKs of the source's other chunks are NOT accessible through
// the target: their AGCK blobs are bound to the source identity (driveFileID +
// fekVersion) in the AAD, so unwrapping them with the target FEK fails closed.
func TestClone_Subset(t *testing.T) {
	env := newEncryptTestEnv(t, 2, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	const size = ChunkSize // 1 MiB
	store := &cfrStore{data: map[uint64][]byte{
		9101: bytes.Repeat([]byte{0xD1}, size),
		9102: bytes.Repeat([]byte{0xD2}, size),
		9103: bytes.Repeat([]byte{0xD3}, size),
	}}

	var srcIno Ino
	var srcAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "subset-src.exr", 0644, 022, 0, &srcIno, &srcAttr))
	require.True(t, srcAttr.Encrypted)

	gm, ok := env.client.(*grpcMeta)
	require.True(t, ok)
	srcFek, srcDFID, srcVer, err := gm.ResolveFileKey(cctx, srcIno)
	require.NoError(t, err)

	cekA, err := chunkenc.NewCEK()
	require.NoError(t, err)
	cekB, err := chunkenc.NewCEK()
	require.NoError(t, err)
	cekC, err := chunkenc.NewCEK()
	require.NoError(t, err)
	wrapSrc := func(cek []byte, id uint64) []byte {
		blob, err := chunkenc.WrapCEK(srcFek, cek, srcDFID, id, srcVer)
		require.NoError(t, err)
		return blob
	}
	require.Equal(t, syscall.Errno(0), env.client.Write(cctx, srcIno, 0, 0, Slice{Id: 9101, Size: size, Len: size, WrappedCEK: wrapSrc(cekA, 9101)}, time.Now()))
	require.Equal(t, syscall.Errno(0), env.client.Write(cctx, srcIno, 1, 0, Slice{Id: 9102, Size: size, Len: size, WrappedCEK: wrapSrc(cekB, 9102)}, time.Now()))
	require.Equal(t, syscall.Errno(0), env.client.Write(cctx, srcIno, 2, 0, Slice{Id: 9103, Size: size, Len: size, WrappedCEK: wrapSrc(cekC, 9103)}, time.Now()))

	var dstIno Ino
	var dstAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "subset-dst.exr", 0644, 022, 0, &dstIno, &dstAttr))
	require.True(t, dstAttr.Encrypted)

	// Copy exactly chunk 1 (the middle chunk) of the source.
	var copied, outLength uint64
	st := env.client.CopyFileRange(cctx, srcIno, size, dstIno, 0, size, 0, &copied, &outLength)
	require.Equal(t, syscall.Errno(0), st)
	require.EqualValues(t, size, copied)

	dstFek, dstDFID, dstVer, err := gm.ResolveFileKey(cctx, dstIno)
	require.NoError(t, err)
	require.NotEqual(t, srcDFID, dstAttr.DriveFileID)

	// The target references exactly the copied slice, re-wrapped under its own FEK.
	dstIDs := sliceIDSet(t, env.meta, dstIno)
	require.Equal(t, map[uint64]bool{9102: true}, dstIDs, "target must reference only the copied chunk")
	var cs []Slice
	require.Equal(t, syscall.Errno(0), env.meta.Read(ctx, dstIno, 0, &cs))
	require.Len(t, cs, 1)
	cek, err := chunkenc.UnwrapCEK(dstFek, cs[0].WrappedCEK, dstDFID, cs[0].Id, dstVer)
	require.NoError(t, err)
	require.Equal(t, cekB, cek)

	// FR-OP-3: the source's other chunks are not accessible through the target.
	// Their AGCK blobs (still wrapped under the source FEK in the source lists) must
	// fail to unwrap with the target FEK — AAD binds them to the source identity.
	var srcCS [][]Slice
	for indx := uint32(0); indx < 3; indx++ {
		var c []Slice
		require.Equal(t, syscall.Errno(0), env.meta.Read(ctx, srcIno, indx, &c))
		srcCS = append(srcCS, c)
	}
	for _, id := range []uint64{9101, 9103} {
		var blob []byte
		for _, c := range srcCS {
			if len(c) > 0 && c[0].Id == id {
				blob = c[0].WrappedCEK
			}
		}
		require.NotEmpty(t, blob)
		_, err = chunkenc.UnwrapCEK(dstFek, blob, dstDFID, id, dstVer)
		require.Error(t, err, "source slice %d must not unwrap under the target FEK", id)
	}

	// The source is intact: all three chunks still read under its own FEK.
	for indx := uint32(0); indx < 3; indx++ {
		s := srcCS[indx][0]
		cek, err := chunkenc.UnwrapCEK(srcFek, s.WrappedCEK, srcDFID, s.Id, srcVer)
		require.NoError(t, err)
		data := readSliceData(t, store, s, cek)
		want := byte(0xD1 + indx)
		require.Equal(t, bytes.Repeat([]byte{want}, size), data)
	}
}
