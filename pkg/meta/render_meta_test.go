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
	"crypto/rand"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// renderFakeMeta is a minimal in-memory Meta for RenderMeta unit tests. The
// embedded nil Meta panics if a test accidentally exercises an unstubbed method.
type renderFakeMeta struct {
	Meta

	openSt      syscall.Errno
	createSt    syscall.Errno
	setCryptoSt syscall.Errno
	attr        *Attr
	created     *FileCrypto
	unlinked    []string
}

var _ Meta = (*renderFakeMeta)(nil)

func (f *renderFakeMeta) Open(ctx Context, inode Ino, flags uint32, attr *Attr) syscall.Errno {
	if f.openSt != 0 {
		return f.openSt
	}
	*attr = *f.attr
	return 0
}

func (f *renderFakeMeta) Create(ctx Context, parent Ino, name string, mode uint16, cumask uint16, flags uint32, inode *Ino, attr *Attr) syscall.Errno {
	if f.createSt != 0 {
		return f.createSt
	}
	*inode = 42
	*attr = *f.attr
	return 0
}

func (f *renderFakeMeta) Unlink(ctx Context, parent Ino, name string, skipCheckTrash ...bool) syscall.Errno {
	f.unlinked = append(f.unlinked, name)
	return 0
}

func (f *renderFakeMeta) SetFileCrypto(ctx Context, inode Ino, c *FileCrypto) syscall.Errno {
	if f.setCryptoSt != 0 {
		return f.setCryptoSt
	}
	f.created = c
	return 0
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func TestRenderMeta_Open_UnwrapsFEK(t *testing.T) {
	kek := randomBytes(t, kekSize)
	fek := randomBytes(t, fekSize)
	wrapped, err := WrapFEK(kek, fek, FekAAD{VolumeUUID: "vol-1", CompanyID: "comp-1", DriveFileID: "dfid-1", Inode: 7, FekVersion: 1}, 1)
	require.NoError(t, err)

	inner := &renderFakeMeta{attr: &Attr{Typ: TypeFile, Encrypted: true, WrappedFek: wrapped, DriveFileID: "dfid-1", FekVersion: 1}}
	rm := NewRenderMeta(inner, kek, 1, "vol-1", "comp-1", "companies/comp-1")

	var attr Attr
	st := rm.Open(Background(), 7, syscall.O_RDONLY, &attr)
	require.Equal(t, syscall.Errno(0), st)
	require.Equal(t, fek, attr.Fek)
	require.EqualValues(t, 1, attr.FekVersion)
}

func TestRenderMeta_CrossCompany_FailClosed(t *testing.T) {
	// The file was wrapped under company A's KEK; a render node of company B
	// (different KEK) must get EIO and no key material (FR-TEST-12, AC-5).
	kekA := randomBytes(t, kekSize)
	kekB := randomBytes(t, kekSize)
	fek := randomBytes(t, fekSize)
	wrapped, err := WrapFEK(kekA, fek, FekAAD{VolumeUUID: "vol-1", CompanyID: "comp-A", DriveFileID: "dfid-1", Inode: 7, FekVersion: 1}, 1)
	require.NoError(t, err)

	inner := &renderFakeMeta{attr: &Attr{Typ: TypeFile, Encrypted: true, WrappedFek: wrapped, DriveFileID: "dfid-1", FekVersion: 1}}
	rm := NewRenderMeta(inner, kekB, 1, "vol-1", "comp-B", "companies/comp-B")

	var attr Attr
	st := rm.Open(Background(), 7, syscall.O_RDONLY, &attr)
	require.Equal(t, syscall.EIO, st)
	require.Nil(t, attr.Fek)
}

func TestRenderMeta_Create_GeneratesFEK(t *testing.T) {
	kek := randomBytes(t, kekSize)
	inner := &renderFakeMeta{attr: &Attr{Typ: TypeFile}}
	rm := NewRenderMeta(inner, kek, 3, "vol-1", "comp-1", "companies/comp-1")

	var inode Ino
	var attr Attr
	st := rm.Create(Background(), RootInode, "f.exr", 0644, 022, 0, &inode, &attr)
	require.Equal(t, syscall.Errno(0), st)
	require.True(t, attr.Encrypted)
	require.Len(t, attr.WrappedFek, agfkLen)
	require.NotEmpty(t, attr.DriveFileID)
	require.Len(t, attr.Fek, fekSize)

	// The persisted blob unwraps to exactly the delivered FEK (FR-RND-9), and
	// embeds the KEK version from FetchCompanyKEK.
	fek, ver, err := UnwrapFEK(kek, inner.created.WrappedFek, FekAAD{
		VolumeUUID: "vol-1", CompanyID: "comp-1", DriveFileID: attr.DriveFileID, Inode: inode, FekVersion: 1,
	})
	require.NoError(t, err)
	require.Equal(t, attr.Fek, fek)
	require.EqualValues(t, 3, ver)

	t.Run("SetFileCrypto failure rolls the file back", func(t *testing.T) {
		inner2 := &renderFakeMeta{attr: &Attr{Typ: TypeFile}, setCryptoSt: syscall.EIO}
		rm2 := NewRenderMeta(inner2, kek, 1, "vol-1", "comp-1", "companies/comp-1")

		var inode2 Ino
		var attr2 Attr
		st := rm2.Create(Background(), RootInode, "g.exr", 0644, 022, 0, &inode2, &attr2)
		require.Equal(t, syscall.EIO, st)
		require.Equal(t, []string{"g.exr"}, inner2.unlinked)
	})
}

func TestRenderMeta_FekCache(t *testing.T) {
	kek := randomBytes(t, kekSize)
	fek1 := randomBytes(t, fekSize)
	aad1 := FekAAD{VolumeUUID: "vol-1", CompanyID: "comp-1", DriveFileID: "d", Inode: 7, FekVersion: 1}
	wrapped1, err := WrapFEK(kek, fek1, aad1, 1)
	require.NoError(t, err)

	inner := &renderFakeMeta{attr: &Attr{Encrypted: true, WrappedFek: wrapped1, DriveFileID: "d", FekVersion: 1}}
	rm := newRenderMetaWithCache(inner, kek, 1, "vol-1", "comp-1", "companies/comp-1", 10, 50*time.Millisecond)

	var attr Attr
	require.Equal(t, syscall.Errno(0), rm.Open(Background(), 7, 0, &attr))
	require.Equal(t, fek1, attr.Fek)

	// Swap the stored blob for a different FEK at the same version: an LRU hit
	// must keep returning the cached FEK without re-unwrapping.
	fek2 := randomBytes(t, fekSize)
	wrapped2, err := WrapFEK(kek, fek2, aad1, 1)
	require.NoError(t, err)
	inner.attr.WrappedFek = wrapped2

	var attr2 Attr
	require.Equal(t, syscall.Errno(0), rm.Open(Background(), 7, 0, &attr2))
	require.Equal(t, fek1, attr2.Fek, "LRU hit must not re-unwrap")

	// After the TTL expires the new blob is unwrapped.
	time.Sleep(60 * time.Millisecond)
	var attr3 Attr
	require.Equal(t, syscall.Errno(0), rm.Open(Background(), 7, 0, &attr3))
	require.Equal(t, fek2, attr3.Fek, "expired entry must be re-unwrapped")

	t.Run("eviction zeroes the FEK in place", func(t *testing.T) {
		fek3 := randomBytes(t, fekSize)
		aad8 := FekAAD{VolumeUUID: "vol-1", CompanyID: "comp-1", DriveFileID: "d", Inode: 8, FekVersion: 1}
		wrapped8, err := WrapFEK(kek, fek3, aad8, 1)
		require.NoError(t, err)

		rmSmall := newRenderMetaWithCache(inner, kek, 1, "vol-1", "comp-1", "companies/comp-1", 1, time.Hour)
		var a Attr
		require.Equal(t, syscall.Errno(0), rmSmall.Open(Background(), 7, 0, &a))
		cached := a.Fek

		inner.attr = &Attr{Encrypted: true, WrappedFek: wrapped8, DriveFileID: "d", FekVersion: 1}
		var b Attr
		require.Equal(t, syscall.Errno(0), rmSmall.Open(Background(), 8, 0, &b))

		for _, by := range cached {
			require.Zero(t, by)
		}
	})
}
