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

	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetSTSCredentials implements StsProxyService (task 7.12, ADR-003): a
// pass-through to the platform KeyManager's GetSTSCredentials. The subject is
// taken exclusively from the authenticated OIDC session — the same
// extractUserIDFromOIDC the authz interceptor relies on — so a caller can never
// obtain another user's credentials (the request message carries no identity).
// Authorization (Read on the company) is enforced by the platform; the proxy
// only authenticates.
func (s *MetaProxyServer) GetSTSCredentials(ctx context.Context, req *pb.StsCredentialsRequest) (*pb.StsCredentialsResponse, error) {
	subject, err := extractUserIDFromOIDC(ctx)
	if err != nil || subject == "" {
		return nil, status.Error(codes.Unauthenticated, "no authenticated user")
	}
	if s.keyManager == nil {
		return nil, status.Error(codes.FailedPrecondition, "sts: keymanager is not configured on the proxy")
	}
	resp, err := s.keyManager.GetSTSCredentials(ctx, &kmpb.GetSTSCredentialsRequest{
		UserId:    subject,
		CompanyId: req.GetCompanyId(),
	})
	if err != nil {
		return nil, err
	}
	return &pb.StsCredentialsResponse{
		AccessKeyId:     resp.GetAccessKeyId(),
		SecretAccessKey: resp.GetSecretAccessKey(),
		SessionToken:    resp.GetSessionToken(),
		ExpirationUnix:  resp.GetExpirationUnix(),
	}, nil
}
