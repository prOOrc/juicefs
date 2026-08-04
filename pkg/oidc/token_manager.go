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
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/prOOrc/oidc-login/pkg/sdk"
)

var tmLogger = utils.GetLogger("juicefs")

// TokenManager wraps oidc-login sdk.Manager.
// The library handles token caching, expiry checks, and refresh automatically.
type TokenManager struct {
	cfg Config
	mgr *sdk.Manager
	mu  sync.Mutex
}

// NewTokenManager creates a manager. OIDC discovery happens lazily on first use.
func NewTokenManager(cfg Config) (*TokenManager, error) {
	if cfg.IsZero() {
		return nil, fmt.Errorf("oidc: config is empty")
	}
	return &TokenManager{cfg: cfg}, nil
}

// ensureManager performs lazy OIDC discovery.
func (m *TokenManager) ensureManager(ctx context.Context) (*sdk.Manager, error) {
	if m.mgr != nil {
		return m.mgr, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	mgr, err := sdk.New(ctx, sdk.Config{
		IssuerURL:    m.cfg.IssuerURL,
		ClientID:     m.cfg.ClientID,
		ClientSecret: m.cfg.ClientSecret,
		RedirectURL:  m.cfg.RedirectURL,
		Scopes:       m.cfg.EffectiveScopes(),
		CacheDir:     m.cfg.CacheDir,
	})
	if err != nil {
		return nil, fmt.Errorf("oidc: create manager: %w", err)
	}

	m.mgr = mgr
	mgr.StartRefresher()
	return mgr, nil
}

// GetToken returns a valid token. The library handles caching, refresh, and
// interactive authentication automatically. Blocks on browser auth if no cached
// token exists or refresh fails.
func (m *TokenManager) GetToken(ctx context.Context) (*sdk.TokenSet, error) {
	mgr, err := m.ensureManager(ctx)
	if err != nil {
		tmLogger.Warnf("OIDC: discovery failed: %v", err)
		return nil, nil
	}
	return mgr.GetToken(ctx)
}

// BearerToken returns the ID token as a "Bearer ..." string, or empty if unavailable.
// Blocks on browser auth if no cached token exists.
func (m *TokenManager) BearerToken(ctx context.Context) string {
	tok, _ := m.GetToken(ctx)
	if tok != nil && tok.IDToken != "" {
		return "Bearer " + tok.IDToken
	}
	return ""
}

// Authenticate runs the full OIDC authentication flow (interactive, blocking).
// Same as GetToken but always triggers browser auth even if a valid cached token exists.
func (m *TokenManager) Authenticate(ctx context.Context) (*sdk.TokenSet, error) {
	mgr, err := m.ensureManager(ctx)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	return mgr.Authenticate(ctx)
}

// Stop is a no-op — the library manages its own lifecycle.
func (m *TokenManager) Stop() {}

// GenerateState generates a random state string for CSRF protection.
func GenerateState() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.URLEncoding.EncodeToString(b)
}
