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
	"strings"
	"testing"
)

func TestNewMetaClient(t *testing.T) {
	t.Run("unknown driver returns error without exiting", func(t *testing.T) {
		m, err := NewMetaClient("unknown://host:1", nil)
		if err == nil {
			t.Fatalf("expected error for unknown driver, got nil (meta=%v)", m)
		}
		if !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("expected error to mention the driver name, got: %v", err)
		}
	})

	t.Run("valid driver with nil conf returns a client", func(t *testing.T) {
		// memkv is an in-memory backend: no external I/O, and nil conf must go
		// through the DefaultConf() path without panicking.
		m, err := NewMetaClient("memkv://test", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m == nil {
			t.Fatalf("expected non-nil Meta, got nil")
		}
	})
}
