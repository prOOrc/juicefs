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
// The proxy uses it to issue (CreateFileKey) and unwrap (GetFileFEK) per-file FEKs.
type KeyManagerClient interface {
	CreateFileKey(ctx context.Context, req *kmpb.CreateFileKeyRequest) (*kmpb.CreateFileKeyResponse, error)
	GetFileFEK(ctx context.Context, req *kmpb.GetFileFEKRequest) (*kmpb.GetFileFEKResponse, error)
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
		creds := credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      cp,
			ServerName:   serverName,
		})
		opts = append(opts, grpc.WithTransportCredentials(creds))
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
