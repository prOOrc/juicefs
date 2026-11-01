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

// Unit tests for platformKeyManager over a real gRPC transport (bufconn): the
// generated client carries variadic call options, so the adapter is exercised
// through the wire, not against an in-process stub (task 2.1,
// add-render-selectel-company-keys).

import (
	"context"
	"net"
	"testing"
	"time"

	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// nodeCredentialCapturingServer answers GetNodeStorageCredentials with a fixed
// response/error and records the incoming node credential metadata.
type nodeCredentialCapturingServer struct {
	kmpb.UnimplementedDriveKeyManagerServiceServer
	resp       *kmpb.GetNodeStorageCredentialsResponse
	err        error
	gotCred    string
	gotCompany string
	gotNodeID  string
}

func (s *nodeCredentialCapturingServer) GetNodeStorageCredentials(ctx context.Context, req *kmpb.GetNodeStorageCredentialsRequest) (*kmpb.GetNodeStorageCredentialsResponse, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(NodeCredentialMetadataKey); len(vals) > 0 {
			s.gotCred = vals[0]
		}
	}
	s.gotCompany = req.CompanyId
	s.gotNodeID = req.NodeId
	return s.resp, s.err
}

// newBufconnKeyManager dials the given server over bufconn and returns the
// production platformKeyManager adapter.
func newBufconnKeyManager(t *testing.T, srv kmpb.DriveKeyManagerServiceServer) *platformKeyManager {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = lis.Close() })
	gs := grpc.NewServer()
	kmpb.RegisterDriveKeyManagerServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	dialer := func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(dialer), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &platformKeyManager{client: kmpb.NewDriveKeyManagerServiceClient(conn), conn: conn}
}

// TestKeyManagerClientGetNodeStorageCredentials pins the client-side contract of
// the new RPC: CompanyId/NodeId are forwarded, the response is mapped 1:1, the
// node credential travels as raw NodeCredentialMetadataKey metadata, and a
// platform Unauthenticated (unknown/revoked credential) surfaces unchanged.
func TestKeyManagerClientGetNodeStorageCredentials(t *testing.T) {
	cases := []struct {
		name    string
		srv     *nodeCredentialCapturingServer
		wantErr codes.Code
	}{
		{"ok", &nodeCredentialCapturingServer{resp: &kmpb.GetNodeStorageCredentialsResponse{
			AccessKeyId:     "AK-SEL",
			SecretAccessKey: "SK-SEL",
			VolumeName:      "vol-sel",
			Bucket:          "bucket-sel",
		}}, codes.OK},
		{"revoked credential", &nodeCredentialCapturingServer{
			err: status.Error(codes.Unauthenticated, "invalid or revoked node credential"),
		}, codes.Unauthenticated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			km := newBufconnKeyManager(t, c.srv)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx = metadata.AppendToOutgoingContext(ctx, NodeCredentialMetadataKey, "test-node-token")

			resp, err := km.GetNodeStorageCredentials(ctx, &kmpb.GetNodeStorageCredentialsRequest{
				CompanyId: testCompanyID,
				NodeId:    "render-node-1",
			})
			require.Equal(t, testCompanyID, c.srv.gotCompany, "CompanyId must be forwarded")
			require.Equal(t, "render-node-1", c.srv.gotNodeID, "NodeId must be forwarded")
			require.Equal(t, "test-node-token", c.srv.gotCred, "the raw node credential must travel in %s metadata", NodeCredentialMetadataKey)
			if c.wantErr != codes.OK {
				require.Error(t, err)
				require.Equal(t, c.wantErr, status.Code(err), "the gRPC status class must survive the client adapter")
				require.Nil(t, resp)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "AK-SEL", resp.GetAccessKeyId())
			require.Equal(t, "SK-SEL", resp.GetSecretAccessKey())
			require.Equal(t, "vol-sel", resp.GetVolumeName())
			require.Equal(t, "bucket-sel", resp.GetBucket())
		})
	}
}
