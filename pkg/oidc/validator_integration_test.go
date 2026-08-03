//go:build integration

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
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
)

const testKeyID = "test-key-1"

// testOIDCServer creates an HTTP server that serves OIDC discovery + JWKS.
type testOIDCServer struct {
	httptest.Server
	IssuerURL string
	PrivKey   *rsa.PrivateKey
}

func newTestOIDCServer(t *testing.T) *testOIDCServer {
	t.Helper()

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	pubKey := &privKey.PublicKey
	nBytes := pubKey.N.Bytes()
	eBytes := big.NewInt(int64(pubKey.E)).Bytes()

	jwk := map[string]interface{}{
		"kty": "RSA",
		"kid": testKeyID,
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(nBytes),
		"e":   base64.RawURLEncoding.EncodeToString(eBytes),
	}

	var issuerURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuerURL,
			"jwks_uri":               issuerURL + "/.well-known/jwks.json",
			"authorization_endpoint": issuerURL + "/auth",
			"token_endpoint":         issuerURL + "/token",
		})
	})

	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"keys": []interface{}{jwk},
		})
	})

	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.Start()
	issuerURL = srv.URL

	return &testOIDCServer{
		Server:    *srv,
		IssuerURL: issuerURL,
		PrivKey:   privKey,
	}
}

// signToken creates a JWT signed with the server's private key.
func (s *testOIDCServer) signToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()

	header := map[string]interface{}{
		"alg": "RS256",
		"kid": testKeyID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header = header

	signed, err := token.SignedString(s.PrivKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func TestNewValidator(t *testing.T) {
	srv := newTestOIDCServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := NewValidator(ctx, srv.IssuerURL, "")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if v.Verifier() == nil {
		t.Fatal("Verifier should not be nil")
	}
}

func TestValidateValidToken(t *testing.T) {
	srv := newTestOIDCServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := NewValidator(ctx, srv.IssuerURL, "")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	tokenStr := srv.signToken(t, jwt.MapClaims{
		"iss": srv.IssuerURL,
		"sub": "user-123",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})

	idToken, err := v.Validate(ctx, tokenStr)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	var claims jwt.MapClaims
	if err := idToken.Claims(&claims); err != nil {
		t.Fatalf("extract claims: %v", err)
	}

	if claims["sub"] != "user-123" {
		t.Errorf("sub = %v, want user-123", claims["sub"])
	}
}

func TestValidateExpiredToken(t *testing.T) {
	srv := newTestOIDCServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := NewValidator(ctx, srv.IssuerURL, "")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	tokenStr := srv.signToken(t, jwt.MapClaims{
		"iss": srv.IssuerURL,
		"sub": "user-123",
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
	})

	_, err = v.Validate(ctx, tokenStr)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestValidateWrongIssuer(t *testing.T) {
	srv := newTestOIDCServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := NewValidator(ctx, srv.IssuerURL, "")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Token with wrong issuer
	tokenStr := srv.signToken(t, jwt.MapClaims{
		"iss": "https://wrong.issuer.local",
		"sub": "user-123",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	_, err = v.Validate(ctx, tokenStr)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestValidateWrongKey(t *testing.T) {
	srv := newTestOIDCServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := NewValidator(ctx, srv.IssuerURL, "")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Sign with a different key
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": srv.IssuerURL,
		"sub": "user-123",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	token.Header = map[string]interface{}{"alg": "RS256", "kid": testKeyID}

	tokenStr, err := token.SignedString(otherKey)
	if err != nil {
		t.Fatalf("sign with other key: %v", err)
	}

	_, err = v.Validate(ctx, tokenStr)
	if err == nil {
		t.Fatal("expected error for wrong key")
	}
}

func TestValidateClientIDMismatch(t *testing.T) {
	srv := newTestOIDCServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Validator requires specific client ID
	v, err := NewValidator(ctx, srv.IssuerURL, "my-client")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Token with wrong audience
	tokenStr := srv.signToken(t, jwt.MapClaims{
		"iss": srv.IssuerURL,
		"sub": "user-123",
		"aud": "other-client",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	_, err = v.Validate(ctx, tokenStr)
	if err == nil {
		t.Fatal("expected error for client ID mismatch")
	}
}

func TestValidateClientIDMatch(t *testing.T) {
	srv := newTestOIDCServer(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Validator requires specific client ID
	v, err := NewValidator(ctx, srv.IssuerURL, "my-client")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Token with matching audience
	tokenStr := srv.signToken(t, jwt.MapClaims{
		"iss": srv.IssuerURL,
		"sub": "user-123",
		"aud": "my-client",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	_, err = v.Validate(ctx, tokenStr)
	if err != nil {
		t.Fatalf("Validate with matching client ID: %v", err)
	}
}
