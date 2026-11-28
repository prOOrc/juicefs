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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInodePathCache_Basic(t *testing.T) {
	c := NewInodePathCache(0) // unlimited

	// Root is always set
	assert.Equal(t, "/", c.Get(context.Background(), RootInode))

	// Set and get
	assert.True(t, c.Set(12345, "/company-abc"))
	assert.Equal(t, "/company-abc", c.Get(context.Background(), 12345))

	// Not found
	assert.Empty(t, c.Get(context.Background(), 99999))
}

func TestInodePathCache_HardlinkSafety(t *testing.T) {
	c := NewInodePathCache(0)

	// First path wins
	assert.True(t, c.Set(100, "/allowed/file"))
	assert.Equal(t, "/allowed/file", c.Get(context.Background(), 100))

	// Second path for same inode is rejected (hardlink)
	assert.False(t, c.Set(100, "/secret/file"))
	assert.Equal(t, "/allowed/file", c.Get(context.Background(), 100)) // unchanged
}

func TestInodePathCache_BuildChildPath(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/company-abc")
	c.Set(200, "/company-abc/projects")

	// Valid parent
	assert.Equal(t, "/company-abc/projects", c.BuildChildPath(context.Background(), 100, "projects"))

	// Unknown parent
	assert.Empty(t, c.BuildChildPath(context.Background(), 99999, "file"))

	// Invalid component names
	assert.Empty(t, c.BuildChildPath(context.Background(), 100, ".."))
	assert.Empty(t, c.BuildChildPath(context.Background(), 100, "."))
	assert.Empty(t, c.BuildChildPath(context.Background(), 100, ""))
	assert.Empty(t, c.BuildChildPath(context.Background(), 100, "foo/bar"))
}

func TestInodePathCache_SetMany(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/dir")

	c.SetMany(map[Ino]string{
		200: "/dir/file1",
		201: "/dir/file2",
		202: "/dir/subdir",
	})

	assert.Equal(t, "/dir/file1", c.Get(context.Background(), 200))
	assert.Equal(t, "/dir/file2", c.Get(context.Background(), 201))
	assert.Equal(t, "/dir/subdir", c.Get(context.Background(), 202))
}

func TestInodePathCache_Unset(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/file")
	assert.Equal(t, "/file", c.Get(context.Background(), 100))
	assert.Equal(t, Ino(100), c.GetInodeByPath("/file"))

	c.Unset(100)
	assert.Empty(t, c.Get(context.Background(), 100))
	assert.Equal(t, Ino(0), c.GetInodeByPath("/file"))
}

func TestInodePathCache_UnsetByPath(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/file")

	c.UnsetByPath("/file")
	assert.Empty(t, c.Get(context.Background(), 100))
	assert.Equal(t, Ino(0), c.GetInodeByPath("/file"))
}

func TestInodePathCache_RenameSubtree(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/old")
	c.Set(200, "/old/a")
	c.Set(300, "/old/a/b")

	c.RenameSubtree("/old", "/new")

	assert.Equal(t, "/new", c.Get(context.Background(), 100))
	assert.Equal(t, "/new/a", c.Get(context.Background(), 200))
	assert.Equal(t, "/new/a/b", c.Get(context.Background(), 300))
}

func TestInodePathCache_FIFO(t *testing.T) {
	c := NewInodePathCache(5) // max 5 entries (including root)

	// Fill to limit
	for i := Ino(10); i < 15; i++ {
		c.Set(i, "/file"+string(rune('a'+i-10)))
	}

	assert.Equal(t, 5, len(c.pathByInode))

	// Add one more — should evict oldest (inode 10)
	c.Set(20, "/file-extra")
	assert.Equal(t, 5, len(c.pathByInode))
	assert.Empty(t, c.Get(context.Background(), 10)) // evicted
	assert.Equal(t, "/file-extra", c.Get(context.Background(), 20))
}

func TestValidPathComponent(t *testing.T) {
	assert.True(t, validPathComponent("file.txt"))
	assert.True(t, validPathComponent("dir-name"))
	assert.False(t, validPathComponent(""))
	assert.False(t, validPathComponent("."))
	assert.False(t, validPathComponent(".."))
	assert.False(t, validPathComponent("foo/bar"))
	assert.False(t, validPathComponent("foo\x00bar"))
}

func TestInodePathCache_GetInodeByPath(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(12345, "/company-abc/projects")

	assert.Equal(t, Ino(12345), c.GetInodeByPath("/company-abc/projects"))
	assert.Equal(t, Ino(0), c.GetInodeByPath("/nonexistent"))
}

func TestInodePathCache_Move(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/old/path")

	// Move to new path
	assert.True(t, c.Move(100, "/new/path"))
	assert.Equal(t, "/new/path", c.Get(context.Background(), 100))
	assert.Equal(t, Ino(100), c.GetInodeByPath("/new/path"))
	assert.Equal(t, Ino(0), c.GetInodeByPath("/old/path")) // old removed

	// Move unknown inode
	assert.False(t, c.Move(999, "/nowhere"))
}

func TestInodePathCache_RemoveSubtree(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/dir")
	c.Set(200, "/dir/a")
	c.Set(300, "/dir/a/b")
	c.Set(400, "/other")

	removed := c.RemoveSubtree("/dir")
	assert.Equal(t, 3, removed) // dir, dir/a, dir/a/b
	assert.Empty(t, c.Get(context.Background(), 100))
	assert.Empty(t, c.Get(context.Background(), 200))
	assert.Empty(t, c.Get(context.Background(), 300))
	assert.Equal(t, "/other", c.Get(context.Background(), 400)) // untouched
}

// TestInodePathCache_RootPinned (spec fix-meta-proxy-per-session-chroot,
// "Session-scoped authz paths under chroot"): since Stage 4 the root inode is
// never stored — its path resolves per the requesting session (I9) and stays
// immune to eviction by construction, while the cache keeps only full volume
// paths.
func TestInodePathCache_RootPinned(t *testing.T) {
	c := NewInodePathCache(1) // capacity for a single entry

	// The root is not stored at all.
	assert.Equal(t, 0, len(c.pathByInode))
	assert.Equal(t, "/", c.Get(context.Background(), RootInode))

	// A chrooted session's context resolves the root to the session root, and
	// children of the root join it (BuildChildPath), not "/".
	sess := &proxySession{root: "companies/acme"}
	ctx := context.WithValue(context.Background(), sessionContextKey{}, sess)
	assert.Equal(t, "/companies/acme", c.Get(ctx, RootInode))
	assert.Equal(t, "/companies/acme/file.exr", c.BuildChildPath(ctx, RootInode, "file.exr"))
	assert.Equal(t, "/file.exr", c.BuildChildPath(context.Background(), RootInode, "file.exr"))

	// Session paths never leak into stored entries: the same session context
	// building a child of a NON-root cached parent yields the full volume path.
	c.Set(100, "/companies/acme/sub/")
	assert.Equal(t, "/companies/acme/sub/entry", c.BuildChildPath(ctx, 100, "entry"))

	// Eviction is unaffected: with maxSize=1 the second Set evicts the first.
	c.Set(200, "/file2")
	assert.Equal(t, 1, len(c.pathByInode))
	assert.Empty(t, c.Get(context.Background(), 100)) // evicted
	assert.Equal(t, "/file2", c.Get(context.Background(), 200))
	assert.Equal(t, "/", c.Get(context.Background(), RootInode)) // root survives eviction
}

func TestInodePathCache_RenameSubtree_SelfGuard(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/a")
	c.Set(200, "/a/b")

	// Renaming /a into descendant should be a no-op
	c.RenameSubtree("/a", "/a/b/a")
	assert.Equal(t, "/a", c.Get(context.Background(), 100))
	assert.Equal(t, "/a/b", c.Get(context.Background(), 200))
}

func TestInodePathCache_SetMany_SkipsExisting(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/existing")

	// SetMany should skip inode 100 (already exists)
	c.SetMany(map[Ino]string{
		100: "/different", // should be skipped
		200: "/new",
	})

	assert.Equal(t, "/existing", c.Get(context.Background(), 100)) // unchanged
	assert.Equal(t, "/new", c.Get(context.Background(), 200))
}

func TestWithDirSlash(t *testing.T) {
	dirAttr := &Attr{Typ: TypeDirectory}
	fileAttr := &Attr{Typ: TypeFile}

	assert.Equal(t, "/a/b/", withDirSlash("/a/b", dirAttr))
	assert.Equal(t, "/a/b", withDirSlash("/a/b", fileAttr))
	assert.Equal(t, "", withDirSlash("", dirAttr))
	assert.Equal(t, "/a/b", withDirSlash("/a/b", nil))
}

func TestInodePathCache_GetInodeByPath_DirTrailingSlash(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/dir/") // directory stored with trailing slash

	// Reverse lookup works with and without the trailing slash
	assert.Equal(t, Ino(100), c.GetInodeByPath("/dir/"))
	assert.Equal(t, Ino(100), c.GetInodeByPath("/dir"))
	assert.Equal(t, Ino(0), c.GetInodeByPath("/other"))

	// Exact match takes precedence: file "/same" wins over directory "/same/"
	c.Set(200, "/same")
	c.Set(300, "/same/")
	assert.Equal(t, Ino(200), c.GetInodeByPath("/same"))
	assert.Equal(t, Ino(300), c.GetInodeByPath("/same/"))
}

func TestInodePathCache_RemoveSubtree_TrailingSlash(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/dir/") // directory stored with trailing slash
	c.Set(200, "/dir/a")
	c.Set(300, "/dir/a/b")
	c.Set(400, "/other")

	// Prefix with trailing slash (as built for directories)
	removed := c.RemoveSubtree("/dir/")
	assert.Equal(t, 3, removed) // dir/, dir/a, dir/a/b
	assert.Empty(t, c.Get(context.Background(), 100))
	assert.Empty(t, c.Get(context.Background(), 200))
	assert.Empty(t, c.Get(context.Background(), 300))
	assert.Equal(t, "/other", c.Get(context.Background(), 400)) // untouched

	// Root is pinned: RemoveSubtree("/") removes nothing
	c.Set(500, "/x")
	assert.Equal(t, 0, c.RemoveSubtree("/"))
	assert.Equal(t, "/", c.Get(context.Background(), RootInode))
	assert.Equal(t, "/x", c.Get(context.Background(), 500))
}

func TestInodePathCache_RenameSubtree_TrailingSlash(t *testing.T) {
	c := NewInodePathCache(0)
	c.Set(100, "/old/") // directory stored with trailing slash
	c.Set(200, "/old/a")
	c.Set(300, "/old/a/b")

	// Prefixes with trailing slashes (as built for directories)
	c.RenameSubtree("/old/", "/new/")

	assert.Equal(t, "/new/", c.Get(context.Background(), 100))
	assert.Equal(t, "/new/a", c.Get(context.Background(), 200))
	assert.Equal(t, "/new/a/b", c.Get(context.Background(), 300))
	assert.Equal(t, Ino(100), c.GetInodeByPath("/new/"))
	assert.Equal(t, Ino(0), c.GetInodeByPath("/old/"))
}
