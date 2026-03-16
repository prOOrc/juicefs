//go:build !noredis
// +build !noredis

/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
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
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// publishEventToPipe publishes a filesystem event to the outbox via pipeline
func (m *redisMeta) publishEventToPipe(ctx Context, pipe redis.Pipeliner, event *JuiceFsEvent) {
	if m.outbox == nil || !m.outbox.Enabled() {
		return
	}
	// Skip internal JuiceFS trash operations
	if event.Inode >= TrashInode {
		return
	}

	event.Timestamp = time.Now()
	event.Volume = m.fmt.Name
	if ctx != nil {
		event.Uid = ctx.Uid()
		event.Gid = ctx.Gid()
	}
	if pipe != nil {
		m.outbox.AddEventToPipe(ctx, pipe, event)
	}
}

// publishFileCreatedToPipe publishes a FileCreated event to the pipeline
func (m *redisMeta) publishFileCreatedToPipe(ctx Context, pipe redis.Pipeliner, inode Ino, parent Ino, name string, mode uint16) {
	m.publishEventToPipe(ctx, pipe, &JuiceFsEvent{
		Type:   FileCreated,
		Inode:  inode,
		Parent: parent,
		Name:   name,
		Path:   m.reconstructPath(ctx, inode, parent, name),
		Mode:   mode,
		Subdir: m.conf.Subdir,
	})
}

// publishDirCreatedToPipe publishes a DirCreated event to the pipeline
func (m *redisMeta) publishDirCreatedToPipe(ctx Context, pipe redis.Pipeliner, inode Ino, parent Ino, name string, mode uint16) {
	m.publishEventToPipe(ctx, pipe, &JuiceFsEvent{
		Type:   DirCreated,
		Inode:  inode,
		Parent: parent,
		Name:   name,
		Path:   m.reconstructPath(ctx, inode, parent, name),
		Mode:   mode,
		Subdir: m.conf.Subdir,
	})
}

// publishFileDeletedToPipe publishes a FileDeleted event to the pipeline
func (m *redisMeta) publishFileDeletedToPipe(ctx Context, pipe redis.Pipeliner, inode Ino, parent Ino, name string) {
	m.publishEventToPipe(ctx, pipe, &JuiceFsEvent{
		Type:   FileDeleted,
		Inode:  inode,
		Parent: parent,
		Name:   name,
		Path:   m.reconstructPath(ctx, inode, parent, name),
		Subdir: m.conf.Subdir,
	})
}

// publishDirDeletedToPipe publishes a DirDeleted event to the pipeline
func (m *redisMeta) publishDirDeletedToPipe(ctx Context, pipe redis.Pipeliner, inode Ino, parent Ino, name string) {
	m.publishEventToPipe(ctx, pipe, &JuiceFsEvent{
		Type:   DirDeleted,
		Inode:  inode,
		Parent: parent,
		Name:   name,
		Path:   m.reconstructPath(ctx, inode, parent, name),
		Subdir: m.conf.Subdir,
	})
}

// publishFileMovedToPipe publishes a FileMoved event to the pipeline
func (m *redisMeta) publishFileMovedToPipe(ctx Context, pipe redis.Pipeliner, inode Ino, parentDst Ino, nameDst string, parentSrc Ino, nameSrc string) {
	m.publishEventToPipe(ctx, pipe, &JuiceFsEvent{
		Type:    FileMoved,
		Inode:   inode,
		Parent:  parentDst,
		Name:    nameDst,
		Path:    m.reconstructPath(ctx, inode, parentDst, nameDst),
		OldPath: m.reconstructPath(ctx, inode, parentSrc, nameSrc),
		Subdir:  m.conf.Subdir,
	})
}

// publishFileWrittenToPipe publishes a FileWritten event to the pipeline
func (m *redisMeta) publishFileWrittenToPipe(ctx Context, pipe redis.Pipeliner, inode Ino, size uint64) {
	path := m.reconstructPath(ctx, inode, 0, "")
	event := &JuiceFsEvent{
		Type:   FileWritten,
		Inode:  inode,
		Size:   size,
		Name:   filepath.Base(path),
		Path:   path,
		Subdir: m.conf.Subdir,
	}
	m.publishEventToPipe(ctx, pipe, event)
}

// reconstructPath rebuilds the full path for an inode
// parent is the direct parent inode, name is the direct name
// If parent is 0 or name is empty, it reconstructs from the inode's attributes
func (m *redisMeta) reconstructPath(ctx Context, inode Ino, parent Ino, name string) string {
	if inode == RootInode {
		return "/"
	}
	if inode == TrashInode {
		return "/.trash"
	}

	// If we don't have parent/name info, try to get it from attributes
	if parent == 0 || name == "" {
		var attr Attr
		if st := m.en.doGetAttr(ctx, inode, &attr); st != 0 {
			return ""
		}
		parent = attr.Parent
		if parent == 0 {
			return ""
		}
		// For files with parent, we need to lookup the name from parent directory
		if name == "" && inode != TrashInode {
			var entries []*Entry
			if st := m.en.doReaddir(ctx, parent, 0, &entries, -1); st == 0 {
				for _, e := range entries {
					if e.Inode == inode {
						name = string(e.Name)
						break
					}
				}
			}
		}
	}

	// If we still don't have name, we can't reconstruct the full path
	if name == "" {
		return ""
	}

	// Build path by traversing up the tree
	var pathParts []string
	pathParts = append(pathParts, name)
	currentParent := parent

	for currentParent != RootInode && currentParent != m.root && currentParent != 0 {
		// Get parent's attributes to find its parent and name
		var attr Attr
		if st := m.en.doGetAttr(ctx, currentParent, &attr); st != 0 {
			break
		}
		if attr.Typ != TypeDirectory {
			break
		}
		grandparent := attr.Parent
		if grandparent == 0 {
			break
		}
		// Lookup currentParent's name from grandparent directory
		var entries []*Entry
		if st := m.en.doReaddir(ctx, grandparent, 0, &entries, -1); st != 0 {
			break
		}
		var foundName string
		for _, e := range entries {
			if e.Inode == currentParent {
				foundName = string(e.Name)
				break
			}
		}
		if foundName == "" {
			break
		}
		pathParts = append(pathParts, foundName)
		currentParent = grandparent
	}

	// Reverse and join
	for i, j := 0, len(pathParts)-1; i < j; i, j = i+1, j-1 {
		pathParts[i], pathParts[j] = pathParts[j], pathParts[i]
	}

	result := "/" + strings.Join(pathParts, "/")
	return result
}
