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

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

func (s *MetaProxyServer) Flock(ctx context.Context, req *pb.FlockRequest) (*pb.FlockResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Flock(mctx, Ino(req.Inode), req.Owner, uint32(req.Ltype), req.Block)
	return &pb.FlockResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Getlk(ctx context.Context, req *pb.GetlkRequest) (*pb.GetlkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var ltype uint32
	var start, end uint64
	var pid uint32
	errno := s.meta.Getlk(mctx, Ino(req.Inode), req.Owner, &ltype, &start, &end, &pid)
	return &pb.GetlkResponse{
		Errno: uint32(errno),
		Ltype: ltype,
		Start: start,
		End:   end,
		Pid:   pid,
	}, nil
}

func (s *MetaProxyServer) Setlk(ctx context.Context, req *pb.SetlkRequest) (*pb.SetlkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Setlk(mctx, Ino(req.Inode), req.Owner, req.Block,
		uint32(req.Ltype), req.Start, req.End, req.Pid)
	return &pb.SetlkResponse{Errno: uint32(errno)}, nil
}
