/*
 * JuiceFS, Copyright 2024 Juicedata, Inc.
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
	"io"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mockValidator implements a simple validator for interceptor tests.
type mockValidator struct {
	acceptToken bool
}

func (m *mockValidator) Validate(ctx context.Context, token string) (interface{}, error) {
	if m.acceptToken {
		return &mockIDToken{sub: "test-user"}, nil
	}
	return nil, io.EOF // simulate validation failure
}

type mockIDToken struct {
	sub string
}

func (m *mockIDToken) Claims(v interface{}) error { return nil }

// mockHandler is a simple unary handler for testing.
func mockHandler(ctx context.Context, req interface{}) (interface{}, error) {
	return map[string]interface{}{"ok": true}, nil
}

func TestUnaryInterceptorNoToken(t *testing.T) {
	v := &mockValidator{acceptToken: false}
	interceptor := UnaryInterceptor(v)

	ctx := context.Background()
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, mockHandler)
	if err != nil {
		t.Errorf("should pass through without token, got error: %v", err)
	}
}

func TestUnaryInterceptorValidToken(t *testing.T) {
	v := &mockValidator{acceptToken: true}
	interceptor := UnaryInterceptor(v)

	md := metadata.Pairs("authorization", "Bearer valid-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, mockHandler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response from handler")
	}
}

func TestUnaryInterceptorInvalidToken(t *testing.T) {
	v := &mockValidator{acceptToken: false}
	interceptor := UnaryInterceptor(v)

	md := metadata.Pairs("authorization", "Bearer invalid-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, mockHandler)
	if err == nil {
		t.Fatal("expected error for invalid token")
	}

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %v", err)
	}
}

func TestUnaryInterceptorWrongBearerFormat(t *testing.T) {
	v := &mockValidator{acceptToken: true}
	interceptor := UnaryInterceptor(v)

	// Wrong prefix — should pass through (no token extracted)
	md := metadata.Pairs("authorization", "Basic abc123")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, mockHandler)
	if err != nil {
		t.Errorf("non-Bearer auth should pass through, got: %v", err)
	}
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		name string
		md   metadata.MD
		want string
	}{
		{"no header", metadata.MD{}, ""},
		{"bearer token", metadata.Pairs("authorization", "Bearer my-token"), "my-token"},
		{"basic auth", metadata.Pairs("authorization", "Basic dXNlcjpwYXNz"), ""},
		{"empty bearer", metadata.Pairs("authorization", "Bearer "), ""},
		{"bearer short", metadata.Pairs("authorization", "Be"), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(context.Background(), tt.md)
			got := ExtractBearerToken(ctx)
			if got != tt.want {
				t.Errorf("ExtractBearerToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Stream interceptor tests ---

type mockStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (m *mockStream) Context() context.Context { return m.ctx }

func TestStreamInterceptorNoToken(t *testing.T) {
	v := &mockValidator{acceptToken: false}
	interceptor := StreamInterceptor(v)

	ctx := context.Background()
	ss := &mockStream{ctx: ctx}
	handled := false
	handler := func(srv interface{}, stream grpc.ServerStream) error {
		handled = true
		return nil
	}

	err := interceptor(nil, ss, &grpc.StreamServerInfo{}, handler)
	if err != nil {
		t.Errorf("should pass through without token: %v", err)
	}
	if !handled {
		t.Error("handler should have been called")
	}
}

func TestStreamInterceptorInvalidToken(t *testing.T) {
	v := &mockValidator{acceptToken: false}
	interceptor := StreamInterceptor(v)

	md := metadata.Pairs("authorization", "Bearer bad-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ss := &mockStream{ctx: ctx}

	handled := false
	handler := func(srv interface{}, stream grpc.ServerStream) error {
		handled = true
		return nil
	}

	err := interceptor(nil, ss, &grpc.StreamServerInfo{}, handler)
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
	if handled {
		t.Error("handler should NOT have been called")
	}
}

func TestStreamInterceptorValidToken(t *testing.T) {
	v := &mockValidator{acceptToken: true}
	interceptor := StreamInterceptor(v)

	md := metadata.Pairs("authorization", "Bearer good-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ss := &mockStream{ctx: ctx}

	var gotCtx context.Context
	handler := func(srv interface{}, stream grpc.ServerStream) error {
		gotCtx = stream.Context()
		return nil
	}

	err := interceptor(nil, ss, &grpc.StreamServerInfo{}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The context should contain claims
	if ClaimsFromContext(gotCtx) == nil {
		t.Error("context should contain claims after valid token")
	}
}

func TestServerStreamWrapper(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyClaims{}, "test-claims")
	wrapped := &serverStream{ctx: ctx}

	if wrapped.Context() != ctx {
		t.Error("serverStream.Context() should return wrapped context")
	}
}

// --- Strict interceptor tests ---

func TestStrictUnaryInterceptorNoToken(t *testing.T) {
	v := &mockValidator{acceptToken: true}
	interceptor := StrictUnaryInterceptor(v)

	ctx := context.Background()
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, mockHandler)
	if err == nil {
		t.Fatal("strict mode should reject requests without token")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %v", err)
	}
}

func TestStrictUnaryInterceptorValidToken(t *testing.T) {
	v := &mockValidator{acceptToken: true}
	interceptor := StrictUnaryInterceptor(v)

	md := metadata.Pairs("authorization", "Bearer valid-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, mockHandler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response from handler")
	}
}

func TestStrictUnaryInterceptorInvalidToken(t *testing.T) {
	v := &mockValidator{acceptToken: false}
	interceptor := StrictUnaryInterceptor(v)

	md := metadata.Pairs("authorization", "Bearer invalid-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, mockHandler)
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %v", err)
	}
}

func TestStrictStreamInterceptorNoToken(t *testing.T) {
	v := &mockValidator{acceptToken: true}
	interceptor := StrictStreamInterceptor(v)

	ctx := context.Background()
	ss := &mockStream{ctx: ctx}

	handled := false
	handler := func(srv interface{}, stream grpc.ServerStream) error {
		handled = true
		return nil
	}

	err := interceptor(nil, ss, &grpc.StreamServerInfo{}, handler)
	if err == nil {
		t.Fatal("strict mode should reject stream requests without token")
	}
	if handled {
		t.Error("handler should NOT have been called")
	}
}

func TestStrictStreamInterceptorValidToken(t *testing.T) {
	v := &mockValidator{acceptToken: true}
	interceptor := StrictStreamInterceptor(v)

	md := metadata.Pairs("authorization", "Bearer good-token")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	ss := &mockStream{ctx: ctx}

	var gotCtx context.Context
	handler := func(srv interface{}, stream grpc.ServerStream) error {
		gotCtx = stream.Context()
		return nil
	}

	err := interceptor(nil, ss, &grpc.StreamServerInfo{}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ClaimsFromContext(gotCtx) == nil {
		t.Error("context should contain claims after valid token")
	}
}
