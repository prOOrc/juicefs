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

package oidc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// strictFailingValidator fails the test if it is ever invoked: service-kind
// requests must short-circuit the OIDC path before any token validation.
type strictFailingValidator struct {
	t *testing.T
}

func (v *strictFailingValidator) Validate(ctx context.Context, token string) (interface{}, error) {
	v.t.Errorf("validator must not be called for a service identity (token len %d)", len(token))
	return nil, context.Canceled
}

// fakeServerStream is a minimal grpc.ServerStream carrying a fixed context.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *fakeServerStream) Context() context.Context { return s.ctx }

func TestServiceIdentityKindContext(t *testing.T) {
	ctx := context.Background()
	kind, ok := IdentityKindFromContext(ctx)
	require.False(t, ok)
	require.False(t, IsServiceIdentity(ctx))

	ctx = WithIdentityKind(ctx, IdentityKindService)
	kind, ok = IdentityKindFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, IdentityKindService, kind)
	assert.True(t, IsServiceIdentity(ctx))

	oidcUserCtx := WithIdentityKind(context.Background(), IdentityKindOIDCUser)
	kind, ok = IdentityKindFromContext(oidcUserCtx)
	require.True(t, ok)
	assert.Equal(t, IdentityKindOIDCUser, kind)
	assert.False(t, IsServiceIdentity(oidcUserCtx))
}

func TestStrictUnaryInterceptorServiceIdentityPassThrough(t *testing.T) {
	interceptor := StrictUnaryInterceptor(&strictFailingValidator{t: t})

	handlerCalled := false
	var handlerCtx context.Context
	ctx := WithIdentityKind(context.Background(), IdentityKindService)
	resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetFormat"},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			handlerCalled = true
			handlerCtx = ctx
			return map[string]interface{}{"ok": true}, nil
		})
	require.NoError(t, err)
	assert.True(t, handlerCalled, "service request must reach the handler")
	assert.NotNil(t, resp)
	kind, ok := IdentityKindFromContext(handlerCtx)
	require.True(t, ok, "identity kind must survive into the handler")
	assert.Equal(t, IdentityKindService, kind)
}

func TestStrictStreamInterceptorServiceIdentityPassThrough(t *testing.T) {
	interceptor := StrictStreamInterceptor(&strictFailingValidator{t: t})

	handlerCalled := false
	var handlerCtx context.Context
	ss := &fakeServerStream{ctx: WithIdentityKind(context.Background(), IdentityKindService)}
	err := interceptor(nil, ss, &grpc.StreamServerInfo{FullMethod: "/pb.MetaService/DumpMeta"},
		func(srv interface{}, stream grpc.ServerStream) error {
			handlerCalled = true
			handlerCtx = stream.Context()
			return nil
		})
	require.NoError(t, err)
	assert.True(t, handlerCalled, "service stream must reach the handler")
	kind, ok := IdentityKindFromContext(handlerCtx)
	require.True(t, ok, "identity kind must survive into the stream handler")
	assert.Equal(t, IdentityKindService, kind)
}

// Requests without a service kind keep the strict behavior untouched.
func TestStrictInterceptorsWithoutKindUnchanged(t *testing.T) {
	v := &mockValidator{acceptToken: true}

	_, err := StrictUnaryInterceptor(v)(context.Background(), nil, &grpc.UnaryServerInfo{}, mockHandler)
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))

	err = StrictStreamInterceptor(v)(nil, &fakeServerStream{ctx: context.Background()},
		&grpc.StreamServerInfo{}, func(srv interface{}, stream grpc.ServerStream) error {
			t.Error("handler must not run without a token")
			return nil
		})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}
