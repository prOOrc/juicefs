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

// Stage 7 integration suite (tasks.md 7.5–7.7): FEK rotation via the proxy RPC
// (zero-copy re-wrap, FR-ROT-4), the offboarding batch (rate-limited, resumable,
// idempotent), and revocation end to end (generation bump on the heartbeat wipes
// the client's keys, FR-TEST-10/AC-10).

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	chunkenc "github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/require"
)

// TestFekRotation (task 7.5, FR-ROT-4): RotateFileKey bumps the FEK version,
// stores a new wrapped FEK and re-wraps every slice CEK under it — S3 objects
// (slice IDs) are untouched, the old FEK no longer opens the re-wrapped CEKs,
// and the new one does.
func TestFekRotation(t *testing.T) {
	env := newEncryptTestEnv(t, 9, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	// 1. Create an encrypted file and open it to get the current FEK.
	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "rotate.exr", 0644, 022, syscall.O_CREAT|syscall.O_EXCL, &inode, &attr))
	require.True(t, attr.Encrypted)
	require.EqualValues(t, 1, attr.FekVersion)

	var openAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Open(cctx, inode, syscall.O_RDONLY, &openAttr))
	oldFek := openAttr.Fek
	require.Len(t, oldFek, fekSize)

	// 2. Fixture data: one chunk with an encrypted slice (AGCK tail under the
	//    current FEK), as the chunk layer would store it.
	cek := randomKey32(t, 42)
	const size = 1 << 20
	sliceID := uint64(7001)
	wrapped, err := chunkenc.WrapCEK(oldFek, cek, attr.DriveFileID, sliceID, 1)
	require.NoError(t, err)
	require.Equal(t, syscall.Errno(0), env.meta.Write(ctx, inode, 0, 0, Slice{Id: sliceID, Size: size, Len: size, WrappedCEK: wrapped}, time.Now()))

	var before []Slice
	require.Equal(t, syscall.Errno(0), env.meta.Read(ctx, inode, 0, &before))
	require.Len(t, before, 1)

	// 3. Rotate via the proxy RPC (admin identity = OIDC subject).
	gm, ok := env.client.(*grpcMeta)
	require.True(t, ok)
	pbClient := pb.NewMetaServiceClient(gm.conn)
	resp, err := pbClient.RotateFileKey(userCtx(t, ctx, user), &pb.RotateFileKeyRequest{
		Inode: uint64(inode), AdminUserId: user,
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)
	require.EqualValues(t, 2, resp.NewFekVersion)

	// 4. Metadata: version bumped, new wrapped FEK stored (server-side GetAttr —
	//    the client's attr cache may still hold the pre-rotation attr).
	var newAttr Attr
	require.Equal(t, syscall.Errno(0), env.meta.GetAttr(ctx, inode, &newAttr))
	require.EqualValues(t, 2, newAttr.FekVersion)
	require.NotEqual(t, attr.WrappedFek, newAttr.WrappedFek)

	// 5. S3 keys unchanged (zero-copy): the same slice ID is in place.
	var after []Slice
	require.Equal(t, syscall.Errno(0), env.meta.Read(ctx, inode, 0, &after))
	require.Len(t, after, 1)
	require.Equal(t, before[0].Id, after[0].Id)

	// 6. FR-ROT-4: the old FEK no longer opens the re-wrapped CEK; the new one
	//    (issued by KeyManager to a fresh client) does, and yields the original CEK.
	remounted, err := newGRPCMeta("grpc", env.addr, testConfig())
	require.NoError(t, err)
	defer remounted.Shutdown()
	var remountAttr Attr
	require.Equal(t, syscall.Errno(0), remounted.Open(userCtx(t, ctx, user), inode, syscall.O_RDONLY, &remountAttr))
	newFek := remountAttr.Fek
	require.Len(t, newFek, fekSize)
	require.EqualValues(t, 2, remountAttr.FekVersion)

	_, err = chunkenc.UnwrapCEK(oldFek, after[0].WrappedCEK, attr.DriveFileID, sliceID, 2)
	require.Error(t, err, "the old FEK must not open the re-wrapped CEK (FR-ROT-4)")
	cekOut, err := chunkenc.UnwrapCEK(newFek, after[0].WrappedCEK, attr.DriveFileID, sliceID, 2)
	require.NoError(t, err)
	require.Equal(t, cek, cekOut)

	// 7. Identity mismatch is rejected (admin_user_id must match the OIDC subject).
	respBad, err := pbClient.RotateFileKey(userCtx(t, ctx, user), &pb.RotateFileKeyRequest{
		Inode: uint64(inode), AdminUserId: uuid.New().String(),
	})
	require.NoError(t, err)
	require.EqualValues(t, uint32(syscall.EACCES), respBad.Errno)
}

// TestRotation_OffboardingBatch (task 7.6): RotateFileKeysByPaths rotates a batch
// of files by path; every file's version is bumped, a re-run from the checkpoint
// performs zero operations (idempotent), missing paths are reported as skipped
// while still advancing the checkpoint, and an identity mismatch is rejected.
func TestRotation_OffboardingBatch(t *testing.T) {
	env := newEncryptTestEnv(t, 10, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	// 10 encrypted files.
	const n = 10
	paths := make([]string, n)
	inodes := make([]Ino, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("off%d.exr", i)
		var inode Ino
		var attr Attr
		require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, name, 0644, 022, 0, &inode, &attr))
		require.True(t, attr.Encrypted)
		paths[i] = "/" + name
		inodes[i] = inode
	}

	gm, ok := env.client.(*grpcMeta)
	require.True(t, ok)
	pbClient := pb.NewMetaServiceClient(gm.conn)

	// First run: all 10 rotated.
	resp, err := pbClient.RotateFileKeysByPaths(userCtx(t, ctx, user), &pb.RotateFileKeysByPathsRequest{
		AdminUserId: user, Paths: paths,
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)
	require.Len(t, resp.RotatedPaths, n)
	require.Empty(t, resp.SkippedPaths)
	require.EqualValues(t, n, resp.NextCheckpoint)

	// Versions bumped to 2 (server-side GetAttr — the client's attr cache may
	// still hold the pre-rotation attrs).
	for i := 0; i < n; i++ {
		var attr Attr
		require.Equal(t, syscall.Errno(0), env.meta.GetAttr(ctx, inodes[i], &attr))
		require.EqualValues(t, 2, attr.FekVersion, "file %d must be rotated", i)
	}

	// Re-run from the checkpoint: zero operations (idempotent).
	resp2, err := pbClient.RotateFileKeysByPaths(userCtx(t, ctx, user), &pb.RotateFileKeysByPathsRequest{
		AdminUserId: user, Paths: paths, Checkpoint: resp.NextCheckpoint,
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp2.Errno)
	require.Empty(t, resp2.RotatedPaths)
	require.Empty(t, resp2.SkippedPaths)
	require.EqualValues(t, n, resp2.NextCheckpoint)

	// A missing path is reported as skipped and still advances the checkpoint.
	resp3, err := pbClient.RotateFileKeysByPaths(userCtx(t, ctx, user), &pb.RotateFileKeysByPathsRequest{
		AdminUserId: user, Paths: []string{"/missing.exr"},
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp3.Errno)
	require.Empty(t, resp3.RotatedPaths)
	require.Len(t, resp3.SkippedPaths, 1)
	require.Contains(t, resp3.SkippedPaths[0], "/missing.exr")
	require.EqualValues(t, 1, resp3.NextCheckpoint)

	// Identity mismatch is rejected.
	resp4, err := pbClient.RotateFileKeysByPaths(userCtx(t, ctx, user), &pb.RotateFileKeysByPathsRequest{
		AdminUserId: uuid.New().String(), Paths: paths,
	})
	require.NoError(t, err)
	require.EqualValues(t, uint32(syscall.EACCES), resp4.Errno)
}

// TestRevoke_LosesFEK (task 7.7, FR-TEST-10/AC-10): grant -> read OK; then the
// platform bumps the permission generation (DeleteRole) and denies the FEK —
// the next heartbeat wipes the client's cached keys (onWipe fires) and a new
// Open fails closed with EACCES.
func TestRevoke_LosesFEK(t *testing.T) {
	env := newEncryptTestEnv(t, 11, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	// grant: create + open OK.
	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "revoke.exr", 0644, 022, 0, &inode, &attr))
	require.True(t, attr.Encrypted)

	var openAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Open(cctx, inode, syscall.O_RDONLY, &openAttr))
	require.Len(t, openAttr.Fek, fekSize)

	gm, ok := env.client.(*grpcMeta)
	require.True(t, ok)
	var wiped int32
	gm.SetOnWipe(func() { atomic.AddInt32(&wiped, 1) })

	// The heartbeat carries the user identity (simulated OIDC).
	gm.heartbeatBaseCtx = userCtx(t, Background(), user)

	// First beat: baseline generation (0), no wipe.
	gm.doHeartbeat()
	require.Equal(t, 1, gm.fekCache.Len(), "the FEK must be cached after Open")
	require.Equal(t, int32(0), atomic.LoadInt32(&wiped))

	// DeleteRole on the platform: generation bumped + FEK denied.
	env.keyMgr.bumpGeneration()
	env.keyMgr.mu.Lock()
	env.keyMgr.denyGetFek = true
	env.keyMgr.mu.Unlock()

	// Next heartbeat: increase -> wipe + InvalidateAllKeys.
	gm.doHeartbeat()
	require.Zero(t, gm.fekCache.Len(), "all cached FEKs must be wiped on a generation increase")
	require.Equal(t, int32(1), atomic.LoadInt32(&wiped), "onWipe (InvalidateAllKeys) must fire")

	// The user loses access: Open fails closed.
	var deniedAttr Attr
	st := env.client.Open(cctx, inode, syscall.O_RDONLY, &deniedAttr)
	require.Equal(t, syscall.EACCES, st)
	require.Nil(t, deniedAttr.Fek)
}
