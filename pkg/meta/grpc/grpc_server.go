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
	"io"
	"sync"
	"syscall"
	"time"

	aclAPI "github.com/juicedata/juicefs/pkg/acl"
	"github.com/juicedata/juicefs/pkg/meta"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MetaProxyServer implements the MetaService gRPC server
type MetaProxyServer struct {
	UnimplementedMetaServiceServer

	meta meta.Meta

	// DirHandler state management
	mu         sync.Mutex
	nextHandle uint64
	handlers   map[uint64]meta.DirHandler
}

// NewMetaProxyServer creates a new MetaProxyServer
func NewMetaProxyServer(m meta.Meta) *MetaProxyServer {
	return &MetaProxyServer{
		meta:     m,
		handlers: make(map[uint64]meta.DirHandler),
	}
}

// helper to convert meta.Context from proto
func (s *MetaProxyServer) metaCtx(ctx context.Context, pb *MetaContext) meta.Context {
	if pb == nil {
		return meta.Background()
	}
	gids := pb.Gids
	if len(gids) == 0 {
		gids = []uint32{pb.Gid}
	}
	return meta.WrapWithCancel(ctx, pb.Pid, pb.Uid, gids)
}

// --- Lifecycle methods ---

func (s *MetaProxyServer) Init(ctx context.Context, req *InitRequest) (*InitResponse, error) {
	format := ProtoToFormat(req.Format)
	err := s.meta.Init(format, req.Force)
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &InitResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) Load(ctx context.Context, req *LoadRequest) (*LoadResponse, error) {
	format, err := s.meta.Load(req.CheckVersion)
	if err != nil {
		return &LoadResponse{Errno: uint32(syscall.EIO)}, nil
	}
	return &LoadResponse{
		Errno:  0,
		Format: FormatToProto(*format),
	}, nil
}

func (s *MetaProxyServer) NewSession(ctx context.Context, req *NewSessionRequest) (*NewSessionResponse, error) {
	err := s.meta.NewSession(req.Record)
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &NewSessionResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) CloseSession(ctx context.Context, req *CloseSessionRequest) (*CloseSessionResponse, error) {
	err := s.meta.CloseSession()
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &CloseSessionResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) FlushSession(ctx context.Context, req *FlushSessionRequest) (*FlushSessionResponse, error) {
	s.meta.FlushSession()
	return &FlushSessionResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) Shutdown(ctx context.Context, req *ShutdownRequest) (*ShutdownResponse, error) {
	err := s.meta.Shutdown()
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &ShutdownResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) Reset(ctx context.Context, req *ResetRequest) (*ResetResponse, error) {
	err := s.meta.Reset()
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &ResetResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) GetSession(ctx context.Context, req *GetSessionRequest) (*GetSessionResponse, error) {
	session, err := s.meta.GetSession(req.Sid, req.Detail)
	if err != nil {
		return &GetSessionResponse{Errno: uint32(syscall.EIO)}, nil
	}
	return &GetSessionResponse{
		Errno:   0,
		Session: SessionToProto(session),
	}, nil
}

func (s *MetaProxyServer) ListSessions(ctx context.Context, req *ListSessionsRequest) (*ListSessionsResponse, error) {
	sessions, err := s.meta.ListSessions()
	if err != nil {
		return &ListSessionsResponse{Errno: uint32(syscall.EIO)}, nil
	}
	protoSessions := make([]*ProtoSession, len(sessions))
	for i, s := range sessions {
		protoSessions[i] = SessionToProto(s)
	}
	return &ListSessionsResponse{
		Errno:    0,
		Sessions: protoSessions,
	}, nil
}

func (s *MetaProxyServer) CleanStaleSessions(ctx context.Context, req *CleanStaleSessionsRequest) (*CleanStaleSessionsResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	s.meta.CleanStaleSessions(mctx)
	return &CleanStaleSessionsResponse{Errno: 0}, nil
}

// --- Core FUSE operations ---

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

// --- Data path ---

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

// --- Locks ---

func (s *MetaProxyServer) Flock(ctx context.Context, req *FlockRequest) (*FlockResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Flock(mctx, meta.Ino(req.Inode), req.Owner, uint32(req.Ltype), req.Block)
	return &FlockResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Getlk(ctx context.Context, req *GetlkRequest) (*GetlkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var ltype uint32
	var start, end uint64
	var pid uint32
	errno := s.meta.Getlk(mctx, meta.Ino(req.Inode), req.Owner, &ltype, &start, &end, &pid)
	return &GetlkResponse{
		Errno: uint32(errno),
		Ltype: ltype,
		Start: start,
		End:   end,
		Pid:   pid,
	}, nil
}

func (s *MetaProxyServer) Setlk(ctx context.Context, req *SetlkRequest) (*SetlkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Setlk(mctx, meta.Ino(req.Inode), req.Owner, req.Block,
		uint32(req.Ltype), req.Start, req.End, req.Pid)
	return &SetlkResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ListLocks(ctx context.Context, req *ListLocksRequest) (*ListLocksResponse, error) {
	plocks, flocks, err := s.meta.ListLocks(ctx, meta.Ino(req.Inode))
	if err != nil {
		return &ListLocksResponse{Errno: uint32(syscall.EIO)}, nil
	}
	// Convert plocks and flocks to proto (these are PLockItem and FLockItem, not Plock/Flock)
	protoPlocks := make([]*ProtoPlock, len(plocks))
	for i, pl := range plocks {
		protoPlocks[i] = &ProtoPlock{
			Inode: uint64(pl.Sid),
			Owner: pl.Owner,
			Records: []*ProtoPlockRecord{{
				Type:  pl.Type,
				Pid:   pl.Pid,
				Start: pl.Start,
				End:   pl.End,
			}},
		}
	}
	protoFlocks := make([]*ProtoFlock, len(flocks))
	for i, fl := range flocks {
		protoFlocks[i] = &ProtoFlock{
			Inode: uint64(fl.Sid),
			Owner: fl.Owner,
			Ltype: fl.Type,
		}
	}
	return &ListLocksResponse{
		Errno:  0,
		Plocks: protoPlocks,
		Flocks: protoFlocks,
	}, nil
}

// --- Xattrs ---

func (s *MetaProxyServer) GetXattr(ctx context.Context, req *GetXattrRequest) (*GetXattrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var value []byte
	errno := s.meta.GetXattr(mctx, meta.Ino(req.Inode), req.Name, &value)
	return &GetXattrResponse{
		Errno: uint32(errno),
		Value: value,
	}, nil
}

func (s *MetaProxyServer) SetXattr(ctx context.Context, req *SetXattrRequest) (*SetXattrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.SetXattr(mctx, meta.Ino(req.Inode), req.Name, req.Value, uint32(req.Flags))
	return &SetXattrResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) RemoveXattr(ctx context.Context, req *RemoveXattrRequest) (*RemoveXattrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.RemoveXattr(mctx, meta.Ino(req.Inode), req.Name)
	return &RemoveXattrResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ListXattr(ctx context.Context, req *ListXattrRequest) (*ListXattrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var names []byte
	errno := s.meta.ListXattr(mctx, meta.Ino(req.Inode), &names)
	return &ListXattrResponse{
		Errno: uint32(errno),
		Names: names,
	}, nil
}

// --- Directory ---

func (s *MetaProxyServer) GetParents(ctx context.Context, req *GetParentsRequest) (*GetParentsResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	parents := s.meta.GetParents(mctx, meta.Ino(req.Inode))
	protoParents := make(map[uint64]int32, len(parents))
	for ino, depth := range parents {
		protoParents[uint64(ino)] = int32(depth)
	}
	return &GetParentsResponse{
		Errno:   0,
		Parents: protoParents,
	}, nil
}

func (s *MetaProxyServer) GetDirStat(ctx context.Context, req *GetDirStatRequest) (*GetDirStatResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	stat, errno := s.meta.GetDirStat(mctx, meta.Ino(req.Inode))
	if errno != 0 || stat == nil {
		return &GetDirStatResponse{Errno: uint32(errno)}, nil
	}
	// Access unexported fields via type assertion to internal representation
	// dirStat has unexported fields: length, space, inodes
	// We need to use reflection or a helper method - for now return error
	return &GetDirStatResponse{Errno: uint32(syscall.ENOSYS)}, nil
}

// --- ACL ---

func (s *MetaProxyServer) SetFacl(ctx context.Context, req *SetFaclRequest) (*SetFaclResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	// ACL conversion is complex, skip for now
	errno := s.meta.SetFacl(mctx, meta.Ino(req.Ino), uint8(req.AclType), nil)
	return &SetFaclResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) GetFacl(ctx context.Context, req *GetFaclRequest) (*GetFaclResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var rule aclAPI.Rule
	errno := s.meta.GetFacl(mctx, meta.Ino(req.Ino), uint8(req.AclType), &rule)
	return &GetFaclResponse{Errno: uint32(errno)}, nil
}

// --- Tokens ---

func (s *MetaProxyServer) StoreToken(ctx context.Context, req *StoreTokenRequest) (*StoreTokenResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	id, errno := s.meta.StoreToken(mctx, req.Token)
	return &StoreTokenResponse{
		Errno: uint32(errno),
		Id:    id,
	}, nil
}

func (s *MetaProxyServer) UpdateToken(ctx context.Context, req *UpdateTokenRequest) (*UpdateTokenResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.UpdateToken(mctx, req.Id, req.Token)
	return &UpdateTokenResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) LoadToken(ctx context.Context, req *LoadTokenRequest) (*LoadTokenResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	token, errno := s.meta.LoadToken(mctx, req.Id)
	return &LoadTokenResponse{
		Errno: uint32(errno),
		Token: token,
	}, nil
}

func (s *MetaProxyServer) DeleteTokens(ctx context.Context, req *DeleteTokensRequest) (*DeleteTokensResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	ids := make([]uint32, len(req.Ids))
	copy(ids, req.Ids)
	errno := s.meta.DeleteTokens(mctx, ids)
	return &DeleteTokensResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ListTokens(ctx context.Context, req *ListTokensRequest) (*ListTokensResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	tokens, errno := s.meta.ListTokens(mctx)
	protoTokens := make(map[uint32][]byte, len(tokens))
	for id, token := range tokens {
		protoTokens[id] = token
	}
	return &ListTokensResponse{
		Errno:  uint32(errno),
		Tokens: protoTokens,
	}, nil
}

// --- Admin operations ---

func (s *MetaProxyServer) GetFormat(ctx context.Context, req *GetFormatRequest) (*GetFormatResponse, error) {
	format := s.meta.GetFormat()
	return &GetFormatResponse{
		Errno:  0,
		Format: FormatToProto(format),
	}, nil
}

func (s *MetaProxyServer) Remove(ctx context.Context, req *RemoveRequest) (*RemoveResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var count uint64
	errno := s.meta.Remove(mctx, meta.Ino(req.Parent), req.Name, req.SkipTrash, int(req.NumThreads), &count)
	return &RemoveResponse{
		Errno: uint32(errno),
		Count: count,
	}, nil
}

func (s *MetaProxyServer) BatchUnlink(ctx context.Context, req *BatchUnlinkRequest) (*BatchUnlinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	entries := ProtoToEntries(req.Entries)
	var count uint64
	errno := s.meta.BatchUnlink(mctx, meta.Ino(req.Parent), entries, &count, req.SkipCheckTrash)
	return &BatchUnlinkResponse{
		Errno: uint32(errno),
		Count: count,
	}, nil
}

func (s *MetaProxyServer) GetSummary(ctx context.Context, req *GetSummaryRequest) (*GetSummaryResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var summary meta.Summary
	errno := s.meta.GetSummary(mctx, meta.Ino(req.Inode), &summary, req.Recursive, req.Strict)
	return &GetSummaryResponse{
		Errno:   uint32(errno),
		Summary: SummaryToProto(&summary),
	}, nil
}

func (s *MetaProxyServer) GetTreeSummary(ctx context.Context, req *GetTreeSummaryRequest) (*GetTreeSummaryResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var tree meta.TreeSummary
	errno := s.meta.GetTreeSummary(mctx, &tree, uint8(req.Depth), uint8(req.TopN), req.Strict, func(uint64, uint64) {})
	return &GetTreeSummaryResponse{
		Errno: uint32(errno),
		Tree:  TreeSummaryToProto(&tree),
	}, nil
}

func (s *MetaProxyServer) Clone(ctx context.Context, req *CloneRequest) (*CloneResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var count, total uint64
	errno := s.meta.Clone(mctx, meta.Ino(req.SrcParentIno), meta.Ino(req.SrcIno),
		meta.Ino(req.DstParentIno), req.DstName, uint8(req.Cmode), uint16(req.Cumask),
		uint8(req.Concurrency), &count, &total)
	return &CloneResponse{
		Errno: uint32(errno),
		Count: count,
		Total: total,
	}, nil
}

func (s *MetaProxyServer) GetPaths(ctx context.Context, req *GetPathsRequest) (*GetPathsResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	paths := s.meta.GetPaths(mctx, meta.Ino(req.Inode))
	return &GetPathsResponse{
		Errno: 0,
		Paths: paths,
	}, nil
}

func (s *MetaProxyServer) Check(ctx context.Context, req *CheckRequest) (*CheckResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	opt := &meta.CheckOpt{
		Repair:        req.Repair,
		Recursive:     req.Recursive,
		SyncDirStat:   req.SyncDirStat,
		RepairDirMode: uint16(req.RepairDirMode),
	}
	err := s.meta.Check(mctx, req.Fpath, opt)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &CheckResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) CompactAll(ctx context.Context, req *CompactAllRequest) (*CompactAllResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.CompactAll(mctx, int(req.Threads), nil)
	return &CompactAllResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Compact(ctx context.Context, req *CompactRequest) (*CompactResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Compact(mctx, meta.Ino(req.Inode), int(req.Concurrency), nil, nil)
	return &CompactResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ListSlices(ctx context.Context, req *ListSlicesRequest) (*ListSlicesResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	slices := ProtoToSliceMap(req.Slices)
	errno := s.meta.ListSlices(mctx, slices, req.ScanPending, req.Delete, func() {})
	return &ListSlicesResponse{
		Errno:  uint32(errno),
		Slices: SliceMapToProto(slices),
	}, nil
}

func (s *MetaProxyServer) HandleQuota(ctx context.Context, req *HandleQuotaRequest) (*HandleQuotaResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	quotas := make(map[string]*meta.Quota)
	for k, v := range req.Quotas {
		quotas[k] = ProtoToQuota(v)
	}
	err := s.meta.HandleQuota(mctx, uint8(req.Cmd), req.Dpath, req.Uid, req.Gid, quotas,
		req.Strict, req.Repair, req.Create)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &HandleQuotaResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ScanUserGroupUsage(ctx context.Context, req *ScanUserGroupUsageRequest) (*ScanUserGroupUsageResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	err := s.meta.ScanUserGroupUsage(mctx)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &ScanUserGroupUsageResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Chroot(ctx context.Context, req *ChrootRequest) (*ChrootResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Chroot(mctx, req.Subdir)
	return &ChrootResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) CleanupTrashBefore(ctx context.Context, req *CleanupTrashBeforeRequest) (*CleanupTrashBeforeResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var stats meta.CleanupTrashStats
	errno := s.meta.CleanupTrashBefore(mctx, time.Unix(req.Edge, 0), func(int) {}, &stats)
	return &CleanupTrashBeforeResponse{
		Errno:        uint32(errno),
		DeletedFiles: stats.DeletedFiles,
	}, nil
}

func (s *MetaProxyServer) CleanupDetachedNodesBefore(ctx context.Context, req *CleanupDetachedNodesBeforeRequest) (*CleanupDetachedNodesBeforeResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	s.meta.CleanupDetachedNodesBefore(mctx, time.Unix(req.Edge, 0), func() {})
	return &CleanupDetachedNodesBeforeResponse{Errno: 0}, nil
}

// --- DirHandler ---

func (s *MetaProxyServer) NewDirHandler(ctx context.Context, req *NewDirHandlerRequest) (*NewDirHandlerResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	initEntries := ProtoToEntries(req.InitEntries)
	handler, errno := s.meta.NewDirHandler(mctx, meta.Ino(req.Inode), req.Plus, initEntries)
	if errno != 0 {
		return &NewDirHandlerResponse{Errno: uint32(errno)}, nil
	}
	s.mu.Lock()
	handleID := s.nextHandle
	s.nextHandle++
	s.handlers[handleID] = handler
	s.mu.Unlock()
	return &NewDirHandlerResponse{
		Errno:  0,
		Handle: &DirHandlerHandle{HandleId: handleID},
	}, nil
}

func (s *MetaProxyServer) DirHandlerList(ctx context.Context, req *DirHandlerListRequest) (*DirHandlerListResponse, error) {
	s.mu.Lock()
	handler, ok := s.handlers[req.Handle.HandleId]
	s.mu.Unlock()
	if !ok {
		return &DirHandlerListResponse{Errno: uint32(syscall.EBADF)}, nil
	}
	entries, errno := handler.List(meta.Background(), int(req.Offset))
	return &DirHandlerListResponse{
		Errno:   uint32(errno),
		Entries: EntriesToProto(entries),
	}, nil
}

func (s *MetaProxyServer) DirHandlerInsert(ctx context.Context, req *DirHandlerInsertRequest) (*DirHandlerInsertResponse, error) {
	s.mu.Lock()
	handler, ok := s.handlers[req.Handle.HandleId]
	s.mu.Unlock()
	if !ok {
		return &DirHandlerInsertResponse{Errno: uint32(syscall.EBADF)}, nil
	}
	handler.Insert(meta.Ino(req.Inode), req.Name, ProtoToAttr(req.Attr))
	return &DirHandlerInsertResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) DirHandlerDelete(ctx context.Context, req *DirHandlerDeleteRequest) (*DirHandlerDeleteResponse, error) {
	s.mu.Lock()
	handler, ok := s.handlers[req.Handle.HandleId]
	s.mu.Unlock()
	if !ok {
		return &DirHandlerDeleteResponse{Errno: uint32(syscall.EBADF)}, nil
	}
	handler.Delete(req.Name)
	return &DirHandlerDeleteResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) DirHandlerClose(ctx context.Context, req *DirHandlerCloseRequest) (*DirHandlerCloseResponse, error) {
	s.mu.Lock()
	handler, ok := s.handlers[req.Handle.HandleId]
	delete(s.handlers, req.Handle.HandleId)
	s.mu.Unlock()
	if ok {
		handler.Close()
	}
	return &DirHandlerCloseResponse{Errno: 0}, nil
}

// --- Streaming Dump/Load ---

func (s *MetaProxyServer) DumpMeta(req *DumpMetaRequest, stream MetaService_DumpMetaServer) error {
	pr, pw := io.Pipe()
	go func() {
		_ = s.meta.DumpMeta(pw, meta.Ino(req.Root), int(req.Threads), req.KeepSecret, req.Fast, req.SkipTrash)
		pw.Close()
	}()
	buf := make([]byte, 64*1024)
	for {
		n, readErr := pr.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&DumpMetaChunk{Data: buf[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func (s *MetaProxyServer) LoadMeta(stream MetaService_LoadMetaServer) error {
	pr, pw := io.Pipe()
	go func() {
		for {
			chunk, recvErr := stream.Recv()
			if recvErr == io.EOF {
				pw.Close()
				return
			}
			if recvErr != nil {
				pw.CloseWithError(recvErr)
				return
			}
			_, writeErr := pw.Write(chunk.Data)
			if writeErr != nil {
				pw.CloseWithError(writeErr)
				return
			}
		}
	}()
	err := s.meta.LoadMeta(pr)
	pw.Close()
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return stream.SendAndClose(&LoadMetaResponse{Errno: 0})
}

func (s *MetaProxyServer) DumpMetaV2(req *DumpMetaV2Request, stream MetaService_DumpMetaV2Server) error {
	mctx := s.metaCtx(stream.Context(), req.Ctx)
	opt := &meta.DumpOption{
		KeepSecret: req.KeepSecret,
		Threads:    int(req.Threads),
	}
	pr, pw := io.Pipe()
	go func() {
		_ = s.meta.DumpMetaV2(mctx, pw, opt)
		pw.Close()
	}()
	buf := make([]byte, 64*1024)
	for {
		n, readErr := pr.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&DumpMetaV2Chunk{Data: buf[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func (s *MetaProxyServer) LoadMetaV2(stream MetaService_LoadMetaV2Server) error {
	pr, pw := io.Pipe()
	go func() {
		for {
			chunk, recvErr := stream.Recv()
			if recvErr == io.EOF {
				pw.Close()
				return
			}
			if recvErr != nil {
				pw.CloseWithError(recvErr)
				return
			}
			_, writeErr := pw.Write(chunk.Data)
			if writeErr != nil {
				pw.CloseWithError(writeErr)
				return
			}
		}
	}()
	opt := &meta.LoadOption{Threads: 10}
	err := s.meta.LoadMetaV2(meta.Background(), pr, opt)
	pw.Close()
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return stream.SendAndClose(&LoadMetaV2Response{Errno: 0})
}
