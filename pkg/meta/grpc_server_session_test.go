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

// Session/engine-pool behavior of the meta proxy (fix-meta-proxy-per-session-
// chroot, tasks 1.5 + 2.3). Every test drives a real gRPC server on a real TCP
// listener at 127.0.0.1:0 — the session is bound by the connection's remote
// address (design A1), which bufconn would not exercise.

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	authzpb "github.com/juicedata/juicefs/pkg/meta/authz_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// sessionContextWith returns a request context carrying the given session —
// what sessionStatsHandler.TagConn does for live connections.
func sessionContextWith(sess *proxySession) context.Context {
	return context.WithValue(context.Background(), sessionContextKey{}, sess)
}

// --- Direct-handler harness (fix-meta-proxy-per-session-chroot, task 7.4):
// tests that call MetaProxyServer handlers directly need a live session in the
// context (handlers fail closed without one, I1) and a pool factory that
// serves the session's engine. ---

// directSessionPeerSeq numbers the synthetic peer addresses so sessions of one
// test server never collide in the registry.
var directSessionPeerSeq atomic.Uint64

// directSessionAddr is a registry-unique synthetic peer address.
func directSessionAddr() net.Addr {
	return testAddr(fmt.Sprintf("direct-session:%d", directSessionPeerSeq.Add(1)))
}

// testAddr is a net.Addr backed by a plain string (only String() is used, by
// the session registry key).
type testAddr string

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }

// passThroughFactory hands out m for every pool creation: the direct-handler
// pattern where every session shares the same engine instance — the pre-pool
// "one base engine" semantics these tests were written against.
func passThroughFactory(m Meta) EngineFactory {
	return func() (Engine, error) {
		return m, nil
	}
}

// newDirectServer builds a MetaProxyServer around m for direct handler calls:
// m doubles as the pooled engine (passThroughFactory), so engineFor serves the
// session's RPCs from the same fake/real engine the test controls. Every
// session is ended and the pool drained at test cleanup.
func newDirectServer(t *testing.T, m Meta, cacheMaxSize int) *MetaProxyServer {
	t.Helper()
	srv := NewMetaProxyServer(m, cacheMaxSize, passThroughFactory(m))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.CloseSessions(ctx)
	})
	return srv
}

// directSessionCtx registers a live session for srv — exactly what
// sessionStatsHandler.TagConn does for a real connection — and returns a
// request context carrying it. The session (and its engine reference and dir
// handles) is ended at test cleanup.
func directSessionCtx(t *testing.T, srv *MetaProxyServer) context.Context {
	t.Helper()
	sess, err := srv.sessions.openSession(srv, directSessionAddr())
	require.NoError(t, err)
	t.Cleanup(func() { srv.endSession(sess) })
	return sessionContextWith(sess)
}

// poolLifecycleMeta supplies the engine-lifecycle no-ops the pool exercises at
// create/teardown (NewSession/CloseSession/Shutdown) for hand-written fake
// engines. Fakes embed it instead of the bare Meta interface so unstubbed
// Meta methods still panic while the pool lifecycle is silently served.
type poolLifecycleMeta struct{ Meta }

func (poolLifecycleMeta) NewSession(bool) error { return nil }
func (poolLifecycleMeta) CloseSession() error   { return nil }
func (poolLifecycleMeta) Shutdown() error       { return nil }

// poolFakeEngine is a recording fake engine: it embeds Meta (unstubbed methods
// panic — the tests only exercise lifecycle calls) and records the calls the
// pool is allowed to make: Chroot/NewSession/CloseSession/Shutdown/Lookup.
type poolFakeEngine struct {
	Meta

	mu         sync.Mutex
	chroots    []string          // recorded Chroot subdir arguments
	newSess    int               // NewSession calls (exactly 1 per engine, I6)
	closed     int               // CloseSession calls (≤1 per engine, I6)
	shutdowns  int               // Shutdown calls (exactly 1 per engine, I5)
	lookups    []string          // recorded Lookup names
	dirEntries map[string]Ino    // directory names visible under the root
	handlers   []*fakeDirHandler // dir handlers handed out via NewDirHandler
	sid        uint64
}

func (e *poolFakeEngine) Chroot(_ Context, subdir string) syscall.Errno {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.chroots = append(e.chroots, subdir)
	return 0
}

func (e *poolFakeEngine) NewSession(bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.newSess++
	e.sid += 1000
	return nil
}

func (e *poolFakeEngine) CloseSession() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed++
	return nil
}

func (e *poolFakeEngine) Shutdown() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdowns++
	return nil
}

// fakeDirHandler is a minimal DirHandler for the session-binding tests: one
// recorded entry and a Close counter (the session end must close its handles).
type fakeDirHandler struct {
	mu      sync.Mutex
	inode   Ino
	entries []*Entry
	closed  int
}

func (h *fakeDirHandler) List(_ Context, offset int) ([]*Entry, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if offset >= len(h.entries) {
		return nil, 0
	}
	return h.entries[offset:], 0
}

func (h *fakeDirHandler) Insert(Ino, string, *Attr) {}

func (h *fakeDirHandler) Delete(string) {}

func (h *fakeDirHandler) Read(int) {}

func (h *fakeDirHandler) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed++
}

func (h *fakeDirHandler) closeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// NewDirHandler hands out a fresh fakeDirHandler (recorded for assertions).
func (e *poolFakeEngine) NewDirHandler(_ Context, inode Ino, _ bool, _ []*Entry) (DirHandler, syscall.Errno) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := &fakeDirHandler{
		inode:   inode,
		entries: []*Entry{{Inode: inode, Name: []byte("entry"), Attr: &Attr{Typ: TypeFile}}},
	}
	e.handlers = append(e.handlers, h)
	return h, 0
}

// dirHandlerCloses sums the Close counter over every handler the engine gave
// out (session end must close exactly the handles its session owns).
func (e *poolFakeEngine) dirHandlerCloses() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := 0
	for _, h := range e.handlers {
		total += h.closeCount()
	}
	return total
}

// Lookup serves a minimal single-level directory map so lookupSubdir can walk
// one component; unknown names answer ENOENT.
func (e *poolFakeEngine) Lookup(_ Context, parent Ino, name string, inode *Ino, attr *Attr, _ bool) syscall.Errno {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lookups = append(e.lookups, name)
	if parent != RootInode {
		return syscall.ENOTDIR
	}
	ino, ok := e.dirEntries[name]
	if !ok {
		return syscall.ENOENT
	}
	*inode = ino
	*attr = Attr{Typ: TypeDirectory, Nlink: 2}
	return 0
}

func (e *poolFakeEngine) calls() (chroots int, newSess, closed, shutdowns int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.chroots), e.newSess, e.closed, e.shutdowns
}

// chrootList returns a copy of the recorded Chroot subdir arguments.
func (e *poolFakeEngine) chrootList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.chroots...)
}

// poolTestFactory builds fake engines with a shared creation counter; the
// first `failNext` creations fail (engine_error path).
type poolTestFactory struct {
	mu       sync.Mutex
	made     []*poolFakeEngine
	failNext int
}

func (f *poolTestFactory) newEngine() (Engine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return nil, syscall.EIO
	}
	eng := &poolFakeEngine{}
	f.made = append(f.made, eng)
	return eng, nil
}

func (f *poolTestFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.made)
}

func (f *poolTestFactory) last() *poolFakeEngine {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.made) == 0 {
		return nil
	}
	return f.made[len(f.made)-1]
}

// sessionTestEnv serves one MetaProxyServer over a real TCP listener with the
// session StatsHandler + interceptors installed exactly like cmd/meta_proxy.go.
type sessionTestEnv struct {
	srv     *MetaProxyServer
	grpcSrv *grpc.Server
	lis     net.Listener
	factory *poolTestFactory
}

func newSessionTestEnv(t *testing.T, maxSessions int) *sessionTestEnv {
	t.Helper()
	factory := &poolTestFactory{}
	srv := NewMetaProxyServer(nil, 0, factory.newEngine)
	if maxSessions > 0 {
		srv.SetMaxSessions(maxSessions)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer(
		grpc.StatsHandler(srv.SessionStatsHandler()),
		grpc.ChainUnaryInterceptor(srv.SessionUnaryInterceptor()),
		grpc.ChainStreamInterceptor(srv.SessionStreamInterceptor()),
	)
	pb.RegisterMetaServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return &sessionTestEnv{srv: srv, grpcSrv: gs, lis: lis, factory: factory}
}

// dial opens one client connection = one proxy session (first RPC attaches it).
func (env *sessionTestEnv) dial(t *testing.T) (pb.MetaServiceClient, *grpc.ClientConn) {
	t.Helper()
	cc, err := grpc.NewClient(env.lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cc.Close() })
	return pb.NewMetaServiceClient(cc), cc
}

func (env *sessionTestEnv) sessionCount() int {
	return len(env.srv.sessions.snapshot())
}

// engineOf returns the pooled engine registered under key (nil if absent).
func (env *sessionTestEnv) engineOf(key string) Engine {
	env.srv.pool.mu.Lock()
	defer env.srv.pool.mu.Unlock()
	e := env.srv.pool.entries[key]
	if e == nil {
		return nil
	}
	return e.engine
}

// waitCond polls cond until it holds or the timeout elapses (teardown and
// connection events are asynchronous).
func waitCond(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

func chrootOK(t *testing.T, cl pb.MetaServiceClient, subdir string) {
	t.Helper()
	resp, err := cl.Chroot(context.Background(), &pb.ChrootRequest{Subdir: subdir})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.GetErrno(), "chroot %q rejected", subdir)
}

// TestMetaProxySessionLifecycleOnConnection: the session is born with the
// connection's first RPC and dies with the RPC CloseSession (D1, D6).
func TestMetaProxySessionLifecycleOnConnection(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	cl, _ := env.dial(t)

	require.Equal(t, 0, env.sessionCount(), "no session before the first RPC")

	resp, err := cl.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err)
	require.Equal(t, uint32(0), resp.GetErrno())
	require.Equal(t, 1, env.sessionCount(), "first RPC opens the session")
	require.Equal(t, 1, env.factory.count(), "lazy root engine created")

	// The client's NewSession gets a clone of the engine's saved response (D4).
	resp2, err := cl.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err)
	require.Equal(t, resp.GetSid(), resp2.GetSid())
	eng := env.engineOf("")
	require.NotNil(t, eng)
	_, newSess, _, _ := eng.(*poolFakeEngine).calls()
	require.Equal(t, 1, newSess, "exactly one engine session (I6)")

	// RPC CloseSession ends only the sender's session (D6); the engine session
	// is closed once at teardown (I5).
	_, err = cl.CloseSession(context.Background(), &pb.CloseSessionRequest{})
	require.NoError(t, err)
	require.Equal(t, 0, env.sessionCount())
	waitCond(t, 2*time.Second, "root engine not torn down", func() bool {
		return env.engineOf("") == nil
	})
	fake := env.factory.last()
	waitCond(t, 2*time.Second, "teardown calls not recorded", func() bool {
		_, _, closed, shutdowns := fake.calls()
		return closed == 1 && shutdowns == 1
	})
	_, newSess, closed, shutdowns := fake.calls()
	require.Equal(t, 1, newSess)
	require.Equal(t, 1, closed)
	require.Equal(t, 1, shutdowns)
}

// TestMetaProxyNoSessionFailsClosed: a handler called without a session (no
// StatsHandler context) fails closed and never touches the pool (I1).
func TestMetaProxyNoSessionFailsClosed(t *testing.T) {
	factory := &poolTestFactory{}
	srv := NewMetaProxyServer(nil, 0, factory.newEngine)

	resp, err := srv.GetAttr(context.Background(), &pb.GetAttrRequest{Inode: uint64(RootInode)})
	require.Error(t, err, "no session — fail closed")
	require.Nil(t, resp)
	require.Equal(t, 0, factory.count(), "no engine created without a session")
	require.Equal(t, 0, len(srv.pool.entries))

	// An ended session is equally rejected (sessionFor does not hand out
	// terminated sessions).
	sess := &proxySession{srv: srv, id: 1, peer: "10.0.0.1:1", start: time.Now()}
	sess.ended = true
	_, err = srv.sessionFor(sessionContextWith(sess))
	require.Error(t, err, "ended session must not serve RPCs")
}

// TestMetaProxySessionCleanupOnConnEnd: closing the TCP connection ends the
// session and tears the engine down exactly once (execution flow step 6).
func TestMetaProxySessionCleanupOnConnEnd(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	cl, cc := env.dial(t)

	_, err := cl.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err)
	require.Equal(t, 1, env.factory.count())
	require.Equal(t, 1, env.sessionCount())

	require.NoError(t, cc.Close())
	waitCond(t, 2*time.Second, "session not cleaned up on conn end", func() bool {
		return env.sessionCount() == 0
	})
	waitCond(t, 2*time.Second, "teardown calls not recorded", func() bool {
		_, _, closed, shutdowns := env.factory.last().calls()
		return closed == 1 && shutdowns == 1
	})
	_, _, closed, shutdowns := env.factory.last().calls()
	require.Equal(t, 1, closed, "exactly one engine CloseSession (I6)")
	require.Equal(t, 1, shutdowns, "exactly one engine Shutdown (I5)")
}

// TestMetaProxyMaxSessions: connections beyond the limit are rejected at open
// and their RPCs fail closed with ResourceExhausted.
func TestMetaProxyMaxSessions(t *testing.T) {
	env := newSessionTestEnv(t, 1)
	clA, _ := env.dial(t)
	_, err := clA.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err)
	require.Equal(t, 1, env.sessionCount())

	clB, _ := env.dial(t)
	_, err = clB.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.Error(t, err)
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "rejected at the session limit")
	require.Equal(t, 1, env.sessionCount(), "rejected connection holds no session")
	require.Equal(t, 1, env.factory.count(), "no extra engine for the rejected session")
}

// TestMetaProxyEngineRefcountTeardown: two sessions share one engine; the
// teardown fires only when the last reference goes away (I3, I5).
func TestMetaProxyEngineRefcountTeardown(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	clA, ccA := env.dial(t)
	clB, _ := env.dial(t)

	chrootOK(t, clA, "a")
	chrootOK(t, clB, "a")
	require.Equal(t, 1, env.factory.count(), "one engine per subdir key")
	eng := env.engineOf("a")
	require.NotNil(t, eng)

	require.NoError(t, ccA.Close())
	waitCond(t, 2*time.Second, "session A not ended", func() bool {
		return env.sessionCount() == 1
	})
	_, _, _, shutdowns := eng.(*poolFakeEngine).calls()
	require.Equal(t, 0, shutdowns, "engine stays while session B holds it")

	_, err := clB.CloseSession(context.Background(), &pb.CloseSessionRequest{})
	require.NoError(t, err)
	waitCond(t, 2*time.Second, "engine not torn down after last session", func() bool {
		_, _, closed, shutdowns := eng.(*poolFakeEngine).calls()
		return env.engineOf("a") == nil && closed == 1 && shutdowns == 1
	})
	fake := eng.(*poolFakeEngine)
	_, newSess, closed, shutdowns := fake.calls()
	require.Equal(t, 1, newSess)
	require.Equal(t, 1, closed)
	require.Equal(t, 1, shutdowns)
}

// TestMetaProxyEngineSharedPerSubdir: sessions chrooted to the same subdir
// share one pooled engine; different subdirs get different engines (D2).
func TestMetaProxyEngineSharedPerSubdir(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	clA, _ := env.dial(t)
	clB, _ := env.dial(t)
	clC, _ := env.dial(t)

	chrootOK(t, clA, "a")
	engA1 := env.engineOf("a")
	chrootOK(t, clB, "a")
	engA2 := env.engineOf("a")
	require.Same(t, engA1, engA2, "same subdir — same pooled engine")
	require.Equal(t, 1, env.factory.count())

	chrootOK(t, clC, "b")
	engB := env.engineOf("b")
	require.NotNil(t, engB)
	require.NotSame(t, engA1, engB, "different subdir — different engine")
	require.Equal(t, 2, env.factory.count())
}

// TestMetaProxyEngineReconnectKeepsEngine: a reconnecting client re-attaches
// to the still-referenced engine (execution flow step 5) — no recreation while
// another session holds the engine.
func TestMetaProxyEngineReconnectKeepsEngine(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	clA, ccA := env.dial(t)
	clB, _ := env.dial(t)

	chrootOK(t, clA, "a")
	chrootOK(t, clB, "a")
	eng := env.engineOf("a")
	require.NotNil(t, eng)
	require.Equal(t, 1, env.factory.count())

	require.NoError(t, ccA.Close())
	waitCond(t, 2*time.Second, "session A not ended", func() bool {
		return env.sessionCount() == 1
	})
	_, _, _, shutdowns := eng.(*poolFakeEngine).calls()
	require.Equal(t, 0, shutdowns, "engine survives while session B holds it")

	clC, _ := env.dial(t)
	chrootOK(t, clC, "a")
	require.Equal(t, 1, env.factory.count(), "no new engine on reconnect")
	require.Same(t, eng, env.engineOf("a"), "reconnect re-attaches to the same engine")
}

// TestMetaProxyEngineReacquireAfterTeardown: after the last session left, the
// key is served by a freshly created engine (D7) — never by the torn-down one.
func TestMetaProxyEngineReacquireAfterTeardown(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	clA, ccA := env.dial(t)
	chrootOK(t, clA, "a")
	eng1 := env.engineOf("a")
	require.NotNil(t, eng1)

	require.NoError(t, ccA.Close())
	waitCond(t, 2*time.Second, "engine not torn down", func() bool {
		_, _, _, shutdowns := eng1.(*poolFakeEngine).calls()
		return shutdowns == 1
	})

	clB, _ := env.dial(t)
	chrootOK(t, clB, "a")
	eng2 := env.engineOf("a")
	require.NotNil(t, eng2)
	require.NotSame(t, eng1, eng2, "torn-down entry is not reused (D7)")
	require.Equal(t, 2, env.factory.count())
	_, newSess, closed, _ := eng2.(*poolFakeEngine).calls()
	require.Equal(t, 1, newSess, "fresh engine opens its own session")
	require.Equal(t, 0, closed)
}

// TestMetaProxyEngineBoundedBySessions: engines exist only while sessions
// reference them (D8) — rejected sessions create nothing, ended sessions tear
// their engines down.
func TestMetaProxyEngineBoundedBySessions(t *testing.T) {
	env := newSessionTestEnv(t, 2)
	clA, ccA := env.dial(t)
	clB, ccB := env.dial(t)
	chrootOK(t, clA, "a")
	chrootOK(t, clB, "b")
	require.Equal(t, 2, env.factory.count())

	clC, _ := env.dial(t)
	_, err := clC.Chroot(context.Background(), &pb.ChrootRequest{Subdir: "c"})
	require.Error(t, err)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Equal(t, 2, env.factory.count(), "rejected session creates no engine")

	require.NoError(t, ccA.Close())
	require.NoError(t, ccB.Close())
	waitCond(t, 2*time.Second, "engines not torn down after sessions ended", func() bool {
		env.srv.pool.mu.Lock()
		left := len(env.srv.pool.entries)
		env.srv.pool.mu.Unlock()
		if left != 0 {
			return false
		}
		_, _, closedA, shutdownA := env.factory.made[0].calls()
		_, _, closedB, shutdownB := env.factory.made[1].calls()
		return closedA == 1 && shutdownA == 1 && closedB == 1 && shutdownB == 1
	})
	fakeA := env.factory.made[0]
	fakeB := env.factory.made[1]
	_, _, closedA, shutdownA := fakeA.calls()
	_, _, closedB, shutdownB := fakeB.calls()
	require.Equal(t, 1, closedA)
	require.Equal(t, 1, shutdownA)
	require.Equal(t, 1, closedB)
	require.Equal(t, 1, shutdownB)
}

// TestMetaProxyEnginePoolGracefulShutdown: CloseSessions ends every session
// and tears every pooled engine down; later RPCs fail closed (execution flow
// step 7 — the cmd graceful path).
func TestMetaProxyEnginePoolGracefulShutdown(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	clA, _ := env.dial(t)
	clB, _ := env.dial(t)
	chrootOK(t, clA, "a")
	_, err := clB.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err)
	require.Equal(t, 2, env.factory.count())
	require.Equal(t, 2, env.sessionCount())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	env.srv.CloseSessions(ctx)

	require.Equal(t, 0, env.sessionCount(), "all sessions ended")
	require.Equal(t, 0, len(env.srv.pool.entries), "pool drained")
	waitCond(t, 2*time.Second, "pool teardown calls not recorded", func() bool {
		for _, fake := range env.factory.made {
			_, _, closed, shutdowns := fake.calls()
			if closed != 1 || shutdowns != 1 {
				return false
			}
		}
		return true
	})
	for i, fake := range env.factory.made {
		_, _, closed, shutdowns := fake.calls()
		require.Equal(t, 1, closed, "engine %d closed once", i)
		require.Equal(t, 1, shutdowns, "engine %d shut down once", i)
	}

	// A late RPC on a still-open connection fails closed: its session is gone.
	_, err = clA.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.Error(t, err, "no live session after CloseSessions")
	_, _, err = env.srv.pool.acquire(context.Background(), "a", false)
	require.Error(t, err, "pool refuses acquisitions after shutdown")
}

// --- Stage 3: the full Chroot handler (fix-meta-proxy-per-session-chroot,
// tasks 3.1–3.3). Idempotent replay and the service identity use fake engines;
// per-session isolation runs on real redis engines below. ---

// TestMetaProxyChrootIdempotency (spec "Per-session chroot isolation"): the
// outcome of the session's first Chroot is final — the same subdir replays the
// recorded errno without touching any engine; a different subdir fails with
// FailedPrecondition and creates nothing.
func TestMetaProxyChrootIdempotency(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	cl, _ := env.dial(t)

	chrootOK(t, cl, "companies/acme")
	eng := env.engineOf("companies/acme")
	require.NotNil(t, eng)

	// Same subdir, with separator noise that normalizes away: recorded
	// outcome, engines untouched.
	chrootOK(t, cl, "/companies//acme/")
	chroots, _, _, _ := eng.(*poolFakeEngine).calls()
	require.Equal(t, 1, chroots, "replay must not chroot the engine again")
	require.Equal(t, 1, env.factory.count(), "replay must not create engines")

	// Another subdir: FailedPrecondition, no engine for it, engines untouched.
	_, err := cl.Chroot(context.Background(), &pb.ChrootRequest{Subdir: "companies/globex"})
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Nil(t, env.engineOf("companies/globex"), "the rejected subdir creates no engine")
	require.Equal(t, 1, env.factory.count())
	chroots, _, _, _ = eng.(*poolFakeEngine).calls()
	require.Equal(t, 1, chroots)
}

// TestMetaProxyChrootServiceIdentity (spec "Service identity Chroot"): a
// service identity skips the authz-mode target pre-check — a missing target is
// created by the engine Chroot, semantics as today (D9) — and the audit user
// classifies as service/sub/- (D10).
func TestMetaProxyChrootServiceIdentity(t *testing.T) {
	factory := &poolTestFactory{}
	srv := NewMetaProxyServer(nil, 0, factory.newEngine)
	srv.SetAuthzMode(true) // authz on: the contrast below must be identity-driven
	newSess := func(id uint64) *proxySession {
		return &proxySession{srv: srv, id: id, peer: fmt.Sprintf("svc-chroot-test:%d", id), start: time.Now()}
	}

	// Service identity: noCreate is false even though authz mode is on — the
	// handler must not consult the "" engine for a target that does not exist;
	// the engine Chroot creates it (D9).
	sess := newSess(1)
	ctx := oidc.WithIdentityKind(sessionContextWith(sess), oidc.IdentityKindService)
	resp, err := srv.Chroot(ctx, &pb.ChrootRequest{Subdir: "companies/new"})
	require.NoError(t, err)
	require.Zero(t, resp.GetErrno(), "service chroot to a missing subdir creates it (D9)")
	require.Equal(t, 1, factory.count())
	eng := factory.last()
	require.NotNil(t, eng)
	require.Equal(t, []string{"companies/new"}, eng.chrootList())

	// The contrast: a user identity chrooting to a missing subdir in authz
	// mode is answered ENOENT by the pre-check on the "" engine (I7); no
	// engine is created for the target.
	const sub = "550e8400-e29b-41d4-a716-446655440000"
	userSess := newSess(2)
	userCtx := oidc.WithIDToken(sessionContextWith(userSess), &oidc.IDToken{Subject: sub})
	resp2, err := srv.Chroot(userCtx, &pb.ChrootRequest{Subdir: "companies/other"})
	require.NoError(t, err)
	require.Equal(t, uint32(syscall.ENOENT), resp2.GetErrno())
	srv.pool.mu.Lock()
	_, hasRejected := srv.pool.entries["companies/other"]
	srv.pool.mu.Unlock()
	require.False(t, hasRejected, "rejected target creates no engine")
	require.Equal(t, 2, factory.count(), "only the root engine (pre-check) plus the service engine exist")

	// D10: the three distinguishable audit users.
	require.Equal(t, "service", chrootLogUser(oidc.WithIdentityKind(context.Background(), oidc.IdentityKindService)))
	require.Equal(t, sub, chrootLogUser(oidc.WithIDToken(context.Background(), &oidc.IDToken{Subject: sub})))
	require.Equal(t, "-", chrootLogUser(context.Background()))
}

// chrootAuthzRecorder implements AuthzClient for the real-engine chroot tests:
// it records every decision input and allows everything, except paths listed
// in deny. The tests assert on the paths and permissions the authz service
// would have seen.
type chrootAuthzRecorder struct {
	mu     sync.Mutex
	deny   map[string]bool // single-check paths to deny
	checks []string        // "user:path:perm:op"
	bulks  []string        // "user:perm:[p1 p2]"
}

func (r *chrootAuthzRecorder) CheckPermission(_ context.Context, userID, path string, perm AuthzPermission, op authzpb.Operation) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, fmt.Sprintf("%s:%s:%d:%d", userID, path, perm, op))
	return !r.deny[path], nil
}

func (r *chrootAuthzRecorder) CheckBulkPermissions(_ context.Context, userID string, paths []string, perm AuthzPermission) ([]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bulks = append(r.bulks, fmt.Sprintf("%s:%d:%v", userID, perm, paths))
	out := make([]bool, len(paths))
	for i := range out {
		out[i] = !r.deny[paths[i]]
	}
	return out, nil
}

func (r *chrootAuthzRecorder) CheckOrganizationAdmin(context.Context, string) (bool, error) {
	return true, nil
}

func (r *chrootAuthzRecorder) snapshot() (checks, bulks []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.checks...), append([]string(nil), r.bulks...)
}

// reset clears the recorded checks (between scenario steps).
func (r *chrootAuthzRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = nil
	r.bulks = nil
}

// chrootRealEnv serves MetaProxyServer over a real TCP listener with pooled
// real redis engines and authz mode on: the exact cmd/meta_proxy.go chain
// (session → authz) with a recording authz client and a fixed user id. Needs
// the local redis (encTestRedisAddr, db 15 like grpc_server_posix_test.go).
const chrootRealTestDB = 15

type chrootRealEnv struct {
	srv   *MetaProxyServer
	lis   net.Listener
	base  Meta
	authz *chrootAuthzRecorder
}

func newChrootRealEnv(t *testing.T, userID string) *chrootRealEnv {
	t.Helper()
	addr := encTestRedisAddr()
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: chrootRealTestDB})
	require.NoError(t, rdb.Ping(context.Background()).Err(), "Redis must be running at "+addr)
	require.NoError(t, rdb.FlushDB(context.Background()).Err())
	t.Cleanup(func() { _ = rdb.Close() })

	// The base engine owns the format and serves pathResolver only (I8).
	base, err := newRedisMeta("redis", fmt.Sprintf("%s/%d", addr, chrootRealTestDB), testConfig())
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Shutdown() })
	require.NoError(t, base.Init(&Format{Name: "chroot-isolation-test", UUID: "chroot-isolation-test", DirStats: true}, true))
	_, err = base.Load(true)
	require.NoError(t, err)

	srv := NewMetaProxyServer(base, 0, func() (Engine, error) {
		eng, err := newRedisMeta("redis", fmt.Sprintf("%s/%d", addr, chrootRealTestDB), testConfig())
		if err != nil {
			return nil, err
		}
		if _, err := eng.Load(true); err != nil {
			_ = eng.Shutdown()
			return nil, err
		}
		return eng, nil
	})
	rec := &chrootAuthzRecorder{}
	ai := NewAuthzInterceptor(rec, srv.InodePathCache(), srv)
	ai.userIDExtractor = func(context.Context) (string, error) { return userID, nil }
	srv.SetAuthzInterceptor(ai)
	srv.SetAuthzMode(true)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer(
		grpc.StatsHandler(srv.SessionStatsHandler()),
		grpc.ChainUnaryInterceptor(srv.SessionUnaryInterceptor(), ai.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(srv.SessionStreamInterceptor()),
	)
	pb.RegisterMetaServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return &chrootRealEnv{srv: srv, lis: lis, base: base, authz: rec}
}

func (env *chrootRealEnv) dial(t *testing.T) pb.MetaServiceClient {
	t.Helper()
	cc, err := grpc.NewClient(env.lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cc.Close() })
	return pb.NewMetaServiceClient(cc)
}

// engineKeys lists the pool registry keys (test assertions only).
func (env *chrootRealEnv) engineKeys() []string {
	env.srv.pool.mu.Lock()
	defer env.srv.pool.mu.Unlock()
	keys := make([]string, 0, len(env.srv.pool.entries))
	for k := range env.srv.pool.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// seed builds the volume tree companies/{acme,globex} with one file each plus
// a plain file companies/plain.file (the ENOTDIR target); it returns the acme
// and globex directory inodes.
func (env *chrootRealEnv) seed(t *testing.T) (acme, globex Ino) {
	t.Helper()
	ctx := Background()
	var companiesIno, acmeIno, globexIno, scratch Ino
	var attr Attr
	require.Zero(t, env.base.Mkdir(ctx, RootInode, "companies", 0755, 0, 0, &companiesIno, &attr))
	require.Zero(t, env.base.Mknod(ctx, companiesIno, "plain.file", TypeFile, 0644, 0, 0, "", &scratch, &attr))
	require.Zero(t, env.base.Mkdir(ctx, companiesIno, "acme", 0755, 0, 0, &acmeIno, &attr))
	require.Zero(t, env.base.Mkdir(ctx, companiesIno, "globex", 0755, 0, 0, &globexIno, &attr))
	require.Zero(t, env.base.Mknod(ctx, acmeIno, "acme.exr", TypeFile, 0644, 0, 0, "", &scratch, &attr))
	require.Zero(t, env.base.Mknod(ctx, globexIno, "globex.exr", TypeFile, 0644, 0, 0, "", &scratch, &attr))
	return acmeIno, globexIno
}

// TestMetaProxyPerSessionChrootIsolation (spec "Per-session chroot isolation"):
// A chroots into companies/acme while B stays on the volume root — B's root is
// untouched, each listing sees only its own root's entries, a file created in
// A's session root lands inside companies/acme (and nowhere else), and a later
// B chroot does not change A's root. Real engines throughout.
func TestMetaProxyPerSessionChrootIsolation(t *testing.T) {
	env := newChrootRealEnv(t, "11111111-1111-4111-8111-111111111111")
	acme, _ := env.seed(t)

	clA := env.dial(t)
	clB := env.dial(t)

	// A chroots; B stays un-chrooted (root engine).
	chrootOK(t, clA, "companies/acme")

	readdir := func(cl pb.MetaServiceClient) map[string]bool {
		return readdirNames(t, cl)
	}

	require.Equal(t, map[string]bool{"acme.exr": true}, readdir(clA),
		"chrooted session A lists its subtree root only")
	require.Equal(t, map[string]bool{"companies": true}, readdir(clB),
		"un-chrooted session B still sees the volume root")
	require.Contains(t, env.engineKeys(), "",
		"the root engine lives while the un-chrooted session B holds it")

	// A creates a file in its session root; it must land in companies/acme.
	created, err := clA.Create(context.Background(), &pb.CreateRequest{
		Parent: uint64(RootInode), Name: "a-created.txt", Mode: 0644,
	})
	require.NoError(t, err)
	require.Zero(t, created.GetErrno())
	var ino Ino
	var attr Attr
	require.Zero(t, env.base.Lookup(Background(), acme, "a-created.txt", &ino, &attr, false),
		"the file lives inside the chroot target")
	var missIno Ino
	var missAttr Attr
	require.Equal(t, syscall.ENOENT,
		env.base.Lookup(Background(), RootInode, "a-created.txt", &missIno, &missAttr, false),
		"nothing leaks into the volume root")

	// B's root is still the volume root; chrooting B elsewhere leaves A alone.
	require.Equal(t, map[string]bool{"companies": true}, readdir(clB))
	chrootOK(t, clB, "companies/globex")
	require.Equal(t, map[string]bool{"acme.exr": true, "a-created.txt": true}, readdir(clA),
		"session A's root is unaffected by B's chroot")
	require.Equal(t, map[string]bool{"globex.exr": true}, readdir(clB))

	// Engines: one per subdir. The root engine is gone — its last session (B)
	// chrooted away, refs hit zero and teardown removed it (I5).
	require.Equal(t, []string{"companies/acme", "companies/globex"}, env.engineKeys())

	// The authz service saw View on both absolute chroot targets.
	checks, _ := env.authz.snapshot()
	require.Contains(t, checks, "11111111-1111-4111-8111-111111111111:/companies/acme:1:0")
	require.Contains(t, checks, "11111111-1111-4111-8111-111111111111:/companies/globex:1:0")

	// Deterministic teardown: end every session and drain the pool.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env.srv.CloseSessions(ctx)
}

// --- Stage 5: dir-handle session binding and lifecycle RPC idempotency
// (fix-meta-proxy-per-session-chroot, tasks 5.1–5.3). ---

// TestMetaProxyDirHandleSessionBinding (spec "Directory handle session
// binding"): handles belong to the session that opened them — a foreign
// session on the SAME pooled engine gets EBADF for every handle RPC and cannot
// close the handle, and the session end closes exactly its own handles.
func TestMetaProxyDirHandleSessionBinding(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	clA, ccA := env.dial(t)
	clB, _ := env.dial(t)

	// One engine group: A and B chroot to the same subdir on purpose — the
	// shared engine must not leak handles across sessions.
	chrootOK(t, clA, "shared")
	chrootOK(t, clB, "shared")
	require.Equal(t, 1, env.factory.count())
	eng := env.engineOf("shared").(*poolFakeEngine)

	opened, err := clA.NewDirHandler(context.Background(), &pb.NewDirHandlerRequest{Inode: uint64(RootInode)})
	require.NoError(t, err)
	require.Zero(t, opened.GetErrno())
	handle := opened.GetHandle()
	require.NotNil(t, handle)

	// The owner lists its own handle.
	own, err := clA.DirHandlerList(context.Background(), &pb.DirHandlerListRequest{Handle: handle, Offset: 0})
	require.NoError(t, err)
	require.Zero(t, own.GetErrno())

	// Session B — same engine, different connection — gets EBADF for every
	// handle RPC, including Close: a foreign close must not kill A's handle.
	foreignList, err := clB.DirHandlerList(context.Background(), &pb.DirHandlerListRequest{Handle: handle, Offset: 0})
	require.NoError(t, err)
	require.Equal(t, uint32(syscall.EBADF), foreignList.GetErrno(), "foreign session sees EBADF")
	foreignInsert, err := clB.DirHandlerInsert(context.Background(), &pb.DirHandlerInsertRequest{Handle: handle, Inode: 7, Name: "x"})
	require.NoError(t, err)
	require.Equal(t, uint32(syscall.EBADF), foreignInsert.GetErrno())
	foreignDelete, err := clB.DirHandlerDelete(context.Background(), &pb.DirHandlerDeleteRequest{Handle: handle, Name: "x"})
	require.NoError(t, err)
	require.Equal(t, uint32(syscall.EBADF), foreignDelete.GetErrno())
	foreignClose, err := clB.DirHandlerClose(context.Background(), &pb.DirHandlerCloseRequest{Handle: handle})
	require.NoError(t, err)
	require.Equal(t, uint32(syscall.EBADF), foreignClose.GetErrno(), "foreign close is rejected")

	// A's handle survived the foreign close.
	own, err = clA.DirHandlerList(context.Background(), &pb.DirHandlerListRequest{Handle: handle, Offset: 0})
	require.NoError(t, err)
	require.Zero(t, own.GetErrno())
	require.Zero(t, eng.dirHandlerCloses(), "nothing closed so far")

	// Session end closes the session's handles exactly once.
	require.NoError(t, ccA.Close())
	waitCond(t, 2*time.Second, "session A's handle not closed on conn end", func() bool {
		return eng.dirHandlerCloses() == 1
	})

	// A fresh session on the same engine group must not resurrect the id.
	clC, _ := env.dial(t)
	chrootOK(t, clC, "shared")
	staleList, err := clC.DirHandlerList(context.Background(), &pb.DirHandlerListRequest{Handle: handle, Offset: 0})
	require.NoError(t, err)
	require.Equal(t, uint32(syscall.EBADF), staleList.GetErrno(), "closed handle is gone for every session")
	require.Equal(t, 1, eng.dirHandlerCloses(), "the close is one-shot")
}

// TestMetaProxyNewSessionIdempotentPerEngine (spec "Session lifecycle RPC
// idempotency"): the engine's NewSession runs exactly once — at engine
// creation — and every group session gets the same saved answer, no matter how
// often the RPC repeats.
func TestMetaProxyNewSessionIdempotentPerEngine(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	clA, _ := env.dial(t)
	clB, _ := env.dial(t)
	chrootOK(t, clA, "one")
	chrootOK(t, clB, "one")
	require.Equal(t, 1, env.factory.count(), "one engine for the subdir group")
	eng := env.engineOf("one").(*poolFakeEngine)

	respA1, err := clA.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err)
	require.Zero(t, respA1.GetErrno())
	respA2, err := clA.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err)
	respB1, err := clB.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err)

	// One identical saved answer for the whole group (same sid; with the fake
	// engine the sid is 0 by design — newEngineSession reads the real sid from
	// *redisMeta only, OQ7).
	require.Equal(t, respA1.GetSid(), respA2.GetSid(), "repeated RPC — same saved answer")
	require.Equal(t, respA1.GetSid(), respB1.GetSid(), "both group sessions — same saved answer")

	_, newSess, _, _ := eng.calls()
	require.Equal(t, 1, newSess, "engine NewSession ran exactly once (I6)")
}

// gatedDirHandlerEngine blocks NewDirHandler after signalling "entered", so the
// test can end the session while the RPC sits between the engine call and the
// handle registration — the window where endSession drops sess.handles.
type gatedDirHandlerEngine struct {
	poolFakeEngine
	entered chan struct{}
	gate    chan struct{}
}

func (e *gatedDirHandlerEngine) NewDirHandler(ctx Context, inode Ino, plus bool, initEntries []*Entry) (DirHandler, syscall.Errno) {
	if e.entered != nil {
		e.entered <- struct{}{}
	}
	<-e.gate
	return e.poolFakeEngine.NewDirHandler(ctx, inode, plus, initEntries)
}

// TestMetaProxyNewDirHandlerRacesSessionEnd (fix-review): a session may end
// while a NewDirHandler RPC is in flight (ConnEnd, or a parallel CloseSession
// RPC). endSession drops sess.handles under sess.mu; the handler must detect
// the dropped map at registration time, close the never-published handle and
// answer EBADF — not crash the whole proxy process on a nil-map write.
func TestMetaProxyNewDirHandlerRacesSessionEnd(t *testing.T) {
	eng := &gatedDirHandlerEngine{
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	srv := newDirectServer(t, eng, 0)
	ctx := directSessionCtx(t, srv)
	sess := sessionFromContext(ctx)
	require.NotNil(t, sess)

	type rpcResult struct {
		resp *pb.NewDirHandlerResponse
		err  error
	}
	done := make(chan rpcResult, 1)
	go func() {
		resp, err := srv.NewDirHandler(ctx, &pb.NewDirHandlerRequest{Inode: uint64(RootInode)})
		done <- rpcResult{resp: resp, err: err}
	}()

	// Wait until the RPC is past sessionFor/engineFor, inside the engine call —
	// the exact window where a session end must no longer crash registration.
	select {
	case <-eng.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("NewDirHandler never reached the gated engine call")
	}
	srv.endSession(sess) // drops sess.handles while the RPC is in flight
	close(eng.gate)      // let the RPC proceed to the registration

	select {
	case r := <-done:
		require.NoError(t, r.err)
		require.Equal(t, uint32(syscall.EBADF), r.resp.GetErrno(),
			"the never-published handle must answer EBADF, not panic on the nil map")
	case <-time.After(5 * time.Second):
		t.Fatal("NewDirHandler did not return after the session ended")
	}
}

// TestMetaProxyCloseSessionRPCReleasesOnlyCaller (spec "CloseSession RPC
// releases only the caller"): RPC CloseSession ends the sender's session
// only — the engine (and its engine session) stays for the remaining group
// sessions, which keep working; the engine teardown fires when the last
// session is gone (D6, I5, I6).
func TestMetaProxyCloseSessionRPCReleasesOnlyCaller(t *testing.T) {
	env := newSessionTestEnv(t, 0)
	clA, _ := env.dial(t)
	clB, ccB := env.dial(t)
	chrootOK(t, clA, "group")
	chrootOK(t, clB, "group")
	require.Equal(t, 1, env.factory.count())
	eng := env.engineOf("group").(*poolFakeEngine)

	// A ends its session by RPC.
	cs, err := clA.CloseSession(context.Background(), &pb.CloseSessionRequest{})
	require.NoError(t, err)
	require.Zero(t, cs.GetErrno())

	// The engine group is untouched: still in the pool, engine session open.
	require.Equal(t, 1, env.sessionCount(), "only session B remains")
	require.NotNil(t, env.engineOf("group"), "engine stays while B holds it")
	_, _, closed, shutdowns := eng.calls()
	require.Zero(t, closed, "engine CloseSession is teardown-only (I6)")
	require.Zero(t, shutdowns, "engine Shutdown is teardown-only (I5)")

	// A's connection fails closed after its own session ended.
	_, err = clA.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.Error(t, err)
	require.Equal(t, codes.Internal, status.Code(err))

	// B keeps working on the same engine: chroot replay + RPC.
	chrootOK(t, clB, "group")
	resp, err := clB.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err)
	require.Zero(t, resp.GetErrno())

	// Only when the last group session goes away is the engine torn down.
	require.NoError(t, ccB.Close())
	waitCond(t, 2*time.Second, "engine not torn down after the last session", func() bool {
		return env.engineOf("group") == nil
	})
	waitCond(t, 2*time.Second, "teardown calls not recorded", func() bool {
		_, _, closed, shutdowns := eng.calls()
		return closed == 1 && shutdowns == 1
	})
}
func readdirEntries(t *testing.T, cl pb.MetaServiceClient) map[string]uint64 {
	t.Helper()
	resp, err := cl.Readdir(context.Background(), &pb.ReaddirRequest{Inode: uint64(RootInode)})
	require.NoError(t, err)
	require.Zero(t, resp.GetErrno())
	out := make(map[string]uint64, len(resp.GetEntries()))
	for _, e := range resp.GetEntries() {
		out[string(e.GetName())] = e.GetInode()
	}
	return out
}

// readdirNames lists the session root (inode 1) and returns the entry names.
func readdirNames(t *testing.T, cl pb.MetaServiceClient) map[string]bool {
	t.Helper()
	entries := readdirEntries(t, cl)
	names := make(map[string]bool, len(entries))
	for name := range entries {
		names[name] = true
	}
	return names
}

// TestMetaProxyAuthzPathsUnderChroot (spec fix-meta-proxy-per-session-chroot,
// "Session-scoped authz paths under chroot", tasks 4.1–4.3): under chroot the
// authz checks run on full volume paths — a chrooted session's root listing
// authorizes entries as /companies/acme/<name>, its inode 1 checks as the
// session root (not the always-allowed "/"), creates inside the session root
// check the session-root path (parent) and the dir-convention child path
// (mkdir), and a path cached by one session resolves to the same full volume
// path for another. Real engines throughout.
func TestMetaProxyAuthzPathsUnderChroot(t *testing.T) {
	const userSub = "33333333-3333-4333-8333-333333333333"
	env := newChrootRealEnv(t, userSub)
	env.seed(t)

	clA := env.dial(t)
	clB := env.dial(t)

	// A chroots into acme; B stays un-chrooted (root engine).
	chrootOK(t, clA, "companies/acme")

	// 1. Root listing under chroot: entries are bulk-checked at
	// /companies/acme/<name>, never at the chroot-relative /<name>.
	env.authz.reset()
	require.Equal(t, map[string]bool{"acme.exr": true}, readdirNames(t, clA))
	_, bulks := env.authz.snapshot()
	require.Len(t, bulks, 1, "one bulk check for the whole listing")
	require.Contains(t, bulks[0], "/companies/acme/acme.exr",
		"entry paths are full volume paths under the session root")

	// 2. inode 1 of a chrooted session checks as the session root.
	env.authz.reset()
	attr, err := clA.GetAttr(context.Background(), &pb.GetAttrRequest{Inode: uint64(RootInode), Ctx: &pb.MetaContext{}})
	require.NoError(t, err)
	require.Zero(t, attr.GetErrno())
	checks, _ := env.authz.snapshot()
	require.Contains(t, checks, userSub+":/companies/acme:1:0")

	// 3. The un-chrooted session resolves inode 1 to "/" — always-allowed,
	// no authz service call.
	env.authz.reset()
	attrB, err := clB.GetAttr(context.Background(), &pb.GetAttrRequest{Inode: uint64(RootInode), Ctx: &pb.MetaContext{}})
	require.NoError(t, err)
	require.Zero(t, attrB.GetErrno())
	checks, _ = env.authz.snapshot()
	require.Empty(t, checks, `"/" + View is always allowed — no authz call`)

	// 4. Create in the session root: Write is checked on the session-root
	// path (the parent inode 1 resolves per session), not on "/".
	env.authz.reset()
	created, err := clA.Create(context.Background(), &pb.CreateRequest{
		Parent: uint64(RootInode), Name: "paths.txt", Mode: 0644, Ctx: &pb.MetaContext{},
	})
	require.NoError(t, err)
	require.Zero(t, created.GetErrno())
	checks, _ = env.authz.snapshot()
	require.Contains(t, checks, userSub+":/companies/acme:3:0", "Write on parent=1 resolves to the session root")

	// 5. Mkdir in the session root: the created directory is checked at the
	// dir-convention child path /companies/acme/<name>/.
	env.authz.reset()
	mkdir, err := clA.Mkdir(context.Background(), &pb.MkdirRequest{
		Parent: uint64(RootInode), Name: "sub", Mode: 0755, Ctx: &pb.MetaContext{},
	})
	require.NoError(t, err)
	require.Zero(t, mkdir.GetErrno())
	checks, _ = env.authz.snapshot()
	require.Contains(t, checks, userSub+":/companies/acme/sub/:3:1", "mkdir checks the child dir path under the session root")

	// 6. Cached paths are full volume paths for every session: A's listing
	// cached acme.exr; B resolving the same inode checks the same full path.
	env.authz.reset()
	entries := readdirEntries(t, clA)
	require.Contains(t, entries, "acme.exr")
	gotB, err := clB.GetAttr(context.Background(), &pb.GetAttrRequest{Inode: entries["acme.exr"], Ctx: &pb.MetaContext{}})
	require.NoError(t, err)
	require.Zero(t, gotB.GetErrno())
	checks, _ = env.authz.snapshot()
	require.Contains(t, checks, userSub+":/companies/acme/acme.exr:1:0",
		"a path cached by session A resolves to the full volume path for session B")

	// Deterministic teardown: end every session and drain the pool.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env.srv.CloseSessions(ctx)
}

// TestMetaProxySessionAuditLogs (spec fix-meta-proxy-per-session-chroot,
// "Chroot and session event logging", tasks 6.1–6.3): the session, engine-pool
// and chroot events reach the pkg/meta logger at their spec levels with the
// spec key=value fields.
func TestMetaProxySessionAuditLogs(t *testing.T) {
	cap := captureLogger(t, logger)

	// Engine creation failure: pool "create failed" + "session engine failed"
	// (engine unavailable outside Chroot), then a successful retry.
	envFail := newSessionTestEnv(t, 0)
	envFail.factory.failNext = 1
	clF, _ := envFail.dial(t)
	_, err := clF.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.Error(t, err, "engine creation failed once")
	_, err = clF.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err, "the retry creates the engine")
	ctxF, cancelF := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelF()
	envFail.srv.CloseSessions(ctxF)

	// Session lifecycle: started, chroot applied, chroot replay rejected,
	// second connection rejected at the limit, session closed.
	env := newSessionTestEnv(t, 1)
	clA, ccA := env.dial(t)
	clB, _ := env.dial(t)

	_, err = clA.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.NoError(t, err)
	chrootOK(t, clA, "sub")
	_, err = clA.Chroot(context.Background(), &pb.ChrootRequest{Subdir: "other"})
	require.Error(t, err, "second chroot is rejected")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = clB.NewSession(context.Background(), &pb.NewSessionRequest{})
	require.Error(t, err, "connection B hits the session limit")
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.NoError(t, ccA.Close())
	waitCond(t, 2*time.Second, "session A not ended", func() bool {
		return env.sessionCount() == 0
	})

	want := []string{
		"meta-proxy engine create failed subdir=/ err=",
		"meta-proxy session engine failed id=1 err=",
		"meta-proxy session started id=1",
		"meta-proxy chroot applied id=1 user=- subdir=/sub",
		"meta-proxy chroot rejected id=1 user=- subdir=/other reason=session_already_chrooted",
		"meta-proxy session rejected id=2 reason=max_sessions limit=1",
		"meta-proxy session closed id=1 chroot=/sub uptime=",
	}
	for _, prefix := range want {
		require.NotEmpty(t, cap.messages(prefix), "missing audit event %q", prefix)
	}
	require.NotEmpty(t, cap.messages("meta-proxy engine created subdir=/sub"), "successful engine creation logged")

	// Levels: Info for started/closed/applied/created, Warn for
	// rejected/chroot-rejected/create-failed/engine-failed.
	for _, e := range cap.all() {
		switch {
		case strings.HasPrefix(e.Message, "meta-proxy session started id="),
			strings.HasPrefix(e.Message, "meta-proxy session closed id="),
			strings.HasPrefix(e.Message, "meta-proxy chroot applied id="),
			strings.HasPrefix(e.Message, "meta-proxy engine created subdir="):
			require.Equal(t, logrus.InfoLevel, e.Level, "%s must be Info", e.Message)
		case strings.HasPrefix(e.Message, "meta-proxy session rejected id="),
			strings.HasPrefix(e.Message, "meta-proxy chroot rejected id="),
			strings.HasPrefix(e.Message, "meta-proxy engine create failed subdir="),
			strings.HasPrefix(e.Message, "meta-proxy session engine failed id="):
			require.Equal(t, logrus.WarnLevel, e.Level, "%s must be Warn", e.Message)
		}
	}
}
