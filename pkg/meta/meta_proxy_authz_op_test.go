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

	authzpb "github.com/juicedata/juicefs/pkg/meta/authz_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"google.golang.org/grpc"
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
	cache.Set(100, "/companies/")

	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/companies/", AuthzPermissionWrite, authzpb.Operation_OP_MKDIR).Return(true, nil)

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

// TestAuthzOpContext_MkdirClassified_SlashlessCacheEntry: even when the parent
// directory lost its trailing slash in the path cache, the mkdir check path is
// normalized back to the dir convention — a slashless "/companies" would be
// stripped to a non-empty path and wrongly denied.
func TestAuthzOpContext_MkdirClassified_SlashlessCacheEntry(t *testing.T) {
	mockClient := &mockAuthzClient{}
	cache := NewInodePathCache(0)
	cache.Set(100, "/companies")

	mockClient.On("CheckPermission", mock.Anything, "user-123",
		"/companies/", AuthzPermissionWrite, authzpb.Operation_OP_MKDIR).Return(true, nil)

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
