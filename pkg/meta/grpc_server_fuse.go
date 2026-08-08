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
	"time"

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

func (s *MetaProxyServer) StatFS(ctx context.Context, req *pb.StatFSRequest) (*pb.StatFSResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var total, avail, iused, iavail uint64
	errno := s.meta.StatFS(mctx, Ino(req.Ino), &total, &avail, &iused, &iavail)
	return &pb.StatFSResponse{
		Errno:      uint32(errno),
		TotalSpace: total,
		AvailSpace: avail,
		Iused:      iused,
		Iavail:     iavail,
	}, nil
}

func (s *MetaProxyServer) Lookup(ctx context.Context, req *pb.LookupRequest) (*pb.LookupResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Lookup(mctx, Ino(req.Parent), req.Name, &inode, &attr, req.CheckPermission)

	if errno == 0 {
		childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if childPath != "" {
			s.inodePathCache.Set(inode, childPath)
		}
	}

	return &pb.LookupResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Resolve(ctx context.Context, req *pb.ResolveRequest) (*pb.ResolveResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Resolve(mctx, Ino(req.Parent), req.Path, &inode, &attr, req.Force)
	return &pb.ResolveResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Access(ctx context.Context, req *pb.AccessRequest) (*pb.AccessResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.Access(mctx, Ino(req.Inode), uint8(req.Modemask), &attr)
	return &pb.AccessResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) GetAttr(ctx context.Context, req *pb.GetAttrRequest) (*pb.GetAttrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.GetAttr(mctx, Ino(req.Inode), &attr)
	return &pb.GetAttrResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) SetAttr(ctx context.Context, req *pb.SetAttrRequest) (*pb.SetAttrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	attr := ProtoToAttr(req.Attr)
	errno := s.meta.SetAttr(mctx, Ino(req.Inode), uint16(req.Set), uint8(req.Sggidclearmode), attr)
	return &pb.SetAttrResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(attr),
	}, nil
}

func (s *MetaProxyServer) CheckSetAttr(ctx context.Context, req *pb.CheckSetAttrRequest) (*pb.CheckSetAttrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	attr := *ProtoToAttr(req.Attr)
	errno := s.meta.CheckSetAttr(mctx, Ino(req.Inode), uint16(req.Set), attr)
	return &pb.CheckSetAttrResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Mknod(ctx context.Context, req *pb.MknodRequest) (*pb.MknodResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Mknod(mctx, Ino(req.Parent), req.Name, uint8(req.Type),
		uint16(req.Mode), uint16(req.Cumask), req.Rdev, req.Path, &inode, &attr)

	if errno == 0 {
		childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if childPath != "" {
			s.inodePathCache.Set(inode, childPath)
		}
	}

	return &pb.MknodResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Mkdir(ctx context.Context, req *pb.MkdirRequest) (*pb.MkdirResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Mkdir(mctx, Ino(req.Parent), req.Name, uint16(req.Mode),
		uint16(req.Cumask), uint8(req.Copysgid), &inode, &attr)

	if errno == 0 {
		childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if childPath != "" {
			s.inodePathCache.Set(inode, childPath)
		}
	}

	return &pb.MkdirResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Create(ctx context.Context, req *pb.CreateRequest) (*pb.CreateResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Create(mctx, Ino(req.Parent), req.Name, uint16(req.Mode),
		uint16(req.Cumask), uint32(req.Flags), &inode, &attr)

	if errno == 0 {
		childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if childPath != "" {
			s.inodePathCache.Set(inode, childPath)
		}
	}

	return &pb.CreateResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Open(ctx context.Context, req *pb.OpenRequest) (*pb.OpenResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.Open(mctx, Ino(req.Inode), uint32(req.Flags), &attr)
	return &pb.OpenResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Close(ctx context.Context, req *pb.CloseRequest) (*pb.CloseResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Close(mctx, Ino(req.Inode))
	return &pb.CloseResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Unlink(ctx context.Context, req *pb.UnlinkRequest) (*pb.UnlinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Unlink(mctx, Ino(req.Parent), req.Name, req.SkipCheckTrash)

	if errno == 0 {
		childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if childPath != "" {
			s.inodePathCache.UnsetByPath(childPath)
		}
	}

	return &pb.UnlinkResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Rmdir(ctx context.Context, req *pb.RmdirRequest) (*pb.RmdirResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Rmdir(mctx, Ino(req.Parent), req.Name, req.SkipCheckTrash)

	if errno == 0 {
		dirPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if dirPath != "" {
			s.inodePathCache.RemoveSubtree(dirPath)
		}
	}

	return &pb.RmdirResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Rename(ctx context.Context, req *pb.RenameRequest) (*pb.RenameResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Rename(mctx, Ino(req.ParentSrc), req.NameSrc,
		Ino(req.ParentDst), req.NameDst, uint32(req.Flags), &inode, &attr)

	if errno == 0 && inode > 0 {
		oldPath := s.inodePathCache.BuildChildPath(Ino(req.ParentSrc), string(req.NameSrc))
		newPath := s.inodePathCache.BuildChildPath(Ino(req.ParentDst), string(req.NameDst))

		if newPath != "" {
			// If destination already exists in cache and is a different inode,
			// remove its mapping first (rename overwrites the destination).
			if dstInode := s.inodePathCache.GetInodeByPath(newPath); dstInode != 0 && dstInode != inode {
				s.inodePathCache.RemoveSubtree(newPath)
			}

			// If old path is known, update subtree (covers both the entry and children).
			if oldPath != "" {
				s.inodePathCache.RenameSubtree(oldPath, newPath)
			}

			// Guarantee that the moved inode has a mapping to the new path.
			if !s.inodePathCache.Move(inode, newPath) {
				s.inodePathCache.Set(inode, newPath)
			}
		}
	}

	return &pb.RenameResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Link(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.Link(mctx, Ino(req.InodeSrc), Ino(req.Parent), req.Name, &attr)

	// Link creates a new directory entry for an existing inode.
	// The inode already has a path; the new name is an additional reference.
	// We don't update the cache because an inode can have multiple paths (hardlinks).
	// Authz checks use the first-cached path, which is sufficient for company-scoped permissions.
	return &pb.LinkResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Symlink(ctx context.Context, req *pb.SymlinkRequest) (*pb.SymlinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Symlink(mctx, Ino(req.Parent), req.Name, req.Path, &inode, &attr)

	if errno == 0 {
		childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if childPath != "" {
			s.inodePathCache.Set(inode, childPath)
		}
	}

	return &pb.SymlinkResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) ReadLink(ctx context.Context, req *pb.ReadLinkRequest) (*pb.ReadLinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var path []byte
	errno := s.meta.ReadLink(mctx, Ino(req.Inode), &path)
	return &pb.ReadLinkResponse{
		Errno: uint32(errno),
		Path:  path,
	}, nil
}

func (s *MetaProxyServer) Truncate(ctx context.Context, req *pb.TruncateRequest) (*pb.TruncateResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.Truncate(mctx, Ino(req.Inode), uint8(req.Flags), req.Length, &attr, req.SkipPermCheck)
	return &pb.TruncateResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Fallocate(ctx context.Context, req *pb.FallocateRequest) (*pb.FallocateResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var length uint64
	errno := s.meta.Fallocate(mctx, Ino(req.Inode), uint8(req.Mode), req.Off, req.Size, &length)
	return &pb.FallocateResponse{
		Errno:  uint32(errno),
		Length: length,
	}, nil
}

func (s *MetaProxyServer) Readdir(ctx context.Context, req *pb.ReaddirRequest) (*pb.ReaddirResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var entries []*Entry
	errno := s.meta.Readdir(mctx, Ino(req.Inode), uint8(req.Wantattr), &entries)

	if errno != 0 {
		return &pb.ReaddirResponse{Errno: uint32(errno)}, nil
	}

	// Post-filter first: filter entries by authorization (like MinIO's filterWalkResultCh)
	filtered := s.filterEntriesByAuthz(ctx, Ino(req.Inode), entries)

	// Cache only the filtered (allowed) entries — prevents cache pollution from
	// unauthorized paths and avoids hardlink first-seen being set from forbidden listings.
	if len(filtered) > 0 {
		mappings := make(map[Ino]string)
		for _, e := range filtered {
			childPath := s.inodePathCache.BuildChildPath(Ino(req.Inode), string(e.Name))
			if childPath != "" {
				mappings[e.Inode] = childPath
			}
		}
		if len(mappings) > 0 {
			s.inodePathCache.SetMany(mappings)
		}
	}

	protoEntries := make([]*pb.ProtoEntry, len(filtered))
	for i, e := range filtered {
		protoEntries[i] = EntryToProto(e)
	}
	return &pb.ReaddirResponse{Errno: 0, Entries: protoEntries}, nil
}

// filterEntriesByAuthz filters directory entries by user permissions.
// Follows MinIO's pattern: get all results from backend, then filter by batch authz check.
// Entries the user cannot see are simply not returned to the client.
func (s *MetaProxyServer) filterEntriesByAuthz(ctx context.Context, parentInode Ino, entries []*Entry) []*Entry {
	if s.authzInterceptor == nil || !s.authzInterceptor.enabled {
		return entries // no authz configured — return all
	}

	userID := s.authzInterceptor.extractUserID(ctx)
	if userID == "" {
		authzLogger.Warnf("Readdir: empty userID — returning empty list (deny)")
		return nil // fail-closed: no user = no access
	}

	// Pre-check: user must have View on the parent directory to list it at all.
	parentPath := s.inodePathCache.Get(parentInode)
	if parentPath == "" {
		authzLogger.Debugf("Readdir: parent inode %d not in cache — returning empty list", parentInode)
		return nil // can't resolve parent path — deny
	}

	allowedParent, err := s.authzInterceptor.client.CheckPermission(ctx, userID, parentPath, AuthzPermissionView)
	if err != nil || !allowedParent {
		authzLogger.Debugf("Readdir: parent check denied for user=%s path=%s err=%v", userID, parentPath, err)
		return nil // fail-closed: can't list this directory
	}

	// Build paths for all entries (already cached by SetMany above)
	paths := make([]string, 0, len(entries))
	validIndices := make([]int, 0, len(entries))
	for i, e := range entries {
		p := s.inodePathCache.Get(e.Inode)
		if p == "" {
			// Fallback: build from the actual parent inode (not RootInode).
			p = s.inodePathCache.BuildChildPath(parentInode, string(e.Name))
		}
		if p == "" {
			continue // empty path — skip this entry
		}
		paths = append(paths, p)
		validIndices = append(validIndices, i)
	}

	if len(paths) == 0 {
		return nil
	}

	// Batch check permissions (View — can see the entry in directory listing)
	allowed, err := s.authzInterceptor.client.CheckBulkPermissions(ctx, userID, paths, AuthzPermissionView)
	if err != nil {
		authzLogger.Warnf("Readdir batch authz error: %v — returning empty list (fail-closed)", err)
		return nil // fail-closed on authz error
	}

	if len(allowed) != len(paths) {
		authzLogger.Warnf("Readdir authz bulk result length mismatch: %d != %d — returning empty list",
			len(allowed), len(paths))
		return nil
	}

	// Filter: keep only allowed entries
	filtered := make([]*Entry, 0, len(entries))
	for i, idx := range validIndices {
		if allowed[i] {
			filtered = append(filtered, entries[idx])
		}
	}

	if len(filtered) != len(entries) {
		authzLogger.Debugf("Readdir filtered %d/%d entries for user %s", len(filtered), len(entries), userID)
	}

	return filtered
}

func (s *MetaProxyServer) Read(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var slices []Slice
	errno := s.meta.Read(mctx, Ino(req.Inode), req.Indx, &slices)
	return &pb.ReadResponse{
		Errno:  uint32(errno),
		Slices: SlicesToProto(slices),
	}, nil
}

func (s *MetaProxyServer) Write(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	slice := ProtoToSlice(req.Slice)
	errno := s.meta.Write(mctx, Ino(req.Inode), req.Indx, req.Off, slice, time.Unix(req.Mtime, 0))
	return &pb.WriteResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) NewSlice(ctx context.Context, req *pb.NewSliceRequest) (*pb.NewSliceResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var id uint64
	errno := s.meta.NewSlice(mctx, &id)
	return &pb.NewSliceResponse{
		Errno: uint32(errno),
		Id:    id,
	}, nil
}

func (s *MetaProxyServer) InvalidateChunkCache(ctx context.Context, req *pb.InvalidateChunkCacheRequest) (*pb.InvalidateChunkCacheResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	s.meta.InvalidateChunkCache(mctx, Ino(req.Inode), req.Indx)
	return &pb.InvalidateChunkCacheResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) CopyFileRange(ctx context.Context, req *pb.CopyFileRangeRequest) (*pb.CopyFileRangeResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var copied, outLength uint64
	errno := s.meta.CopyFileRange(mctx, Ino(req.Fin), req.OffIn, Ino(req.Fout),
		req.OffOut, req.Size, uint32(req.Flags), &copied, &outLength)
	return &pb.CopyFileRangeResponse{
		Errno:     uint32(errno),
		Copied:    copied,
		OutLength: outLength,
	}, nil
}
