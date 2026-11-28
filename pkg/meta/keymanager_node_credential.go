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
	"github.com/juicedata/juicefs/pkg/oidc"
	"google.golang.org/grpc/metadata"
)

// nodeCredentialKeyManager decorates a KeyManagerClient so that keymanager
// calls originated by service-identity requests carry the proxy's node
// credential in the outgoing x-node-credential metadata
// (add-meta-proxy-service-identity, design D2): the platform resolves them
// through its node branch instead of the per-user path. Calls from
// oidc-user requests (and identity-less background calls) go out unchanged —
// the platform never sees the node credential for them.
type nodeCredentialKeyManager struct {
	KeyManagerClient
	nodeCredential string
}

// WithNodeCredentialRouting wraps km with node-credential routing for
// service-originated FEK calls. A nil client or empty credential returns km
// unchanged (without a configured credential, service connections are already
// rejected at the door by the service identity interceptor).
func WithNodeCredentialRouting(km KeyManagerClient, nodeCredential string) KeyManagerClient {
	if km == nil || nodeCredential == "" {
		return km
	}
	return &nodeCredentialKeyManager{KeyManagerClient: km, nodeCredential: nodeCredential}
}

// withNodeCredential returns ctx enriched with the outgoing node credential
// when it belongs to a service-identity request; any other context goes out
// byte-identical (fail-closed on the platform: no metadata, no node branch).
func (k *nodeCredentialKeyManager) withNodeCredential(ctx context.Context) context.Context {
	if oidc.IsServiceIdentity(ctx) {
		return metadata.AppendToOutgoingContext(ctx, MetadataNodeCredential, k.nodeCredential)
	}
	return ctx
}

func (k *nodeCredentialKeyManager) CreateFileKey(ctx context.Context, req *kmpb.CreateFileKeyRequest) (*kmpb.CreateFileKeyResponse, error) {
	return k.KeyManagerClient.CreateFileKey(k.withNodeCredential(ctx), req)
}

func (k *nodeCredentialKeyManager) GetFileFEK(ctx context.Context, req *kmpb.GetFileFEKRequest) (*kmpb.GetFileFEKResponse, error) {
	return k.KeyManagerClient.GetFileFEK(k.withNodeCredential(ctx), req)
}

func (k *nodeCredentialKeyManager) RotateFileFEK(ctx context.Context, req *kmpb.RotateFileFEKRequest) (*kmpb.RotateFileFEKResponse, error) {
	return k.KeyManagerClient.RotateFileFEK(k.withNodeCredential(ctx), req)
}
