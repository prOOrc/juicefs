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
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// TestGRPCMetaSTSRefreshMarginParsing (task 2.1, design configure-sts-refresh-ttl
// D4): the sts-refresh-margin parameter parses in both spellings, defaults to
// half of the TTL when absent, and any value outside (0,1] fails client
// creation (fail-fast).
func TestGRPCMetaSTSRefreshMarginParsing(t *testing.T) {
	t.Run("absent defaults to half of the TTL", func(t *testing.T) {
		m, err := newGRPCMeta("grpc", "localhost:9561", DefaultConf())
		require.NoError(t, err)
		defer m.Shutdown()
		assert.Equal(t, DefaultSTSRefreshMargin, m.(*grpcMeta).stsRefreshMargin)
	})

	t.Run("kebab-case value is kept", func(t *testing.T) {
		m, err := newGRPCMeta("grpc", "localhost:9561?sts-refresh-margin=0.25", DefaultConf())
		require.NoError(t, err)
		defer m.Shutdown()
		assert.Equal(t, 0.25, m.(*grpcMeta).stsRefreshMargin)
	})

	t.Run("snake_case alias is accepted", func(t *testing.T) {
		m, err := newGRPCMeta("grpc", "localhost:9561?sts_refresh_margin=0.75", DefaultConf())
		require.NoError(t, err)
		defer m.Shutdown()
		assert.Equal(t, 0.75, m.(*grpcMeta).stsRefreshMargin)
	})

	t.Run("upper bound 1 is accepted", func(t *testing.T) {
		m, err := newGRPCMeta("grpc", "localhost:9561?sts-refresh-margin=1", DefaultConf())
		require.NoError(t, err)
		defer m.Shutdown()
		assert.Equal(t, 1.0, m.(*grpcMeta).stsRefreshMargin)
	})

	for _, v := range []string{"0", "-0.5", "1.5", "half", "NaN"} {
		t.Run("out of range "+v+" fails client creation", func(t *testing.T) {
			_, err := newGRPCMeta("grpc", "localhost:9561?sts-refresh-margin="+v, DefaultConf())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "sts-refresh-margin")
		})
	}
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

// reconnectEventRecorder records ordered client-side events (Chroot calls,
// reconnect hooks) for ordering assertions (design D4). The Chroot-recording
// fake (fakeReconnectClient) lives in hub_state_test.go.
type reconnectEventRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *reconnectEventRecorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, e)
}

func (r *reconnectEventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// TestReconnectReissuesChroot (fix-meta-proxy-session-churn, task 3.4 /
// design D4): after a proxy restart the fresh process forgot the session's
// subdir. On the offline→online transition the client must re-send Chroot
// before the replay hook runs, so the write journal replays against the
// right root.
func TestReconnectReissuesChroot(t *testing.T) {
	rec := &reconnectEventRecorder{}
	fc := &fakeReconnectClient{rec: rec}
	m := &grpcMeta{
		fekCache:       expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		offlineTimeout: 15 * time.Minute,
		client:         fc,
		chrootSubdir:   "companies/acme", // remembered from the mount-time Chroot
	}
	m.SetOnReconnect(func() { rec.add("replay") })

	// failing beat → offline-connected: no Chroot while offline
	fc.set(errors.New("hub down"), nil)
	m.doHeartbeat()
	require.Equal(t, HubOfflineConnected, m.HubState())
	require.Empty(t, rec.snapshot())

	// recovery beat: re-Chroot strictly before replay
	fc.set(nil, &pb.FlushSessionResponse{})
	m.doHeartbeat()
	require.Eventually(t, func() bool {
		order := rec.snapshot()
		return len(order) == 2 && order[0] == "chroot:companies/acme" && order[1] == "replay"
	}, time.Second, time.Millisecond)
}

// statusErrClient answers the inode-level RPCs with a configured gRPC status
// error (simulating an authz deny or a vanished inode at the proxy).
type statusErrClient struct {
	pb.MetaServiceClient
	err error
}

func (c *statusErrClient) Lookup(ctx context.Context, req *pb.LookupRequest, opts ...grpc.CallOption) (*pb.LookupResponse, error) {
	return nil, c.err
}

func (c *statusErrClient) GetAttr(ctx context.Context, req *pb.GetAttrRequest, opts ...grpc.CallOption) (*pb.GetAttrResponse, error) {
	return nil, c.err
}

func (c *statusErrClient) NewDirHandler(ctx context.Context, req *pb.NewDirHandlerRequest, opts ...grpc.CallOption) (*pb.NewDirHandlerResponse, error) {
	return nil, c.err
}

// newErrnoTestMeta builds a grpcMeta whose client answers with the given error.
func newErrnoTestMeta(err error) *grpcMeta {
	return &grpcMeta{
		client:    &statusErrClient{err: err},
		attrCache: expirable.NewLRU[uint64, *Attr](10, nil, time.Minute),
	}
}

// TestDeniedLookupMapsToEACCES (design D3, fix-meta-proxy-session-churn): a
// proxy authz deny arrives as codes.PermissionDenied and must surface as
// EACCES — not a generic, undiagnosable EIO.
func TestDeniedLookupMapsToEACCES(t *testing.T) {
	m := newErrnoTestMeta(status.Error(codes.PermissionDenied, "access denied"))
	var inode Ino
	var attr Attr
	errno := m.Lookup(Background(), 1, "entry", &inode, &attr, false)
	assert.Equal(t, syscall.EACCES, errno, "denied Lookup must map to EACCES")
}

// TestDeniedGetAttrMapsToEACCES (design D3): same mapping for GetAttr.
func TestDeniedGetAttrMapsToEACCES(t *testing.T) {
	m := newErrnoTestMeta(status.Error(codes.PermissionDenied, "access denied"))
	var attr Attr
	errno := m.GetAttr(Background(), 1, &attr)
	assert.Equal(t, syscall.EACCES, errno, "denied GetAttr must map to EACCES")
}

// TestDeniedNewDirHandlerMapsToEACCES (design D3): same mapping for
// NewDirHandler (readdir denied at the proxy).
func TestDeniedNewDirHandlerMapsToEACCES(t *testing.T) {
	m := newErrnoTestMeta(status.Error(codes.PermissionDenied, "access denied"))
	_, errno := m.NewDirHandler(Background(), 1, false, nil)
	assert.Equal(t, syscall.EACCES, errno, "denied NewDirHandler must map to EACCES")
}

// TestVanishedInodeMapsToENOENT (design D3/I7): codes.NotFound (the proxy
// answers ENOENT when the engine reports the inode gone) maps to ENOENT on
// all three errno-mapped methods.
func TestVanishedInodeMapsToENOENT(t *testing.T) {
	cases := []struct {
		name string
		call func(m *grpcMeta) syscall.Errno
	}{
		{"lookup", func(m *grpcMeta) syscall.Errno {
			var inode Ino
			var attr Attr
			return m.Lookup(Background(), 1, "entry", &inode, &attr, false)
		}},
		{"getattr", func(m *grpcMeta) syscall.Errno {
			var attr Attr
			return m.GetAttr(Background(), 1, &attr)
		}},
		{"newdirhandler", func(m *grpcMeta) syscall.Errno {
			_, errno := m.NewDirHandler(Background(), 1, false, nil)
			return errno
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newErrnoTestMeta(status.Error(codes.NotFound, "inode does not exist"))
			assert.Equal(t, syscall.ENOENT, c.call(m), "NotFound must map to ENOENT")
		})
	}
}
