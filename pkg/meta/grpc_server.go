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
	"sync"
	"syscall"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dirHandlerEntry pairs a DirHandler with its inode for authz resolution and
// with the session that opened it: handles are owned by their proxy session
// (fix-meta-proxy-per-session-chroot) — another session sees EBADF, and the
// session end closes every handle it owns.
type dirHandlerEntry struct {
	handler DirHandler
	inode   Ino
	sess    *proxySession // owning session; lives as long as the handle

	// Authz mode serves listings from a stable filtered snapshot instead of
	// the DirHandler's offset machinery: filtering after handler.List()
	// desyncs the FUSE offset protocol (the handler cursor advances by
	// unfiltered counts while the kernel advances by filtered counts), which
	// re-serves earlier entries as duplicates.
	mu         sync.Mutex
	authzList  []*Entry
	authzReady bool
}

// MetaProxyServer implements the MetaService gRPC server
type MetaProxyServer struct {
	pb.UnimplementedMetaServiceServer
	// StsProxyService pass-through (task 7.12, ADR-003) is served by the same
	// server: GetSTSCredentials in grpc_server_sts.go.
	pb.UnimplementedStsProxyServiceServer

	// Session-aware engine pool (fix-meta-proxy-per-session-chroot): handlers
	// reach engines only through engineFor (I8); the base engine lives only in
	// pathResolver (its constructor argument).
	pool         *enginePool
	sessions     *sessionRegistry
	maxSessions  int
	authzEnabled bool

	// DirHandler id allocation. The handles themselves live in the owning
	// session (proxySession.handles) — there is no server-wide registry.
	mu         sync.Mutex
	nextHandle uint64

	// Authorization (optional)
	authzInterceptor *AuthzInterceptor
	// authzMode: the authz interceptor is the only access gate — engine-level
	// POSIX permission checks are skipped (see metaCtx). The client-carried
	// POSIX uid is untrusted input, so enforcing stored ownership against it
	// provides no security and only produces false denials.
	authzMode      bool
	inodePathCache *InodePathCache
	// pathResolver reconstructs inode→path mappings on cache miss (restart,
	// eviction) through direct engine reads — the fix for proxy-restart
	// session churn (fix-meta-proxy-session-churn, design D1).
	pathResolver *pathResolver

	// Encryption (optional): KeyManager client for per-file FEKs and the volume
	// name used to resolve the companies prefix on the platform side.
	keyManager KeyManagerClient
	volumeName string

	// rotateRate is the per-call rate limit (files/second) of the batch FEK
	// rotation RPC (offboarding, task 7.6).
	rotateRate int
}

// fileCryptoSetter is implemented by metadata engines that can persist per-file
// crypto metadata (redisMeta). Checked via type assertion so the Meta interface
// stays unchanged for other engines.
type fileCryptoSetter interface {
	SetFileCrypto(ctx Context, inode Ino, c *FileCrypto) syscall.Errno
}

// NewMetaProxyServer creates a new MetaProxyServer.
// cacheMaxSize sets the maximum inode→path mappings (0 = unlimited). m is the
// base engine, kept only for pathResolver (inode→path recovery); newEngine is
// the factory the pool uses to build chrooted engines (NewClient + Load(true),
// no session) — it may be nil only when no RPC ever reaches engineFor.
func NewMetaProxyServer(m Meta, cacheMaxSize int, newEngine EngineFactory) *MetaProxyServer {
	s := &MetaProxyServer{
		pool:           newEnginePool(newEngine),
		sessions:       newSessionRegistry(),
		inodePathCache: NewInodePathCache(cacheMaxSize),
		rotateRate:     defaultRotateRate,
	}
	s.pathResolver = newPathResolver(m, s.inodePathCache)
	return s
}

// SetMaxSessions caps concurrent proxy sessions (gRPC connections); 0 (the
// default) disables the limit.
func (s *MetaProxyServer) SetMaxSessions(n int) {
	s.maxSessions = n
	s.sessions.mu.Lock()
	s.sessions.max = n
	s.sessions.mu.Unlock()
}

// SetAuthzInterceptor configures the authorization interceptor and wires the
// server's path recovery into it (fix-meta-proxy-session-churn D1).
// Called during server setup in cmd/meta_proxy.go (before gRPC Serve).
func (s *MetaProxyServer) SetAuthzInterceptor(ai *AuthzInterceptor) {
	ai.SetPathResolver(s.pathResolver)
	s.authzInterceptor = ai
}

// SetAuthzMode enables authz-first access control: engine-level POSIX
// permission checks are skipped (metaCtx builds contexts with
// CheckPermission()==false) because the authz interceptor is the only access
// gate. Called during server setup in cmd/meta_proxy.go when --authz-service
// is configured.
func (s *MetaProxyServer) SetAuthzMode(enabled bool) {
	s.authzMode = enabled
	s.authzEnabled = enabled
}

// SetKeyManager configures the KeyManager client for per-file FEK encryption.
// Called during server setup in cmd/meta_proxy.go (before gRPC Serve).
func (s *MetaProxyServer) SetKeyManager(km KeyManagerClient) {
	s.keyManager = km
}

// SetVolumeName sets the JuiceFS volume name passed to KeyManager requests
// (the platform resolves the companies prefix from it).
func (s *MetaProxyServer) SetVolumeName(name string) {
	s.volumeName = name
}

// SetRotateRate overrides the batch rotation rate limit in files/second
// (task 7.6; default 10). Values <= 0 disable the pacing.
func (s *MetaProxyServer) SetRotateRate(rate int) {
	if rate > 0 {
		s.rotateRate = rate
	}
}

// encryptionEnabled reports whether per-file FEK encryption is on for the
// session engine's volume.
func (s *MetaProxyServer) encryptionEnabled(eng Engine) bool {
	return eng.GetFormat().EncryptionEnabled
}

// InodePathCache returns the inode→path cache (for cmd/meta_proxy.go).
func (s *MetaProxyServer) InodePathCache() *InodePathCache {
	return s.inodePathCache
}

// ResolveHandle returns the inode for a DirHandler handle (for authz
// interceptor). Handles live in their owning session, and this scan carries no
// session of its own — that is fine: handle ids are unique server-wide.
func (s *MetaProxyServer) ResolveHandle(handle uint64) (Ino, bool) {
	for _, sess := range s.sessions.snapshot() {
		sess.mu.Lock()
		entry, ok := sess.handles[handle]
		sess.mu.Unlock()
		if ok {
			return entry.inode, true
		}
	}
	return 0, false
}

// helper to convert Context from proto
func (s *MetaProxyServer) metaCtx(ctx context.Context, ctx2 *pb.MetaContext) Context {
	if ctx2 == nil || (ctx2.Uid == 0 && ctx2.Gid == 0 && len(ctx2.Gids) == 0 && ctx2.Pid == 0) {
		if s.authzMode {
			return WrapWithCancelSkipPermCheck(ctx, 0, 0, []uint32{0})
		}
		return Background()
	}
	gids := ctx2.Gids
	if len(gids) == 0 {
		gids = []uint32{ctx2.Gid}
	}
	if s.authzMode {
		return WrapWithCancelSkipPermCheck(ctx, ctx2.Pid, ctx2.Uid, gids)
	}
	return WrapWithCancel(ctx, ctx2.Pid, ctx2.Uid, gids)
}

// engineFor is the only path from the RPC handlers to a session's engine
// (I1, I8): it resolves the connection session and lazily acquires the root
// engine ("") when the session has none yet.
func (s *MetaProxyServer) engineFor(ctx context.Context) (Engine, error) {
	sess, err := s.sessionFor(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.sessionEntry(sess, ctx)
	if err != nil {
		return nil, err
	}
	return e.engine, nil
}

// sessionEntry returns the session's pooled engine, acquiring and binding the
// root engine ("") on first use.
func (s *MetaProxyServer) sessionEntry(sess *proxySession, ctx context.Context) (*poolEntry, error) {
	sess.mu.Lock()
	e := sess.entry
	sess.mu.Unlock()
	if e != nil {
		return e, nil
	}
	ne, errno, err := s.pool.acquire(ctx, "", false)
	if err != nil {
		// Audit event for engine unavailability outside Chroot (spec
		// "Engine unavailable outside Chroot").
		logger.Warnf("meta-proxy session engine failed id=%d err=%v", sess.id, err)
		return nil, err
	}
	if errno != 0 {
		// Unreachable for the root key (create chroots only non-empty keys) and
		// the failed reference is already settled away by acquire — kept as a
		// guard so the caller never sees a chrooted-by-mistake engine.
		return nil, status.Errorf(codes.Internal, "meta-proxy: unexpected chroot errno %d on the root engine", errno)
	}
	s.bind(sess, ne)
	return ne, nil
}

// sessionEntryFor is sessionFor + sessionEntry for handlers that need the
// pool entry itself (e.g. the saved engine NewSession response).
func (s *MetaProxyServer) sessionEntryFor(ctx context.Context) (*poolEntry, error) {
	sess, err := s.sessionFor(ctx)
	if err != nil {
		return nil, err
	}
	return s.sessionEntry(sess, ctx)
}

// CloseSessions ends every live session and tears the pool down (execution
// flow step 7, graceful stop). ctx bounds the engine teardown budget.
func (s *MetaProxyServer) CloseSessions(ctx context.Context) {
	for _, sess := range s.sessions.snapshot() {
		s.endSession(sess)
	}
	s.pool.shutdown(ctx)
}
