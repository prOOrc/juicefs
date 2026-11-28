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
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net"
	"sync"
	"syscall"
	"testing"

	"github.com/google/uuid"
	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// End-to-end service-identity flow over a real gRPC stack (TLS bufconn):
// client → service interceptor → handler → keymanager, asserting the request
// content the platform would see (user_id empty for service, x-node-credential
// only on the service-originated call — tasks 2.1/2.2 at the call-site level).

// userRecordingKeyManager records the identity fields of GetFileFEK calls
// before delegating the actual unwrap to the production-shape fake.
type userRecordingKeyManager struct {
	*fakeKeyManager

	mu           sync.Mutex
	lastUserID   string
	lastNodeCred []string
}

func (r *userRecordingKeyManager) GetFileFEK(ctx context.Context, req *kmpb.GetFileFEKRequest) (*kmpb.GetFileFEKResponse, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	r.mu.Lock()
	r.lastUserID = req.UserId
	r.lastNodeCred = md.Get(MetadataNodeCredential)
	r.mu.Unlock()
	return r.fakeKeyManager.GetFileFEK(ctx, req)
}

func (r *userRecordingKeyManager) snapshot() (string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastUserID, r.lastNodeCred
}

// newServiceIdentityEnv serves an encrypted volume over TLS bufconn with the
// production interceptor order (service identity → simulated OIDC identity)
// and a node-credential-routing keymanager; returns the recording client view.
func newServiceIdentityEnv(t *testing.T) (pb.MetaServiceClient, *userRecordingKeyManager) {
	t.Helper()
	format := &Format{Name: "service-identity-test", UUID: uuid.New().String(), EncryptionEnabled: true}
	fake := newFakeKeyManager()
	fek := randomBytes(t, fekSize)
	wrapped, err := WrapFEK(fake.kek, fek, FekAAD{
		VolumeUUID: format.UUID, CompanyID: testCompanyID, DriveFileID: "dfid-svc-e2e", Inode: testFekInode, FekVersion: 1,
	}, 1)
	require.NoError(t, err)

	srv := NewMetaProxyServer(&fekFakeMeta{
		attr: &Attr{
			Typ:         TypeFile,
			Encrypted:   true,
			DriveFileID: "dfid-svc-e2e",
			FekVersion:  1,
			WrappedFek:  wrapped,
		},
		format: format,
	}, 100)
	recording := &userRecordingKeyManager{fakeKeyManager: fake}
	srv.SetKeyManager(WithNodeCredentialRouting(recording, "node-token-e2e"))
	srv.InodePathCache().Set(testFekInode, "/secret.exr")
	srv.SetVolumeName(format.Name)

	t.Setenv("TEST_NODE_CREDENTIAL", "node-token-e2e")
	cfg, err := NewServiceIdentityConfig(
		func() string {
			sum := sha256.Sum256([]byte(testServiceCredential))
			return hex.EncodeToString(sum[:])
		}(), "env:TEST_NODE_CREDENTIAL")
	require.NoError(t, err)
	require.NotNil(t, cfg)

	cert := generateTestTLSCert(t)
	lis := bufconn.Listen(1 << 20)
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{cert.cert},
		MinVersion:   tls.VersionTLS12,
	}
	clientTLS := func() *tls.Config {
		pool := x509.NewCertPool()
		pool.AddCert(cert.leaf)
		return &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	}()
	opts := []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(serverTLS)),
		grpc.ChainUnaryInterceptor(
			NewServiceIdentityInterceptor(cfg).UnaryInterceptor(),
			testIdentityInterceptor(),
		),
		grpc.ChainStreamInterceptor(
			NewServiceIdentityInterceptor(cfg).StreamInterceptor(),
		),
	}
	grpcSrv := grpc.NewServer(opts...)
	pb.RegisterMetaServiceServer(grpcSrv, srv)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	cc, err := grpc.NewClient("bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cc.Close() })
	return pb.NewMetaServiceClient(cc), recording
}

// TestServiceIdentityEndToEnd: a service-authenticated Open delivers the FEK,
// the keymanager request carries an EMPTY user_id and the node credential;
// the oidc-user Open on the same stack carries the session subject and no
// node credential (specs "Node credential attached only for service-originated
// calls" + design D4).
func TestServiceIdentityEndToEnd(t *testing.T) {
	client, recording := newServiceIdentityEnv(t)

	t.Run("service open: empty user_id, node credential attached", func(t *testing.T) {
		ctx := metadata.AppendToOutgoingContext(context.Background(),
			MetadataServiceCredential, testServiceCredential)
		resp, err := client.Open(ctx, &pb.OpenRequest{
			Inode: testFekInode, Flags: uint32(syscall.O_RDONLY),
		})
		require.NoError(t, err)
		require.EqualValues(t, 0, resp.Errno)

		userID, nodeCred := recording.snapshot()
		assert.Empty(t, userID, "service-kind keymanager request must carry no user_id")
		assert.Equal(t, []string{"node-token-e2e"}, nodeCred,
			"service-originated GetFileFEK must carry the node credential")
	})

	t.Run("oidc open: session subject, no node credential", func(t *testing.T) {
		ctx := metadata.AppendToOutgoingContext(context.Background(),
			testUserMetadataKey, uuid.NewString())
		resp, err := client.Open(ctx, &pb.OpenRequest{
			Inode: testFekInode, Flags: uint32(syscall.O_RDONLY),
		})
		require.NoError(t, err)
		require.EqualValues(t, 0, resp.Errno)

		userID, nodeCred := recording.snapshot()
		assert.NotEmpty(t, userID, "oidc-kind keymanager request carries the session subject")
		assert.Empty(t, nodeCred, "oidc-originated GetFileFEK must not carry the node credential")
	})
}

// TestServiceIdentityAdminOverWire: DumpMeta through the real gRPC stack is
// denied for a service-authenticated client (the streaming authz gap stays
// closed for service), while the same call from an oidc session passes the
// interceptor layer (its authz coverage is out of this change's scope).
func TestServiceIdentityAdminOverWire(t *testing.T) {
	client, _ := newServiceIdentityEnv(t)

	t.Run("service DumpMeta denied", func(t *testing.T) {
		ctx := metadata.AppendToOutgoingContext(context.Background(),
			MetadataServiceCredential, testServiceCredential)
		stream, err := client.DumpMeta(ctx, &pb.DumpMetaRequest{})
		require.NoError(t, err)
		_, err = stream.Recv()
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
	})

	t.Run("wrong credential DumpMeta unauthenticated", func(t *testing.T) {
		ctx := metadata.AppendToOutgoingContext(context.Background(),
			MetadataServiceCredential, "not-the-credential")
		stream, err := client.DumpMeta(ctx, &pb.DumpMetaRequest{})
		require.NoError(t, err)
		_, err = stream.Recv()
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("both credentials DumpMeta unauthenticated", func(t *testing.T) {
		ctx := metadata.AppendToOutgoingContext(context.Background(),
			MetadataServiceCredential, testServiceCredential,
			"authorization", "Bearer some-token")
		stream, err := client.DumpMeta(ctx, &pb.DumpMetaRequest{})
		require.NoError(t, err)
		_, err = stream.Recv()
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
	})
}
