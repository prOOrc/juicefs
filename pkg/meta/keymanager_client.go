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

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// KeyManagerClient is the interface to the agio-platform DriveKeyManagerService.
// The proxy uses it to issue (CreateFileKey) and unwrap (GetFileFEK) per-file FEKs;
// render nodes use FetchCompanyKEK once at mount (FR-RND-2). Stage 7 adds
// GetPermissionGeneration (heartbeat revocation, task 7.3), GetSTSCredentials
// (short-lived S3 credentials, task 7.4) and RotateFileFEK (offboarding rotation,
// task 7.5).
type KeyManagerClient interface {
	CreateFileKey(ctx context.Context, req *kmpb.CreateFileKeyRequest) (*kmpb.CreateFileKeyResponse, error)
	GetFileFEK(ctx context.Context, req *kmpb.GetFileFEKRequest) (*kmpb.GetFileFEKResponse, error)
	FetchCompanyKEK(ctx context.Context, req *kmpb.FetchCompanyKEKRequest) (*kmpb.FetchCompanyKEKResponse, error)
	GetPermissionGeneration(ctx context.Context, req *kmpb.GetPermissionGenerationRequest) (*kmpb.GetPermissionGenerationResponse, error)
	GetSTSCredentials(ctx context.Context, req *kmpb.GetSTSCredentialsRequest) (*kmpb.GetSTSCredentialsResponse, error)
	RotateFileFEK(ctx context.Context, req *kmpb.RotateFileFEKRequest) (*kmpb.RotateFileFEKResponse, error)
	Close() error
}

// platformKeyManager wraps the generated gRPC client for DriveKeyManagerService.
type platformKeyManager struct {
	client kmpb.DriveKeyManagerServiceClient
}

func (c *platformKeyManager) CreateFileKey(ctx context.Context, req *kmpb.CreateFileKeyRequest) (*kmpb.CreateFileKeyResponse, error) {
	return c.client.CreateFileKey(ctx, req)
}

func (c *platformKeyManager) GetFileFEK(ctx context.Context, req *kmpb.GetFileFEKRequest) (*kmpb.GetFileFEKResponse, error) {
	return c.client.GetFileFEK(ctx, req)
}

func (c *platformKeyManager) FetchCompanyKEK(ctx context.Context, req *kmpb.FetchCompanyKEKRequest) (*kmpb.FetchCompanyKEKResponse, error) {
	return c.client.FetchCompanyKEK(ctx, req)
}

func (c *platformKeyManager) GetPermissionGeneration(ctx context.Context, req *kmpb.GetPermissionGenerationRequest) (*kmpb.GetPermissionGenerationResponse, error) {
	return c.client.GetPermissionGeneration(ctx, req)
}

func (c *platformKeyManager) GetSTSCredentials(ctx context.Context, req *kmpb.GetSTSCredentialsRequest) (*kmpb.GetSTSCredentialsResponse, error) {
	return c.client.GetSTSCredentials(ctx, req)
}

func (c *platformKeyManager) RotateFileFEK(ctx context.Context, req *kmpb.RotateFileFEKRequest) (*kmpb.RotateFileFEKResponse, error) {
	return c.client.RotateFileFEK(ctx, req)
}

// Close closes the underlying gRPC connection.
func (c *platformKeyManager) Close() error {
	if closer, ok := c.client.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// NewKeyManagerClient creates a gRPC client for the KeyManager service.
// For production, use TLS via --keymanager-tls-cert, --keymanager-tls-key, and
// --keymanager-tls-ca flags (same pattern as the authz client).
func NewKeyManagerClient(addr, tlsCert, tlsKey, tlsCA, serverName string) (KeyManagerClient, error) {
	return newKeyManagerClient(addr, tlsCert, tlsKey, tlsCA, serverName, 0)
}

// NewRenderKeyManagerClient creates a gRPC client for the KeyManager service for
// render nodes. When a CA is given the transport enforces TLS 1.3 (NFR-SEC-2):
// the Company KEK travels in plaintext over this channel, so no downgrade to
// older protocol versions is acceptable.
func NewRenderKeyManagerClient(addr, tlsCA, serverName string) (KeyManagerClient, error) {
	return newKeyManagerClient(addr, "", "", tlsCA, serverName, tls.VersionTLS13)
}

func newKeyManagerClient(addr, tlsCert, tlsKey, tlsCA, serverName string, minTLSVersion uint16) (KeyManagerClient, error) {
	opts := []grpc.DialOption{}

	if tlsCert != "" && tlsKey != "" && tlsCA != "" {
		cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			return nil, fmt.Errorf("load TLS cert/key: %w", err)
		}
		ca, err := os.ReadFile(tlsCA)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		cp := x509.NewCertPool()
		if !cp.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		cfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      cp,
			ServerName:   serverName,
		}
		if minTLSVersion != 0 {
			cfg.MinVersion = minTLSVersion
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	} else if tlsCA != "" {
		ca, err := os.ReadFile(tlsCA)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		cp := x509.NewCertPool()
		if !cp.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		cfg := &tls.Config{RootCAs: cp, ServerName: serverName}
		if minTLSVersion != 0 {
			cfg.MinVersion = minTLSVersion
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to keymanager service %s: %w", addr, err)
	}

	return &platformKeyManager{
		client: kmpb.NewDriveKeyManagerServiceClient(conn),
	}, nil
}
