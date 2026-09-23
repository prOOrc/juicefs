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
	"os"
	"path/filepath"
	crand "crypto/rand"
	"syscall"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/stretchr/testify/require"
)

// TestLogoutFile_WipesKeys verifies the _JFS_LOGOUT control file (task 6.2,
// design 6.6): when the platform creates the file at the mount point root, the
// watcher wipes the open handles' keys and removes the file.
func TestLogoutFile_WipesKeys(t *testing.T) {
	v, _ := createTestVFS(nil, "")
	mp := t.TempDir()
	v.Conf.Meta.MountPoint = mp
	ctx := NewLogContext(meta.NewContext(10, 1, []uint32{2, 3}))

	fe, e := v.Mknod(ctx, 1, "enc", 0644|syscall.S_IFREG, 0, 0)
	require.Equal(t, syscall.Errno(0), e)
	fek := make([]byte, 32)
	_, err := crand.Read(fek)
	require.NoError(t, err)
	attr := &meta.Attr{Encrypted: true, Fek: fek, FekVersion: 1}
	fh := v.newFileHandle(fe.Inode, 0, syscall.O_RDWR, 0, attr)

	v.startLogoutWatcher()

	// The platform logs the client out.
	require.NoError(t, os.WriteFile(filepath.Join(mp, logoutFileName), nil, 0600))

	// The watcher (1s poll) must remove the file and mark the handle stale.
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(filepath.Join(mp, logoutFileName))
		return os.IsNotExist(statErr)
	}, 5*time.Second, 50*time.Millisecond, "logout file must be removed by the watcher")

	h := v.findHandle(fe.Inode, fh)
	require.NotNil(t, h)
	require.True(t, h.stale, "handle must be stale after logout")
	require.Nil(t, h.fek)
	require.Equal(t, make([]byte, 32), fek, "FEK backing array must be zeroed")

	buf := make([]byte, 4)
	_, e = v.Read(ctx, fe.Inode, buf, 0, fh)
	require.Equal(t, syscall.EIO, e, "read after logout must fail closed")
}
