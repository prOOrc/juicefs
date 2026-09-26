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
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	chunkenc "github.com/juicedata/juicefs/pkg/chunk"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestSetFileCrypto(t *testing.T) {
	if os.Getenv("SKIP_NON_CORE") == "true" {
		t.Skipf("skip non-core test")
	}
	m, err := newRedisMeta("redis", encTestRedisAddr()+"/13", testConfig())
	require.NoError(t, err)
	rm := m.(*redisMeta)
	defer rm.Shutdown()
	require.NoError(t, rm.rdb.FlushAll(Background()).Err())

	if err := m.Init(testFormat(), true); err != nil {
		t.Fatalf("init: %s", err)
	}
	if _, err := m.Load(true); err != nil {
		t.Fatalf("load: %s", err)
	}

	ctx := Background()
	var inode Ino
	var attr Attr
	if st := m.Mknod(ctx, RootInode, "encfile", TypeFile, 0644, 022, 0, "", &inode, &attr); st != 0 {
		t.Fatalf("mknod: %s", st)
	}

	crypto := &FileCrypto{
		WrappedFek:  []byte("agfk-blob"),
		DriveFileID: "dfid-1",
		FekVersion:  1,
		CryptoAlg:   "AES-256-GCM",
	}
	if st := rm.SetFileCrypto(ctx, inode, crypto); st != 0 {
		t.Fatalf("setfilecrypto: %s", st)
	}

	var got Attr
	if st := m.GetAttr(ctx, inode, &got); st != 0 {
		t.Fatalf("getattr: %s", st)
	}
	require.True(t, got.Encrypted)
	require.Equal(t, []byte("agfk-blob"), got.WrappedFek)
	require.Equal(t, "dfid-1", got.DriveFileID)
	require.EqualValues(t, 1, got.FekVersion)
	require.Equal(t, "AES-256-GCM", got.CryptoAlg)
	// non-crypto fields are preserved
	require.EqualValues(t, 0644, got.Mode)
	require.EqualValues(t, TypeFile, got.Typ)

	t.Run("missing inode returns ENOENT", func(t *testing.T) {
		st := rm.SetFileCrypto(ctx, Ino(999999), crypto)
		require.Equal(t, syscall.ENOENT, st)
	})

	t.Run("update overwrites previous crypto fields", func(t *testing.T) {
		crypto2 := &FileCrypto{
			WrappedFek:  []byte("agfk-blob-v2"),
			DriveFileID: "dfid-1",
			FekVersion:  2,
			CryptoAlg:   "AES-256-GCM",
		}
		require.Equal(t, syscall.Errno(0), rm.SetFileCrypto(ctx, inode, crypto2))
		var got2 Attr
		require.Equal(t, syscall.Errno(0), m.GetAttr(ctx, inode, &got2))
		require.Equal(t, []byte("agfk-blob-v2"), got2.WrappedFek)
		require.EqualValues(t, 2, got2.FekVersion)
	})
}

// newRewrapTestEnv wires a real Redis (DB 11) with a file whose chunk lists hold
// slices wrapped under FEK_A: chunk 0 = legacy slice + encrypted slice (cek1),
// chunk 1 = two encrypted slices (cek1/cek2). Returns the meta, the inode and the
// AADs/keys.
func newRewrapTestEnv(t *testing.T) (*redisMeta, Ino, []byte, []byte, []byte, []byte, SliceCryptoAAD, SliceCryptoAAD) {
	t.Helper()
	if os.Getenv("SKIP_NON_CORE") == "true" {
		t.Skipf("skip non-core test")
	}
	m, err := newRedisMeta("redis", encTestRedisAddr()+"/11", testConfig())
	require.NoError(t, err)
	rm := m.(*redisMeta)
	t.Cleanup(func() { _ = rm.Shutdown() })
	require.NoError(t, rm.rdb.FlushDB(Background()).Err())
	require.NoError(t, m.Init(testFormat(), true))
	_, err = m.Load(true)
	require.NoError(t, err)

	ctx := Background()
	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), m.Mknod(ctx, RootInode, "rw", TypeFile, 0644, 022, 0, "", &inode, &attr))

	fekA, fekB := randomKey32(t, 1), randomKey32(t, 2)
	cek1, cek2 := randomKey32(t, 3), randomKey32(t, 4)
	aadA := SliceCryptoAAD{DriveFileID: "dfid-src", FekVersion: 1}
	aadB := SliceCryptoAAD{DriveFileID: "dfid-dst", FekVersion: 7}

	wrapA := func(cek []byte, sliceID uint64) []byte {
		blob, err := chunkenc.WrapCEK(fekA, cek, aadA.DriveFileID, sliceID, aadA.FekVersion)
		require.NoError(t, err)
		return blob
	}

	const size = 1 << 20
	// chunk 0: legacy slice + encrypted slice; chunk 1: two encrypted slices
	require.Equal(t, syscall.Errno(0), m.Write(ctx, inode, 0, 0, Slice{Id: 5001, Size: size, Len: size}, time.Now()))
	require.Equal(t, syscall.Errno(0), m.Write(ctx, inode, 0, size, Slice{Id: 5002, Size: size, Len: size, WrappedCEK: wrapA(cek1, 5002)}, time.Now()))
	for i := 0; i < 2; i++ {
		sliceID := uint64(6000 + i)
		cek := cek1
		if i == 1 {
			cek = cek2
		}
		require.Equal(t, syscall.Errno(0), m.Write(ctx, inode, 1, uint32(i)*size, Slice{Id: sliceID, Size: size, Len: size, WrappedCEK: wrapA(cek, sliceID)}, time.Now()))
	}
	return rm, inode, fekA, fekB, cek1, cek2, aadA, aadB
}

// TestRewrapSlices_RoundTrip (task 5.1): re-wrap all AGCK tails from under FEK_A to
// under FEK_B; every record must unwrap with the new AAD to the original CEK and the
// old wrap must be gone. Slice IDs/positions are untouched (zero-copy, AC-7).
func TestRewrapSlices_RoundTrip(t *testing.T) {
	rm, inode, fekA, fekB, cek1, cek2, aadA, aadB := newRewrapTestEnv(t)
	ctx := Background()

	// sanity: the records carry AGCK tails that unwrap under FEK_A
	var cs []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 1, &cs))
	require.Len(t, cs, 2)
	for _, s := range cs {
		require.NotEmpty(t, s.WrappedCEK)
	}

	require.Equal(t, syscall.Errno(0), rm.RewrapSlices(ctx, inode, fekA, fekB, aadA, aadB))

	wantCEKs := map[uint64][]byte{5002: cek1, 6000: cek1, 6001: cek2}
	for indx := uint32(0); indx < 2; indx++ {
		var cs []Slice
		require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, indx, &cs))
		for _, s := range cs {
			if s.Id == 5001 {
				continue // legacy slice — no CEK
			}
			require.NotEmpty(t, s.WrappedCEK)
			cek, err := chunkenc.UnwrapCEK(fekB, s.WrappedCEK, aadB.DriveFileID, s.Id, aadB.FekVersion)
			require.NoError(t, err, "slice %d must unwrap under the new FEK/AAD", s.Id)
			require.Equal(t, wantCEKs[s.Id], cek)
			// the old wrap is gone: the source FEK/AAD no longer opens the blob
			_, err = chunkenc.UnwrapCEK(fekA, s.WrappedCEK, aadA.DriveFileID, s.Id, aadA.FekVersion)
			require.Error(t, err)
		}
	}
}

// TestRewrapSlices_LegacyUntouched (task 5.1): records without an AGCK tail pass
// through byte-identical; only the encrypted records change.
func TestRewrapSlices_LegacyUntouched(t *testing.T) {
	rm, inode, fekA, fekB, _, _, aadA, aadB := newRewrapTestEnv(t)
	ctx := Background()

	key := rm.chunkKey(inode, 0)
	before, err := rm.rdb.LRange(ctx, key, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, before, 2)

	require.Equal(t, syscall.Errno(0), rm.RewrapSlices(ctx, inode, fekA, fekB, aadA, aadB))

	after, err := rm.rdb.LRange(ctx, key, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, after, 2)
	require.Equal(t, before[0], after[0], "legacy record must be byte-identical")
	require.NotEqual(t, before[1], after[1], "encrypted record must be re-wrapped")
}

// TestRewrapSlices_FailClosed (task 5.1): an unwrap error (wrong source FEK) aborts
// with EIO and leaves every record untouched — no partial rewrite.
func TestRewrapSlices_FailClosed(t *testing.T) {
	rm, inode, _, fekB, _, _, aadA, aadB := newRewrapTestEnv(t)
	ctx := Background()

	snapshot := make(map[uint32][]string)
	for indx := uint32(0); indx < 2; indx++ {
		vals, err := rm.rdb.LRange(ctx, rm.chunkKey(inode, indx), 0, -1).Result()
		require.NoError(t, err)
		snapshot[indx] = vals
	}

	st := rm.RewrapSlices(ctx, inode, randomKey32(t, 99), fekB, aadA, aadB) // wrong srcFek
	require.Equal(t, syscall.EIO, st)

	for indx := uint32(0); indx < 2; indx++ {
		vals, err := rm.rdb.LRange(ctx, rm.chunkKey(inode, indx), 0, -1).Result()
		require.NoError(t, err)
		require.Equal(t, snapshot[indx], vals, "chunk %d must be untouched after fail-closed", indx)
	}
}

// TestRewrapSlices_Range (task 5.5): RewrapSlicesRange re-wraps only the given chunk
// lists; the rest of the file keeps its original wraps.
func TestRewrapSlices_Range(t *testing.T) {
	rm, inode, fekA, fekB, _, _, aadA, aadB := newRewrapTestEnv(t)
	ctx := Background()

	require.Equal(t, syscall.Errno(0), rm.RewrapSlicesRange(ctx, inode, fekA, fekB, aadA, aadB, 1, 1))

	// chunk 1 re-wrapped under FEK_B
	var cs []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 1, &cs))
	for _, s := range cs {
		_, err := chunkenc.UnwrapCEK(fekB, s.WrappedCEK, aadB.DriveFileID, s.Id, aadB.FekVersion)
		require.NoError(t, err)
	}
	// chunk 0 still unwraps under the source AAD (its encrypted record is untouched)
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 0, &cs))
	for _, s := range cs {
		if len(s.WrappedCEK) == 0 {
			continue
		}
		_, err := chunkenc.UnwrapCEK(fekA, s.WrappedCEK, aadA.DriveFileID, s.Id, aadA.FekVersion)
		require.NoError(t, err, "chunk 0 must keep the source wrap")
	}
}

// TestCompaction_SkippedWhenNotAllowed (task 5.6, FR-VER-3): when the hook rejects
// the inode, compactChunk must not emit a CompactChunk message and must leave the
// chunk list untouched.
func TestCompaction_SkippedWhenNotAllowed(t *testing.T) {
	rm, inode, _, _, _, _, _, _ := newRewrapTestEnv(t)

	compacted := false
	rm.OnMsg(CompactChunk, func(args ...interface{}) error {
		compacted = true
		return nil
	})
	rm.compactionAllowed = func(Ino) bool { return false }

	rm.compactChunk(inode, 0, false, true, -1)

	require.False(t, compacted, "CompactChunk must not be emitted when compaction is disallowed")
	var cs []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(Background(), inode, 0, &cs))
	require.Len(t, cs, 2)
}

// TestSliceDeletable_Hook (task 5.6, FR-VER-2): when the hook rejects a slice id,
// deleteSlice_ must not emit a DeleteSlice message and must not clean up the
// refcount field; the other slices are deleted as usual.
func TestSliceDeletable_Hook(t *testing.T) {
	rm, _, _, _, _, _, _, _ := newRewrapTestEnv(t)
	require.NoError(t, rm.NewSession(false))

	ctx := Background()
	var mu sync.Mutex
	deleted := make(map[uint64]bool)
	rm.OnMsg(DeleteSlice, func(args ...interface{}) error {
		mu.Lock()
		deleted[args[0].(uint64)] = true
		mu.Unlock()
		return nil
	})
	rm.sliceDeletable = func(id uint64) bool { return id != 5001 }

	require.Equal(t, syscall.Errno(0), rm.Unlink(ctx, RootInode, "rw"))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return deleted[5002] && deleted[6000] && deleted[6001]
	}, 15*time.Second, 10*time.Millisecond)

	mu.Lock()
	require.False(t, deleted[5001], "slice 5001 must not be deleted while the hook blocks it")
	mu.Unlock()

	const size = 1 << 20
	val, err := rm.rdb.HGet(ctx, rm.sliceRefs(), rm.sliceKey(5001, size)).Result()
	require.NoError(t, err)
	require.Equal(t, "-1", val, "blocked slice keeps its refcount field")
	_, err = rm.rdb.HGet(ctx, rm.sliceRefs(), rm.sliceKey(5002, size)).Result()
	require.ErrorIs(t, err, redis.Nil, "deleted slice refcount field must be cleaned up")
}

// mWrite is a convenience wrapper around redisMeta.Write for tests.
func mWrite(t *testing.T, rm *redisMeta, ctx Context, inode Ino, indx, off uint32, s Slice) syscall.Errno {
	t.Helper()
	return rm.Write(ctx, inode, indx, off, s, time.Now())
}

// newFekTestEnv wires a real Redis (DB 11) with an encrypted file: Mknod +
// SetFileCrypto(version 1). Returns the meta, the inode and the AADs/keys.
func newFekTestEnv(t *testing.T) (*redisMeta, Ino, []byte, []byte, SliceCryptoAAD, SliceCryptoAAD) {
	t.Helper()
	if os.Getenv("SKIP_NON_CORE") == "true" {
		t.Skipf("skip non-core test")
	}
	m, err := newRedisMeta("redis", encTestRedisAddr()+"/11", testConfig())
	require.NoError(t, err)
	rm := m.(*redisMeta)
	t.Cleanup(func() { _ = rm.Shutdown() })
	require.NoError(t, rm.rdb.FlushDB(Background()).Err())
	require.NoError(t, m.Init(testFormat(), true))
	_, err = m.Load(true)
	require.NoError(t, err)

	ctx := Background()
	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), m.Mknod(ctx, RootInode, "fekt", TypeFile, 0644, 022, 0, "", &inode, &attr))

	oldFek, newFek := randomKey32(t, 1), randomKey32(t, 2)
	srcAAD := SliceCryptoAAD{DriveFileID: "dfid-fekt", FekVersion: 1}
	dstAAD := SliceCryptoAAD{DriveFileID: "dfid-fekt", FekVersion: 2}
	require.Equal(t, syscall.Errno(0), rm.SetFileCrypto(ctx, inode, &FileCrypto{
		WrappedFek:  []byte("agfk-blob"),
		DriveFileID: srcAAD.DriveFileID,
		FekVersion:  1,
		CryptoAlg:   "AES-256-GCM",
	}))
	return rm, inode, oldFek, newFek, srcAAD, dstAAD
}

// TestRewrapSlices_Conflict (task 5.8, design Addendum A1): a concurrent writer
// appending records while RewrapSlices runs must not lose or corrupt them: the
// origin check rejects the stale apply with EAGAIN and the retry re-snapshots,
// so every record — including those appended mid-rewrap — ends up wrapped under
// the new FEK.
func TestRewrapSlices_Conflict(t *testing.T) {
	t.Run("origin comparison", func(t *testing.T) {
		cases := []struct {
			name   string
			origin []string
			cur    []string
			want   bool
		}{
			{"identical lists", []string{"a", "b"}, []string{"a", "b"}, false},
			{"empty lists", nil, nil, false},
			{"appended record", []string{"a"}, []string{"a", "b"}, true},
			{"changed record", []string{"a", "b"}, []string{"a", "c"}, true},
			{"removed record", []string{"a", "b"}, []string{"a"}, true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				require.Equal(t, tc.want, rewrapOriginChanged(tc.origin, tc.cur))
			})
		}
	})

	rm, inode, oldFek, newFek, srcAAD, dstAAD := newFekTestEnv(t)
	ctx := Background()

	const recLen = 4096
	wrapOld := func(cek []byte, id uint64) []byte {
		blob, err := chunkenc.WrapCEK(oldFek, cek, srcAAD.DriveFileID, id, srcAAD.FekVersion)
		require.NoError(t, err)
		return blob
	}
	// one initial record in chunk 0
	require.Equal(t, syscall.Errno(0), mWrite(t, rm, ctx, inode, 0, 0, Slice{Id: 1001, Size: recLen, Len: recLen, WrappedCEK: wrapOld(randomKey32(t, 3), 1001), FekVersion: 1}))

	// Concurrent appender: new records wrapped under the OLD FEK every ~5ms. The
	// file's attr still says version 1 (no SetFileCrypto(2) in this test), so the
	// writes pass the fencing check and land under the old FEK — exactly the
	// records the origin-check retry must pick up and re-wrap.
	stop := make(chan struct{})
	var appender sync.WaitGroup
	var nextID uint64 = 2000
	appender.Add(1)
	go func() {
		defer appender.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			id := atomic.AddUint64(&nextID, 1)
			st := mWrite(t, rm, ctx, inode, 0, uint32((id-2000)*recLen), Slice{Id: id, Size: recLen, Len: recLen, WrappedCEK: wrapOld(randomKey32(t, byte(id%251)), id), FekVersion: 1})
			if st != 0 {
				t.Errorf("appender write %d: %s", id, st)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	var rewErr syscall.Errno
	var rewWg sync.WaitGroup
	rewWg.Add(1)
	go func() {
		defer rewWg.Done()
		rewErr = rm.RewrapSlices(ctx, inode, oldFek, newFek, srcAAD, dstAAD)
	}()
	time.Sleep(100 * time.Millisecond) // let appends race the rewrap
	close(stop)
	appender.Wait()
	rewWg.Wait()
	require.Equal(t, syscall.Errno(0), rewErr)

	// A record may have landed after the concurrent run's final apply; a quiet
	// second run (idempotent) brings the list to the fixed point before asserting.
	require.Equal(t, syscall.Errno(0), rm.RewrapSlices(ctx, inode, oldFek, newFek, srcAAD, dstAAD))

	vals, err := rm.rdb.LRange(ctx, rm.chunkKey(inode, 0), 0, -1).Result()
	require.NoError(t, err)
	require.NotEmpty(t, vals)
	for _, val := range vals {
		s := new(slice)
		s.read([]byte(val))
		require.NotEmpty(t, s.wrappedCEK)
		_, err := chunkenc.UnwrapCEK(newFek, s.wrappedCEK, dstAAD.DriveFileID, s.id, dstAAD.FekVersion)
		require.NoError(t, err, "slice %d must unwrap under the new FEK/AAD", s.id)
	}
}

// TestRewrapSlices_AlreadyRewrapped (task 5.8, design Addendum A1): a record
// already wrapped under the NEW FEK/version (a writer that re-resolved its FEK
// mid-rotation) survives RewrapSlices byte-identical; a record under a foreign
// key/AAD still fails closed with EIO and leaves the list untouched.
func TestRewrapSlices_AlreadyRewrapped(t *testing.T) {
	rm, inode, oldFek, newFek, srcAAD, dstAAD := newFekTestEnv(t)
	ctx := Background()

	const recLen = 4096
	cekOld, cekNew := randomKey32(t, 3), randomKey32(t, 4)
	// record A under the old FEK/version (committed before the bump)
	blobA, err := chunkenc.WrapCEK(oldFek, cekOld, srcAAD.DriveFileID, 3001, srcAAD.FekVersion)
	require.NoError(t, err)
	require.Equal(t, syscall.Errno(0), mWrite(t, rm, ctx, inode, 0, 0, Slice{Id: 3001, Size: recLen, Len: recLen, WrappedCEK: blobA, FekVersion: 1}))
	// rotation bumps the version...
	require.Equal(t, syscall.Errno(0), rm.SetFileCrypto(ctx, inode, &FileCrypto{
		WrappedFek:  []byte("agfk-blob-v2"),
		DriveFileID: srcAAD.DriveFileID,
		FekVersion:  2,
		CryptoAlg:   "AES-256-GCM",
	}))
	// ...and a writer that re-resolved mid-rotation commits record B already
	// under the new FEK/version.
	blobB, err := chunkenc.WrapCEK(newFek, cekNew, dstAAD.DriveFileID, 3002, dstAAD.FekVersion)
	require.NoError(t, err)
	require.Equal(t, syscall.Errno(0), mWrite(t, rm, ctx, inode, 0, recLen, Slice{Id: 3002, Size: recLen, Len: recLen, WrappedCEK: blobB, FekVersion: 2}))

	key := rm.chunkKey(inode, 0)
	before, err := rm.rdb.LRange(ctx, key, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, before, 2)

	require.Equal(t, syscall.Errno(0), rm.RewrapSlices(ctx, inode, oldFek, newFek, srcAAD, dstAAD))

	after, err := rm.rdb.LRange(ctx, key, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, after, 2)
	require.NotEqual(t, before[0], after[0], "record A must be re-wrapped")
	require.Equal(t, before[1], after[1], "record B (already under the new FEK) must be byte-identical")

	sa := new(slice)
	sa.read([]byte(after[0]))
	cekOut, err := chunkenc.UnwrapCEK(newFek, sa.wrappedCEK, dstAAD.DriveFileID, sa.id, dstAAD.FekVersion)
	require.NoError(t, err)
	require.Equal(t, cekOld, cekOut)

	t.Run("foreign key fails closed", func(t *testing.T) {
		var fIno Ino
		var fAttr Attr
		require.Equal(t, syscall.Errno(0), rm.Mknod(ctx, RootInode, "foreign", TypeFile, 0644, 022, 0, "", &fIno, &fAttr))
		require.Equal(t, syscall.Errno(0), rm.SetFileCrypto(ctx, fIno, &FileCrypto{
			WrappedFek:  []byte("agfk-blob"),
			DriveFileID: srcAAD.DriveFileID,
			FekVersion:  1,
			CryptoAlg:   "AES-256-GCM",
		}))

		foreign := randomKey32(t, 99)
		blobC, err := chunkenc.WrapCEK(foreign, randomKey32(t, 5), srcAAD.DriveFileID, 4001, srcAAD.FekVersion)
		require.NoError(t, err)
		require.Equal(t, syscall.Errno(0), mWrite(t, rm, ctx, fIno, 0, 0, Slice{Id: 4001, Size: recLen, Len: recLen, WrappedCEK: blobC, FekVersion: 1}))

		fKey := rm.chunkKey(fIno, 0)
		fBefore, err := rm.rdb.LRange(ctx, fKey, 0, -1).Result()
		require.NoError(t, err)

		st := rm.RewrapSlices(ctx, fIno, oldFek, newFek, srcAAD, dstAAD)
		require.Equal(t, syscall.EIO, st)

		fAfter, err := rm.rdb.LRange(ctx, fKey, 0, -1).Result()
		require.NoError(t, err)
		require.Equal(t, fBefore, fAfter, "the list must be untouched after fail-closed")
	})
}

// TestRotation_ConcurrentWriter (task 5.8, design Addendum A1): full fencing
// scenario at the redisMeta level — a writer committing slices while rotation
// bumps the FEK version and re-wraps. The writer fences on EAGAIN, re-reads the
// version, re-wraps and retries (mirrors the VFS commit path); after both finish
// and the old FEK is zeroed, every record must unwrap under the new FEK/version.
func TestRotation_ConcurrentWriter(t *testing.T) {
	rm, inode, oldFek, newFek, srcAAD, dstAAD := newFekTestEnv(t)
	ctx := Background()

	const recLen = 4096
	const nWrites = 50

	// Writer: wrap the CEK under the current FEK version (from the attr), commit
	// with Slice.FekVersion set; on EAGAIN re-read the version, re-wrap and retry.
	var writerErr error
	var writerWg sync.WaitGroup
	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		for i := 0; i < nWrites; i++ {
			id := uint64(5000 + i)
			var st syscall.Errno
			for attempt := 0; ; attempt++ {
				var a Attr
				if eno := rm.GetAttr(ctx, inode, &a); eno != 0 {
					writerErr = eno
					return
				}
				fek, aad := oldFek, srcAAD
				if a.FekVersion == 2 {
					fek, aad = newFek, dstAAD
				}
				blob, err := chunkenc.WrapCEK(fek, randomKey32(t, byte(i)), aad.DriveFileID, id, a.FekVersion)
				if err != nil {
					writerErr = err
					return
				}
				st = mWrite(t, rm, ctx, inode, 0, uint32(i*recLen), Slice{Id: id, Size: recLen, Len: recLen, WrappedCEK: blob, FekVersion: a.FekVersion})
				if st == 0 || st != syscall.EAGAIN || attempt >= 5 {
					break
				}
			}
			if st != 0 {
				writerErr = st
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// Rotation: bump the version, then re-wrap (the order of rotateFileFEK).
	var rotErr syscall.Errno
	var rotWg sync.WaitGroup
	rotWg.Add(1)
	go func() {
		defer rotWg.Done()
		time.Sleep(10 * time.Millisecond) // let some writes land under the old FEK
		if st := rm.SetFileCrypto(ctx, inode, &FileCrypto{
			WrappedFek:  []byte("agfk-blob-v2"),
			DriveFileID: srcAAD.DriveFileID,
			FekVersion:  2,
			CryptoAlg:   "AES-256-GCM",
		}); st != 0 {
			rotErr = st
			return
		}
		rotErr = rm.RewrapSlices(ctx, inode, oldFek, newFek, srcAAD, dstAAD)
	}()

	writerWg.Wait()
	rotWg.Wait()
	require.NoError(t, writerErr)
	require.Equal(t, syscall.Errno(0), rotErr)

	// Zero the old FEK: any record still wrapped under it can no longer be
	// opened, so the assertions below prove every record is under the new FEK.
	for i := range oldFek {
		oldFek[i] = 0
	}
	var finalAttr Attr
	require.Equal(t, syscall.Errno(0), rm.GetAttr(ctx, inode, &finalAttr))
	require.EqualValues(t, 2, finalAttr.FekVersion)

	for indx := uint32(0); indx <= uint32(finalAttr.Length/ChunkSize); indx++ {
		vals, err := rm.rdb.LRange(ctx, rm.chunkKey(inode, indx), 0, -1).Result()
		require.NoError(t, err)
		for _, val := range vals {
			s := new(slice)
			s.read([]byte(val))
			require.NotEmpty(t, s.wrappedCEK)
			_, err := chunkenc.UnwrapCEK(newFek, s.wrappedCEK, dstAAD.DriveFileID, s.id, dstAAD.FekVersion)
			require.NoError(t, err, "slice %d must unwrap under the new FEK/version", s.id)
		}
	}
}

// TestWrite_Fencing (task 5.8, design Addendum A1): doWrite fences encrypted
// commits whose FEK version does not match the attr's — mismatched version →
// EAGAIN, matching version → 0, legacy slices (no WrappedCEK) pass even into
// encrypted files (mixed chunk lists are valid during migration).
func TestWrite_Fencing(t *testing.T) {
	rm, inode, oldFek, _, srcAAD, _ := newFekTestEnv(t)
	ctx := Background()

	const recLen = 4096
	wrap := func(cek []byte, id uint64, ver uint32) []byte {
		blob, err := chunkenc.WrapCEK(oldFek, cek, srcAAD.DriveFileID, id, ver)
		require.NoError(t, err)
		return blob
	}

	t.Run("mismatched version is fenced", func(t *testing.T) {
		st := mWrite(t, rm, ctx, inode, 0, 0, Slice{Id: 6001, Size: recLen, Len: recLen, WrappedCEK: wrap(randomKey32(t, 2), 6001, 1), FekVersion: 2})
		require.Equal(t, syscall.EAGAIN, st)
	})

	t.Run("matching version passes", func(t *testing.T) {
		st := mWrite(t, rm, ctx, inode, 0, 0, Slice{Id: 6002, Size: recLen, Len: recLen, WrappedCEK: wrap(randomKey32(t, 3), 6002, 1), FekVersion: 1})
		require.Equal(t, syscall.Errno(0), st)
	})

	t.Run("legacy slice passes into encrypted file", func(t *testing.T) {
		st := mWrite(t, rm, ctx, inode, 0, recLen, Slice{Id: 6003, Size: recLen, Len: recLen})
		require.Equal(t, syscall.Errno(0), st)
	})

	// After a version bump, the old version is fenced (the rotation direction).
	require.Equal(t, syscall.Errno(0), rm.SetFileCrypto(ctx, inode, &FileCrypto{
		WrappedFek:  []byte("agfk-blob-v2"),
		DriveFileID: srcAAD.DriveFileID,
		FekVersion:  2,
		CryptoAlg:   "AES-256-GCM",
	}))
	t.Run("pre-bump version fenced after rotation", func(t *testing.T) {
		st := mWrite(t, rm, ctx, inode, 0, 2*recLen, Slice{Id: 6004, Size: recLen, Len: recLen, WrappedCEK: wrap(randomKey32(t, 4), 6004, 1), FekVersion: 1})
		require.Equal(t, syscall.EAGAIN, st)
	})
	t.Run("new version passes after rotation", func(t *testing.T) {
		st := mWrite(t, rm, ctx, inode, 0, 2*recLen, Slice{Id: 6005, Size: recLen, Len: recLen, WrappedCEK: wrap(randomKey32(t, 5), 6005, 2), FekVersion: 2})
		require.Equal(t, syscall.Errno(0), st)
	})
}
