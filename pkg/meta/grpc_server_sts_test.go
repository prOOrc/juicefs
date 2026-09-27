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

	"github.com/google/uuid"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestStsProxyPassThrough (task 7.12, ADR-003): the proxy must fill user_id in
// the KeyManager request from the authenticated OIDC session — the request
// message carries no identity field, so a client can never name one — and map
// the platform response fields verbatim.
func TestStsProxyPassThrough(t *testing.T) {
	km := newFakeKeyManager()
	s := NewMetaProxyServer(nil, 0)
	s.SetKeyManager(km)

	sub := uuid.New().String()
	ctx := oidc.WithIDToken(context.Background(), &oidc.IDToken{Subject: sub})
	resp, err := s.GetSTSCredentials(ctx, &pb.StsCredentialsRequest{CompanyId: testCompanyID})
	require.NoError(t, err)
	require.Equal(t, sub, km.lastSTSUser, "user_id must be the session subject, nothing else")
	require.Equal(t, "STS-FAKE", resp.GetAccessKeyId())
	require.Equal(t, "fake-secret", resp.GetSecretAccessKey())
	require.Equal(t, "fake-token", resp.GetSessionToken())
	require.NotZero(t, resp.GetExpirationUnix())
}

// TestStsProxyRequiresSession: without an authenticated OIDC session the proxy
// must fail closed — no subject, no credentials (FR-REV-3).
func TestStsProxyRequiresSession(t *testing.T) {
	s := NewMetaProxyServer(nil, 0)
	s.SetKeyManager(newFakeKeyManager())

	t.Run("no oidc claims", func(t *testing.T) {
		_, err := s.GetSTSCredentials(context.Background(), &pb.StsCredentialsRequest{CompanyId: testCompanyID})
		require.Error(t, err)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("non-uuid subject", func(t *testing.T) {
		ctx := oidc.WithIDToken(context.Background(), &oidc.IDToken{Subject: "not-a-uuid"})
		_, err := s.GetSTSCredentials(ctx, &pb.StsCredentialsRequest{CompanyId: testCompanyID})
		require.Error(t, err)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})
}

// TestStsProxyRequiresKeyManager: the pass-through is served only when the
// proxy has a KeyManager configured (--keymanager-service).
func TestStsProxyRequiresKeyManager(t *testing.T) {
	s := NewMetaProxyServer(nil, 0) // no SetKeyManager
	sub := uuid.New().String()
	ctx := oidc.WithIDToken(context.Background(), &oidc.IDToken{Subject: sub})
	_, err := s.GetSTSCredentials(ctx, &pb.StsCredentialsRequest{CompanyId: testCompanyID})
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}
