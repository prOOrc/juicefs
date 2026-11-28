//go:build !windows
// +build !windows

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

// Тесты unix-символов cmd (parseUIDGID живёт в mount_unix.go, !windows):
// в нетегированном mount_test.go они ломали компиляцию/vet на Windows
// (fix-cfapi-followups A6, попутный пре-существенный фикс).
package cmd

import "testing"

func TestParseUIDGID(t *testing.T) {
	tests := []struct {
		input       string
		defaultUid  uint32
		defaultGid  uint32
		expectedUid uint32
		expectedGid uint32
	}{
		{"1000:1000", 65534, 65534, 1000, 1000},
		{"1000:", 65534, 65534, 1000, 65534},
		{":1000", 65534, 65534, 65534, 1000},
		{"", 65534, 65534, 65534, 65534},
		{"0:1000", 65534, 65534, 65534, 1000},
		{"1000:0", 65534, 65534, 1000, 65534},
	}

	for _, tt := range tests {
		uid, gid := parseUIDGID(tt.input, tt.defaultUid, tt.defaultGid)
		if uid != tt.expectedUid || gid != tt.expectedGid {
			t.Errorf("parseUIDGID(%q) = (%d, %d), want (%d, %d)", tt.input, uid, gid, tt.expectedUid, tt.expectedGid)
		}
	}
}
