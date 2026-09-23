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
	"syscall"
	"testing"
	"time"

	chunkenc "github.com/juicedata/juicefs/pkg/chunk"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// newReencryptTestEnv wires a real Redis (DB 0) with a legacy file:
// chunk 0 = two legacy slices, chunk 1 = one legacy slice. Returns the meta
// client and the file inode.
func newReencryptTestEnv(t *testing.T) (*redisMeta, Ino) {
	t.Helper()
	if os.Getenv("SKIP_NON_CORE") == "true" {
		t.Skipf("skip non-core test")
	}
	m, err := newRedisMeta("redis", "127.0.0.1:6379/0", testConfig())
	require.NoError(t, err)
	rm := m.(*redisMeta)
	t.Cleanup(func() { _ = rm.Shutdown() })
	require.NoError(t, rm.rdb.FlushDB(Background()).Err())
	// like the reencrypt worker (cmd/reencrypt.go): a DeleteSlice handler must
	// be registered for deleteSlice_ to proceed to the meta cleanup
	rm.OnMsg(DeleteSlice, func(args ...interface{}) error { return nil })
	require.NoError(t, m.Init(testFormat(), true))
	_, err = m.Load(true)
	require.NoError(t, err)

	ctx := Background()
	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), m.Mknod(ctx, RootInode, "legacy", TypeFile, 0644, 022, 0, "", &inode, &attr))

	const size = 1 << 20
	require.Equal(t, syscall.Errno(0), m.Write(ctx, inode, 0, 0, Slice{Id: 7001, Size: size, Len: size}, time.Now()))
	require.Equal(t, syscall.Errno(0), m.Write(ctx, inode, 0, size, Slice{Id: 7002, Size: size, Len: size}, time.Now()))
	require.Equal(t, syscall.Errno(0), m.Write(ctx, inode, 1, 0, Slice{Id: 7003, Size: size, Len: size}, time.Now()))
	return rm, inode
}

// TestReencryptChunk_Refcount (task 8.1): the swap replaces the whole chunk
// list with one encrypted slice; refcounts of the old slices are decremented
// in the same transaction — a shared slice (refs=1, i.e. two references)
// survives at refs=0, an exclusive slice is dropped, and the new slice ends
// up with the implicit refcount (no entry).
func TestReencryptChunk_Refcount(t *testing.T) {
	rm, inode := newReencryptTestEnv(t)
	ctx := Background()
	const size = 1 << 20

	// 7001 is shared with another file: refs=1 means two total references.
	require.NoError(t, rm.rdb.HSet(ctx, rm.sliceRefs(), rm.sliceKey(7001, size), "1").Err())

	var origin []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 0, &origin))
	require.Len(t, origin, 2)
	require.EqualValues(t, 7001, origin[0].Id)
	require.EqualValues(t, 7002, origin[1].Id)

	var newID uint64
	require.Equal(t, syscall.Errno(0), rm.NewSlice(ctx, &newID))
	fek := randomKey32(t, 1)
	cek := randomKey32(t, 2)
	blob, err := chunkenc.WrapCEK(fek, cek, "dfid-reenc", newID, 1)
	require.NoError(t, err)

	newSlice := Slice{Id: newID, Size: 2 * size, Off: 0, Len: 2 * size, WrappedCEK: blob}
	require.Equal(t, syscall.Errno(0), rm.ReencryptChunk(ctx, inode, 0, origin, newSlice, 0))

	// the chunk list is now exactly the one merged slice at pos 0
	var after []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 0, &after))
	require.Len(t, after, 1)
	require.Equal(t, newSlice, after[0])

	// shared slice: decremented to 0, still referenced elsewhere — kept
	v, err := rm.rdb.HGet(ctx, rm.sliceRefs(), rm.sliceKey(7001, size)).Int()
	require.NoError(t, err)
	require.EqualValues(t, 0, v)
	// exclusive slice: implicit refcount consumed — entry removed
	_, err = rm.rdb.HGet(ctx, rm.sliceRefs(), rm.sliceKey(7002, size)).Result()
	require.ErrorIs(t, err, redis.Nil)
	// new slice: HSet "0" then cleanupZeroRef — back to the implicit refcount
	_, err = rm.rdb.HGet(ctx, rm.sliceRefs(), rm.sliceKey(newID, 2*size)).Result()
	require.ErrorIs(t, err, redis.Nil)

	// untouched chunk keeps its list
	var c1 []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 1, &c1))
	require.Len(t, c1, 1)
	require.EqualValues(t, 7003, c1[0].Id)
}

// TestReencryptChunk_Conflict (task 8.1): if the chunk list changed between the
// caller's Read and the swap, the transaction fails with EAGAIN, the list is
// untouched, and the new slice is marked unreferenced for GC.
func TestReencryptChunk_Conflict(t *testing.T) {
	rm, inode := newReencryptTestEnv(t)
	ctx := Background()
	const size = 1 << 20

	var origin []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 1, &origin))
	require.Len(t, origin, 1)

	// concurrent write extends the chunk list after the caller's Read
	require.Equal(t, syscall.Errno(0), rm.Write(ctx, inode, 1, size, Slice{Id: 7004, Size: size, Len: size}, time.Now()))

	var newID uint64
	require.Equal(t, syscall.Errno(0), rm.NewSlice(ctx, &newID))
	newSlice := Slice{Id: newID, Size: size, Off: 0, Len: size}
	st := rm.ReencryptChunk(ctx, inode, 1, origin, newSlice, 0)
	require.Equal(t, syscall.EAGAIN, st)

	// the list is untouched (both slices, original order)
	var after []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 1, &after))
	require.Len(t, after, 2)
	require.EqualValues(t, 7003, after[0].Id)
	require.EqualValues(t, 7004, after[1].Id)

	// the orphaned new slice is marked unreferenced (refs=-1) for GC
	v, err := rm.rdb.HGet(ctx, rm.sliceRefs(), rm.sliceKey(newID, size)).Int()
	require.NoError(t, err)
	require.EqualValues(t, -1, v)
}

// TestReencryptChunk_RetrySucceeds (task 8.1): after an EAGAIN conflict the
// caller re-reads and retries; the second swap with the fresh origin succeeds.
func TestReencryptChunk_RetrySucceeds(t *testing.T) {
	rm, inode := newReencryptTestEnv(t)
	ctx := Background()
	const size = 1 << 20

	var origin []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 1, &origin))
	require.Equal(t, syscall.Errno(0), rm.Write(ctx, inode, 1, size, Slice{Id: 7004, Size: size, Len: size}, time.Now()))

	var newID uint64
	require.Equal(t, syscall.Errno(0), rm.NewSlice(ctx, &newID))
	newSlice := Slice{Id: newID, Size: 2 * size, Off: 0, Len: 2 * size}
	require.Equal(t, syscall.EAGAIN, rm.ReencryptChunk(ctx, inode, 1, origin, newSlice, 0))

	// re-read and retry with the fresh list
	var fresh []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 1, &fresh))
	require.Len(t, fresh, 2)
	require.Equal(t, syscall.Errno(0), rm.ReencryptChunk(ctx, inode, 1, fresh, newSlice, 0))

	var after []Slice
	require.Equal(t, syscall.Errno(0), rm.Read(ctx, inode, 1, &after))
	require.Len(t, after, 1)
	require.Equal(t, newSlice, after[0])
}
