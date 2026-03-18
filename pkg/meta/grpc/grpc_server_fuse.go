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

package grpc

import (
	"context"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
)

func (s *MetaProxyServer) StatFS(ctx context.Context, req *StatFSRequest) (*StatFSResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var total, avail, iused, iavail uint64
	errno := s.meta.StatFS(mctx, meta.Ino(req.Ino), &total, &avail, &iused, &iavail)
	return &StatFSResponse{
		Errno:      uint32(errno),
		TotalSpace: total,
		AvailSpace: avail,
		Iused:      iused,
		Iavail:     iavail,
	}, nil
}

func (s *MetaProxyServer) Lookup(ctx context.Context, req *LookupRequest) (*LookupResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode meta.Ino
	var attr meta.Attr
	errno := s.meta.Lookup(mctx, meta.Ino(req.Parent), req.Name, &inode, &attr, req.CheckPermission)
	return &LookupResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Resolve(ctx context.Context, req *ResolveRequest) (*ResolveResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode meta.Ino
	var attr meta.Attr
	errno := s.meta.Resolve(mctx, meta.Ino(req.Parent), req.Path, &inode, &attr)
	return &ResolveResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Access(ctx context.Context, req *AccessRequest) (*AccessResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr meta.Attr
	errno := s.meta.Access(mctx, meta.Ino(req.Inode), uint8(req.Modemask), &attr)
	return &AccessResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) GetAttr(ctx context.Context, req *GetAttrRequest) (*GetAttrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr meta.Attr
	errno := s.meta.GetAttr(mctx, meta.Ino(req.Inode), &attr)
	return &GetAttrResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) SetAttr(ctx context.Context, req *SetAttrRequest) (*SetAttrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	attr := ProtoToAttr(req.Attr)
	errno := s.meta.SetAttr(mctx, meta.Ino(req.Inode), uint16(req.Set), uint8(req.Sggidclearmode), attr)
	return &SetAttrResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) CheckSetAttr(ctx context.Context, req *CheckSetAttrRequest) (*CheckSetAttrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	attr := *ProtoToAttr(req.Attr)
	errno := s.meta.CheckSetAttr(mctx, meta.Ino(req.Inode), uint16(req.Set), attr)
	return &CheckSetAttrResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Mknod(ctx context.Context, req *MknodRequest) (*MknodResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode meta.Ino
	var attr meta.Attr
	errno := s.meta.Mknod(mctx, meta.Ino(req.Parent), req.Name, uint8(req.Type),
		uint16(req.Mode), uint16(req.Cumask), req.Rdev, req.Path, &inode, &attr)
	return &MknodResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Mkdir(ctx context.Context, req *MkdirRequest) (*MkdirResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode meta.Ino
	var attr meta.Attr
	errno := s.meta.Mkdir(mctx, meta.Ino(req.Parent), req.Name, uint16(req.Mode),
		uint16(req.Cumask), uint8(req.Copysgid), &inode, &attr)
	return &MkdirResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Create(ctx context.Context, req *CreateRequest) (*CreateResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode meta.Ino
	var attr meta.Attr
	errno := s.meta.Create(mctx, meta.Ino(req.Parent), req.Name, uint16(req.Mode),
		uint16(req.Cumask), uint32(req.Flags), &inode, &attr)
	return &CreateResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Open(ctx context.Context, req *OpenRequest) (*OpenResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr meta.Attr
	errno := s.meta.Open(mctx, meta.Ino(req.Inode), uint32(req.Flags), &attr)
	return &OpenResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Close(ctx context.Context, req *CloseRequest) (*CloseResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Close(mctx, meta.Ino(req.Inode))
	return &CloseResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Unlink(ctx context.Context, req *UnlinkRequest) (*UnlinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Unlink(mctx, meta.Ino(req.Parent), req.Name, req.SkipCheckTrash)
	return &UnlinkResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Rmdir(ctx context.Context, req *RmdirRequest) (*RmdirResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Rmdir(mctx, meta.Ino(req.Parent), req.Name, req.SkipCheckTrash)
	return &RmdirResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Rename(ctx context.Context, req *RenameRequest) (*RenameResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode meta.Ino
	var attr meta.Attr
	errno := s.meta.Rename(mctx, meta.Ino(req.ParentSrc), req.NameSrc,
		meta.Ino(req.ParentDst), req.NameDst, uint32(req.Flags), &inode, &attr)
	return &RenameResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Link(ctx context.Context, req *LinkRequest) (*LinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr meta.Attr
	errno := s.meta.Link(mctx, meta.Ino(req.InodeSrc), meta.Ino(req.Parent), req.Name, &attr)
	return &LinkResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Symlink(ctx context.Context, req *SymlinkRequest) (*SymlinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode meta.Ino
	var attr meta.Attr
	errno := s.meta.Symlink(mctx, meta.Ino(req.Parent), req.Name, req.Path, &inode, &attr)
	return &SymlinkResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) ReadLink(ctx context.Context, req *ReadLinkRequest) (*ReadLinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var path []byte
	errno := s.meta.ReadLink(mctx, meta.Ino(req.Inode), &path)
	return &ReadLinkResponse{
		Errno: uint32(errno),
		Path:  path,
	}, nil
}

func (s *MetaProxyServer) Truncate(ctx context.Context, req *TruncateRequest) (*TruncateResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr meta.Attr
	errno := s.meta.Truncate(mctx, meta.Ino(req.Inode), uint8(req.Flags), req.Length, &attr, req.SkipPermCheck)
	return &TruncateResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Fallocate(ctx context.Context, req *FallocateRequest) (*FallocateResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var length uint64
	errno := s.meta.Fallocate(mctx, meta.Ino(req.Inode), uint8(req.Mode), req.Off, req.Size, &length)
	return &FallocateResponse{
		Errno:  uint32(errno),
		Length: length,
	}, nil
}

func (s *MetaProxyServer) Readdir(ctx context.Context, req *ReaddirRequest) (*ReaddirResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var entries []*meta.Entry
	errno := s.meta.Readdir(mctx, meta.Ino(req.Inode), uint8(req.Wantattr), &entries)
	return &ReaddirResponse{
		Errno:   uint32(errno),
		Entries: EntriesToProto(entries),
	}, nil
}

func (s *MetaProxyServer) Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var slices []meta.Slice
	errno := s.meta.Read(mctx, meta.Ino(req.Inode), req.Indx, &slices)
	return &ReadResponse{
		Errno:  uint32(errno),
		Slices: SlicesToProto(slices),
	}, nil
}

func (s *MetaProxyServer) Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	slice := ProtoToSlice(req.Slice)
	errno := s.meta.Write(mctx, meta.Ino(req.Inode), req.Indx, req.Off, slice, time.Unix(req.Mtime, 0))
	return &WriteResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) NewSlice(ctx context.Context, req *NewSliceRequest) (*NewSliceResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var id uint64
	errno := s.meta.NewSlice(mctx, &id)
	return &NewSliceResponse{
		Errno: uint32(errno),
		Id:    id,
	}, nil
}

func (s *MetaProxyServer) InvalidateChunkCache(ctx context.Context, req *InvalidateChunkCacheRequest) (*InvalidateChunkCacheResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	s.meta.InvalidateChunkCache(mctx, meta.Ino(req.Inode), req.Indx)
	return &InvalidateChunkCacheResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) CopyFileRange(ctx context.Context, req *CopyFileRangeRequest) (*CopyFileRangeResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var copied, outLength uint64
	errno := s.meta.CopyFileRange(mctx, meta.Ino(req.Fin), req.OffIn, meta.Ino(req.Fout),
		req.OffOut, req.Size, uint32(req.Flags), &copied, &outLength)
	return &CopyFileRangeResponse{
		Errno:     uint32(errno),
		Copied:    copied,
		OutLength: outLength,
	}, nil
}
