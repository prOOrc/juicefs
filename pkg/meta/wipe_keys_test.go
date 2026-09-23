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
	"bytes"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/stretchr/testify/require"
)

// TestWipeKeys verifies that WipeKeys zeroes all plaintext key material and
// empties the FEK cache (task 6.1, NFR-SEC-3).
func TestWipeKeys(t *testing.T) {
	t.Run("grpcMeta", func(t *testing.T) {
		m := &grpcMeta{
			fekCache: expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		}
		fek := bytes.Repeat([]byte{0xAB}, 32)
		m.fekCache.Add(1, &fekEntry{fek: fek, version: 1})

		m.WipeKeys()

		require.Equal(t, make([]byte, 32), fek, "FEK backing array must be zeroed")
		require.Zero(t, m.fekCache.Len(), "FEK cache must be empty after WipeKeys")
	})

	t.Run("renderMeta", func(t *testing.T) {
		rm := &RenderMeta{
			kek:      bytes.Repeat([]byte{0xCD}, 32),
			fekCache: expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		}
		fek := bytes.Repeat([]byte{0xEF}, 32)
		rm.fekCache.Add(7, &fekEntry{fek: fek, version: 2})

		rm.WipeKeys()

		require.Equal(t, make([]byte, 32), rm.kek, "KEK must be zeroed")
		require.Equal(t, make([]byte, 32), fek, "FEK backing array must be zeroed")
		require.Zero(t, rm.fekCache.Len(), "FEK cache must be empty after WipeKeys")
	})

	t.Run("nilFekCache", func(t *testing.T) {
		m := &grpcMeta{}
		require.NotPanics(t, m.WipeKeys)
	})
}
