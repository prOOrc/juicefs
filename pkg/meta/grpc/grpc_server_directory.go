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
	"syscall"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/meta/pb"
)

func (s *MetaProxyServer) GetParents(ctx context.Context, req *pb.GetParentsRequest) (*pb.GetParentsResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	parents := s.meta.GetParents(mctx, meta.Ino(req.Inode))
	protoParents := make(map[uint64]int32, len(parents))
	for ino, depth := range parents {
		protoParents[uint64(ino)] = int32(depth)
	}
	return &pb.GetParentsResponse{
		Errno:   0,
		Parents: protoParents,
	}, nil
}

func (s *MetaProxyServer) GetDirStat(ctx context.Context, req *pb.GetDirStatRequest) (*pb.GetDirStatResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	stat, errno := s.meta.GetDirStat(mctx, meta.Ino(req.Inode))
	if errno != 0 || stat == nil {
		return &pb.GetDirStatResponse{Errno: uint32(errno)}, nil
	}
	return &pb.GetDirStatResponse{Errno: uint32(syscall.ENOSYS)}, nil
}
