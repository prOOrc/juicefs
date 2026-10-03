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

package meta

// TLS bufconn harness (implement-grpc-tls, tasks 2.2/2.3): an in-process
// MetaProxyServer served over bufconn with either TLS (self-signed certificate
// generated on the fly, mirroring the production server config shape) or
// plaintext, so Open/ResolveFileKey can be exercised over both transports.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// fekFakeMeta is a minimal in-memory Meta for the transport-level Open/FEK
// tests: it serves a single configurable attr and reports a configurable
// volume format. The embedded nil Meta panics if a test accidentally
// exercises an unstubbed method.
type fekFakeMeta struct {
	Meta

	attr   *Attr
	format *Format
}

func (f *fekFakeMeta) Open(ctx Context, inode Ino, flags uint32, attr *Attr) syscall.Errno {
	*attr = *f.attr
	return 0
}

func (f *fekFakeMeta) GetAttr(ctx Context, inode Ino, attr *Attr) syscall.Errno {
	*attr = *f.attr
	return 0
}

func (f *fekFakeMeta) GetFormat() Format { return *f.format }

// testTLSCert bundles a generated self-signed certificate in every form the
// harness needs: server creds, PEM files (exercising the file-based loading
// paths) and the parsed leaf for the client trust pool.
type testTLSCert struct {
	cert     tls.Certificate
	leaf     *x509.Certificate
	certPEM  []byte
	keyPEM   []byte
	certPath string
	keyPath  string
}

// generateTestTLSCert creates a self-signed server certificate for localhost
// (valid 24h, ECDSA P-256) and writes the PEM files to a temp directory.
func generateTestTLSCert(t *testing.T) testTLSCert {
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
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0600))

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return testTLSCert{cert: tlsCert, leaf: leaf, certPEM: certPEM, keyPEM: keyPEM, certPath: certPath, keyPath: keyPath}
}

// generateSelfSignedCAPEM returns the path of a freshly generated self-signed
// certificate PEM (the certificate is its own root, so it doubles as a CA).
func generateSelfSignedCAPEM(t *testing.T) string {
	t.Helper()
	return generateTestTLSCert(t).certPath
}

// fekTransportServer serves one MetaProxyServer over bufconn; the client is
// connected through the same buffer with matching (or absent) credentials.
type fekTransportServer struct {
	pb.MetaServiceClient
	server   *MetaProxyServer
	grpcSrv  *grpc.Server
	lis      *bufconn.Listener
	tlsCert  testTLSCert
	tlsOn    bool
	caPath   string
	fek      []byte // plaintext FEK the server's KeyManager will unwrap to
	identity string // 'sub' the test client presents via testIdentityInterceptor
}

// newFekTransportServer wires a MetaProxyServer around a fekFakeMeta exposing
// an encrypted file at inode testFekInode ("/secret.exr" in the path cache).
// With tlsOn the listener requires TLS (same config shape as cmd/meta_proxy.go:
// loaded key pair, MinVersion 1.2) and the client verifies it against the
// generated certificate; otherwise both sides are plaintext.
func newFekTransportServer(t *testing.T, encryptionEnabled, tlsOn bool) *fekTransportServer {
	t.Helper()
	format := &Format{Name: "transport-test", UUID: uuid.New().String(), EncryptionEnabled: encryptionEnabled}
	km := newFakeKeyManager()
	fek := randomBytes(t, fekSize)
	wrapped, err := WrapFEK(km.kek, fek, FekAAD{
		VolumeUUID: format.UUID, CompanyID: testCompanyID, DriveFileID: "dfid-tls-test", Inode: testFekInode, FekVersion: 1,
	}, 1)
	require.NoError(t, err)

	m := &fekFakeMeta{
		attr: &Attr{
			Typ:         TypeFile,
			Encrypted:   true,
			DriveFileID: "dfid-tls-test",
			FekVersion:  1,
			WrappedFek:  wrapped,
		},
		format: format,
	}
	srv := NewMetaProxyServer(m, 100)
	srv.SetKeyManager(km)
	srv.InodePathCache().Set(testFekInode, "/secret.exr")
	srv.SetVolumeName(format.Name)

	ts := &fekTransportServer{server: srv, tlsOn: tlsOn, fek: fek, identity: uuid.New().String()}
	if tlsOn {
		ts.tlsCert = generateTestTLSCert(t)
		ts.caPath = ts.tlsCert.certPath
	}
	ts.lis = bufconn.Listen(1 << 20)

	var opts []grpc.ServerOption
	if tlsOn {
		opts = append(opts, grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{ts.tlsCert.cert},
			MinVersion:   tls.VersionTLS12,
		})))
	}
	opts = append(opts, grpc.ChainUnaryInterceptor(testIdentityInterceptor()))
	ts.grpcSrv = grpc.NewServer(opts...)
	pb.RegisterMetaServiceServer(ts.grpcSrv, srv)
	go func() { _ = ts.grpcSrv.Serve(ts.lis) }()

	ts.MetaServiceClient = ts.dial(t)

	t.Cleanup(func() {
		ts.grpcSrv.Stop()
	})
	return ts
}

// dial opens a gRPC client channel to the bufconn listener: TLS with the
// generated certificate as root when tlsOn, plaintext otherwise.
func (ts *fekTransportServer) dial(t *testing.T) pb.MetaServiceClient {
	t.Helper()
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return ts.lis.Dial()
	}
	var opts []grpc.DialOption
	opts = append(opts, grpc.WithContextDialer(dialer))
	if ts.tlsOn {
		pool := x509.NewCertPool()
		pool.AddCert(ts.tlsCert.leaf)
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs:    pool,
			ServerName: "localhost",
			MinVersion: tls.VersionTLS12,
		})))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	cc, err := grpc.Dial("bufnet", opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cc.Close() })
	return pb.NewMetaServiceClient(cc)
}

// userSubCtx attaches the simulated OIDC identity expected by
// testIdentityInterceptor on the server.
func (ts *fekTransportServer) userSubCtx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, testUserMetadataKey, ts.identity)
}

// testFekInode is the inode of the fake encrypted file every transport test
// opens ("/secret.exr" in the server's path cache).
const testFekInode = 42
