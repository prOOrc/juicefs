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
	"path"
	"strings"
	"sync"
)

// InodePathCache maintains bidirectional mappings between inodes and filesystem paths.
// It is populated by observing MetaProxyServer operations (Lookup, Readdir, Mkdir, etc.).
//
// Design decisions:
//   - inode → path: single mapping per inode (first-seen wins for hardlinks)
//   - path → inode: reverse lookup for Unlink/Rmdir cache cleanup
//   - FIFO eviction: bounded size to prevent memory exhaustion
//   - RootInode is pinned and never evicted
//   - Path validation: rejects "..", "/", and null bytes in component names
//   - Hardlinks: stores only the first observed path per inode. Operations via
//     alternative hardlink paths may be denied or authorized based on the primary
//     cached path. For production, hardlink support requires multiple paths per
//     inode or client-provided paths.
type InodePathCache struct {
	mu          sync.RWMutex
	pathByInode map[Ino]string // inode → full path (primary)
	inodeByPath map[string]Ino // path → inode (reverse for cleanup)
	order       []Ino          // insertion order for FIFO eviction
	maxSize     int            // 0 = unlimited
}

// NewInodePathCache creates a new cache with root inode registered as "/".
func NewInodePathCache(maxSize int) *InodePathCache {
	c := &InodePathCache{
		pathByInode: make(map[Ino]string),
		inodeByPath: make(map[string]Ino),
		maxSize:     maxSize,
	}
	c.Set(RootInode, "/")
	return c
}

// validPathComponent checks that a name is a single filesystem component.
func validPathComponent(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.Contains(name, "/") {
		return false
	}
	if strings.ContainsRune(name, '\x00') {
		return false
	}
	return true
}

// Get returns the cached path for an inode.
// Returns empty string if not found.
func (c *InodePathCache) Get(inode Ino) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pathByInode[inode]
}

// GetInodeByPath returns the cached inode for a path (reverse lookup).
// Directories are stored with a trailing slash; both forms are checked,
// with the exact match taking precedence (a file and a directory may share a name).
func (c *InodePathCache) GetInodeByPath(p string) Ino {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if inode, ok := c.inodeByPath[p]; ok {
		return inode
	}
	return c.inodeByPath[p+"/"]
}

// Set maps an inode to a path. If the inode already has a different path,
// it is NOT overwritten (hardlink safety). Returns true if set, false if skipped.
func (c *InodePathCache) Set(inode Ino, p string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Don't overwrite existing mapping (hardlink safety)
	if existing, ok := c.pathByInode[inode]; ok {
		if existing == p {
			return true // same path, no-op
		}
		return false // different path — hardlink, skip
	}

	c.pathByInode[inode] = p
	c.inodeByPath[p] = inode

	// Deduplicate: don't add to order if already present (e.g., after Unset+Set)
	for _, existing := range c.order {
		if existing == inode {
			return true // already in order, no need to add again
		}
	}
	c.order = append(c.order, inode)

	// FIFO eviction if over limit
	if c.maxSize > 0 && len(c.pathByInode) > c.maxSize {
		c.evictOldestLocked()
	}

	return true
}

// SetMany maps multiple inodes to paths (batch for Readdir).
// Skips inodes that already exist in the cache (both hardlink and dedup).
func (c *InodePathCache) SetMany(mappings map[Ino]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for inode, p := range mappings {
		if _, ok := c.pathByInode[inode]; ok {
			continue
		}
		c.pathByInode[inode] = p
		c.inodeByPath[p] = inode
		c.order = append(c.order, inode)
	}

	// FIFO eviction if over limit
	if c.maxSize > 0 && len(c.pathByInode) > c.maxSize {
		for len(c.pathByInode) > c.maxSize {
			c.evictOldestLocked()
		}
	}
}

// Unset removes a path mapping by inode.
func (c *InodePathCache) Unset(inode Ino) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if p, ok := c.pathByInode[inode]; ok {
		delete(c.inodeByPath, p)
		delete(c.pathByInode, inode)
	}
}

// UnsetByPath removes a path mapping by path string.
func (c *InodePathCache) UnsetByPath(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if inode, ok := c.inodeByPath[p]; ok {
		delete(c.pathByInode, inode)
		delete(c.inodeByPath, p)
	}
}

// Move updates the path for an existing inode (for Rename operations).
// Unlike Set(), this overwrites the existing mapping even if the inode is already present.
// Returns true if the inode was found and updated.
func (c *InodePathCache) Move(inode Ino, newPath string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldPath, ok := c.pathByInode[inode]
	if !ok {
		return false // inode not in cache
	}

	// Update reverse mapping
	delete(c.inodeByPath, oldPath)
	c.inodeByPath[newPath] = inode
	c.pathByInode[inode] = newPath

	return true
}

// RemoveSubtree removes all paths under a prefix (for Rmdir operations).
// The prefix may end with "/" or not — directories are stored with a trailing slash.
// Returns the number of removed entries.
func (c *InodePathCache) RemoveSubtree(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return 0 // root — pinned, never removed
	}
	withSlash := prefix + "/"
	removed := 0

	for inode, p := range c.pathByInode {
		if p == prefix || p == withSlash || strings.HasPrefix(p, withSlash) {
			delete(c.inodeByPath, p)
			delete(c.pathByInode, inode)
			removed++
		}
	}

	return removed
}

// RenameSubtree updates all paths under oldPrefix to newPrefix.
// Called when a directory is renamed.
// Prefixes may end with "/" or not — directories are stored with a trailing slash.
// Guards against renaming a directory into itself (e.g., /a → /a/b/a).
func (c *InodePathCache) RenameSubtree(oldPrefix, newPrefix string) {
	oldPrefix = strings.TrimSuffix(oldPrefix, "/")
	newPrefix = strings.TrimSuffix(newPrefix, "/")

	// Guard: don't rename into self or descendant
	if newPrefix == oldPrefix || strings.HasPrefix(newPrefix, oldPrefix+"/") {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	oldWithSlash := oldPrefix + "/"
	newWithSlash := newPrefix + "/"

	for inode, p := range c.pathByInode {
		if strings.HasPrefix(p, oldWithSlash) {
			// Remove old reverse mapping
			delete(c.inodeByPath, p)

			// Set new path
			newPath := newWithSlash + p[len(oldWithSlash):]
			c.pathByInode[inode] = newPath
			c.inodeByPath[newPath] = inode
		}
	}

	// Also update the directory itself (stored as oldPrefix or oldPrefix+"/")
	for _, self := range []string{oldPrefix, oldWithSlash} {
		if inode, ok := c.inodeByPath[self]; ok {
			delete(c.inodeByPath, self)
			newSelf := newPrefix
			if self == oldWithSlash {
				newSelf = newWithSlash
			}
			c.pathByInode[inode] = newSelf
			c.inodeByPath[newSelf] = inode
		}
	}
}

// BuildChildPath constructs the full path for a child entry given its parent inode and name.
// Returns empty string if parent is not in cache or name is invalid.
func (c *InodePathCache) BuildChildPath(parent Ino, name string) string {
	if !validPathComponent(name) {
		return ""
	}

	c.mu.RLock()
	parentPath, ok := c.pathByInode[parent]
	c.mu.RUnlock()

	if !ok {
		return ""
	}

	return path.Join(parentPath, name)
}

// withDirSlash returns p with a trailing slash if attr describes a directory.
// Authorization paths follow the S3/DriveAuth convention: folder paths end with
// "/" (like S3 prefixes), file paths don't.
func withDirSlash(p string, attr *Attr) string {
	if p != "" && attr != nil && attr.Typ == TypeDirectory {
		return p + "/"
	}
	return p
}

// evictOldest removes the oldest entry (must be called with lock held).
func (c *InodePathCache) evictOldestLocked() {
	for len(c.order) > 0 {
		inode := c.order[0]
		c.order = c.order[1:]

		// Pin RootInode — never evict it, or the cache becomes useless
		if inode == RootInode {
			continue
		}

		// Skip stale entries (already removed by Unset/UnsetByPath)
		if p, ok := c.pathByInode[inode]; ok {
			delete(c.inodeByPath, p)
			delete(c.pathByInode, inode)
			return
		}
	}
}
