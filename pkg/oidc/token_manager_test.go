/*
 * JuiceFS, Copyright 2024 Juicedata, Inc.
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

package oidc

import (
	"testing"
)

func TestNewTokenManagerEmptyConfig(t *testing.T) {
	_, err := NewTokenManager(Config{})
	if err == nil {
		t.Fatal("expected error for empty config")
	}
}

func TestGenerateState(t *testing.T) {
	s1 := GenerateState()
	s2 := GenerateState()
	if s1 == s2 {
		t.Error("GenerateState should produce unique values")
	}
	if len(s1) == 0 {
		t.Error("GenerateState should not return empty string")
	}
}
