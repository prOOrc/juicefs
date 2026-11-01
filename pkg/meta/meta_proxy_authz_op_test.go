/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
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
	"syscall"
	"testing"
	"time"

	authzpb "github.com/juicedata/juicefs/pkg/meta/authz_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestAuthzOpContext_MkdirClassified (domain-meta-proxy: Authz operation
// context pass-through): a Mkdir through the proxy reaches the authz service
// flagged as a directory creation, with the checked path in the dir convention
// (trailing slash). The trailing slash is load-bearing: the authz service
// strips the volume's companies prefix on it ("/companies/" → ""), which is
// what the self-provision mkdir rule allows.
func TestAuthzOpContext_MkdirClassified(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	// "companies" dir cached the way handlers cache directory entries
	// (withDirSlash) — e.g. after the client looked it up in the root.
	cache.Set(RootInode, "/")
	cache.Set(100, "/companies/")

	// The check targets the CREATED DIRECTORY ("/companies/acme/", not the
	// parent): the platform allows MKDIR on the stripped namespace root
	// ("/companies/" → "") and on the caller's own company dir — anything else
	// (arbitrary root names) must be denied.
	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/companies/acme/", AuthzPermissionWrite, authzpb.Operation_OP_MKDIR).Return(true, nil)

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.MkdirResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.MkdirRequest{Parent: 100, Name: "acme"},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Mkdir"}, handler)

	assert.NoError(t, err)
	assert.True(t, called, "allowed mkdir must reach the handler")
	mockClient.AssertExpectations(t)
}

// TestAuthzOpContext_Mkdir_SlashlessParentStillNormalized: even when the
// parent directory lost its trailing slash in the path cache, the checked
// CHILD path is normalized back to the dir convention — a slashless
// "/companies" parent must still yield "/companies/acme/" (stripped to
// "acme" on the platform, resolved by the company-scoped rule).
func TestAuthzOpContext_Mkdir_SlashlessParentStillNormalized(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	cache.Set(100, "/companies")

	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/companies/acme/", AuthzPermissionWrite, authzpb.Operation_OP_MKDIR).Return(true, nil)

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return &pb.MkdirResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.MkdirRequest{Parent: 100, Name: "acme"},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Mkdir"}, handler)

	assert.NoError(t, err)
	mockClient.AssertExpectations(t)
}

// TestAuthzOpContext_OtherOpsDefault: all non-mkdir operations go out with the
// default OP_OTHER context and their paths untouched.
func TestAuthzOpContext_OtherOpsDefault(t *testing.T) {
	t.Run("SetAttr (inode-based)", func(t *testing.T) {
		mockClient := &mockAuthzClient{}
		cache := NewInodePathCache(0)
		cache.Set(100, "/companies/acme/render.exr")

		mockClient.On("CheckPermission", mock.Anything, "user-123",
			"/companies/acme/render.exr", AuthzPermissionWrite, authzpb.Operation_OP_OTHER).Return(true, nil)

		ai := newTestInterceptor(mockClient, cache, "user-123")
		interceptor := ai.UnaryInterceptor()

		handler := func(ctx context.Context, req interface{}) (interface{}, error) {
			return &pb.SetAttrResponse{}, nil
		}
		_, err := interceptor(context.Background(), &pb.SetAttrRequest{Inode: 100},
			&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/SetAttr"}, handler)

		assert.NoError(t, err)
		mockClient.AssertExpectations(t)
	})

	t.Run("Unlink (parent-based)", func(t *testing.T) {
		mockClient := &mockAuthzClient{}
		cache := NewInodePathCache(0)
		cache.Set(200, "/companies/acme/projects")

		mockClient.On("CheckPermission", mock.Anything, "user-123",
			"/companies/acme/projects", AuthzPermissionWrite, authzpb.Operation_OP_OTHER).Return(true, nil)

		ai := newTestInterceptor(mockClient, cache, "user-123")
		interceptor := ai.UnaryInterceptor()

		handler := func(ctx context.Context, req interface{}) (interface{}, error) {
			return &pb.UnlinkResponse{}, nil
		}
		_, err := interceptor(context.Background(), &pb.UnlinkRequest{Parent: 200, Name: "render.exr"},
			&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Unlink"}, handler)

		assert.NoError(t, err)
		mockClient.AssertExpectations(t)
	})
}

// TestAuthzOpContext_MkdirArbitraryRootNameDenied: a root-level mkdir of an
// arbitrary name checks the child path ("/xyz/" → stripped "xyz", no such
// company) — the platform denies it, and the proxy must not reach the handler.
func TestAuthzOpContext_MkdirArbitraryRootNameDenied(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	cache.Set(RootInode, "/")

	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/xyz-security-probe/", AuthzPermissionWrite, authzpb.Operation_OP_MKDIR).Return(false, nil)

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.MkdirResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.MkdirRequest{Parent: uint64(RootInode), Name: "xyz-security-probe"},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Mkdir"}, handler)

	assert.Error(t, err, "denied mkdir must not reach the handler")
	assert.False(t, called)
	mockClient.AssertExpectations(t)
}

// --- Restart-churn regression (fix-meta-proxy-session-churn, task 4.1) ---
//
// The stage incident: a proxy restart empties the inode→path cache (only the
// pinned root survives), a live client keeps issuing inode-only requests, and
// everything denied with EACCES until remount. These tests run a FRESH
// MetaProxyServer (cold cache = post-restart state) over a LIVE engine (the
// fake tree) with the authz interceptor enabled and verify that
// reconstruction through the engine restores access, keeps fail-closed
// semantics for unauthorized inodes, and answers ENOENT for vanished ones.

// churnDirHandler is a DirHandler stub: in authz mode the listing is served
// by authzListing from s.meta.Readdir, the handler cursor is never advanced.
type churnDirHandler struct{}

func (h *churnDirHandler) List(Context, int) ([]*Entry, syscall.Errno) { return nil, 0 }
func (h *churnDirHandler) Insert(Ino, string, *Attr)                   {}
func (h *churnDirHandler) Delete(string)                               {}
func (h *churnDirHandler) Read(int)                                    {}
func (h *churnDirHandler) Close()                                      {}

// churnMeta extends the fake tree engine with the ops the proxy handlers
// exercise in these tests (Lookup/Open/NewDirHandler).
type churnMeta struct {
	*fakeTreeMeta
}

func (m *churnMeta) Lookup(ctx Context, parent Ino, name string, inode *Ino, attr *Attr, checkPerm bool) syscall.Errno {
	m.mu.Lock()
	defer m.mu.Unlock()
	child, ok := m.names[parent][name]
	if !ok {
		return syscall.ENOENT
	}
	*inode = child
	a := *m.attrs[child]
	*attr = a
	return 0
}

func (m *churnMeta) Open(ctx Context, inode Ino, flags uint32, attr *Attr) syscall.Errno {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.attrs[inode]
	if !ok {
		return syscall.ENOENT
	}
	*attr = *a
	return 0
}

func (m *churnMeta) Rename(ctx Context, parentSrc Ino, nameSrc string, parentDst Ino, nameDst string, flags uint32, inode *Ino, attr *Attr) syscall.Errno {
	m.mu.Lock()
	defer m.mu.Unlock()
	child, ok := m.names[parentSrc][nameSrc]
	if !ok {
		return syscall.ENOENT
	}
	delete(m.names[parentSrc], nameSrc)
	if m.names[parentDst] == nil {
		m.names[parentDst] = map[string]Ino{}
	}
	m.names[parentDst][nameDst] = child
	m.attrs[child].Parent = parentDst
	*inode = child
	a := *m.attrs[child]
	*attr = a
	return 0
}

func (m *churnMeta) Unlink(ctx Context, parent Ino, name string, skipCheckTrash ...bool) syscall.Errno {
	m.mu.Lock()
	defer m.mu.Unlock()
	child, ok := m.names[parent][name]
	if !ok {
		return syscall.ENOENT
	}
	delete(m.names[parent], name)
	delete(m.attrs, child)
	return 0
}

func (m *churnMeta) NewDirHandler(ctx Context, inode Ino, plus bool, initEntries []*Entry) (DirHandler, syscall.Errno) {
	m.mu.Lock()
	_, ok := m.attrs[inode]
	m.mu.Unlock()
	if !ok {
		return nil, syscall.ENOENT
	}
	return &churnDirHandler{}, 0
}

// newChurnProxy builds the post-restart stack: a fresh MetaProxyServer (cold
// inode→path cache) over a live fake engine, authz enabled with the given
// mock client. The server auto-wires path recovery into the interceptor.
func newChurnProxy(engine *fakeTreeMeta, userID string) (*MetaProxyServer, *AuthzInterceptor, *mockAuthzClient) {
	server := NewMetaProxyServer(&churnMeta{engine}, 0)
	server.SetAuthzMode(true)
	mockClient := &mockAuthzClient{}
	ai := newTestInterceptor(mockClient, server.InodePathCache(), userID)
	server.SetAuthzInterceptor(ai)
	return server, ai, mockClient
}

// callThrough runs one RPC through the authz interceptor into the server
// handler, mirroring the real gRPC path.
func callThrough(t *testing.T, ai *AuthzInterceptor, ctx context.Context, method string, req interface{}, handler grpc.UnaryHandler) (interface{}, error) {
	t.Helper()
	return ai.UnaryInterceptor()(ctx, req, &grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/" + method}, handler)
}

// churnTree builds /a/b/c/ with two files plus an unauthorized sibling subtree.
func churnTree() *fakeTreeMeta {
	return newFakeTreeMeta().
		mkdir(RootInode, "a", 10).
		mkdir(10, "b", 11).
		mkdir(11, "c", 12).
		create(12, "f.exr", 13).
		create(12, "g.txt", 14).
		mkdir(11, "private", 20).
		create(20, "secret", 21)
}

func TestProxyRestartChurn_DeepPathsRecover(t *testing.T) {
	engine := churnTree()
	server, ai, client := newChurnProxy(engine, "user-1")
	ctx := context.Background()

	// GetAttr on a deep file: the path is reconstructed through the engine
	// and the authz decision is made on it ("/a/b/c/f.exr").
	client.On("CheckPermission", mock.Anything, "user-1", "/a/b/c/f.exr", AuthzPermissionView, authzpb.Operation_OP_OTHER).Return(true, nil).Once()
	resp, err := callThrough(t, ai, ctx, "GetAttr", &pb.GetAttrRequest{Inode: uint64(13)},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			return server.GetAttr(ctx, req.(*pb.GetAttrRequest))
		})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.(*pb.GetAttrResponse).Errno)
	client.AssertExpectations(t)
	// Recovery warmed the cache: the next request on the same inode is served
	// without another walk.
	require.Equal(t, "/a/b/c/f.exr", server.InodePathCache().Get(13))

	// Open (O_RDONLY) on the same file: View/Read on the recovered path.
	client.On("CheckPermission", mock.Anything, "user-1", "/a/b/c/f.exr", AuthzPermissionRead, authzpb.Operation_OP_OTHER).Return(true, nil).Once()
	resp, err = callThrough(t, ai, ctx, "Open", &pb.OpenRequest{Inode: uint64(13), Flags: 0},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			return server.Open(ctx, req.(*pb.OpenRequest))
		})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.(*pb.OpenResponse).Errno)

	// Lookup under a recovered parent: the kernel re-Lookup loop works after
	// the restart (parent path "/a/b/c/" reconstructed, then allowed).
	client.On("CheckPermission", mock.Anything, "user-1", "/a/b/c/", AuthzPermissionView, authzpb.Operation_OP_OTHER).Return(true, nil).Once()
	resp, err = callThrough(t, ai, ctx, "Lookup", &pb.LookupRequest{Parent: uint64(12), Name: "f.exr"},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			return server.Lookup(ctx, req.(*pb.LookupRequest))
		})
	require.NoError(t, err)
	lr := resp.(*pb.LookupResponse)
	require.EqualValues(t, 0, lr.Errno)
	require.EqualValues(t, 13, lr.Inode)
	client.AssertExpectations(t)
}

func TestProxyRestartChurn_ReaddirRecoversParent(t *testing.T) {
	engine := churnTree()
	server, _, client := newChurnProxy(engine, "user-1")
	ctx := context.Background()

	// Readdir is post-filtered in the handler: the parent path is recovered
	// ("/a/b/c/"), the parent View check passes, and the batch check runs on
	// the reconstructed child paths.
	client.On("CheckPermission", mock.Anything, "user-1", "/a/b/c/", AuthzPermissionView, authzpb.Operation_OP_OTHER).Return(true, nil).Once()
	var bulkPaths []string
	client.On("CheckBulkPermissions", mock.Anything, "user-1", mock.Anything, AuthzPermissionView).
		Run(func(args mock.Arguments) { bulkPaths = args.Get(2).([]string) }).
		Return([]bool{true, true}, nil).Once()

	resp, err := server.Readdir(ctx, &pb.ReaddirRequest{Inode: uint64(12)})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)
	require.ElementsMatch(t, []string{"/a/b/c/f.exr", "/a/b/c/g.txt"}, bulkPaths)
	require.Len(t, resp.Entries, 2)
	client.AssertExpectations(t)
}

func TestProxyRestartChurn_UnauthorizedStaysDenied(t *testing.T) {
	engine := churnTree()
	server, ai, client := newChurnProxy(engine, "user-1")
	ctx := context.Background()

	// The unauthorized inode IS recoverable (the engine reconstructs
	// "/a/b/private/secret") — and the authz decision still denies it (I2).
	client.On("CheckPermission", mock.Anything, "user-1", "/a/b/private/secret", AuthzPermissionView, authzpb.Operation_OP_OTHER).Return(false, nil).Once()

	handlerCalled := false
	_, err := callThrough(t, ai, ctx, "GetAttr", &pb.GetAttrRequest{Inode: uint64(21)},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			handlerCalled = true
			return server.GetAttr(ctx, req.(*pb.GetAttrRequest))
		})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.PermissionDenied, st.Code())
	require.False(t, handlerCalled, "recovery must never bypass the authz decision")
	client.AssertExpectations(t)
}

func TestProxyRestartChurn_VanishedInodeENOENT(t *testing.T) {
	engine := churnTree()
	server, ai, _ := newChurnProxy(engine, "user-1")
	ctx := context.Background()

	// Inode 999 never existed: gone → codes.NotFound, which the client maps
	// to ENOENT (I7) — an honest POSIX answer, not a deny.
	handlerCalled := false
	_, err := callThrough(t, ai, ctx, "GetAttr", &pb.GetAttrRequest{Inode: uint64(999)},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			handlerCalled = true
			return server.GetAttr(ctx, req.(*pb.GetAttrRequest))
		})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.NotFound, st.Code())
	require.False(t, handlerCalled)
}

func TestProxyRestartChurn_ReaddirUnrecoverableEACCES(t *testing.T) {
	engine := churnTree()
	server, _, _ := newChurnProxy(engine, "user-1")
	// The engine errors on the walk: the parent path stays unrecoverable even
	// after recovery — the listing must fail with EACCES, never a silent
	// empty listing with errno 0 (design D2).
	engine.failReaddir(10, syscall.EIO)
	ctx := context.Background()

	resp, err := server.Readdir(ctx, &pb.ReaddirRequest{Inode: uint64(11)})
	require.NoError(t, err)
	require.EqualValues(t, syscall.EACCES, resp.Errno, "unrecoverable parent path must answer EACCES, not an empty listing")
}

func TestProxyRestartChurn_ConcurrentMissesOneWalk(t *testing.T) {
	engine := churnTree()
	server, ai, client := newChurnProxy(engine, "user-1")
	client.On("CheckPermission", mock.Anything, "user-1", "/a/b/c/f.exr", AuthzPermissionView, authzpb.Operation_OP_OTHER).Return(true, nil)

	// Hold the leader's walk in flight until all goroutines are inside the
	// proxy: one reconstruction must serve all of them (design I5).
	gate := make(chan struct{})
	engine.gate = gate
	engine.gatedIno = 13

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := ai.UnaryInterceptor()(context.Background(), &pb.GetAttrRequest{Inode: uint64(13)},
				&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetAttr"},
				func(ctx context.Context, req interface{}) (interface{}, error) {
					return server.GetAttr(ctx, req.(*pb.GetAttrRequest))
				})
			assert.NoError(t, err)
			if resp != nil {
				assert.EqualValues(t, 0, resp.(*pb.GetAttrResponse).Errno)
			}
		}()
	}
	require.Eventually(t, func() bool {
		return engine.getAttrCalls(13) == 1
	}, 5*time.Second, time.Millisecond)
	close(gate)
	wg.Wait()

	// The walk is measured on the parent's readdir — the only reader of it is
	// the recovery walk itself (the handler GetAttr of 8 goroutines must not
	// be confused with walk traffic).
	require.Equal(t, int32(1), engine.readdirCalls(11), "concurrent cold-cache misses must collapse into one engine walk")
	require.Equal(t, int32(1), engine.getAttrCalls(11), "the shared parent must be GetAttr'd once")
	client.AssertExpectations(t)
}

// TestProxyInvalidation_RenameAuthorizesNewPath (task 2.4): after a rename the
// cached path of the moved inode must be rewritten, so the next GetAttr is
// authorized against the NEW location — not the stale one. Both rename parents
// come from recovery (cold-cache start): recovery makes the interceptor's
// two-parent Rename check pass.
func TestProxyInvalidation_RenameAuthorizesNewPath(t *testing.T) {
	engine := churnTree()
	server, ai, client := newChurnProxy(engine, "user-1")
	ctx := context.Background()

	// Warm-up: GetAttr(13) recovers "/a/b/c/f.exr" and the whole ancestor chain.
	client.On("CheckPermission", mock.Anything, "user-1", "/a/b/c/f.exr", AuthzPermissionView, authzpb.Operation_OP_OTHER).Return(true, nil).Once()
	resp, err := callThrough(t, ai, ctx, "GetAttr", &pb.GetAttrRequest{Inode: uint64(13)},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			return server.GetAttr(ctx, req.(*pb.GetAttrRequest))
		})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.(*pb.GetAttrResponse).Errno)
	client.AssertExpectations(t)

	// Rename f.exr → /a/renamed.txt: Write on BOTH parents (source "/a/b/c/",
	// destination "/a/" — both known after recovery).
	client.On("CheckPermission", mock.Anything, "user-1", "/a/b/c/", AuthzPermissionWrite, authzpb.Operation_OP_OTHER).Return(true, nil).Once()
	client.On("CheckPermission", mock.Anything, "user-1", "/a/", AuthzPermissionWrite, authzpb.Operation_OP_OTHER).Return(true, nil).Once()
	resp, err = callThrough(t, ai, ctx, "Rename", &pb.RenameRequest{ParentSrc: uint64(12), NameSrc: "f.exr", ParentDst: uint64(10), NameDst: "renamed.txt"},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			return server.Rename(ctx, req.(*pb.RenameRequest))
		})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.(*pb.RenameResponse).Errno)
	client.AssertExpectations(t)

	// GetAttr after the rename: ONLY the new path is acceptable — if the cache
	// still held the old path, this check would target "/a/b/c/f.exr" and the
	// expectation below would never match.
	client.On("CheckPermission", mock.Anything, "user-1", "/a/renamed.txt", AuthzPermissionView, authzpb.Operation_OP_OTHER).Return(true, nil).Once()
	resp, err = callThrough(t, ai, ctx, "GetAttr", &pb.GetAttrRequest{Inode: uint64(13)},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			return server.GetAttr(ctx, req.(*pb.GetAttrRequest))
		})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.(*pb.GetAttrResponse).Errno)
	client.AssertExpectations(t)
}

// TestProxyInvalidation_UnlinkGivesHonestENOENT (task 2.4 + I7): after an
// unlink the cached mapping is dropped; the next GetAttr recovers, the engine
// reports the inode gone, and the client sees ENOENT — not EACCES.
func TestProxyInvalidation_UnlinkGivesHonestENOENT(t *testing.T) {
	engine := churnTree()
	server, ai, client := newChurnProxy(engine, "user-1")
	ctx := context.Background()

	// Warm-up: GetAttr(14) recovers "/a/b/c/g.txt".
	client.On("CheckPermission", mock.Anything, "user-1", "/a/b/c/g.txt", AuthzPermissionView, authzpb.Operation_OP_OTHER).Return(true, nil).Once()
	resp, err := callThrough(t, ai, ctx, "GetAttr", &pb.GetAttrRequest{Inode: uint64(14)},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			return server.GetAttr(ctx, req.(*pb.GetAttrRequest))
		})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.(*pb.GetAttrResponse).Errno)
	client.AssertExpectations(t)

	// Unlink g.txt: Write on the parent.
	client.On("CheckPermission", mock.Anything, "user-1", "/a/b/c/", AuthzPermissionWrite, authzpb.Operation_OP_OTHER).Return(true, nil).Once()
	resp, err = callThrough(t, ai, ctx, "Unlink", &pb.UnlinkRequest{Parent: uint64(12), Name: "g.txt"},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			return server.Unlink(ctx, req.(*pb.UnlinkRequest))
		})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.(*pb.UnlinkResponse).Errno)
	client.AssertExpectations(t)

	// GetAttr on the removed inode: gone → codes.NotFound (client → ENOENT).
	handlerCalled := false
	_, err = callThrough(t, ai, ctx, "GetAttr", &pb.GetAttrRequest{Inode: uint64(14)},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			handlerCalled = true
			return server.GetAttr(ctx, req.(*pb.GetAttrRequest))
		})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.NotFound, st.Code())
	require.False(t, handlerCalled)
}
