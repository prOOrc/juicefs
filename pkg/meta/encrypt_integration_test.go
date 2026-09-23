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

// Stage 3 integration suite (tasks.md 3.7): real Redis + in-process MetaProxyServer
// + fake KeyManager + gRPC client. The data path (chunk/AGDF in S3) is covered from
// stage 4 on; here we verify the metadata side of per-file FEK encryption:
// Create issues a wrapped FEK, Open delivers the plaintext FEK, cache hits skip
// KeyManager, denials fail closed, and legacy volumes never touch KeyManager.

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"

	"github.com/google/uuid"
	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// testCompanyID is the company the fake KeyManager pretends to derive from the path
// (decision 3.8: the real service derives it server-side; the AAD only needs a
// consistent value between wrap and unwrap).
const testCompanyID = "test-company"

// fakeKeyManager is an in-process KeyManagerClient. It wraps/unwraps FEKs with a
// fixed KEK using the production AGFK format, so the AAD binding (volume UUID,
// drive file ID, inode, FEK version) is genuinely verified end to end.
type fakeKeyManager struct {
	mu             sync.Mutex
	kek            []byte
	denyCreate     bool
	denyGetFek     bool
	createCalls    int
	getFekCalls    int
	lastCreatePath string
	lastGetFekPath string
}

var _ KeyManagerClient = (*fakeKeyManager)(nil)

func newFakeKeyManager() *fakeKeyManager {
	kek := make([]byte, kekSize)
	for i := range kek {
		kek[i] = byte(i)
	}
	return &fakeKeyManager{kek: kek}
}

// newFakeKeyManagerWithKEK builds a fake KeyManager around a caller-provided KEK
// (render tests need two distinct company KEKs).
func newFakeKeyManagerWithKEK(kek []byte) *fakeKeyManager {
	k := make([]byte, len(kek))
	copy(k, kek)
	return &fakeKeyManager{kek: k}
}

func (f *fakeKeyManager) aad(volumeUUID, driveFileID string, inode int64, fekVersion uint32) FekAAD {
	return FekAAD{
		VolumeUUID:  volumeUUID,
		CompanyID:   testCompanyID,
		DriveFileID: driveFileID,
		Inode:       Ino(inode),
		FekVersion:  fekVersion,
	}
}

func (f *fakeKeyManager) CreateFileKey(ctx context.Context, req *kmpb.CreateFileKeyRequest) (*kmpb.CreateFileKeyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.lastCreatePath = req.Path
	if f.denyCreate {
		return nil, status.Error(codes.PermissionDenied, "create denied")
	}
	fek, err := NewFEK()
	if err != nil {
		return nil, err
	}
	wrapped, err := WrapFEK(f.kek, fek, f.aad(req.VolumeUuid, req.DriveFileId, req.Inode, 1), 1)
	if err != nil {
		return nil, err
	}
	return &kmpb.CreateFileKeyResponse{
		WrappedFek:  wrapped,
		DriveFileId: req.DriveFileId,
		FekVersion:  1,
		KekVersion:  1,
		CryptoAlg:   "AES-256-GCM",
	}, nil
}

func (f *fakeKeyManager) GetFileFEK(ctx context.Context, req *kmpb.GetFileFEKRequest) (*kmpb.GetFileFEKResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getFekCalls++
	f.lastGetFekPath = req.Path
	if f.denyGetFek {
		return nil, status.Error(codes.PermissionDenied, "get FEK denied")
	}
	fek, _, err := UnwrapFEK(f.kek, req.WrappedFek, f.aad(req.VolumeUuid, req.DriveFileId, req.Inode, req.FekVersion))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unwrap: %v", err)
	}
	return &kmpb.GetFileFEKResponse{
		Fek:         fek,
		DriveFileId: req.DriveFileId,
		FekVersion:  req.FekVersion,
		KekVersion:  1,
		CryptoAlg:   "AES-256-GCM",
	}, nil
}

func (f *fakeKeyManager) FetchCompanyKEK(ctx context.Context, req *kmpb.FetchCompanyKEKRequest) (*kmpb.FetchCompanyKEKResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.CompanyId != testCompanyID {
		return nil, status.Error(codes.NotFound, "unknown company")
	}
	kek := make([]byte, len(f.kek))
	copy(kek, f.kek)
	return &kmpb.FetchCompanyKEKResponse{Kek: kek, KekVersion: 1}, nil
}

func (f *fakeKeyManager) Close() error { return nil }

// testUserMetadataKey carries the simulated OIDC 'sub' claim from client to server.
const testUserMetadataKey = "x-test-user"

// testIdentityInterceptor simulates the OIDC interceptor: it reads the user UUID
// from outgoing metadata and injects a verified IDToken into the server context,
// exactly what StrictUnaryInterceptorWithValidator does after token validation.
func testIdentityInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get(testUserMetadataKey); len(vals) > 0 && vals[0] != "" {
				ctx = oidc.WithIDToken(ctx, &oidc.IDToken{Subject: vals[0]})
			}
		}
		return handler(ctx, req)
	}
}

// userCtx attaches the simulated OIDC identity to an outgoing request context.
func userCtx(t *testing.T, ctx Context, userUUID string) Context {
	t.Helper()
	return WrapContext(metadata.AppendToOutgoingContext(ctx, testUserMetadataKey, userUUID))
}

// encryptTestEnv wires the production proxy stack in-process: redisMeta +
// MetaProxyServer + fake KeyManager behind a gRPC server, with a grpcMeta client.
// Mirrors cmd/meta_proxy.go wiring minus real OIDC/authz (identity via metadata).
type encryptTestEnv struct {
	rdb     *redis.Client
	meta    *redisMeta
	server  *MetaProxyServer
	keyMgr  *fakeKeyManager
	grpcSrv *grpc.Server
	addr    string
	client  Meta
}

func newEncryptTestEnv(t *testing.T, db int, encryptionEnabled bool) *encryptTestEnv {
	t.Helper()
	if os.Getenv("SKIP_NON_CORE") == "true" {
		t.Skipf("skip non-core test")
	}

	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: db})
	require.NoError(t, rdb.Ping(Background()).Err(), "local Redis must be running")
	require.NoError(t, rdb.FlushDB(Background()).Err())

	m, err := newRedisMeta("redis", fmt.Sprintf("127.0.0.1:6379/%d", db), testConfig())
	require.NoError(t, err)
	rm := m.(*redisMeta)

	format := &Format{
		Name:              "encrypt-test",
		UUID:              uuid.New().String(),
		DirStats:          true,
		EncryptionEnabled: encryptionEnabled,
	}
	require.NoError(t, rm.Init(format, true))
	_, err = rm.Load(true)
	require.NoError(t, err)

	server := NewMetaProxyServer(rm, 1000)
	env := &encryptTestEnv{rdb: rdb, meta: rm, server: server, keyMgr: newFakeKeyManager()}
	server.SetKeyManager(env.keyMgr)
	server.SetVolumeName(format.Name)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcSrv := grpc.NewServer(grpc.ChainUnaryInterceptor(testIdentityInterceptor()))
	pb.RegisterMetaServiceServer(grpcSrv, server)
	go func() { _ = grpcSrv.Serve(lis) }()

	client, err := newGRPCMeta("grpc", lis.Addr().String(), testConfig())
	require.NoError(t, err)

	env.grpcSrv = grpcSrv
	env.addr = lis.Addr().String()
	env.client = client

	t.Cleanup(func() {
		_ = client.Shutdown()
		grpcSrv.Stop()
		_ = rm.Shutdown()
		_ = rdb.Close()
	})
	return env
}

// TestEncryptedFullCycle (FR-TEST-8): create → attr carries the wrapped FEK;
// open → plaintext FEK delivered; reopen on the same client → KeyManager skipped
// (decision 3.1); a fresh client ("remount") → FEK re-issued by KeyManager.
func TestEncryptedFullCycle(t *testing.T) {
	env := newEncryptTestEnv(t, 4, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	var inode Ino
	var attr Attr
	st := env.client.Create(cctx, RootInode, "secret.exr", 0644, 022, syscall.O_CREAT|syscall.O_EXCL, &inode, &attr)
	require.Equal(t, syscall.Errno(0), st)

	// Create returned the crypto fields (FR-USR-1)
	require.True(t, attr.Encrypted)
	require.Len(t, attr.WrappedFek, agfkLen)
	require.NotEmpty(t, attr.DriveFileID)
	require.EqualValues(t, 1, attr.FekVersion)
	require.Equal(t, "AES-256-GCM", attr.CryptoAlg)
	require.Equal(t, 1, env.keyMgr.createCalls)
	require.Equal(t, "/secret.exr", env.keyMgr.lastCreatePath)

	// Open delivers the plaintext FEK (FR-USR-4)
	var openAttr Attr
	st = env.client.Open(cctx, inode, syscall.O_RDONLY, &openAttr)
	require.Equal(t, syscall.Errno(0), st)
	require.Len(t, openAttr.Fek, fekSize)
	require.EqualValues(t, 1, openAttr.FekVersion)
	require.Equal(t, 1, env.keyMgr.getFekCalls)

	// Reopen on the same client: FEK cache hit — KeyManager must not be called again.
	st = env.client.Open(cctx, inode, syscall.O_RDONLY, &openAttr)
	require.Equal(t, syscall.Errno(0), st)
	require.Len(t, openAttr.Fek, fekSize)
	require.Equal(t, 1, env.keyMgr.getFekCalls, "cache hit must not call KeyManager")

	// The delivered FEK is exactly the one inside the stored AGFK blob (AAD intact).
	format := env.meta.GetFormat()
	fek, _, err := UnwrapFEK(env.keyMgr.kek, attr.WrappedFek, FekAAD{
		VolumeUUID:  format.UUID,
		CompanyID:   testCompanyID,
		DriveFileID: attr.DriveFileID,
		Inode:       inode,
		FekVersion:  1,
	})
	require.NoError(t, err)
	require.Equal(t, openAttr.Fek, fek)

	// "Remount": a fresh client has an empty FEK cache, so the server re-issues.
	remounted, err := newGRPCMeta("grpc", env.addr, testConfig())
	require.NoError(t, err)
	defer remounted.Shutdown()

	var remountAttr Attr
	st = remounted.Open(cctx, inode, syscall.O_RDONLY, &remountAttr)
	require.Equal(t, syscall.Errno(0), st)
	require.Len(t, remountAttr.Fek, fekSize)
	require.Equal(t, 2, env.keyMgr.getFekCalls, "fresh client must trigger a KeyManager re-issue")
}

// TestUserWithoutPermission_Denied (FR-TEST-9): KeyManager denies the FEK → Open
// fails closed with EACCES and no key material reaches the client.
func TestUserWithoutPermission_Denied(t *testing.T) {
	env := newEncryptTestEnv(t, 5, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "denied.exr", 0644, 022, 0, &inode, &attr))

	env.keyMgr.mu.Lock()
	env.keyMgr.denyGetFek = true
	env.keyMgr.mu.Unlock()

	var openAttr Attr
	st := env.client.Open(cctx, inode, syscall.O_RDONLY, &openAttr)
	require.Equal(t, syscall.EACCES, st)
	require.Nil(t, openAttr.Fek)
	require.Equal(t, 1, env.keyMgr.getFekCalls, "the denied call must still be audited by KeyManager")
}

// TestCreateRollback (FR-USR-2): KeyManager denies key creation → Create returns
// EIO and the file is rolled back (absent from the directory listing).
func TestCreateRollback(t *testing.T) {
	env := newEncryptTestEnv(t, 6, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	env.keyMgr.mu.Lock()
	env.keyMgr.denyCreate = true
	env.keyMgr.mu.Unlock()

	var inode Ino
	var attr Attr
	st := env.client.Create(cctx, RootInode, "rollback.exr", 0644, 022, 0, &inode, &attr)
	require.Equal(t, syscall.EIO, st)

	var entries []*Entry
	require.Equal(t, syscall.Errno(0), env.client.Readdir(cctx, RootInode, 1, &entries))
	for _, e := range entries {
		require.NotEqual(t, "rollback.exr", string(e.Name))
	}
}

// TestOwnerBypass (FR-TEST-18): a user with access (the "owner" case — allow on
// everything) gets the FEK and can open the file.
func TestOwnerBypass(t *testing.T) {
	env := newEncryptTestEnv(t, 7, true)
	ctx := Background()
	owner := uuid.New().String()
	cctx := userCtx(t, ctx, owner)

	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "owner.exr", 0644, 022, 0, &inode, &attr))
	require.True(t, attr.Encrypted)

	var openAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Open(cctx, inode, syscall.O_RDWR, &openAttr))
	require.Len(t, openAttr.Fek, fekSize)
	require.Equal(t, 1, env.keyMgr.getFekCalls)
	require.Equal(t, "/owner.exr", env.keyMgr.lastGetFekPath)
}

// TestLegacyVolume_Unchanged: EncryptionEnabled=false → Create/Open work exactly
// as before and KeyManager is never called.
func TestLegacyVolume_Unchanged(t *testing.T) {
	env := newEncryptTestEnv(t, 1, false)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "plain.txt", 0644, 022, 0, &inode, &attr))
	require.False(t, attr.Encrypted)
	require.Empty(t, attr.WrappedFek)

	var openAttr Attr
	require.Equal(t, syscall.Errno(0), env.client.Open(cctx, inode, syscall.O_RDWR, &openAttr))
	require.Nil(t, openAttr.Fek)

	require.Equal(t, 0, env.keyMgr.createCalls, "legacy volume must not call CreateFileKey")
	require.Equal(t, 0, env.keyMgr.getFekCalls, "legacy volume must not call GetFileFEK")
}

// TestResolveFileKey (task 5.2): ResolveFileKey returns the plaintext FEK and the
// drive file ID and writes the client's FEK LRU; the returned key must be exactly
// the one inside the stored AGFK blob (AAD intact).
func TestResolveFileKey(t *testing.T) {
	env := newEncryptTestEnv(t, 0, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "resolve.exr", 0644, 022, 0, &inode, &attr))
	require.True(t, attr.Encrypted)

	gm, ok := env.client.(*grpcMeta)
	require.True(t, ok)

	fek, driveFileID, ver, err := gm.ResolveFileKey(cctx, inode)
	require.NoError(t, err)
	require.Len(t, fek, fekSize)
	require.Equal(t, attr.DriveFileID, driveFileID)
	require.EqualValues(t, 1, ver)
	require.Equal(t, 1, env.keyMgr.getFekCalls)

	// The returned FEK is exactly the one inside the stored AGFK blob.
	format := env.meta.GetFormat()
	want, _, err := UnwrapFEK(env.keyMgr.kek, attr.WrappedFek, FekAAD{
		VolumeUUID:  format.UUID,
		CompanyID:   testCompanyID,
		DriveFileID: attr.DriveFileID,
		Inode:       inode,
		FekVersion:  1,
	})
	require.NoError(t, err)
	require.Equal(t, want, fek)

	// The client LRU holds the resolved key (task 5.2).
	e, ok := gm.fekCache.Get(uint64(inode))
	require.True(t, ok)
	require.Equal(t, fek, e.fek)
	require.EqualValues(t, 1, e.version)
}

// TestResolveFileKey_Denied (task 5.2): KeyManager denies GetFileFEK → the client
// gets EACCES and no key material, and nothing is cached (fail-closed). This env
// has no authz interceptor; the denial is simulated at the KeyManager level, as in
// TestUserWithoutPermission_Denied.
func TestResolveFileKey_Denied(t *testing.T) {
	env := newEncryptTestEnv(t, 12, true)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "denied-resolve.exr", 0644, 022, 0, &inode, &attr))

	env.keyMgr.mu.Lock()
	env.keyMgr.denyGetFek = true
	env.keyMgr.mu.Unlock()

	gm, ok := env.client.(*grpcMeta)
	require.True(t, ok)

	fek, _, _, err := gm.ResolveFileKey(cctx, inode)
	require.Error(t, err)
	require.Nil(t, fek)
	require.Equal(t, syscall.EACCES, errno(err))
	_, cached := gm.fekCache.Get(uint64(inode))
	require.False(t, cached, "a denied resolve must not cache anything")
}

// TestResolveFileKey_Legacy (task 5.2): a non-encrypted file answers
// encrypted=false with no FEK and KeyManager is never called.
func TestResolveFileKey_Legacy(t *testing.T) {
	env := newEncryptTestEnv(t, 15, false)
	ctx := Background()
	user := uuid.New().String()
	cctx := userCtx(t, ctx, user)

	var inode Ino
	var attr Attr
	require.Equal(t, syscall.Errno(0), env.client.Create(cctx, RootInode, "plain-resolve.txt", 0644, 022, 0, &inode, &attr))
	require.False(t, attr.Encrypted)

	gm, ok := env.client.(*grpcMeta)
	require.True(t, ok)

	fek, driveFileID, ver, err := gm.ResolveFileKey(cctx, inode)
	require.NoError(t, err)
	require.Nil(t, fek)
	require.Empty(t, driveFileID)
	require.EqualValues(t, 0, ver)
	require.Equal(t, 0, env.keyMgr.getFekCalls, "legacy file must not call GetFileFEK")
}
