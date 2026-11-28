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

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMetaProxyServiceIdentityFlags (add-meta-proxy-service-identity, task
// 2.3): the meta-proxy command registers the service identity flags; their
// values are consumed only via meta.NewServiceIdentityConfig and are never
// logged (log statements audited by grep — no loggerProxy call references
// either flag value).
func TestMetaProxyServiceIdentityFlags(t *testing.T) {
	cmd := cmdMetaProxy()
	require.NotNil(t, cmd)

	names := make(map[string]bool)
	for _, f := range cmd.Flags {
		names[f.Names()[0]] = true
	}
	assert.True(t, names["service-credential-hash"], "--service-credential-hash must be registered")
	assert.True(t, names["node-credential"], "--node-credential must be registered")
}
