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
	"fmt"

	oidcPkg "github.com/coreos/go-oidc/v3/oidc"
)

// IDToken is an alias for the go-oidc IDToken type, exported for use by consumers.
type IDToken = oidcPkg.IDToken

// Validator verifies OIDC ID tokens using JWKS from the issuer.
type Validator struct {
	provider *oidcPkg.Provider
	verifier *oidcPkg.IDTokenVerifier
}

// NewValidator creates a validator by performing OIDC discovery against the issuer.
// If clientID is non-empty, only tokens issued to that client are accepted.
// If clientID is empty, any valid token from the issuer is accepted.
func NewValidator(ctx context.Context, issuerURL string, clientID string) (*Validator, error) {
	prov, err := oidcPkg.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover provider %s: %w", issuerURL, err)
	}

	verifier := prov.Verifier(&oidcPkg.Config{
		ClientID:          clientID,
		SkipClientIDCheck: clientID == "",
	})

	return &Validator{
		provider: prov,
		verifier: verifier,
	}, nil
}

// Validate verifies the JWT signature, issuer, and expiry.
// Returns the parsed ID token on success.
func (v *Validator) Validate(ctx context.Context, idToken string) (*oidcPkg.IDToken, error) {
	token, err := v.verifier.Verify(ctx, idToken)
	if err != nil {
		return nil, fmt.Errorf("oidc: verify token: %w", err)
	}
	return token, nil
}

// Verifier returns the underlying verifier for advanced usage.
func (v *Validator) Verifier() *oidcPkg.IDTokenVerifier {
	return v.verifier
}
