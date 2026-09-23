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

package utils

import "golang.org/x/sys/unix"

// MlockPage locks the memory backing b into RAM so it cannot be swapped to
// disk (NFR-SEC-3). Best-effort: callers must treat failure as non-fatal.
func MlockPage(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return unix.Mlock(b)
}
