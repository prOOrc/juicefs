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
	s.handlers[handleID] = handler
	s.mu.Unlock()
	return &pb.NewDirHandlerResponse{
		Errno:  0,
		Handle: &pb.DirHandlerHandle{HandleId: handleID},
	}, nil
}

func (s *MetaProxyServer) DirHandlerList(ctx context.Context, req *pb.DirHandlerListRequest) (*pb.DirHandlerListResponse, error) {
	s.mu.Lock()
	handler, ok := s.handlers[req.Handle.HandleId]
	s.mu.Unlock()
	if !ok {
		return &pb.DirHandlerListResponse{Errno: uint32(syscall.EBADF)}, nil
	}
	entries, errno := handler.List(Background(), int(req.Offset))
	return &pb.DirHandlerListResponse{
		Errno:   uint32(errno),
		Entries: EntriesToProto(entries),
	}, nil
}

func (s *MetaProxyServer) DirHandlerInsert(ctx context.Context, req *pb.DirHandlerInsertRequest) (*pb.DirHandlerInsertResponse, error) {
	s.mu.Lock()
	handler, ok := s.handlers[req.Handle.HandleId]
	s.mu.Unlock()
	if !ok {
		return &pb.DirHandlerInsertResponse{Errno: uint32(syscall.EBADF)}, nil
	}
	handler.Insert(Ino(req.Inode), req.Name, ProtoToAttr(req.Attr))
	return &pb.DirHandlerInsertResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) DirHandlerDelete(ctx context.Context, req *pb.DirHandlerDeleteRequest) (*pb.DirHandlerDeleteResponse, error) {
	s.mu.Lock()
	handler, ok := s.handlers[req.Handle.HandleId]
	s.mu.Unlock()
	if !ok {
		return &pb.DirHandlerDeleteResponse{Errno: uint32(syscall.EBADF)}, nil
	}
	handler.Delete(req.Name)
	return &pb.DirHandlerDeleteResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) DirHandlerClose(ctx context.Context, req *pb.DirHandlerCloseRequest) (*pb.DirHandlerCloseResponse, error) {
	s.mu.Lock()
	handler, ok := s.handlers[req.Handle.HandleId]
	delete(s.handlers, req.Handle.HandleId)
	s.mu.Unlock()
	if ok {
		handler.Close()
	}
	return &pb.DirHandlerCloseResponse{Errno: 0}, nil
}
