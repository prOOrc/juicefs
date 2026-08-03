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

import "path/filepath"

// DefaultScopes returned by the OIDC provider.
var DefaultScopes = []string{"openid", "profile", "email"}

// Config holds OIDC client configuration.
type Config struct {
	IssuerURL    string   // OIDC issuer (e.g., Hydra public URL)
	ClientID     string   // OIDC client ID
	ClientSecret string   // OIDC client secret (optional)
	RedirectURL  string   // Redirect URL for auth code callback
	Scopes       []string // OIDC scopes; defaults to DefaultScopes
	CacheDir     string   // Directory for token cache; defaults to ~/.juicefs/oidc_cache
}

// IsZero returns true if the config is empty (OIDC not enabled).
func (c *Config) IsZero() bool {
	return c.IssuerURL == "" && c.ClientID == ""
}

// CacheFile returns the path for the token cache file.
func (c *Config) CacheFile() string {
	dir := c.CacheDir
	if dir == "" {
		home, err := filepath.Abs("~/.juicefs/oidc_cache")
		if err != nil {
			home = "/tmp/juicefs_oidc_cache"
		}
		dir = home
	}
	return filepath.Join(dir, "token.json")
}

// EffectiveScopes returns the scopes to use, falling back to defaults.
func (c *Config) EffectiveScopes() []string {
	if len(c.Scopes) == 0 {
		return DefaultScopes
	}
	return c.Scopes
}
