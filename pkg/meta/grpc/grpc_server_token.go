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

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

func (s *MetaProxyServer) StoreToken(ctx context.Context, req *pb.StoreTokenRequest) (*pb.StoreTokenResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	id, errno := s.meta.StoreToken(mctx, req.Token)
	return &pb.StoreTokenResponse{
		Errno: uint32(errno),
		Id:    id,
	}, nil
}

func (s *MetaProxyServer) UpdateToken(ctx context.Context, req *pb.UpdateTokenRequest) (*pb.UpdateTokenResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.UpdateToken(mctx, req.Id, req.Token)
	return &pb.UpdateTokenResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) LoadToken(ctx context.Context, req *pb.LoadTokenRequest) (*pb.LoadTokenResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	token, errno := s.meta.LoadToken(mctx, req.Id)
	return &pb.LoadTokenResponse{
		Errno: uint32(errno),
		Token: token,
	}, nil
}

func (s *MetaProxyServer) DeleteTokens(ctx context.Context, req *pb.DeleteTokensRequest) (*pb.DeleteTokensResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	ids := make([]uint32, len(req.Ids))
	copy(ids, req.Ids)
	errno := s.meta.DeleteTokens(mctx, ids)
	return &pb.DeleteTokensResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ListTokens(ctx context.Context, req *pb.ListTokensRequest) (*pb.ListTokensResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	tokens, errno := s.meta.ListTokens(mctx)
	protoTokens := make(map[uint32][]byte, len(tokens))
	for id, token := range tokens {
		protoTokens[id] = token
	}
	return &pb.ListTokensResponse{
		Errno:  uint32(errno),
		Tokens: protoTokens,
	}, nil
}
