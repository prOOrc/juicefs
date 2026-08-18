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
	"sync"

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// dirHandlerEntry pairs a DirHandler with its inode for authz resolution.
type dirHandlerEntry struct {
	handler DirHandler
	inode   Ino

	// Authz mode serves listings from a stable filtered snapshot instead of
	// the DirHandler's offset machinery: filtering after handler.List()
	// desyncs the FUSE offset protocol (the handler cursor advances by
	// unfiltered counts while the kernel advances by filtered counts), which
	// re-serves earlier entries as duplicates.
	mu         sync.Mutex
	authzList  []*Entry
	authzReady bool
}

// MetaProxyServer implements the MetaService gRPC server
type MetaProxyServer struct {
	pb.UnimplementedMetaServiceServer

	meta Meta

	// DirHandler state management
	mu         sync.Mutex
	nextHandle uint64
	handlers   map[uint64]*dirHandlerEntry

	// Authorization (optional)
	authzInterceptor *AuthzInterceptor
	inodePathCache   *InodePathCache
}

// NewMetaProxyServer creates a new MetaProxyServer.
// cacheMaxSize sets the maximum inode→path mappings (0 = unlimited).
func NewMetaProxyServer(m Meta, cacheMaxSize int) *MetaProxyServer {
	return &MetaProxyServer{
		meta:           m,
		handlers:       make(map[uint64]*dirHandlerEntry),
		inodePathCache: NewInodePathCache(cacheMaxSize),
	}
}

// SetAuthzInterceptor configures the authorization interceptor.
// Called during server setup in cmd/meta_proxy.go (before gRPC Serve).
func (s *MetaProxyServer) SetAuthzInterceptor(ai *AuthzInterceptor) {
	s.authzInterceptor = ai
}

// InodePathCache returns the inode→path cache (for cmd/meta_proxy.go).
func (s *MetaProxyServer) InodePathCache() *InodePathCache {
	return s.inodePathCache
}

// ResolveHandle returns the inode for a DirHandler handle (for authz interceptor).
func (s *MetaProxyServer) ResolveHandle(handle uint64) (Ino, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.handlers[handle]
	if !ok {
		return 0, false
	}
	return entry.inode, true
}

// helper to convert Context from proto
func (s *MetaProxyServer) metaCtx(ctx context.Context, ctx2 *pb.MetaContext) Context {
	if ctx2 == nil || (ctx2.Uid == 0 && ctx2.Gid == 0 && len(ctx2.Gids) == 0 && ctx2.Pid == 0) {
		return Background()
	}
	gids := ctx2.Gids
	if len(gids) == 0 {
		gids = []uint32{ctx2.Gid}
	}
	return WrapWithCancel(ctx, ctx2.Pid, ctx2.Uid, gids)
}
