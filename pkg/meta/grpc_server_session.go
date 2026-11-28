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

// Session and engine-pool machinery of the meta proxy
// (fix-meta-proxy-per-session-chroot): a proxy session is one gRPC connection
// (TagConn opens it, ConnEnd closes it), and each session lazily acquires a
// reference to a shared engine keyed by the normalized chroot subdir. Engines
// are created by an injected EngineFactory, chrooted exactly once at creation
// time (invariant I2) and torn down (CloseSession + Shutdown) when the last
// referencing session goes away (I5).

import (
	"context"
	"net"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// Engine is the metadata engine type the pool manages: the same Meta interface
// the proxy previously kept as its single base engine.
type Engine = Meta

// EngineFactory builds a fully loaded (format read) engine with no session
// opened: NewClient + Load(true). The pool owns Chroot and NewSession (D4).
type EngineFactory func() (Engine, error)

const (
	// engineCreateTimeout bounds factory + final lookup + Chroot + NewSession.
	engineCreateTimeout = 30 * time.Second
	// engineCloseTimeout bounds the engine CloseSession during teardown; the
	// following Shutdown is unconditional (I5).
	engineCloseTimeout = 5 * time.Second
)

// poolEntry is one pooled engine shared by every session chrooted to key.
type poolEntry struct {
	key     string // normalized subdir; "" — volume root
	created time.Time

	ready chan struct{} // closed when creation finished (success or failure)
	// Fields below are read only after ready is closed.
	engine      Engine
	errno       uint32                 // Chroot of the fresh engine returned errno
	err         error                  // factory or NewSession failed
	newSessResp *pb.NewSessionResponse // engine NewSession answer for the whole group

	refs int // under enginePool.mu

	closeOnce    sync.Once // engine CloseSession (≤1 per engine, I6)
	shutdownOnce sync.Once // engine Shutdown (exactly 1 per engine, I5)
}

// enginePool keeps at most one entry per key (I3). Creation is single-flight
// through the placeholder published in the map (D3): the first acquirer builds
// the engine outside the pool mutex, later acquirers wait on its ready channel.
type enginePool struct {
	factory EngineFactory

	mu      sync.Mutex
	entries map[string]*poolEntry
	closed  bool
}

func newEnginePool(factory EngineFactory) *enginePool {
	return &enginePool{
		factory: factory,
		entries: make(map[string]*poolEntry),
	}
}

// acquire binds the engine of key to the caller (refs++). The first acquirer
// creates the engine; the rest wait on the same creation.
func (p *enginePool) acquire(ctx context.Context, key string, noCreate bool) (*poolEntry, uint32, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, 0, status.Error(codes.Unavailable, "meta-proxy: engine pool is closed")
	}
	e, ok := p.entries[key]
	if !ok {
		e = &poolEntry{key: key, ready: make(chan struct{}), refs: 1}
		p.entries[key] = e // placeholder published before creation starts
		p.mu.Unlock()
		p.create(e, noCreate)
		return p.settle(e)
	}
	e.refs++
	p.mu.Unlock()
	select {
	case <-e.ready:
	case <-ctx.Done():
		p.release(e)
		return nil, 0, status.FromContextError(ctx.Err()).Err()
	}
	return p.settle(e)
}

// settle resolves a finished creation for the caller, releasing the reference
// when the entry carries no usable engine.
func (p *enginePool) settle(e *poolEntry) (*poolEntry, uint32, error) {
	if e.err != nil {
		p.release(e)
		return nil, 0, e.err
	}
	if e.errno != 0 {
		p.release(e)
		return nil, e.errno, nil
	}
	return e, 0, nil
}

// create builds the engine outside the pool mutex. The engine root is written
// exactly here and never again (I2).
func (p *enginePool) create(e *poolEntry, noCreate bool) {
	ctx, cancel := context.WithTimeout(context.Background(), engineCreateTimeout)
	defer cancel()
	defer close(e.ready)

	if p.factory == nil {
		e.err = status.Error(codes.Unavailable, "meta-proxy: engine factory is not configured")
		logger.Warnf("meta-proxy engine create failed subdir=%s err=%v", displaySubdir(e.key), e.err)
		p.discard(e)
		return
	}

	eng, err := p.factory()
	if err != nil {
		e.err = status.Errorf(codes.Unavailable, "meta-proxy: create engine for %q: %v", e.key, err)
		logger.Warnf("meta-proxy engine create failed subdir=%s err=%v", displaySubdir(e.key), err)
		p.discard(e)
		return
	}
	mctx := WrapWithCancelSkipPermCheck(ctx, 0, 0, []uint32{0})
	defer mctx.Cancel()

	if e.key != "" {
		// authz mode: the chroot target must exist. The final lookup runs on the
		// fresh engine itself, narrowing the TOCTOU window to the Chroot call
		// (which knows how to create the directories it needs).
		if noCreate {
			if errno, lerr := lookupSubdir(mctx, eng, e.key); lerr != nil || errno != 0 {
				_ = eng.Shutdown()
				e.errno, e.err = uint32(errno), lerr
				logger.Warnf("meta-proxy engine create failed subdir=%s err=lookup errno=%d", displaySubdir(e.key), errno)
				p.discard(e)
				return
			}
		}
		if errno := eng.Chroot(mctx, e.key); errno != 0 {
			_ = eng.Shutdown()
			e.errno = uint32(errno)
			logger.Warnf("meta-proxy engine create failed subdir=%s err=chroot errno=%d", displaySubdir(e.key), errno)
			p.discard(e)
			return
		}
	}
	resp, err := newEngineSession(mctx, eng)
	if err != nil {
		// The NewSession may have half-registered a redis session (sid>0):
		// CloseSession stops the engine's session goroutines and cleans the
		// record; bare Shutdown would leave both until the TTL (design A7).
		_ = eng.CloseSession()
		_ = eng.Shutdown()
		e.err = status.Errorf(codes.Unavailable, "meta-proxy: open engine session for %q: %v", e.key, err)
		logger.Warnf("meta-proxy engine create failed subdir=%s err=%v", displaySubdir(e.key), err)
		p.discard(e)
		return
	}
	e.engine, e.newSessResp, e.created = eng, resp, time.Now()
	logger.Infof("meta-proxy engine created subdir=%s", displaySubdir(e.key))
}

// discard removes from the registry an entry whose creation failed.
func (p *enginePool) discard(e *poolEntry) {
	p.mu.Lock()
	if p.entries[e.key] == e {
		delete(p.entries, e.key)
	}
	p.mu.Unlock()
}

// release detaches one session reference (refs--). The entry leaves the
// registry only here, under the mutex and only at refs == 0 (I3, D7) — after
// that nobody can join the entry, and its teardown owns the engine (I5).
func (p *enginePool) release(e *poolEntry) {
	p.mu.Lock()
	if e.refs > 0 {
		e.refs--
	}
	last := e.refs == 0 && p.entries[e.key] == e
	if last {
		delete(p.entries, e.key)
	}
	p.mu.Unlock()
	if last {
		go p.teardown(e, engineCloseTimeout)
	}
}

// teardown closes the engine: CloseSession at most once with a budget, then an
// unconditional Shutdown (I5, I6).
func (p *enginePool) teardown(e *poolEntry, budget time.Duration) {
	<-e.ready // the entry may leave the registry while creation is still running
	if e.engine == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.closeOnce.Do(func() { _ = e.engine.CloseSession() })
	}()
	timedOut := true
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		timedOut = false
	case <-timer.C:
	}
	e.shutdownOnce.Do(func() { _ = e.engine.Shutdown() })
	uptime := time.Since(e.created).Round(time.Second)
	if timedOut {
		logger.Warnf("meta-proxy engine closed subdir=%s refs=0 uptime=%s close_timeout=true", displaySubdir(e.key), uptime)
	} else {
		logger.Infof("meta-proxy engine closed subdir=%s refs=0 uptime=%s", displaySubdir(e.key), uptime)
	}
}

// shutdown tears down every pooled engine (pool stop, execution flow step 7).
// Each engine gets the same CloseSession-then-Shutdown teardown as a natural
// refs==0 departure (I5); ctx bounds the wait.
func (p *enginePool) shutdown(ctx context.Context) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	entries := make([]*poolEntry, 0, len(p.entries))
	for _, e := range p.entries {
		entries = append(entries, e)
	}
	p.entries = make(map[string]*poolEntry)
	p.mu.Unlock()

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *poolEntry) {
			defer wg.Done()
			p.teardown(e, engineCloseTimeout)
		}(e)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		logger.Warnf("meta-proxy engine pool shutdown timed out engines=%d", len(entries))
	}
}

// normalizeSubdir is the pool key: path.Clean("/"+subdir) without the leading
// slash ("" is the volume root).
func normalizeSubdir(subdir string) string {
	return strings.TrimPrefix(path.Clean("/"+subdir), "/")
}

// displaySubdir renders a pool key for logs: "/" for the root key.
func displaySubdir(key string) string {
	if key == "" {
		return "/"
	}
	return "/" + key
}

// lookupSubdir walks the components of key from the engine root, creating
// nothing. It returns the errno for ENOENT/ENOTDIR and an error for any other
// engine failure.
func lookupSubdir(ctx Context, eng Engine, key string) (syscall.Errno, error) {
	ino := RootInode
	for _, name := range strings.Split(key, "/") {
		if name == "" {
			continue
		}
		var child Ino
		var attr Attr
		// checkPerm=false: engine-level POSIX checks are off on the proxy — the
		// lookup is a server-side fact check, same stance as path recovery.
		if st := eng.Lookup(ctx, ino, name, &child, &attr, false); st != 0 {
			if st == syscall.ENOENT {
				return syscall.ENOENT, nil
			}
			if st == syscall.ENOTDIR {
				return syscall.ENOTDIR, nil
			}
			return 0, status.Errorf(codes.Unavailable, "meta-proxy: lookup %q: %v", name, st)
		}
		if attr.Typ != TypeDirectory {
			return syscall.ENOTDIR, nil
		}
		ino = child
	}
	return 0, nil
}

// chrootRejectReason names the chroot rejection for logs/audit.
func chrootRejectReason(errno uint32) string {
	switch syscall.Errno(errno) {
	case syscall.ENOENT:
		return "target_missing"
	case syscall.ENOTDIR:
		return "target_not_dir"
	}
	return "engine_error"
}

// fixedSessionArgs returns the arguments every pooled engine session is opened
// with (D5): record=true — the proxy shares one engine session across the
// group, so the first client's own record flag is not re-issued to the engine.
func fixedSessionArgs() bool {
	return true
}

// newEngineSession opens the engine's one and only session (I6) with fixed
// arguments (D5, fixedSessionArgs) and builds the response every group RPC
// NewSession clones. ctx is the create budget — the engine itself runs on its
// own session context (NewSession takes no ctx, interface.go).
func newEngineSession(ctx context.Context, eng Engine) (*pb.NewSessionResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := eng.NewSession(fixedSessionArgs()); err != nil {
		return nil, err
	}
	var sid uint64
	if rm, ok := eng.(*redisMeta); ok {
		sid = rm.sid
	}
	return &pb.NewSessionResponse{Errno: 0, Sid: sid}, nil
}

// proxySession is one gRPC connection's proxy state. It holds at most one pool
// reference, to exactly one engine (I4).
type proxySession struct {
	srv   *MetaProxyServer
	id    uint64
	peer  string
	start time.Time

	chrootMu    sync.Mutex // serializes Chroot RPCs of the session
	mu          sync.Mutex // guards the fields below
	entry       *poolEntry // pooled engine; nil until first needed
	chroot      string     // key of the first Chroot (idempotent replay)
	chrootErrno uint32     // errno of the first Chroot
	root        string     // key of the successful Chroot; "" — volume root
	ended       bool
	handles     map[uint64]*dirHandlerEntry
}

// rootPath is the session's volume-space root: "/" before a successful chroot.
func (sess *proxySession) rootPath() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.root == "" {
		return "/"
	}
	return "/" + sess.root
}

func (sess *proxySession) isEnded() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.ended
}

// sessionContextKey carries the session through the connection context tagged
// by sessionStatsHandler.TagConn.
type sessionContextKey struct{}

func sessionFromContext(ctx context.Context) *proxySession {
	if ctx == nil {
		return nil
	}
	sess, _ := ctx.Value(sessionContextKey{}).(*proxySession)
	return sess
}

// sessionRegistry tracks live sessions by peer address, enforcing
// maxSessions (0 = no limit).
type sessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*proxySession
	max      int
	nextID   uint64
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: make(map[string]*proxySession)}
}

// openSession registers a session for addr, or rejects it at the limit.
func (r *sessionRegistry) openSession(srv *MetaProxyServer, addr net.Addr) (*proxySession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.max > 0 && len(r.sessions) >= r.max {
		r.nextID++ // the rejected connection still consumes an id (audit trail)
		logger.Warnf("meta-proxy session rejected id=%d reason=max_sessions limit=%d",
			r.nextID, r.max)
		return nil, status.Errorf(codes.ResourceExhausted, "meta-proxy: session limit reached (%d)", r.max)
	}
	r.nextID++
	sess := &proxySession{
		srv:     srv,
		id:      r.nextID,
		peer:    addr.String(),
		start:   time.Now(),
		handles: make(map[uint64]*dirHandlerEntry),
	}
	r.sessions[sess.peer] = sess
	logger.Infof("meta-proxy session started id=%d addr=%s active=%d", sess.id, sess.peer, len(r.sessions))
	return sess, nil
}

// remove forgets the session (no-op if a newer session took the peer slot).
func (r *sessionRegistry) remove(sess *proxySession) {
	r.mu.Lock()
	if cur, ok := r.sessions[sess.peer]; ok && cur == sess {
		delete(r.sessions, sess.peer)
	}
	r.mu.Unlock()
}

func (r *sessionRegistry) full() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.max > 0 && len(r.sessions) >= r.max
}

// snapshot lists the live sessions (for CloseSessions).
func (r *sessionRegistry) snapshot() []*proxySession {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*proxySession, 0, len(r.sessions))
	for _, sess := range r.sessions {
		out = append(out, sess)
	}
	return out
}

// sessionStatsHandler binds the session lifecycle to the gRPC connection:
// TagConn opens the session, HandleConn(stats.ConnEnd) closes it.
type sessionStatsHandler struct {
	srv *MetaProxyServer
}

func (h *sessionStatsHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

func (h *sessionStatsHandler) HandleRPC(context.Context, stats.RPCStats) {}

func (h *sessionStatsHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	sess, err := h.srv.sessions.openSession(h.srv, info.RemoteAddr)
	if err != nil {
		// No session in the context: every RPC of this connection fails closed
		// in sessionFor (sessionFor, I1).
		return ctx
	}
	return context.WithValue(ctx, sessionContextKey{}, sess)
}

func (h *sessionStatsHandler) HandleConn(ctx context.Context, s stats.ConnStats) {
	if _, ok := s.(*stats.ConnEnd); ok {
		if sess := sessionFromContext(ctx); sess != nil {
			h.srv.endSession(sess)
		}
	}
}

// SessionUnaryInterceptor guards every unary RPC: it fails closed when the
// connection carries no (live) session (I1). Installed first in the chain.
func (s *MetaProxyServer) SessionUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if _, err := s.sessionFor(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// SessionStreamInterceptor is the stream twin of SessionUnaryInterceptor.
func (s *MetaProxyServer) SessionStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if _, err := s.sessionFor(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// SessionStatsHandler returns the stats handler driving the session lifecycle
// (grpc.StatsHandler server option).
func (s *MetaProxyServer) SessionStatsHandler() stats.Handler {
	return &sessionStatsHandler{srv: s}
}

// sessionFor returns the session of the current RPC. A missing session fails
// closed: ResourceExhausted when the registry is at its limit (the connection
// was rejected at TagConn), Internal otherwise.
func (s *MetaProxyServer) sessionFor(ctx context.Context) (*proxySession, error) {
	if sess := sessionFromContext(ctx); sess != nil && !sess.isEnded() {
		return sess, nil
	}
	if s.sessions.full() {
		return nil, status.Errorf(codes.ResourceExhausted, "meta-proxy: session limit reached (%d)", s.maxSessions)
	}
	return nil, status.Error(codes.Internal, "meta-proxy: no session for rpc")
}

// bind gives the session its engine reference, holding exactly one (I4): any
// previously held reference (a different engine) is released back to the pool.
func (s *MetaProxyServer) bind(sess *proxySession, e *poolEntry) {
	sess.mu.Lock()
	var extra *poolEntry
	switch {
	case sess.ended, sess.entry == e:
		extra = e // return the acquired reference right away
	case sess.entry == nil:
		sess.entry = e
	default:
		extra, sess.entry = sess.entry, e
	}
	sess.mu.Unlock()
	if extra != nil {
		s.pool.release(extra)
	}
}

// endSession tears the session down exactly once: dir handles, the pool
// reference, the registry slot.
func (s *MetaProxyServer) endSession(sess *proxySession) {
	sess.mu.Lock()
	if sess.ended {
		sess.mu.Unlock()
		return
	}
	sess.ended = true
	entry, handles, root := sess.entry, sess.handles, sess.root
	sess.entry, sess.handles = nil, nil
	sess.mu.Unlock()

	s.sessions.remove(sess)
	s.closeDirHandles(handles)
	if entry != nil {
		s.pool.release(entry)
	}
	logger.Infof("meta-proxy session closed id=%d chroot=%s uptime=%s",
		sess.id, displaySubdir(root), time.Since(sess.start).Round(time.Second))
}

// closeDirHandles closes the session's dir handlers. The session owns its
// handles exclusively (they live in sess.handles, not in any server-wide
// registry), so dropping the map is the whole cleanup.
func (s *MetaProxyServer) closeDirHandles(handles map[uint64]*dirHandlerEntry) {
	for _, entry := range handles {
		entry.handler.Close()
	}
}
