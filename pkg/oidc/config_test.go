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

import "testing"

func TestConfigIsZero(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, true},
		{"issuer only", Config{IssuerURL: "https://example.com"}, false},
		{"client id only", Config{ClientID: "abc"}, false},
		{"both set", Config{IssuerURL: "https://x", ClientID: "y"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsZero(); got != tt.want {
				t.Errorf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigEffectiveScopes(t *testing.T) {
	cfg := Config{}
	if got := cfg.EffectiveScopes(); len(got) != len(DefaultScopes) {
		t.Errorf("Expected default scopes, got %v", got)
	}

	cfg.Scopes = []string{"openid", "custom"}
	if got := cfg.EffectiveScopes(); len(got) != 2 || got[1] != "custom" {
		t.Errorf("Expected custom scopes, got %v", got)
	}
}

func TestConfigCacheFile(t *testing.T) {
	cfg := Config{}
	path := cfg.CacheFile()
	if path == "" {
		t.Error("CacheFile() should not return empty string")
	}

	cfg.CacheDir = "/custom/dir"
	if got := cfg.CacheFile(); got != "/custom/dir/token.json" {
		t.Errorf("CacheFile() = %q, want /custom/dir/token.json", got)
	}
}
