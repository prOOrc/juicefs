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
	"syscall"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestLogout_CacheUnreadable (AC-8, NFR-SEC-5): on logout all plaintext keys on
// the client are wiped immediately; the client is disconnected (terminal) and
// fails closed, and with the platform's permissions revoked a fresh client
// ("remount") cannot obtain a FEK either — cached data is unreadable.
func TestLogout_CacheUnreadable(t *testing.T) {
	env := newEncryptTestEnv(t, 12, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "logout.exr", 0644, 022, 0, &inode, &attr))
	require.Equal(t, 1, env.keyMgr.createCalls)

	// Open caches the plaintext FEK on the client.
	var openAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Open(cctx, inode, syscall.O_RDONLY, &openAttr))
	require.Len(t, openAttr.Fek, fekSize)
	require.Equal(t, 1, env.keyMgr.getFekCalls)

	gm, ok := env.client.(*grpcMeta)
	require.True(t, ok)
	require.Equal(t, HubOnline, gm.HubState())
	require.Equal(t, 1, gm.fekCache.Len())

	// The platform revokes the user's permissions as part of the logout.
	env.keyMgr.mu.Lock()
	env.keyMgr.denyGetFek = true
	env.keyMgr.mu.Unlock()

	// Logout (what the VFS _JFS_LOGOUT watcher triggers): keys wiped, terminal.
	gm.Logout()

	require.Equal(t, HubDisconnected, gm.HubState())
	require.Zero(t, gm.fekCache.Len(), "FEK cache must be empty after logout")
	require.Equal(t, make([]byte, fekSize), openAttr.Fek, "cached FEK backing array must be zeroed")

	// The logged-out client fails closed without even reaching KeyManager.
	var reopenAttr Attr
	st := env.client.Open(cctx, inode, syscall.O_RDONLY, &reopenAttr)
	require.Equal(t, syscall.EIO, st, "re-open on a disconnected client must fail fast")
	require.Nil(t, reopenAttr.Fek)
	require.Equal(t, 1, env.keyMgr.getFekCalls, "a disconnected client must not call KeyManager")

	// A fresh client ("remount") hits the revoked permissions: fail-closed EACCES.
	remounted := env.dial(t)
	defer remounted.Shutdown()

	var remountAttr Attr
	st = remounted.Open(cctx, inode, syscall.O_RDONLY, &remountAttr)
	require.Equal(t, syscall.EACCES, st, "revoked permissions must deny the FEK")
	require.Nil(t, remountAttr.Fek)
	require.Equal(t, 2, env.keyMgr.getFekCalls, "the denied call must still be audited by KeyManager")

	// The stored metadata is intact: with valid permissions a fresh client gets
	// a working FEK (logout wipes client keys, not the volume).
	env.keyMgr.mu.Lock()
	env.keyMgr.denyGetFek = false
	env.keyMgr.mu.Unlock()

	st = remounted.Open(cctx, inode, syscall.O_RDONLY, &remountAttr)
	require.Equal(t, syscall.Errno(0), st)
	require.Len(t, remountAttr.Fek, fekSize)
	format := env.meta.GetFormat()
	fek, _, err := UnwrapFEK(env.keyMgr.kek, attr.WrappedFek, FekAAD{
		VolumeUUID:  format.UUID,
		CompanyID:   testCompanyID,
		DriveFileID: attr.DriveFileID,
		Inode:       inode,
		FekVersion:  1,
	})
	require.NoError(t, err)
	require.Equal(t, remountAttr.Fek, fek, "the re-issued FEK must match the stored AGFK blob")
}
