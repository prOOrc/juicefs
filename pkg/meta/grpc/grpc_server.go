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
	"sync"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// MetaProxyServer implements the MetaService gRPC server
type MetaProxyServer struct {
	pb.UnimplementedMetaServiceServer

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
func (s *MetaProxyServer) metaCtx(ctx context.Context, ctx2 *pb.MetaContext) meta.Context {
	if ctx2 == nil {
		return meta.Background()
	}
	gids := ctx2.Gids
	if len(gids) == 0 {
		gids = []uint32{ctx2.Gid}
	}
	return meta.WrapWithCancel(ctx, ctx2.Pid, ctx2.Uid, gids)
}
