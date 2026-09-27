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

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"google.golang.org/grpc"
)

// StsProxyClient returns a client for the meta proxy's StsProxyService
// (task 7.12, ADR-003): the user-path pass-through for short-lived storage
// credentials. It shares the meta client's gRPC connection, and every call is
// wrapped with withAuth so the request carries the same OIDC session (and hub
// fail-fast behaviour) as regular meta RPCs — the proxy derives the identity
// from it. Only available on gRPC (proxy) meta clients.
func (m *grpcMeta) StsProxyClient() pb.StsProxyServiceClient {
	return &stsProxyClient{m: m}
}

// stsProxyClient binds the generated StsProxyServiceClient to the meta client's
// authenticated context.
type stsProxyClient struct {
	m *grpcMeta
}

func (c *stsProxyClient) GetSTSCredentials(ctx context.Context, in *pb.StsCredentialsRequest, opts ...grpc.CallOption) (*pb.StsCredentialsResponse, error) {
	return pb.NewStsProxyServiceClient(c.m.conn).GetSTSCredentials(c.m.withAuth(ctx), in, opts...)
}
