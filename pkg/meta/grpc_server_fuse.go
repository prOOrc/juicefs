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
	return &pb.SetAttrResponse{Errno: uint32(errno)}, nil
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
	return &pb.UnlinkResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Rmdir(ctx context.Context, req *pb.RmdirRequest) (*pb.RmdirResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Rmdir(mctx, Ino(req.Parent), req.Name, req.SkipCheckTrash)
	return &pb.RmdirResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Rename(ctx context.Context, req *pb.RenameRequest) (*pb.RenameResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Rename(mctx, Ino(req.ParentSrc), req.NameSrc,
		Ino(req.ParentDst), req.NameDst, uint32(req.Flags), &inode, &attr)
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
	return &pb.ReaddirResponse{
		Errno:   uint32(errno),
		Entries: EntriesToProto(entries),
	}, nil
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
