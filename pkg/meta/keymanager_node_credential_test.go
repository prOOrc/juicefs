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
	"net"
	"sync"
	"testing"

	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// fakePlatformKeyManager is an in-process DriveKeyManagerService (the
// "platform") that records the x-node-credential metadata of every request,
// so the decorator's header routing is asserted against what the platform
// would actually receive on the wire.
type fakePlatformKeyManager struct {
	kmpb.UnimplementedDriveKeyManagerServiceServer

	mu     sync.Mutex
	seenMD map[string]metadata.MD
}

func (s *fakePlatformKeyManager) record(method string, ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenMD == nil {
		s.seenMD = make(map[string]metadata.MD)
	}
	s.seenMD[method] = md
}

func (s *fakePlatformKeyManager) nodeValues(method string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	md := s.seenMD[method]
	if md == nil {
		return nil
	}
	return md.Get(MetadataNodeCredential)
}

func (s *fakePlatformKeyManager) CreateFileKey(ctx context.Context, req *kmpb.CreateFileKeyRequest) (*kmpb.CreateFileKeyResponse, error) {
	s.record("CreateFileKey", ctx)
	return &kmpb.CreateFileKeyResponse{}, nil
}

func (s *fakePlatformKeyManager) GetFileFEK(ctx context.Context, req *kmpb.GetFileFEKRequest) (*kmpb.GetFileFEKResponse, error) {
	s.record("GetFileFEK", ctx)
	return &kmpb.GetFileFEKResponse{}, nil
}

func (s *fakePlatformKeyManager) RotateFileFEK(ctx context.Context, req *kmpb.RotateFileFEKRequest) (*kmpb.RotateFileFEKResponse, error) {
	s.record("RotateFileFEK", ctx)
	return &kmpb.RotateFileFEKResponse{}, nil
}

func (s *fakePlatformKeyManager) FetchCompanyKEK(ctx context.Context, req *kmpb.FetchCompanyKEKRequest) (*kmpb.FetchCompanyKEKResponse, error) {
	s.record("FetchCompanyKEK", ctx)
	return &kmpb.FetchCompanyKEKResponse{}, nil
}

func (s *fakePlatformKeyManager) GetSTSCredentials(ctx context.Context, req *kmpb.GetSTSCredentialsRequest) (*kmpb.GetSTSCredentialsResponse, error) {
	s.record("GetSTSCredentials", ctx)
	return &kmpb.GetSTSCredentialsResponse{}, nil
}

// newFakePlatformKeyManagerClient serves the fake platform over bufconn and
// returns a KeyManagerClient bound to it (same wiring as the production
// platformKeyManager, without TLS).
func newFakePlatformKeyManagerClient(t *testing.T, srv *fakePlatformKeyManager) KeyManagerClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	grpcSrv := grpc.NewServer()
	kmpb.RegisterDriveKeyManagerServiceServer(grpcSrv, srv)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	conn, err := grpc.NewClient("bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &platformKeyManager{
		client: kmpb.NewDriveKeyManagerServiceClient(conn),
		conn:   conn,
	}
}

func TestNodeCredentialRoutingMatrix(t *testing.T) {
	platform := &fakePlatformKeyManager{}
	km := WithNodeCredentialRouting(newFakePlatformKeyManagerClient(t, platform), "raw-node-token")

	svcCtx := oidc.WithIdentityKind(context.Background(), oidc.IdentityKindService)
	oidcCtx := oidc.WithIdentityKind(context.Background(), oidc.IdentityKindOIDCUser)

	type call func(ctx context.Context) error
	fekCalls := map[string]call{
		"CreateFileKey": func(ctx context.Context) error {
			_, err := km.CreateFileKey(ctx, &kmpb.CreateFileKeyRequest{})
			return err
		},
		"GetFileFEK": func(ctx context.Context) error {
			_, err := km.GetFileFEK(ctx, &kmpb.GetFileFEKRequest{})
			return err
		},
		"RotateFileFEK": func(ctx context.Context) error {
			_, err := km.RotateFileFEK(ctx, &kmpb.RotateFileFEKRequest{})
			return err
		},
	}

	// FEK methods × service-kind → node credential present on the wire.
	for name, invoke := range fekCalls {
		t.Run("service "+name+" carries node credential", func(t *testing.T) {
			require.NoError(t, invoke(svcCtx))
			assert.Equal(t, []string{"raw-node-token"}, platform.nodeValues(name))
		})
	}

	// FEK methods × oidc-kind / no identity → no metadata (byte-identical).
	for name, invoke := range fekCalls {
		t.Run("oidc "+name+" carries no node credential", func(t *testing.T) {
			require.NoError(t, invoke(oidcCtx))
			assert.Empty(t, platform.nodeValues(name))
		})
		t.Run("background "+name+" carries no node credential", func(t *testing.T) {
			require.NoError(t, invoke(context.Background()))
			assert.Empty(t, platform.nodeValues(name))
		})
	}

	// Non-FEK methods × service-kind → no metadata.
	t.Run("service FetchCompanyKEK carries no node credential", func(t *testing.T) {
		_, err := km.FetchCompanyKEK(svcCtx, &kmpb.FetchCompanyKEKRequest{})
		require.NoError(t, err)
		assert.Empty(t, platform.nodeValues("FetchCompanyKEK"))
	})
	t.Run("service GetSTSCredentials carries no node credential", func(t *testing.T) {
		_, err := km.GetSTSCredentials(svcCtx, &kmpb.GetSTSCredentialsRequest{})
		require.NoError(t, err)
		assert.Empty(t, platform.nodeValues("GetSTSCredentials"))
	})
}

// stubKeyManager records whether the decorator passed the ctx through — used
// for the no-op wiring cases.
type stubKeyManager struct {
	KeyManagerClient
}

func TestWithNodeCredentialRoutingNoop(t *testing.T) {
	km := &stubKeyManager{}
	assert.Nil(t, WithNodeCredentialRouting(nil, "raw-node-token"))
	assert.Same(t, km, WithNodeCredentialRouting(km, ""))
}
