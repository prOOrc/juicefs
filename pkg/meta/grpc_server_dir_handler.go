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
	"context"
	"syscall"

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

func (s *MetaProxyServer) NewDirHandler(ctx context.Context, req *pb.NewDirHandlerRequest) (*pb.NewDirHandlerResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	initEntries := ProtoToEntries(req.InitEntries)
	handler, errno := s.meta.NewDirHandler(mctx, Ino(req.Inode), req.Plus, initEntries)
	if errno != 0 {
		return &pb.NewDirHandlerResponse{Errno: uint32(errno)}, nil
	}
	s.mu.Lock()
	handleID := s.nextHandle
	s.nextHandle++
	s.handlers[handleID] = &dirHandlerEntry{handler: handler, inode: Ino(req.Inode)}
	s.mu.Unlock()
	return &pb.NewDirHandlerResponse{
		Errno:  0,
		Handle: &pb.DirHandlerHandle{HandleId: handleID},
	}, nil
}

func (s *MetaProxyServer) DirHandlerList(ctx context.Context, req *pb.DirHandlerListRequest) (*pb.DirHandlerListResponse, error) {
	s.mu.Lock()
	entry, ok := s.handlers[req.Handle.HandleId]
	s.mu.Unlock()
	if !ok {
		return &pb.DirHandlerListResponse{Errno: uint32(syscall.EBADF)}, nil
	}

	var filtered []*Entry
	if s.authzInterceptor != nil && s.authzInterceptor.enabled {
		// Authz mode: serve from a stable filtered snapshot. The FUSE offset
		// protocol counts entries the kernel actually received, so the offset
		// indexes the filtered stream directly — no duplicates, clean EOF.
		list, errno := s.authzListing(ctx, entry)
		if errno != 0 {
			return &pb.DirHandlerListResponse{Errno: uint32(errno)}, nil
		}
		off := int(req.Offset)
		if off < 0 || off >= len(list) {
			return &pb.DirHandlerListResponse{Errno: 0}, nil
		}
		filtered = list[off:]
	} else {
		entries, errno := entry.handler.List(Background(), int(req.Offset))
		if errno != 0 {
			return &pb.DirHandlerListResponse{Errno: uint32(errno)}, nil
		}
		filtered = entries
	}

	protoEntries := make([]*pb.ProtoEntry, len(filtered))
	for i, e := range filtered {
		protoEntries[i] = EntryToProto(e)
	}
	return &pb.DirHandlerListResponse{Errno: 0, Entries: protoEntries}, nil
}

// authzListing returns the full filtered listing for an authz dir handler,
// priming the per-handle snapshot on first use. Fetching everything and
// filtering once (instead of filtering each DirHandler batch) keeps the
// listing a stable stream: the DirHandler cursor would otherwise advance by
// unfiltered counts while the kernel advances by filtered counts, re-serving
// earlier entries as duplicates.
func (s *MetaProxyServer) authzListing(ctx context.Context, entry *dirHandlerEntry) ([]*Entry, syscall.Errno) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.authzReady {
		return entry.authzList, 0
	}

	var entries []*Entry
	errno := s.meta.Readdir(Background(), entry.inode, 1, &entries)
	if errno != 0 {
		return nil, errno
	}
	// baseMeta.Readdir prepends "." and ".."; the FUSE kernel synthesizes those.
	if len(entries) >= 2 && string(entries[0].Name) == "." && string(entries[1].Name) == ".." {
		entries = entries[2:]
	}

	filtered := s.filterEntriesByAuthz(ctx, entry.inode, entries)

	// Cache paths for allowed entries (prevents cache pollution from unauthorized paths).
	if len(filtered) > 0 {
		mappings := make(map[Ino]string)
		for _, e := range filtered {
			childPath := s.inodePathCache.BuildChildPath(entry.inode, string(e.Name))
			if childPath != "" {
				mappings[e.Inode] = withDirSlash(childPath, e.Attr)
			}
		}
		if len(mappings) > 0 {
			s.inodePathCache.SetMany(mappings)
		}
	}

	entry.authzList = filtered
	entry.authzReady = true
	return entry.authzList, 0
}

func (s *MetaProxyServer) DirHandlerInsert(ctx context.Context, req *pb.DirHandlerInsertRequest) (*pb.DirHandlerInsertResponse, error) {
	s.mu.Lock()
	entry, ok := s.handlers[req.Handle.HandleId]
	s.mu.Unlock()
	if !ok {
		return &pb.DirHandlerInsertResponse{Errno: uint32(syscall.EBADF)}, nil
	}
	entry.handler.Insert(Ino(req.Inode), req.Name, ProtoToAttr(req.Attr))
	return &pb.DirHandlerInsertResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) DirHandlerDelete(ctx context.Context, req *pb.DirHandlerDeleteRequest) (*pb.DirHandlerDeleteResponse, error) {
	s.mu.Lock()
	entry, ok := s.handlers[req.Handle.HandleId]
	s.mu.Unlock()
	if !ok {
		return &pb.DirHandlerDeleteResponse{Errno: uint32(syscall.EBADF)}, nil
	}
	entry.handler.Delete(req.Name)
	return &pb.DirHandlerDeleteResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) DirHandlerClose(ctx context.Context, req *pb.DirHandlerCloseRequest) (*pb.DirHandlerCloseResponse, error) {
	s.mu.Lock()
	entry, ok := s.handlers[req.Handle.HandleId]
	delete(s.handlers, req.Handle.HandleId)
	s.mu.Unlock()
	if ok {
		entry.handler.Close()
	}
	return &pb.DirHandlerCloseResponse{Errno: 0}, nil
}
