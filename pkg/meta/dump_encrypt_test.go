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

// Stage 9 (tasks.md 9.8): dump→load round-trip on an encrypted volume. The
// crypto metadata (wrapped_fek / drive_file_id / fek_version / crypto_alg in
// the attr, per-slice wrapped_cek in the chunk lists, encryption_enabled /
// kek_version in the Format) must survive byte-for-byte through all three
// format paths — JSON dump, binary V2 dump, and V2 streamed through the Meta
// Proxy — and the restored volume must read the data back with decryption.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	chunkenc "github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// DBs 6/7 follow the encrypt integration suite's convention (it uses 0-15;
// the local Redis has exactly 16 databases). Meta-package tests run
// sequentially and every env flushes its own DB at creation, so reusing 6/7
// (TestCreateRollback / TestOwnerBypass) is safe; pkg/vfs tests use DB 2.
const (
	encDumpSrcDB    = 6
	encDumpDstDB    = 7
	encDumpLegacyDB = 7 // sequential with the subtests above; flushed at start
)

const encDumpFileName = "dump.exr"

// encDumpStore is an in-memory stand-in for S3 holding the AGDF ciphertext of
// each slice (one block per slice, block index 0). Readers decrypt with the
// CEK passed via NewReaderWithKey, mirroring how the stage-5 tests read data
// back through a chunk store.
type encDumpStore struct {
	mu   sync.Mutex
	data map[uint64][]byte // slice id -> AGDF ciphertext
}

func newEncDumpStore() *encDumpStore {
	return &encDumpStore{data: make(map[uint64][]byte)}
}

func (s *encDumpStore) put(id uint64, ciphertext []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[id] = append([]byte(nil), ciphertext...)
}

func (s *encDumpStore) NewReaderWithKey(id uint64, length int, key []byte) chunkenc.Reader {
	s.mu.Lock()
	ct := append([]byte(nil), s.data[id]...)
	s.mu.Unlock()
	plain, err := chunkenc.DecryptBlock(key, ct, id, 0)
	if err != nil {
		return &encDumpReader{err: err}
	}
	return &encDumpReader{data: plain}
}

type encDumpReader struct {
	data []byte
	err  error
}

func (r *encDumpReader) ReadAt(ctx context.Context, p *chunkenc.Page, off int) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if off >= len(r.data) {
		return 0, io.EOF
	}
	return copy(p.Data, r.data[off:]), nil
}

// readEncSlice decrypts the s.Len bytes of slice record s from the store using cek.
func readEncSlice(t *testing.T, store *encDumpStore, s Slice, cek []byte) []byte {
	t.Helper()
	r := store.NewReaderWithKey(s.Id, int(s.Size), cek)
	buf := make([]byte, s.Len)
	page := &chunkenc.Page{Data: buf}
	n, err := r.ReadAt(context.Background(), page, int(s.Off))
	require.NoError(t, err)
	require.Equal(t, int(s.Len), n)
	return buf
}

// encDumpContent returns deterministic pseudo-random content (splitmix64) so a
// corrupted read is caught by the byte comparison.
func encDumpContent(id uint64, size uint32) []byte {
	out := make([]byte, size)
	var x uint64 = id * 0x9E3779B97F4A7C15
	for i := range out {
		x += 0x243F6A8885A308D3
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		out[i] = byte(z ^ (z >> 31))
	}
	return out
}

// assertEncryptedRestore verifies that envB (restored from a dump of envA)
// carries the crypto metadata byte-for-byte and reads the data back.
func assertEncryptedRestore(t *testing.T, envA, envB *encryptTestEnv, inode Ino, srcAttr Attr, rawAttrA []byte, store *encDumpStore, plains map[uint64][]byte) {
	t.Helper()
	ctx := Background()

	// Format: the encryption settings are preserved (the UUID is part of the
	// AGFK AAD, so it must round-trip exactly).
	fmtA := envA.meta.GetFormat()
	fmtB := envB.meta.GetFormat()
	require.True(t, fmtB.EncryptionEnabled)
	require.Equal(t, fmtA.KEKVersion, fmtB.KEKVersion)
	require.Equal(t, fmtA.UUID, fmtB.UUID)

	// Attr: field-level equality of the crypto fields...
	var restored Attr
	require.Equal(t, syscall.Errno(0), envB.meta.GetAttr(ctx, inode, &restored))
	require.True(t, restored.Encrypted)
	require.True(t, bytes.Equal(restored.WrappedFek, srcAttr.WrappedFek))
	require.Equal(t, srcAttr.DriveFileID, restored.DriveFileID)
	require.Equal(t, srcAttr.FekVersion, restored.FekVersion)
	require.Equal(t, srcAttr.CryptoAlg, restored.CryptoAlg)
	// ...and byte-for-byte: re-marshaling the restored attr reproduces the
	// source attr bytes stored in envA's Redis.
	require.True(t, bytes.Equal(envB.meta.marshal(&restored), rawAttrA))

	// Chunk lists: every slice record (24-byte base + AGCK tail) is
	// byte-identical to the source record.
	for indx := uint32(0); indx <= uint32(restored.Length/ChunkSize); indx++ {
		srcRecs, err := envA.rdb.LRange(ctx, envA.meta.chunkKey(inode, indx), 0, -1).Result()
		require.NoError(t, err)
		dstRecs, err := envB.rdb.LRange(ctx, envB.meta.chunkKey(inode, indx), 0, -1).Result()
		require.NoError(t, err)
		require.Equal(t, srcRecs, dstRecs, "chunk %d slice records", indx)
	}

	// Read-back with decryption: Open on the restored volume delivers the FEK
	// (the fake KeyManager unwraps the AGFK under the same KEK and AAD)...
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)
	var lino Ino
	var lattr Attr
	require.Equal(t, syscall.Errno(0), envB.client.Lookup(cctx, RootInode, encDumpFileName, &lino, &lattr, false))
	require.Equal(t, inode, lino)
	var openAttr Attr
	require.Equal(t, syscall.Errno(0), envB.client.Open(cctx, inode, syscall.O_RDONLY, &openAttr))
	require.Len(t, openAttr.Fek, fekSize)
	require.Equal(t, srcAttr.FekVersion, openAttr.FekVersion)

	// ...and every slice decrypts to exactly what was written.
	for indx := uint32(0); indx <= uint32(restored.Length/ChunkSize); indx++ {
		var cs []Slice
		require.Equal(t, syscall.Errno(0), envB.meta.Read(ctx, inode, indx, &cs))
		for _, s := range cs {
			if s.Id == 0 {
				continue // hole
			}
			cek, err := chunkenc.UnwrapCEK(openAttr.Fek, s.WrappedCEK, restored.DriveFileID, s.Id, openAttr.FekVersion)
			require.NoError(t, err, "slice %d must unwrap under the restored FEK", s.Id)
			require.Equal(t, plains[s.Id], readEncSlice(t, store, s, cek), "slice %d content", s.Id)
		}
	}
}

// TestDumpLoad_Encrypted (task 9.8): crypto metadata survives dump→load
// byte-for-byte on every format path and the restored volume decrypts.
func TestDumpLoad_Encrypted(t *testing.T) {
	envA := newEncryptTestEnv(t, encDumpSrcDB, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	// Give the source volume a non-zero KEK version so the preservation check
	// below is non-trivial (mirrors what enable-encryption records).
	fmtA := envA.meta.GetFormat()
	fmtA.KEKVersion = 1
	fmtJSON, err := json.MarshalIndent(&fmtA, "", "")
	require.NoError(t, err)
	require.NoError(t, envA.rdb.Set(ctx, envA.meta.setting(), fmtJSON, 0).Err())
	envA.meta.Lock()
	envA.meta.fmt = &fmtA
	envA.meta.Unlock()

	// Create an encrypted file through the proxy client: the attr carries the
	// wrapped FEK (AGFK), the drive file ID and the FEK version.
	var inode Ino
	var srcAttr Attr
	require.Equal(t, syscall.Errno(0), envA.client.Create(cctx, RootInode, encDumpFileName, 0644, 022, 0, &inode, &srcAttr))
	require.True(t, srcAttr.Encrypted)
	require.NotEmpty(t, srcAttr.WrappedFek)
	require.NotEmpty(t, srcAttr.DriveFileID)
	require.EqualValues(t, 1, srcAttr.FekVersion)

	gmA, ok := envA.client.(*grpcMeta)
	require.True(t, ok)
	srcFek, srcDFID, srcVer, err := gmA.ResolveFileKey(cctx, inode)
	require.NoError(t, err)
	require.Equal(t, srcAttr.DriveFileID, srcDFID)

	// Write three encrypted slices through the real write path: two in chunk 0
	// and one in chunk 1 (the records carry AGCK tails under the file FEK;
	// FekVersion is mandatory since task 5.8). The data objects are AGDF
	// ciphertext in the in-memory store.
	const size = 1 << 20 // 1 MiB per slice: small enough to keep in RAM
	store := newEncDumpStore()
	plains := make(map[uint64][]byte)
	wrapAndWrite := func(id uint64, indx, pos uint32) {
		t.Helper()
		cek, err := chunkenc.NewCEK()
		require.NoError(t, err)
		blob, err := chunkenc.WrapCEK(srcFek, cek, srcDFID, id, srcVer)
		require.NoError(t, err)
		require.Equal(t, syscall.Errno(0), envA.client.Write(cctx, inode, indx, pos,
			Slice{Id: id, Size: size, Len: size, WrappedCEK: blob, FekVersion: srcVer}, time.Now()))
		plain := encDumpContent(id, size)
		plains[id] = plain
		ct, err := chunkenc.EncryptBlock(cek, plain, id, 0)
		require.NoError(t, err)
		store.put(id, ct)
	}
	wrapAndWrite(6001, 0, 0)
	wrapAndWrite(6002, 0, size)
	wrapAndWrite(6003, 1, 0)

	// Ground truth: the raw attr bytes and slice records in envA's Redis.
	rawAttrA, err := envA.rdb.Get(ctx, envA.meta.inodeKey(inode)).Bytes()
	require.NoError(t, err)

	t.Run("JSON", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, envA.meta.DumpMeta(&buf, RootInode, 1, true, false, false))

		envB := newEncryptTestEnv(t, encDumpDstDB, true)
		require.NoError(t, envB.meta.Reset()) // LoadMeta requires an empty database
		require.NoError(t, envB.meta.LoadMeta(bytes.NewReader(buf.Bytes())))
		assertEncryptedRestore(t, envA, envB, inode, srcAttr, rawAttrA, store, plains)
	})

	t.Run("V2", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, envA.meta.DumpMetaV2(ctx, &buf, &DumpOption{Threads: 4}))

		envB := newEncryptTestEnv(t, encDumpDstDB, true)
		require.NoError(t, envB.meta.Reset()) // prepareLoad requires an empty database
		require.NoError(t, envB.meta.LoadMetaV2(ctx, bytes.NewReader(buf.Bytes()), &LoadOption{Threads: 4}))
		// LoadMetaV2 stores the setting but does not refresh the cached format;
		// the proxy resolves the AGFK AAD from GetFormat(), so reload it.
		_, err := envB.meta.Load(true)
		require.NoError(t, err)
		assertEncryptedRestore(t, envA, envB, inode, srcAttr, rawAttrA, store, plains)
	})

	t.Run("ProxyStreaming", func(t *testing.T) {
		// The grpcMeta client returns ENOSYS for Dump/LoadMetaV2; stream through
		// the generated pb clients over the in-process gRPC servers instead:
		// dump from envA's server, load into envB's (the handler loads into its
		// own meta).
		dstream, err := pb.NewMetaServiceClient(gmA.conn).DumpMetaV2(ctx, &pb.DumpMetaV2Request{KeepSecret: true, Threads: 4})
		require.NoError(t, err)
		var buf bytes.Buffer
		for {
			chunk, err := dstream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			_, err = buf.Write(chunk.Data)
			require.NoError(t, err)
		}

		envB := newEncryptTestEnv(t, encDumpDstDB, true)
		require.NoError(t, envB.meta.Reset()) // prepareLoad requires an empty database
		gmB, ok := envB.client.(*grpcMeta)
		require.True(t, ok)

		lstream, err := pb.NewMetaServiceClient(gmB.conn).LoadMetaV2(ctx)
		require.NoError(t, err)
		data := buf.Bytes()
		for len(data) > 0 {
			n := len(data)
			if n > 64*1024 {
				n = 64 * 1024
			}
			require.NoError(t, lstream.Send(&pb.LoadMetaV2Chunk{Data: data[:n]}))
			data = data[n:]
		}
		resp, err := lstream.CloseAndRecv()
		require.NoError(t, err)
		require.EqualValues(t, 0, resp.Errno)

		_, err = envB.meta.Load(true)
		require.NoError(t, err)
		assertEncryptedRestore(t, envA, envB, inode, srcAttr, rawAttrA, store, plains)
	})

	t.Run("V2LegacyFallback", func(t *testing.T) {
		// A bak file written by an old binary: fixed 24-byte records in Slices
		// and no SliceBlobs — the loader must take the legacy fixed-step path.
		rec1 := marshalSlice(0, 101, 4096, 0, 4096)
		rec2 := marshalSlice(4096, 102, 8192, 0, 8192)
		legacy := append(append([]byte{}, rec1...), rec2...)

		var w bytes.Buffer
		bak := newBakFormat()
		legacyFmt, err := json.Marshal(&Format{Name: "legacy", UUID: uuid.New().String()})
		require.NoError(t, err)
		require.NoError(t, bak.writeSegment(&w, newBakSegment(&pb.Format{Data: legacyFmt})))
		require.NoError(t, bak.writeSegment(&w, newBakSegment(&pb.Batch{
			Chunks: []*pb.Chunk{{Inode: 2, Index: 0, Slices: legacy}},
		})))
		require.NoError(t, bak.writeFooter(&w))

		rdb := redis.NewClient(&redis.Options{Addr: encTestRedisAddr(), DB: encDumpLegacyDB})
		require.NoError(t, rdb.Ping(Background()).Err())
		require.NoError(t, rdb.FlushDB(Background()).Err())
		t.Cleanup(func() { _ = rdb.Close() })

		m, err := newRedisMeta("redis", fmt.Sprintf("%s/%d", encTestRedisAddr(), encDumpLegacyDB), testConfig())
		require.NoError(t, err)
		rm := m.(*redisMeta)
		require.NoError(t, rm.LoadMetaV2(Background(), bytes.NewReader(w.Bytes()), &LoadOption{Threads: 1}))

		vals, err := rdb.LRange(Background(), rm.chunkKey(2, 0), 0, -1).Result()
		require.NoError(t, err)
		require.Equal(t, []string{string(rec1), string(rec2)}, vals)
	})
}
