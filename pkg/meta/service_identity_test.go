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
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// --- harness -------------------------------------------------------------

const testServiceCredential = "svc-token-raw-value"

func testServiceCredentialHashHex(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(testServiceCredential))
	return hex.EncodeToString(sum[:])
}

// newTestServiceIdentityInterceptor builds an interceptor configured with the
// test credential hash and a non-empty node credential.
func newTestServiceIdentityInterceptor(t *testing.T) *ServiceIdentityInterceptor {
	t.Helper()
	t.Setenv("TEST_NODE_CREDENTIAL", "node-token-raw")
	cfg, err := NewServiceIdentityConfig(testServiceCredentialHashHex(t), "env:TEST_NODE_CREDENTIAL")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	return NewServiceIdentityInterceptor(cfg)
}

// serviceIncomingCtx attaches the given headers to a fresh incoming context.
func serviceIncomingCtx(t *testing.T, headers map[string]string) context.Context {
	t.Helper()
	if len(headers) == 0 {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.New(headers))
}

// recordingUnaryHandler records whether it ran and the ctx it received.
type recordingUnaryHandler struct {
	called bool
	ctx    context.Context
}

func (h *recordingUnaryHandler) invoke(ctx context.Context, req interface{}) (interface{}, error) {
	h.called = true
	h.ctx = ctx
	return map[string]interface{}{"ok": true}, nil
}

// fakeServerStream is a minimal grpc.ServerStream with a fixed context.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *fakeServerStream) Context() context.Context { return s.ctx }

// recordingStreamHandler records whether the stream handler ran.
type recordingStreamHandler struct {
	called bool
	ctx    context.Context
}

func (h *recordingStreamHandler) invoke(srv interface{}, stream grpc.ServerStream) error {
	h.called = true
	h.ctx = stream.Context()
	return nil
}

// --- matrix --------------------------------------------------------------

// TestServiceIdentityUnaryMatrix walks the authn/authz matrix for unary RPCs:
// service-ok / both / wrong / no-header × GetFormat / admin / lifecycle /
// file-RPC (tasks 1.1).
func TestServiceIdentityUnaryMatrix(t *testing.T) {
	si := newTestServiceIdentityInterceptor(t)
	interceptor := si.UnaryInterceptor()

	okHeaders := map[string]string{MetadataServiceCredential: testServiceCredential}
	bothHeaders := map[string]string{
		MetadataServiceCredential: testServiceCredential,
		"authorization":           "Bearer some-valid-token",
	}
	wrongHeaders := map[string]string{MetadataServiceCredential: "not-the-credential"}
	wrongPlusBearer := map[string]string{
		MetadataServiceCredential: "not-the-credential",
		"authorization":           "Bearer some-valid-token",
	}

	cases := []struct {
		name        string
		method      string
		headers     map[string]string
		wantCode    codes.Code
		wantHandler bool
		wantService bool
	}{
		// service-ok
		{name: "ok GetFormat allowed", method: "/pb.MetaService/GetFormat", headers: okHeaders, wantHandler: true, wantService: true},
		{name: "ok lifecycle Init passes", method: "/pb.MetaService/Init", headers: okHeaders, wantHandler: true, wantService: true},
		{name: "ok file RPC GetAttr passes", method: "/pb.MetaService/GetAttr", headers: okHeaders, wantHandler: true, wantService: true},
		{name: "ok file RPC Lookup passes", method: "/pb.MetaService/Lookup", headers: okHeaders, wantHandler: true, wantService: true},
		{name: "ok admin unary Remove denied", method: "/pb.MetaService/Remove", headers: okHeaders, wantCode: codes.PermissionDenied},
		{name: "ok admin unary Check denied", method: "/pb.MetaService/Check", headers: okHeaders, wantCode: codes.PermissionDenied},
		{name: "ok admin token ListTokens denied", method: "/pb.MetaService/ListTokens", headers: okHeaders, wantCode: codes.PermissionDenied},
		// both headers
		{name: "both GetFormat unauthenticated", method: "/pb.MetaService/GetFormat", headers: bothHeaders, wantCode: codes.Unauthenticated},
		{name: "both GetAttr unauthenticated", method: "/pb.MetaService/GetAttr", headers: bothHeaders, wantCode: codes.Unauthenticated},
		{name: "both Remove unauthenticated", method: "/pb.MetaService/Remove", headers: bothHeaders, wantCode: codes.Unauthenticated},
		// wrong credential — no fallback to OIDC, even with a valid bearer
		{name: "wrong GetFormat unauthenticated", method: "/pb.MetaService/GetFormat", headers: wrongHeaders, wantCode: codes.Unauthenticated},
		{name: "wrong GetAttr unauthenticated", method: "/pb.MetaService/GetAttr", headers: wrongHeaders, wantCode: codes.Unauthenticated},
		{name: "wrong plus bearer still unauthenticated", method: "/pb.MetaService/GetAttr", headers: wrongPlusBearer, wantCode: codes.Unauthenticated},
		// no header — legacy path untouched
		{name: "no header GetFormat passes without kind", method: "/pb.MetaService/GetFormat", headers: nil, wantHandler: true, wantService: false},
		{name: "no header GetAttr passes without kind", method: "/pb.MetaService/GetAttr", headers: nil, wantHandler: true, wantService: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := &recordingUnaryHandler{}
			_, err := interceptor(serviceIncomingCtx(t, tc.headers), nil,
				&grpc.UnaryServerInfo{FullMethod: tc.method}, handler.invoke)

			if tc.wantCode != codes.OK {
				require.Error(t, err)
				assert.Equal(t, tc.wantCode, status.Code(err))
				assert.False(t, handler.called, "handler must not run for a rejected request")
				return
			}
			require.NoError(t, err)
			assert.True(t, handler.called, "handler must run for an allowed request")
			kind, ok := oidc.IdentityKindFromContext(handler.ctx)
			assert.Equal(t, tc.wantService, ok && kind == oidc.IdentityKindService,
				"identity kind in handler ctx mismatch")
		})
	}
}

// TestServiceIdentityStreamMatrix repeats the matrix for streaming RPCs,
// including the streaming admin surface (DumpMeta / ScanChangelog) that must
// be denied for service identities.
func TestServiceIdentityStreamMatrix(t *testing.T) {
	si := newTestServiceIdentityInterceptor(t)
	interceptor := si.StreamInterceptor()

	okHeaders := map[string]string{MetadataServiceCredential: testServiceCredential}
	bothHeaders := map[string]string{
		MetadataServiceCredential: testServiceCredential,
		"authorization":           "Bearer some-valid-token",
	}
	wrongHeaders := map[string]string{MetadataServiceCredential: "not-the-credential"}

	cases := []struct {
		name        string
		method      string
		headers     map[string]string
		wantCode    codes.Code
		wantHandler bool
		wantService bool
	}{
		{name: "ok admin stream DumpMeta denied", method: "/pb.MetaService/DumpMeta", headers: okHeaders, wantCode: codes.PermissionDenied},
		{name: "ok admin stream LoadMeta denied", method: "/pb.MetaService/LoadMeta", headers: okHeaders, wantCode: codes.PermissionDenied},
		{name: "ok admin stream ScanChangelog denied", method: "/pb.MetaService/ScanChangelog", headers: okHeaders, wantCode: codes.PermissionDenied},
		{name: "ok admin stream ScanDeletedObject denied", method: "/pb.MetaService/ScanDeletedObject", headers: okHeaders, wantCode: codes.PermissionDenied},
		{name: "ok lifecycle stream Init passes", method: "/pb.MetaService/Init", headers: okHeaders, wantHandler: true, wantService: true},
		{name: "both stream unauthenticated", method: "/pb.MetaService/Init", headers: bothHeaders, wantCode: codes.Unauthenticated},
		{name: "wrong stream unauthenticated", method: "/pb.MetaService/DumpMeta", headers: wrongHeaders, wantCode: codes.Unauthenticated},
		{name: "no header stream passes without kind", method: "/pb.MetaService/GetAttr", headers: nil, wantHandler: true, wantService: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := &recordingStreamHandler{}
			ss := &fakeServerStream{ctx: serviceIncomingCtx(t, tc.headers)}
			err := interceptor(nil, ss, &grpc.StreamServerInfo{FullMethod: tc.method}, handler.invoke)

			if tc.wantCode != codes.OK {
				require.Error(t, err)
				assert.Equal(t, tc.wantCode, status.Code(err))
				assert.False(t, handler.called, "stream handler must not run for a rejected request")
				return
			}
			require.NoError(t, err)
			assert.True(t, handler.called, "stream handler must run for an allowed request")
			kind, ok := oidc.IdentityKindFromContext(handler.ctx)
			assert.Equal(t, tc.wantService, ok && kind == oidc.IdentityKindService,
				"identity kind in stream handler ctx mismatch")
		})
	}
}

// TestServiceIdentityDisabledHeaderIgnored: without a configured hash the
// x-service-credential header is ignored entirely — OIDC-only behavior
// unchanged, no kind is set.
func TestServiceIdentityDisabledHeaderIgnored(t *testing.T) {
	si := NewServiceIdentityInterceptor(nil) // not wired by cmd in this state; defensive
	interceptor := si.UnaryInterceptor()

	headers := map[string]string{MetadataServiceCredential: testServiceCredential}
	handler := &recordingUnaryHandler{}
	_, err := interceptor(serviceIncomingCtx(t, headers), nil,
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetFormat"}, handler.invoke)
	require.NoError(t, err)
	assert.True(t, handler.called)
	_, ok := oidc.IdentityKindFromContext(handler.ctx)
	assert.False(t, ok, "no identity kind must be set when the mode is disabled")

	// A both-headers request also keeps the legacy path (OIDC owns it).
	both := map[string]string{
		MetadataServiceCredential: testServiceCredential,
		"authorization":           "Bearer token",
	}
	handler2 := &recordingUnaryHandler{}
	_, err = interceptor(serviceIncomingCtx(t, both), nil,
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetFormat"}, handler2.invoke)
	require.NoError(t, err)
	assert.True(t, handler2.called)
}

// TestServiceIdentityNoNodeCredentialFailClosed: service mode enabled without
// a node credential rejects service connections at the door, while requests
// without the header keep the OIDC path.
func TestServiceIdentityNoNodeCredentialFailClosed(t *testing.T) {
	cfg, err := NewServiceIdentityConfig(testServiceCredentialHashHex(t), "")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, cfg.NodeCredential())

	si := NewServiceIdentityInterceptor(cfg)

	handler := &recordingUnaryHandler{}
	_, err = si.UnaryInterceptor()(serviceIncomingCtx(t, map[string]string{
		MetadataServiceCredential: testServiceCredential,
	}), nil, &grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetFormat"}, handler.invoke)
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.False(t, handler.called)

	// No header → legacy path.
	handler2 := &recordingUnaryHandler{}
	_, err = si.UnaryInterceptor()(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetAttr"}, handler2.invoke)
	require.NoError(t, err)
	assert.True(t, handler2.called)
}

// --- method table --------------------------------------------------------

func TestServiceMethodDecision(t *testing.T) {
	cases := []struct {
		method      string
		wantDeny    bool
		description string
	}{
		{"/pb.MetaService/GetFormat", false, "GetFormat is the only admin method allowed for service"},
		{"/pb.MetaService/DumpMeta", true, "streaming admin"},
		{"/pb.MetaService/LoadMeta", true, "streaming admin"},
		{"/pb.MetaService/DumpMetaV2", true, "streaming admin"},
		{"/pb.MetaService/LoadMetaV2", true, "streaming admin"},
		{"/pb.MetaService/ScanSlices", true, "streaming admin"},
		{"/pb.MetaService/ScanDeletedObject", true, "streaming admin"},
		{"/pb.MetaService/ScanChangelog", true, "streaming admin"},
		{"/pb.MetaService/Remove", true, "admin unary"},
		{"/pb.MetaService/BatchUnlink", true, "admin unary"},
		{"/pb.MetaService/GetSummary", true, "admin unary"},
		{"/pb.MetaService/GetTreeSummary", true, "admin unary"},
		{"/pb.MetaService/Clone", true, "admin unary"},
		{"/pb.MetaService/GetPaths", true, "admin unary"},
		{"/pb.MetaService/Check", true, "admin unary"},
		{"/pb.MetaService/CompactAll", true, "admin unary"},
		{"/pb.MetaService/Compact", true, "admin unary"},
		{"/pb.MetaService/ListSlices", true, "admin unary"},
		{"/pb.MetaService/CleanupSlices", true, "admin unary"},
		{"/pb.MetaService/HandleQuota", true, "admin unary"},
		{"/pb.MetaService/ScanUserGroupUsage", true, "admin unary"},
		{"/pb.MetaService/CleanupTrashBefore", true, "admin unary"},
		{"/pb.MetaService/CleanupDetachedNodesBefore", true, "admin unary"},
		{"/pb.MetaService/RotateFileKey", true, "admin unary"},
		{"/pb.MetaService/RotateFileKeysByPaths", true, "admin unary"},
		{"/pb.MetaService/StoreToken", true, "token admin"},
		{"/pb.MetaService/UpdateToken", true, "token admin"},
		{"/pb.MetaService/LoadToken", true, "token admin"},
		{"/pb.MetaService/DeleteTokens", true, "token admin"},
		{"/pb.MetaService/ListTokens", true, "token admin"},
		{"/pb.MetaService/Init", false, "lifecycle"},
		{"/pb.MetaService/Load", false, "lifecycle"},
		{"/pb.MetaService/NewSession", false, "lifecycle"},
		{"/pb.MetaService/CloseSession", false, "lifecycle"},
		{"/pb.MetaService/Chroot", false, "session-scoped restriction, not an escalation"},
		{"/pb.MetaService/Lookup", false, "file RPC"},
		{"/pb.MetaService/GetAttr", false, "file RPC"},
		{"/pb.MetaService/Open", false, "file RPC"},
		{"/pb.MetaService/GetSTSCredentials", false, "sts pass-through (handler enforces its own subject)"},
	}
	for _, tc := range cases {
		decision := serviceMethodDecision(tc.method)
		if tc.wantDeny {
			assert.Equal(t, serviceDecisionDeny, decision, "%s (%s)", tc.method, tc.description)
		} else {
			assert.Equal(t, serviceDecisionPass, decision, "%s (%s)", tc.method, tc.description)
		}
	}
}

// --- config loader -------------------------------------------------------

func TestNewServiceIdentityConfig(t *testing.T) {
	t.Run("disabled when no flags", func(t *testing.T) {
		cfg, err := NewServiceIdentityConfig("", "")
		require.NoError(t, err)
		assert.Nil(t, cfg)
	})
	t.Run("node credential without hash is an error", func(t *testing.T) {
		cfg, err := NewServiceIdentityConfig("", "env:SOME_VAR")
		require.Error(t, err)
		assert.Nil(t, cfg)
	})
	t.Run("short hash is an error", func(t *testing.T) {
		cfg, err := NewServiceIdentityConfig("abcd", "")
		require.Error(t, err)
		assert.Nil(t, cfg)
	})
	t.Run("non-hex hash is an error", func(t *testing.T) {
		bad := make([]byte, sha256.Size)
		for i := range bad {
			bad[i] = 'z'
		}
		cfg, err := NewServiceIdentityConfig(string(bad), "")
		require.Error(t, err)
		assert.Nil(t, cfg)
	})
	t.Run("valid hash from file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "node-token")
		require.NoError(t, os.WriteFile(path, []byte("  raw-node-token\n"), 0600))
		cfg, err := NewServiceIdentityConfig(testServiceCredentialHashHex(t), path)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "raw-node-token", cfg.NodeCredential())
		assert.Len(t, cfg.credentialHash, sha256.Size)
	})
	t.Run("valid hash from env", func(t *testing.T) {
		t.Setenv("TEST_NODE_CREDENTIAL", "env-token\n")
		cfg, err := NewServiceIdentityConfig(testServiceCredentialHashHex(t), "env:TEST_NODE_CREDENTIAL")
		require.NoError(t, err)
		assert.Equal(t, "env-token", cfg.NodeCredential())
	})
	t.Run("missing env var is an error", func(t *testing.T) {
		cfg, err := NewServiceIdentityConfig(testServiceCredentialHashHex(t), "env:TEST_NODE_CREDENTIAL_UNSET")
		require.Error(t, err)
		assert.Nil(t, cfg)
	})
	t.Run("missing file is an error", func(t *testing.T) {
		cfg, err := NewServiceIdentityConfig(testServiceCredentialHashHex(t), filepath.Join(t.TempDir(), "absent"))
		require.Error(t, err)
		assert.Nil(t, cfg)
	})
}

// --- identity-dependent consumers ----------------------------------------

// TestExtractUserIDFromOIDCServiceKind: service identity has no user id —
// empty without error so call sites proceed unchanged (design D4). The OIDC
// path is untouched.
func TestExtractUserIDFromOIDCServiceKind(t *testing.T) {
	sub := uuid.NewString()
	oidcCtx := oidc.WithIDToken(context.Background(), &oidc.IDToken{Subject: sub})
	got, err := extractUserIDFromOIDC(oidcCtx)
	require.NoError(t, err)
	assert.Equal(t, sub, got, "oidc path unchanged")

	svcCtx := oidc.WithIdentityKind(oidcCtx, oidc.IdentityKindService)
	got, err = extractUserIDFromOIDC(svcCtx)
	require.NoError(t, err, "service kind must not fail the extract")
	assert.Empty(t, got, "service identity carries no user id")

	_, err = extractUserIDFromOIDC(context.Background())
	require.Error(t, err, "no claims → error (unchanged)")
}

// TestAuthzInterceptorSkipsService: a service-kind request passes the authz
// interceptor without a single authz client call; oidc-kind requests keep the
// normal path.
func TestAuthzInterceptorSkipsService(t *testing.T) {
	client := &mockAuthzClient{}
	client.On("CheckPermission", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(false, nil).Maybe()
	cache := NewInodePathCache(10)
	ai := NewAuthzInterceptor(client, cache, nil)

	svcCtx := oidc.WithIdentityKind(context.Background(), oidc.IdentityKindService)
	handlerCalled := false
	_, err := ai.UnaryInterceptor()(svcCtx, &pb.GetAttrRequest{Inode: 7},
		&grpc.UnaryServerInfo{FullMethod: "/pb.MetaService/GetAttr"},
		func(ctx context.Context, req interface{}) (interface{}, error) {
			handlerCalled = true
			return map[string]interface{}{"ok": true}, nil
		})
	require.NoError(t, err)
	assert.True(t, handlerCalled, "service request reaches the handler")
	client.AssertNotCalled(t, "CheckPermission", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	client.AssertNotCalled(t, "CheckOrganizationAdmin", mock.Anything, mock.Anything)
}

// TestFilterEntriesByAuthzSkipsService: the in-handler Readdir/DirHandlerList
// authz filter returns all entries for service-kind requests without authz
// calls (spec "Service request skips per-user authz").
func TestFilterEntriesByAuthzSkipsService(t *testing.T) {
	client := &mockAuthzClient{}
	client.On("CheckPermission", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(false, nil).Maybe()
	client.On("CheckBulkPermissions", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return([]bool{false}, nil).Maybe()

	server := NewMetaProxyServer(&fekFakeMeta{format: &Format{}}, 10)
	ai := NewAuthzInterceptor(client, server.InodePathCache(), nil)
	server.SetAuthzInterceptor(ai)

	entries := []*Entry{
		{Name: []byte("a"), Inode: 2},
		{Name: []byte("b"), Inode: 3},
	}
	svcCtx := oidc.WithIdentityKind(context.Background(), oidc.IdentityKindService)
	got, errno := server.filterEntriesByAuthz(svcCtx, 1, entries)
	require.Equal(t, 0, int(errno))
	assert.Len(t, got, len(entries), "service listing is unfiltered")
	client.AssertNotCalled(t, "CheckPermission", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	client.AssertNotCalled(t, "CheckBulkPermissions", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
