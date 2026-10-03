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

package cmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTestCertPair generates a self-signed certificate/key pair (implement-
// grpc-tls tests) and returns both PEM file paths.
func writeTestCertPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0600))
	return certPath, keyPath
}

// TestMetaProxyTLS (task 1.3): the --tls-cert/--tls-key pair validation —
// no flags keeps plaintext, a half-configured pair names the missing flag,
// a valid pair yields a config with minimum TLS version 1.2.
func TestMetaProxyTLS(t *testing.T) {
	t.Run("no flags keeps plaintext", func(t *testing.T) {
		cfg, err := serverTLSConfig("", "")
		require.NoError(t, err)
		assert.Nil(t, cfg, "nil config means the listener stays plaintext")
	})

	t.Run("cert without key names the missing flag", func(t *testing.T) {
		certPath, _ := writeTestCertPair(t)
		cfg, err := serverTLSConfig(certPath, "")
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "--tls-key", "error must name the missing flag")
	})

	t.Run("key without cert names the missing flag", func(t *testing.T) {
		_, keyPath := writeTestCertPair(t)
		cfg, err := serverTLSConfig("", keyPath)
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "--tls-cert", "error must name the missing flag")
	})

	t.Run("valid pair enforces minimum TLS 1.2", func(t *testing.T) {
		certPath, keyPath := writeTestCertPair(t)
		cfg, err := serverTLSConfig(certPath, keyPath)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Len(t, cfg.Certificates, 1)
		assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	})

	t.Run("unreadable certificate fails", func(t *testing.T) {
		_, err := serverTLSConfig("/nonexistent/cert.pem", "/nonexistent/key.pem")
		require.Error(t, err)
	})
}
