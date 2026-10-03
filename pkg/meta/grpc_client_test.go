/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
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
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestGRPCMetaCreation tests that grpcMeta can be created
func TestGRPCMetaCreation(t *testing.T) {
	// Just test that we can create a new grpcMeta instance
	// (connection will fail without server, but that's OK)
	conf := DefaultConf()
	_, err := newGRPCMeta("grpc", "localhost:9561", conf)
	// Expected to fail if no server is running
	t.Logf("Connection result (expected to fail without server): %v", err)
}

// writeTestCAPEM generates a self-signed CA PEM and returns its path
// (harness helper from grpc_server_test_helpers_test.go).
func writeTestCAPEM(t *testing.T) string {
	t.Helper()
	return generateSelfSignedCAPEM(t)
}

// TestBuildClientTLSConfig (task 1.2): query-parameter parsing and credential
// choice — plaintext by default, system pool without tls-ca, CA file loading,
// server-name override, fail-fast on an unreadable CA file.
func TestBuildClientTLSConfig(t *testing.T) {
	t.Run("no tls param stays plaintext", func(t *testing.T) {
		cfg, err := buildClientTLSConfig(false, "", "")
		require.NoError(t, err)
		assert.Nil(t, cfg, "nil config means WithInsecure dial")
	})

	t.Run("tls=1 without tls-ca uses the system pool", func(t *testing.T) {
		cfg, err := buildClientTLSConfig(true, "", "")
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Nil(t, cfg.RootCAs, "system pool must be used when no tls-ca is given")
		assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
		assert.Empty(t, cfg.ServerName)
	})

	t.Run("tls-server-name overrides verification name", func(t *testing.T) {
		cfg, err := buildClientTLSConfig(true, "", "proxy.example.com")
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "proxy.example.com", cfg.ServerName)
	})

	t.Run("valid tls-ca loads the root pool", func(t *testing.T) {
		caPath := writeTestCAPEM(t)
		cfg, err := buildClientTLSConfig(true, caPath, "")
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.NotNil(t, cfg.RootCAs, "CA file must populate the root pool")
	})

	t.Run("unreadable tls-ca fails with the file path", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nonexistent", "ca.pem")
		cfg, err := buildClientTLSConfig(true, missing, "")
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), missing, "error must mention the CA file path")
	})

	t.Run("garbage tls-ca fails", func(t *testing.T) {
		garbage := filepath.Join(t.TempDir(), "ca.pem")
		require.NoError(t, os.WriteFile(garbage, []byte("not a pem"), 0600))
		_, err := buildClientTLSConfig(true, garbage, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), garbage)
	})
}

// TestGRPCMetaTLSQueryParsing (task 1.2): the full meta URL path — an
// unreadable tls-ca fails client creation (fail-fast), the snake_case aliases
// are accepted.
func TestGRPCMetaTLSQueryParsing(t *testing.T) {
	t.Run("missing ca file fails client creation", func(t *testing.T) {
		_, err := newGRPCMeta("grpc", "localhost:9561?tls=1&tls-ca=/nonexistent/ca.pem", DefaultConf())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/nonexistent/ca.pem")
	})

	t.Run("snake_case alias for tls-ca", func(t *testing.T) {
		_, err := newGRPCMeta("grpc", "localhost:9561?tls=1&tls_ca=/nonexistent/ca.pem", DefaultConf())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/nonexistent/ca.pem")
	})

	t.Run("tls=0 keeps plaintext dial", func(t *testing.T) {
		m, err := newGRPCMeta("grpc", "localhost:9561?tls=0", DefaultConf())
		if err == nil {
			// Dial itself is lazy; client creation must not fail on TLS.
			m.Shutdown()
		}
	})
}

// TestGRPCMetaName tests that grpcMeta returns correct name
func TestGRPCMetaName(t *testing.T) {
	// This is a simple test - actual connection tests require a running server
	conf := DefaultConf()
	meta, err := newGRPCMeta("grpc", "localhost:9561", conf)
	if err != nil {
		// Expected to fail if no server is running
		t.Logf("Expected error when no server is running: %v", err)
		return
	}
	defer meta.Shutdown()

	assert.Contains(t, meta.Name(), "grpc://")
}

// MockGRPCClient implements pb.MetaServiceClient for testing
type MockGRPCClient struct {
	pb.MetaServiceClient
	ListResponse   *pb.DirHandlerListResponse
	InsertResponse *pb.DirHandlerInsertResponse
	DeleteResponse *pb.DirHandlerDeleteResponse
	CloseResponse  *pb.DirHandlerCloseResponse
}

func (m *MockGRPCClient) DirHandlerList(ctx context.Context, req *pb.DirHandlerListRequest, opts ...grpc.CallOption) (*pb.DirHandlerListResponse, error) {
	if m.ListResponse != nil {
		return m.ListResponse, nil
	}
	return &pb.DirHandlerListResponse{}, nil
}

func (m *MockGRPCClient) DirHandlerInsert(ctx context.Context, req *pb.DirHandlerInsertRequest, opts ...grpc.CallOption) (*pb.DirHandlerInsertResponse, error) {
	if m.InsertResponse != nil {
		return m.InsertResponse, nil
	}
	return &pb.DirHandlerInsertResponse{}, nil
}

func (m *MockGRPCClient) DirHandlerDelete(ctx context.Context, req *pb.DirHandlerDeleteRequest, opts ...grpc.CallOption) (*pb.DirHandlerDeleteResponse, error) {
	if m.DeleteResponse != nil {
		return m.DeleteResponse, nil
	}
	return &pb.DirHandlerDeleteResponse{}, nil
}

func (m *MockGRPCClient) DirHandlerClose(ctx context.Context, req *pb.DirHandlerCloseRequest, opts ...grpc.CallOption) (*pb.DirHandlerCloseResponse, error) {
	if m.CloseResponse != nil {
		return m.CloseResponse, nil
	}
	return &pb.DirHandlerCloseResponse{}, nil
}

// TestGRPCDirHandlerList tests the List method of grpcDirHandler
func TestGRPCDirHandlerList(t *testing.T) {
	mockClient := &MockGRPCClient{
		ListResponse: &pb.DirHandlerListResponse{
			Entries: []*pb.ProtoEntry{
				{Inode: 1, Name: []byte("file1")},
				{Inode: 2, Name: []byte("file2")},
			},
		},
	}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	ctx := Context(nil)
	entries, errno := handler.List(ctx, 0)

	assert.Equal(t, syscall.Errno(0), errno)
	assert.Len(t, entries, 2)
	assert.Equal(t, Ino(1), entries[0].Inode)
	assert.Equal(t, []byte("file1"), entries[0].Name)
	assert.Equal(t, Ino(2), entries[1].Inode)
	assert.Equal(t, []byte("file2"), entries[1].Name)
}

// TestGRPCDirHandlerListError tests error handling in List
func TestGRPCDirHandlerListError(t *testing.T) {
	mockClient := &MockGRPCClient{
		ListResponse: &pb.DirHandlerListResponse{
			Errno: uint32(syscall.ENOENT),
		},
	}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	ctx := Context(nil)
	entries, errno := handler.List(ctx, 0)

	assert.Equal(t, syscall.ENOENT, errno)
	assert.Nil(t, entries)
}

// TestGRPCDirHandlerInsert tests the Insert method
func TestGRPCDirHandlerInsert(t *testing.T) {
	mockClient := &MockGRPCClient{}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	attr := &Attr{Mode: 0100644, Mtime: time.Now().Unix()}
	handler.Insert(Ino(100), "newfile", attr)
}

// TestGRPCDirHandlerDelete tests the Delete method
func TestGRPCDirHandlerDelete(t *testing.T) {
	mockClient := &MockGRPCClient{}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	handler.Delete("fileToDelete")
}

// TestGRPCDirHandlerClose tests the Close method
func TestGRPCDirHandlerClose(t *testing.T) {
	mockClient := &MockGRPCClient{}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	handler.Close()
}

// TestGRPCDirHandlerListWithOffset tests List with different offsets
func TestGRPCDirHandlerListWithOffset(t *testing.T) {
	mockClient := &MockGRPCClient{
		ListResponse: &pb.DirHandlerListResponse{
			Entries: []*pb.ProtoEntry{
				{Inode: 3, Name: []byte("file3")},
			},
		},
	}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	ctx := Context(nil)
	entries, errno := handler.List(ctx, 10)

	assert.Equal(t, syscall.Errno(0), errno)
	assert.Len(t, entries, 1)
	assert.Equal(t, Ino(3), entries[0].Inode)
}
