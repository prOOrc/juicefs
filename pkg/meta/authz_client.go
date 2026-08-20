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

	authzpb "github.com/juicedata/juicefs/pkg/meta/authz_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// platformAuthzClient wraps the generated gRPC client for agio-platform AuthzService.
type platformAuthzClient struct {
	client     authzpb.AuthzServiceClient
	volumeName string
}

// CheckPermission calls the authz service to check a single path permission.
func (c *platformAuthzClient) CheckPermission(ctx context.Context, userID, filePath string, perm AuthzPermission) (bool, error) {
	resp, err := c.client.CheckPermission(ctx, &authzpb.CheckPermissionRequest{
		UserId:     userID,
		Path:       filePath,
		Permission: toPBPermission(perm),
		VolumeName: c.volumeName,
	})
	if err != nil {
		return false, err
	}
	return resp.Allowed, nil
}

// CheckBulkPermissions calls the authz service to batch-check multiple paths.
func (c *platformAuthzClient) CheckBulkPermissions(ctx context.Context, userID string, paths []string, perm AuthzPermission) ([]bool, error) {
	resp, err := c.client.CheckBulkPermissions(ctx, &authzpb.CheckBulkPermissionsRequest{
		UserId:     userID,
		Paths:      paths,
		Permission: toPBPermission(perm),
		VolumeName: c.volumeName,
	})
	if err != nil {
		return nil, err
	}
	return resp.Allowed, nil
}

// CheckOrganizationAdmin calls the authz service to check org admin status.
func (c *platformAuthzClient) CheckOrganizationAdmin(ctx context.Context, userID string) (bool, error) {
	resp, err := c.client.CheckOrganizationAdmin(ctx, &authzpb.CheckOrganizationAdminRequest{
		UserId:     userID,
		VolumeName: c.volumeName,
	})
	if err != nil {
		return false, err
	}
	return resp.IsAdmin, nil
}

// Close closes the underlying gRPC connection.
func (c *platformAuthzClient) Close() error {
	if closer, ok := c.client.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// toPBPermission maps internal AuthzPermission to the protobuf Permission enum.
func toPBPermission(p AuthzPermission) authzpb.Permission {
	switch p {
	case AuthzPermissionView:
		return authzpb.Permission_PERMISSION_VIEW
	case AuthzPermissionRead:
		return authzpb.Permission_PERMISSION_READ
	case AuthzPermissionWrite:
		return authzpb.Permission_PERMISSION_EDIT
	default:
		return authzpb.Permission_PERMISSION_NONE
	}
}

// NewPlatformAuthzClient creates a gRPC client for the authz service.
// For production, use TLS via --authz-tls-cert, --authz-tls-key, and --authz-tls-ca flags.
func NewPlatformAuthzClient(addr, volumeName, tlsCert, tlsKey, tlsCA, serverName string) (AuthzClient, error) {
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
		return nil, fmt.Errorf("connect to authz service %s: %w", addr, err)
	}

	return &platformAuthzClient{
		client:     authzpb.NewAuthzServiceClient(conn),
		volumeName: volumeName,
	}, nil
}
