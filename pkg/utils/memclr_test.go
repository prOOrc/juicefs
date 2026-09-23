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

import (
	"bytes"
	"testing"
)

func TestMemClear(t *testing.T) {
	b := []byte{1, 2, 3, 4, 5}
	MemClear(b)
	if !bytes.Equal(b, make([]byte, len(b))) {
		t.Fatalf("MemClear did not zero the slice: %v", b)
	}

	// nil and empty slices must not panic
	MemClear(nil)
	MemClear([]byte{})
}

func TestMlockPage(t *testing.T) {
	if err := MlockPage(nil); err != nil {
		t.Fatalf("MlockPage(nil): %s", err)
	}
	b := make([]byte, 4096)
	if err := MlockPage(b); err != nil {
		t.Skipf("mlock not available on this platform: %s", err)
	}
}
