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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

// TestWithAuthServiceCredential (add-gateway-platform-mode): with a service
// credential configured, every outgoing call carries x-service-credential
// instead of an OIDC authorization header; the session id is kept.
func TestWithAuthServiceCredential(t *testing.T) {
	m := &grpcMeta{sid: 42, serviceCredential: "gateway-service-token"}
	ctx := m.withAuth(context.Background())
	md, ok := metadata.FromOutgoingContext(ctx)
	require.True(t, ok)
	assert.Equal(t, []string{"gateway-service-token"}, md.Get(MetadataServiceCredential))
	assert.Equal(t, []string{"42"}, md.Get("x-session-id"))
	assert.Empty(t, md.Get("authorization"), "service identity must not send an OIDC authorization header")
}

// TestWithAuthServiceCredentialExcludesOIDC: if both are configured (a
// misconfiguration the proxy would reject at the door), the service
// credential wins and the OIDC token is not attached.
func TestWithAuthServiceCredentialExcludesOIDC(t *testing.T) {
	m := &grpcMeta{
		sid:               1,
		serviceCredential: "gateway-service-token",
		tokenManager:      &mockTokenProvider{token: "oidc-token"},
	}
	ctx := m.withAuth(context.Background())
	md, ok := metadata.FromOutgoingContext(ctx)
	require.True(t, ok)
	assert.Equal(t, []string{"gateway-service-token"}, md.Get(MetadataServiceCredential))
	assert.Empty(t, md.Get("authorization"))
}

// TestWithAuthWithoutServiceCredential: without a service credential the
// metadata is byte-identical to the legacy path (session id only, no
// x-service-credential).
func TestWithAuthWithoutServiceCredential(t *testing.T) {
	m := &grpcMeta{sid: 7}
	ctx := m.withAuth(context.Background())
	md, ok := metadata.FromOutgoingContext(ctx)
	require.True(t, ok)
	assert.Empty(t, md.Get(MetadataServiceCredential))
	assert.Equal(t, []string{"7"}, md.Get("x-session-id"))
}

// TestNewGRPCMetaServiceCredentialEnv: the meta URL names the env variable
// holding the service token; the token is read from the environment and never
// stored in (or logged from) the URL. Missing env fails construction.
// newGRPCMeta is called directly: NewClient Fatals on construction errors and
// is therefore unusable for the error cases.
func TestNewGRPCMetaServiceCredentialEnv(t *testing.T) {
	t.Run("token from env", func(t *testing.T) {
		t.Setenv("TEST_GW_SERVICE_TOKEN", "gateway-service-token")
		m, err := newGRPCMeta("grpc", "127.0.0.1:1?service-credential-env=TEST_GW_SERVICE_TOKEN", nil)
		require.NoError(t, err)
		gm, ok := m.(*grpcMeta)
		require.True(t, ok)
		assert.Equal(t, "gateway-service-token", gm.serviceCredential)
	})

	t.Run("missing env fails fast", func(t *testing.T) {
		t.Setenv("TEST_GW_SERVICE_TOKEN", "")
		_, err := newGRPCMeta("grpc", "127.0.0.1:1?service-credential-env=TEST_GW_SERVICE_TOKEN", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TEST_GW_SERVICE_TOKEN")
	})

	t.Run("cannot combine with oidc", func(t *testing.T) {
		t.Setenv("TEST_GW_SERVICE_TOKEN", "gateway-service-token")
		_, err := newGRPCMeta("grpc", "127.0.0.1:1?service-credential-env=TEST_GW_SERVICE_TOKEN&oidc-issuer=https://idp&oidc-client-id=abc", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "service-credential-env")
	})
}
