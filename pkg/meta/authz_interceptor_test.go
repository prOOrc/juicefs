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
	"testing"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockAuthzClient implements AuthzClient for testing.
type mockAuthzClient struct {
	mock.Mock
}

func (m *mockAuthzClient) CheckPermission(ctx context.Context, userID, path string, perm AuthzPermission) (bool, error) {
	args := m.Called(ctx, userID, path, perm)
	return args.Bool(0), args.Error(1)
}

func (m *mockAuthzClient) CheckBulkPermissions(ctx context.Context, userID string, paths []string, perm AuthzPermission) ([]bool, error) {
	args := m.Called(ctx, userID, paths, perm)
	return args.Get(0).([]bool), args.Error(1)
}

func (m *mockAuthzClient) CheckOrganizationAdmin(ctx context.Context, userID string) (bool, error) {
	args := m.Called(ctx, userID)
	return args.Bool(0), args.Error(1)
}

// newTestInterceptor creates an AuthzInterceptor with a fixed userID extractor for testing.
func newTestInterceptor(client AuthzClient, cache *InodePathCache, userID string) *AuthzInterceptor {
	ai := NewAuthzInterceptor(client, cache, nil)
	ai.userIDExtractor = func(ctx context.Context) string {
		return userID
	}
	return ai
}

func TestRequiredPermission_Lifecycle(t *testing.T) {
	ai := &AuthzInterceptor{}

	assert.Equal(t, AuthzPermissionNone, ai.requiredPermission("/pb.MetaService/Init"))
	assert.Equal(t, AuthzPermissionNone, ai.requiredPermission("/pb.MetaService/NewSession"))
	assert.Equal(t, AuthzPermissionNone, ai.requiredPermission("/pb.MetaService/CloseSession"))
	// Chroot is a per-session restriction (limits user's view), not an escalation.
	// Called unconditionally during mount init; all subsequent file ops are still checked.
	assert.Equal(t, AuthzPermissionNone, ai.requiredPermission("/pb.MetaService/Chroot"))
}

func TestRequiredPermission_View(t *testing.T) {
	ai := &AuthzInterceptor{}

	assert.Equal(t, AuthzPermissionView, ai.requiredPermission("/pb.MetaService/Lookup"))
	assert.Equal(t, AuthzPermissionView, ai.requiredPermission("/pb.MetaService/GetAttr"))
	assert.Equal(t, AuthzPermissionView, ai.requiredPermission("/pb.MetaService/ReadLink"))
}

func TestRequiredPermission_Readdir(t *testing.T) {
	ai := &AuthzInterceptor{}

	// Readdir is handled by post-filter in handler, not by interceptor
	assert.Equal(t, AuthzPermissionNone, ai.requiredPermission("/pb.MetaService/Readdir"))
}

func TestRequiredPermission_Read(t *testing.T) {
	ai := &AuthzInterceptor{}

	assert.Equal(t, AuthzPermissionRead, ai.requiredPermission("/pb.MetaService/Open"))
	assert.Equal(t, AuthzPermissionRead, ai.requiredPermission("/pb.MetaService/Read"))
}

func TestRequiredPermission_Write(t *testing.T) {
	ai := &AuthzInterceptor{}

	assert.Equal(t, AuthzPermissionWrite, ai.requiredPermission("/pb.MetaService/Create"))
	assert.Equal(t, AuthzPermissionWrite, ai.requiredPermission("/pb.MetaService/Write"))
	assert.Equal(t, AuthzPermissionWrite, ai.requiredPermission("/pb.MetaService/Unlink"))
	assert.Equal(t, AuthzPermissionWrite, ai.requiredPermission("/pb.MetaService/Mkdir"))
	assert.Equal(t, AuthzPermissionWrite, ai.requiredPermission("/pb.MetaService/Rename"))
	assert.Equal(t, AuthzPermissionWrite, ai.requiredPermission("/pb.MetaService/SetAttr"))
}

func TestRequiredPermission_Close(t *testing.T) {
	ai := &AuthzInterceptor{}

	// Close should be View (not Write) — read-only files should not require Write
	assert.Equal(t, AuthzPermissionView, ai.requiredPermission("/pb.MetaService/Close"))
}

func TestRequiredPermission_Admin(t *testing.T) {
	ai := &AuthzInterceptor{}

	assert.Equal(t, AuthzPermissionAdmin, ai.requiredPermission("/pb.MetaService/GetFormat"))
	assert.Equal(t, AuthzPermissionAdmin, ai.requiredPermission("/pb.MetaService/Compact"))
	assert.Equal(t, AuthzPermissionAdmin, ai.requiredPermission("/pb.MetaService/HandleQuota"))
	assert.Equal(t, AuthzPermissionAdmin, ai.requiredPermission("/pb.MetaService/Remove"))
}

func TestRequiredPermission_Unknown(t *testing.T) {
	ai := &AuthzInterceptor{}

	// Unknown methods should be denied for everyone
	assert.Equal(t, AuthzPermissionDenied, ai.requiredPermission("/pb.MetaService/NonExistentMethod"))
}

func TestInterceptor_AllowsWithoutAuthz(t *testing.T) {
	// When authz is disabled (no client), all requests pass through
	ai := NewAuthzInterceptor(nil, NewInodePathCache(0), nil)
	assert.False(t, ai.enabled)

	interceptor := ai.UnaryInterceptor()
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return &pb.GetAttrResponse{}, nil
	}

	resp, err := interceptor(context.Background(), &pb.GetAttrRequest{Inode: 1},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetAttr"}, handler)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestInterceptor_DeniesUnauthorizedAccess(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	cache.Set(100, "/company-abc/secret/file.exr")

	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/company-abc/secret/file.exr", AuthzPermissionRead).Return(false, nil)

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return &pb.OpenResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.OpenRequest{Inode: 100},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Open"}, handler)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.PermissionDenied, st.Code())
}

func TestInterceptor_AllowsAuthorizedAccess(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	cache.Set(100, "/company-abc/projects/file.exr")

	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/company-abc/projects/file.exr", AuthzPermissionRead).Return(true, nil)

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.OpenResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.OpenRequest{Inode: 100},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Open"}, handler)

	assert.NoError(t, err)
	assert.True(t, called, "handler should be called when authorized")
}

func TestInterceptor_DenyUnmappedInode(t *testing.T) {
	ai := newTestInterceptor(&mockAuthzClient{}, NewInodePathCache(0), "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.OpenResponse{}, nil
	}

	// Inode 999 is not in cache — should deny (deny-by-default)
	_, err := interceptor(context.Background(), &pb.OpenRequest{Inode: 999},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Open"}, handler)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.False(t, called, "handler should NOT be called for unmapped inodes")
}

func TestInterceptor_DenyOnAuthzError(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	cache.Set(100, "/file")

	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/file", AuthzPermissionRead).Return(false, assert.AnError)

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.OpenResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.OpenRequest{Inode: 100},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Open"}, handler)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.False(t, called, "handler should NOT be called when authz service errors")
}

func TestInterceptor_AllowsLookupChecksParent(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	// Root is always in cache; Lookup on root parent is short-circuited (always allowed)

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.LookupResponse{Inode: 12345}, nil
	}

	_, err := interceptor(context.Background(), &pb.LookupRequest{Parent: 1, Name: "company-abc"},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Lookup"}, handler)

	assert.NoError(t, err)
	assert.True(t, called, "handler should be called for Lookup on root (always allowed)")
}

func TestInterceptor_SkipsLifecycleMethods(t *testing.T) {
	ai := newTestInterceptor(&mockAuthzClient{}, NewInodePathCache(0), "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.InitResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.InitRequest{},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Init"}, handler)

	assert.NoError(t, err)
	assert.True(t, called, "lifecycle methods should skip authz check")
}

func TestInterceptor_DeniesNonAdminOnAdminOps(t *testing.T) {
	mockClient := &mockAuthzClient{}
	mockClient.On("CheckOrganizationAdmin", mock.Anything, "user-123").Return(false, nil)

	ai := newTestInterceptor(mockClient, NewInodePathCache(0), "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.GetFormatResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.GetFormatRequest{},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetFormat"}, handler)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.False(t, called, "handler should NOT be called for non-admin on admin ops")
}

func TestInterceptor_AllowsAdminOnAdminOps(t *testing.T) {
	mockClient := &mockAuthzClient{}
	mockClient.On("CheckOrganizationAdmin", mock.Anything, "admin-user").Return(true, nil)

	ai := newTestInterceptor(mockClient, NewInodePathCache(0), "admin-user")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.GetFormatResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.GetFormatRequest{},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetFormat"}, handler)

	assert.NoError(t, err)
	assert.True(t, called, "handler should be called for org admin on admin ops")
}

func TestInterceptor_SkipsReaddir(t *testing.T) {
	// Readdir is handled by post-filter in the handler, not by interceptor
	ai := newTestInterceptor(&mockAuthzClient{}, NewInodePathCache(0), "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.ReaddirResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.ReaddirRequest{Inode: 100},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Readdir"}, handler)

	assert.NoError(t, err)
	assert.True(t, called, "Readdir should skip interceptor (post-filter in handler)")
}

func TestInterceptor_DenyUnknownMethod(t *testing.T) {
	ai := newTestInterceptor(&mockAuthzClient{}, NewInodePathCache(0), "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return nil, nil
	}

	_, err := interceptor(context.Background(), &pb.GetAttrRequest{Inode: 1},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/NonExistentMethod"}, handler)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.False(t, called, "handler should NOT be called for unknown methods")
}

func TestInterceptor_DenyEmptyUserID(t *testing.T) {
	ai := newTestInterceptor(&mockAuthzClient{}, NewInodePathCache(0), "")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.GetAttrResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.GetAttrRequest{Inode: 1},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetAttr"}, handler)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
	assert.False(t, called, "handler should NOT be called without authenticated user")
}

func TestInterceptor_RenameRequiresBothParents(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	// Only source parent is in cache; destination parent is missing
	cache.Set(100, "/src-dir")

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.RenameResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.RenameRequest{
		ParentSrc: 100, NameSrc: "file",
		ParentDst: 999, NameDst: "file2", // 999 not in cache
	}, &grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Rename"}, handler)

	assert.Error(t, err)
	assert.False(t, called, "handler should NOT be called when dst parent is missing")
}

func TestInterceptor_LinkRequiresBothPaths(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	// Only destination parent is in cache; source inode is missing
	cache.Set(200, "/dst-dir")

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.LinkResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.LinkRequest{
		InodeSrc: 999, Parent: 200, Name: "link", // 999 not in cache
	}, &grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Link"}, handler)

	assert.Error(t, err)
	assert.False(t, called, "handler should NOT be called when src inode is missing")
}

func TestInterceptor_CopyFileRangeRequiresBothPaths(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	cache.Set(100, "/src-file")
	// destination inode not in cache

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.CopyFileRangeResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.CopyFileRangeRequest{
		Fin: 100, Fout: 999, // 999 not in cache
	}, &grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/CopyFileRange"}, handler)

	assert.Error(t, err)
	assert.False(t, called, "handler should NOT be called when dst inode is missing")
}

func TestInterceptor_OpenWriteFlagsRequiresWrite(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	cache.Set(100, "/file")

	// O_WRONLY=1 → requires Write permission
	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/file", AuthzPermissionWrite).Return(true, nil)

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.OpenResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.OpenRequest{Inode: 100, Flags: 1}, // O_WRONLY
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Open"}, handler)

	assert.NoError(t, err)
	assert.True(t, called)
	mockClient.AssertExpectations(t)
}

func TestInterceptor_AccessR_OKRequiresRead(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	cache.Set(100, "/file")

	// R_OK=4 → requires Read permission
	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/file", AuthzPermissionRead).Return(true, nil)

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.AccessResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.AccessRequest{Inode: 100, Modemask: 4}, // R_OK
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Access"}, handler)

	assert.NoError(t, err)
	assert.True(t, called)
	mockClient.AssertExpectations(t)
}

func TestPermissionFromFlags(t *testing.T) {
	// O_RDONLY=0 → Read
	assert.Equal(t, AuthzPermissionRead, permissionFromFlags(0))
	// O_WRONLY=1 → Write
	assert.Equal(t, AuthzPermissionWrite, permissionFromFlags(1))
	// O_RDWR=2 → Write
	assert.Equal(t, AuthzPermissionWrite, permissionFromFlags(2))
}

func TestPermissionFromAccessMask(t *testing.T) {
	// F_OK=0 → View
	assert.Equal(t, AuthzPermissionView, permissionFromAccessMask(0))
	// R_OK=4 → Read
	assert.Equal(t, AuthzPermissionRead, permissionFromAccessMask(4))
	// W_OK=2 → Write
	assert.Equal(t, AuthzPermissionWrite, permissionFromAccessMask(2))
	// X_OK=1 → View
	assert.Equal(t, AuthzPermissionView, permissionFromAccessMask(1))
}

func TestAuthzInterceptor_RootPathViewAlwaysAllowed(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	// Root inode (1) is pre-populated with "/"

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.GetAttrResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.GetAttrRequest{Inode: 1},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetAttr"}, handler)

	assert.NoError(t, err)
	assert.True(t, called, "handler should be called for root GetAttr (View always allowed)")
	// CheckPermission must NOT have been called
	mockClient.AssertNotCalled(t, "CheckPermission", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestAuthzInterceptor_RootPathWriteStillChecked(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	// Root inode (1) is pre-populated with "/"

	// Write on root should still go through authz service (not always-allowed)
	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/", AuthzPermissionWrite).Return(false, nil)

	ai := newTestInterceptor(mockClient, cache, "user-123")
	interceptor := ai.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		called = true
		return &pb.MkdirResponse{}, nil
	}

	_, err := interceptor(context.Background(), &pb.MkdirRequest{Parent: 1, Name: "new-company"},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/Mkdir"}, handler)

	assert.Error(t, err)
	st, ok := status.FromError(err)
	assert.True(t, ok)
	assert.Equal(t, codes.PermissionDenied, st.Code())
	assert.False(t, called, "handler should NOT be called for Write on root (not always-allowed)")
	mockClient.AssertExpectations(t)
}

func TestIsAlwaysAllowed(t *testing.T) {
	// Root + View → always allowed
	assert.True(t, isAlwaysAllowed("/", AuthzPermissionView))
	// Root + Write → NOT always allowed
	assert.False(t, isAlwaysAllowed("/", AuthzPermissionWrite))
	// Root + Read → NOT always allowed
	assert.False(t, isAlwaysAllowed("/", AuthzPermissionRead))
	// Non-root + View → NOT always allowed
	assert.False(t, isAlwaysAllowed("/company-abc", AuthzPermissionView))
	// Non-root + Write → NOT always allowed
	assert.False(t, isAlwaysAllowed("/company-abc/file.exr", AuthzPermissionWrite))
}
