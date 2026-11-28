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
	"fmt"

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// ProbeGetFormat performs the GetFormat RPC against the meta proxy, carrying
// the client identity metadata (service credential in gateway platform mode,
// OIDC bearer otherwise). It is the cheap startup probe of the gateway
// platform mode (add-gateway-platform-mode): success proves that the proxy
// accepted the connection, the credential and the volume before any S3
// surface is opened — GetFormat is the only admin method a service identity
// is allowed to call (serviceMethodDecision).
func (m *grpcMeta) ProbeGetFormat(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := m.client.GetFormat(m.withAuth(ctx), &pb.GetFormatRequest{})
	if err != nil {
		return fmt.Errorf("GetFormat probe: %w", err)
	}
	if eno := resp.GetErrno(); eno != 0 {
		return fmt.Errorf("GetFormat probe: errno %d", eno)
	}
	return nil
}
