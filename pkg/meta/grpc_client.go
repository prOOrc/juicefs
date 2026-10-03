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
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	// Cache defaults
	defaultAttrCacheSize     = 100_000
	defaultDirCacheSize      = 5_000
	defaultAttrCacheTTL      = time.Second
	defaultDirCacheTTL       = time.Second
	defaultHeartbeatInterval = 12 * time.Second

	// FEK cache defaults (FR-USR-9)
	defaultFekCacheSize = 100_000
	defaultFekCacheTTL  = 15 * time.Minute

	// defaultOfflineTimeout is how long the client may stay offline-connected
	// before it disconnects and wipes its keys (NFR-OFF-4).
	defaultOfflineTimeout = 15 * time.Minute
)

// HubState is the connectivity state of the meta proxy (hub) as seen by this
// client (stage 6, NFR-OFF-1..4).
type HubState int32

const (
	HubOnline HubState = iota // hub reachable
	// HubOfflineConnected: hub unreachable but within offlineTimeout — cached
	// reads still work, new encrypted Opens fail.
	HubOfflineConnected
	// HubDisconnected: offlineTimeout exceeded (or logout/OIDC expiry) — keys
	// wiped, terminal until remount (design 6.6).
	HubDisconnected
)

// fekEntry is a cached plaintext per-file FEK with the version it was issued at.
type fekEntry struct {
	fek     []byte
	version uint32
}

// grpcMeta implements Meta interface as a gRPC client (Variant B - no baseMeta embedding)
type grpcMeta struct {
	addr   string
	conf   *Config
	client pb.MetaServiceClient
	conn   *grpc.ClientConn
	sid    uint64
	format *Format
	closed int32
	mu     sync.RWMutex

	// Attribute cache (inode -> Attr)
	attrCache *expirable.LRU[uint64, *Attr]
	// Directory cache (inode -> []*Entry)
	dirCache *expirable.LRU[uint64, []*Entry]
	// FEK cache (inode -> plaintext FEK); encrypted volumes only (FR-USR-9)
	fekCache *expirable.LRU[uint64, *fekEntry]
	// Cache TTLs
	attrCacheTTL time.Duration
	dirCacheTTL  time.Duration

	// Heartbeat
	heartbeatInterval time.Duration
	heartbeatCancel   context.CancelFunc
	heartbeatWg       sync.WaitGroup

	// Hub state machine (stage 6, NFR-OFF-1..4)
	hubState       int32 // atomic; HubState
	hubMu          sync.Mutex
	lastFail       time.Time // first failed RPC of the current offline window
	offlineTimeout time.Duration
	onWipe         func() // VFS hook (SetOnWipe): InvalidateAllKeys
	onReconnect    func() // VFS hook (SetOnReconnect): ReplayWriteJournal

	// Permission generation (stage 7, task 7.3): last generation seen on the
	// heartbeat; -1 until the first successful beat. Only the heartbeat
	// goroutine touches it.
	lastPermGen int64
	// heartbeatBaseCtx is a test seam: base context for heartbeats (nil →
	// Background). Tests use it to carry a simulated OIDC identity so the
	// proxy can resolve the user for GetPermissionGeneration.
	heartbeatBaseCtx context.Context

	// OIDC (nil if not configured)
	oidcConfig   *oidc.Config
	tokenManager tokenProvider      // interface for testability
	authGroup    singleflight.Group // coalesces concurrent token requests
}

// tokenProvider is the minimal interface needed by withAuth, the heartbeat
// OIDC check and Shutdown. Implemented by *oidc.TokenManager.
type tokenProvider interface {
	BearerToken(ctx context.Context) string
	// CachedBearerToken is non-blocking: it never triggers browser auth.
	CachedBearerToken(ctx context.Context) string
	// Subject is the 'sub' claim of the cached ID token (non-blocking).
	Subject(ctx context.Context) string
	Stop()
}

var _ tokenProvider = (*oidc.TokenManager)(nil)

var _ Meta = (*grpcMeta)(nil)

// buildClientTLSConfig builds the client TLS config for the grpcMeta query
// parameters (implement-grpc-tls, NFR-SEC-8). A nil config (tls=1 absent)
// means plaintext. With caFile set, the root CA pool is loaded from that file
// (fail-fast on an unreadable file — the error names it); otherwise the system
// pool is used. serverName overrides the server name used for verification.
func buildClientTLSConfig(enabled bool, caFile, serverName string) (*tls.Config, error) {
	if !enabled {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read tls-ca %q: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls-ca %q contains no valid certificates", caFile)
		}
		cfg.RootCAs = pool
	}
	if serverName != "" {
		cfg.ServerName = serverName
	}
	return cfg, nil
}

// newGRPCMeta creates a new gRPC meta client
func newGRPCMeta(driver, addr string, conf *Config) (Meta, error) {
	uri := driver + "://" + addr
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("url parse %s: %s", uri, err)
	}
	values := u.Query()
	query := queryMap{&values}

	// Cache configuration
	attrCacheSize := query.getInt("attr-cache-size", "attr_cache_size", defaultAttrCacheSize)
	dirCacheSize := query.getInt("dir-cache-size", "dir_cache_size", defaultDirCacheSize)
	attrCacheTTL := query.duration("attr-cache-ttl", "attr_cache_ttl", defaultAttrCacheTTL)
	dirCacheTTL := query.duration("dir-cache-ttl", "dir_cache_ttl", defaultDirCacheTTL)
	heartbeatInterval := query.duration("heartbeat-interval", "heartbeat_interval", defaultHeartbeatInterval)
	offlineTimeout := query.duration("offline-timeout", "offline_timeout", defaultOfflineTimeout)

	// OIDC configuration (optional)
	var oidcCfg *oidc.Config
	if issuer := query.get("oidc-issuer", "oidc_issuer"); issuer != "" {
		clientID := query.get("oidc-client-id", "oidc_client_id")
		if clientID == "" {
			return nil, fmt.Errorf("oidc_issuer requires oidc_client_id")
		}
		clientSecret := query.get("oidc-client-secret", "oidc_client_secret")
		redirectURL := query.get("oidc-redirect-url", "oidc_redirect_url")
		scopesStr := query.get("oidc-scopes", "oidc_scopes")
		var scopes []string
		if scopesStr != "" {
			scopes = strings.Split(scopesStr, ",")
		}
		cacheDir := query.get("oidc-cache-dir", "oidc_cache_dir")

		oidcCfg = &oidc.Config{
			IssuerURL:    issuer,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Scopes:       scopes,
			CacheDir:     cacheDir,
		}
	}

	// TLS configuration (optional; without tls=1 the connection stays
	// plaintext — backward compatible with existing deployments)
	tlsEnabled := query.pop("tls") == "1"
	tlsCfg, err := buildClientTLSConfig(tlsEnabled,
		query.get("tls-ca", "tls_ca"), query.get("tls-server-name", "tls_server_name"))
	if err != nil {
		return nil, err
	}

	u.RawQuery = values.Encode()
	addr = u.Host

	// Create gRPC connection
	var dialOpts []grpc.DialOption
	if tlsCfg != nil {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		// plaintext as before (insecure.NewCredentials is the non-deprecated
		// spelling of the previous WithInsecure() default)
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	conn, err := grpc.Dial(addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}

	client := pb.NewMetaServiceClient(conn)

	// Create caches
	attrCache := expirable.NewLRU[uint64, *Attr](attrCacheSize, nil, attrCacheTTL)
	dirCache := expirable.NewLRU[uint64, []*Entry](dirCacheSize, nil, dirCacheTTL)
	fekCache := expirable.NewLRU[uint64, *fekEntry](defaultFekCacheSize, func(_ uint64, e *fekEntry) {
		utils.MemClear(e.fek) // zero the plaintext FEK on eviction (NFR-SEC-3)
	}, defaultFekCacheTTL)

	// OIDC token manager (optional)
	var tm *oidc.TokenManager
	if oidcCfg != nil {
		var err error
		tm, err = oidc.NewTokenManager(*oidcCfg)
		if err != nil {
			return nil, fmt.Errorf("oidc token manager: %w", err)
		}
	}

	m := &grpcMeta{
		addr:              strings.TrimPrefix(addr, "://"),
		conf:              conf,
		client:            client,
		conn:              conn,
		attrCache:         attrCache,
		dirCache:          dirCache,
		fekCache:          fekCache,
		attrCacheTTL:      attrCacheTTL,
		dirCacheTTL:       dirCacheTTL,
		heartbeatInterval: heartbeatInterval,
		offlineTimeout:    offlineTimeout,
		lastPermGen:       -1, // unset until the first successful heartbeat (task 7.3)
		oidcConfig:        oidcCfg,
	}
	// Assign only when non-nil: wrapping a nil *oidc.TokenManager in the
	// tokenProvider interface makes the interface non-nil and panics in withAuth.
	if tm != nil {
		m.tokenManager = tm
	}

	return m, nil
}

// Register gRPC meta driver
func init() {
	Register("grpc", newGRPCMeta)
}

func (m *grpcMeta) Name() string {
	return "grpc://" + m.addr
}

func (m *grpcMeta) Shutdown() error {
	if !atomic.CompareAndSwapInt32(&m.closed, 0, 1) {
		return nil
	}

	// Stop heartbeat with timeout
	if m.heartbeatCancel != nil {
		m.heartbeatCancel()
		done := make(chan struct{})
		go func() {
			m.heartbeatWg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			logger.Warnf("Heartbeat stop timeout after 3s")
		}
	}

	// Stop OIDC token refresher
	if m.tokenManager != nil {
		m.tokenManager.Stop()
	}

	// Close session
	_ = m.CloseSession()

	// Close gRPC connection
	if m.conn != nil {
		if err := m.conn.Close(); err != nil {
			fmt.Printf("grpc close error: %v\n", err)
		}
	}

	m.attrCache.Purge()
	m.dirCache.Purge()
	m.WipeKeys()
	return nil
}

// WipeKeys zeroes all cached plaintext FEKs and drops the cache (NFR-SEC-3).
// Called on logout, OIDC token expiry and offline timeout; after it returns
// the client can no longer decrypt anything from its caches.
func (m *grpcMeta) WipeKeys() {
	if m.fekCache == nil {
		return
	}
	for _, e := range m.fekCache.Values() {
		utils.MemClear(e.fek)
	}
	m.fekCache.Purge() // onEvict zeroes the entries again — idempotent
}

// cacheFek stores a plaintext FEK in the LRU, pinning it in RAM first
// (best-effort, NFR-SEC-3).
func (m *grpcMeta) cacheFek(inode Ino, fek []byte, version uint32) {
	mlockBestEffort("FEK", fek)
	m.fekCache.Add(uint64(inode), &fekEntry{fek: fek, version: version})
}

// HubState returns the current hub connectivity state.
func (m *grpcMeta) HubState() HubState {
	return HubState(atomic.LoadInt32(&m.hubState))
}

// Subject returns the OIDC 'sub' claim of the cached ID token (the platform
// user ID, invariant A6); empty when OIDC is not configured or no token is
// cached. Used by the STS refresher to name the requesting user (task 7.4).
func (m *grpcMeta) Subject(ctx context.Context) string {
	if m.tokenManager == nil {
		return ""
	}
	return m.tokenManager.Subject(ctx)
}

// SetOnWipe installs the callback invoked when keys are wiped due to
// disconnect or logout (VFS.InvalidateAllKeys, task 6.4 wiring).
func (m *grpcMeta) SetOnWipe(cb func()) {
	m.hubMu.Lock()
	defer m.hubMu.Unlock()
	m.onWipe = cb
}

// SetOnReconnect installs the callback fired when the hub transitions from
// offline-connected back to online, so deferred writes can be synced
// (VFS.ReplayWriteJournal, task 6.3). It is suppressed on a beat that also
// wipes the keys (permission generation increase): replaying under destroyed
// FEKs would commit undecryptable data (design A2).
func (m *grpcMeta) SetOnReconnect(cb func()) {
	m.hubMu.Lock()
	defer m.hubMu.Unlock()
	m.onReconnect = cb
}

// offlineEvents counts transitions of the hub state into offline-connected
// (the first lost contact of an outage, not every failed heartbeat; task 10.4).
var offlineEvents = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "offline_events",
	Help: "number of times the hub connection was lost (client went offline-connected)",
})

// markHubOffline records a failed RPC: the first failure of an outage starts
// the offline window (lastFail), later failures do not extend it.
func (m *grpcMeta) markHubOffline() {
	if atomic.CompareAndSwapInt32(&m.hubState, int32(HubOnline), int32(HubOfflineConnected)) {
		offlineEvents.Inc()
		m.hubMu.Lock()
		m.lastFail = time.Now()
		m.hubMu.Unlock()
		logger.Warnf("hub unreachable: offline-connected for up to %s", m.offlineTimeout)
	}
}

// markHubOnline records a successful RPC. A disconnected client is terminal
// until remount (design 6.6): keys are already wiped, so a late recovery does
// not resurrect the session. It returns true iff the state transitioned
// offline-connected → online on this call; doHeartbeat fires the reconnect
// hook (write journal replay, task 6.3) only after it has also applied the
// beat's permission generation — a revocation wipe must never race the replay
// (design A2). The hook runs in a goroutine so a large replay does not stall
// the heartbeat.
func (m *grpcMeta) markHubOnline() bool {
	if m.HubState() == HubDisconnected {
		return false
	}
	wasOffline := atomic.CompareAndSwapInt32(&m.hubState, int32(HubOfflineConnected), int32(HubOnline))
	if wasOffline {
		logger.Infof("hub reachable again: back online")
	}
	return wasOffline
}

// checkOfflineTimeout enforces the offline window (NFR-OFF-4): once
// offlineTimeout has elapsed since the first failed RPC, disconnect and wipe.
func (m *grpcMeta) checkOfflineTimeout() {
	if m.HubState() != HubOfflineConnected {
		return
	}
	m.hubMu.Lock()
	exceeded := time.Since(m.lastFail) >= m.offlineTimeout
	m.hubMu.Unlock()
	if exceeded {
		m.disconnect(fmt.Sprintf("offline for %s", m.offlineTimeout))
	}
}

// Logout wipes all keys and marks the client disconnected (terminal until
// remount). Called by the VFS logout watcher on _JFS_LOGOUT (design 6.6).
func (m *grpcMeta) Logout() {
	m.disconnect("logout")
}

// disconnect wipes all keys and marks the client disconnected (one-shot,
// terminal until remount — design 6.6). Called on logout, OIDC expiry and
// offline timeout.
func (m *grpcMeta) disconnect(reason string) {
	m.hubMu.Lock()
	defer m.hubMu.Unlock()
	if m.HubState() == HubDisconnected {
		return
	}
	atomic.StoreInt32(&m.hubState, int32(HubDisconnected))
	logger.Warnf("hub disconnected (%s): wiping keys", reason)
	m.WipeKeys()
	if m.onWipe != nil {
		m.onWipe()
	}
}

// withAuth adds session ID and OIDC bearer token (if configured) to gRPC metadata.
// Uses singleflight to coalesce concurrent token requests into one auth flow.
func (m *grpcMeta) withAuth(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	// Fail fast once disconnected: keys are wiped and the state is terminal
	// until remount, so every RPC must fail immediately instead of hanging on
	// a dead connection (NFR-OFF-4).
	if m.HubState() == HubDisconnected {
		c, cancel := context.WithCancel(context.Background())
		cancel()
		return c
	}

	md := make(metadata.MD, 2)

	if m.sid != 0 {
		md["x-session-id"] = []string{fmt.Sprintf("%d", m.sid)}
	}

	// Add OIDC bearer token if configured.
	// singleflight coalesces concurrent calls so only one triggers browser auth.
	if m.tokenManager != nil {
		bearer, _, _ := m.authGroup.Do("token", func() (interface{}, error) {
			return m.tokenManager.BearerToken(ctx), nil
		})
		if token, ok := bearer.(string); ok && token != "" {
			md["authorization"] = []string{token}
		}
	}

	if len(md) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, md)
	}

	return ctx
}

// withSessionID is deprecated; use withAuth instead.
// Kept for backward compatibility during migration.
func (m *grpcMeta) withSessionID(ctx context.Context) context.Context {
	return m.withAuth(ctx)
}

// grpcContext converts Context to pb.MetaContext
func (m *grpcMeta) grpcContext(ctx Context) *pb.MetaContext {
	if ctx == nil {
		return &pb.MetaContext{SessionId: m.sid}
	}
	gids := ctx.Gids()
	if len(gids) == 0 {
		gids = []uint32{ctx.Gid()}
	}
	return &pb.MetaContext{
		Uid:             uint32(ctx.Uid()),
		Gid:             uint32(ctx.Gid()),
		Gids:            gids,
		Pid:             uint32(ctx.Pid()),
		CheckPermission: true,
		SessionId:       m.sid,
	}
}

// Cache operations

// invalidateAttrCache removes an inode from the attribute cache
func (m *grpcMeta) invalidateAttrCache(inode uint64) {
	m.attrCache.Remove(inode)
}

// invalidateDirCache removes an inode from the directory cache
func (m *grpcMeta) invalidateDirCache(inode uint64) {
	m.dirCache.Remove(inode)
}

// getAttrFromCache retrieves an attribute from cache
func (m *grpcMeta) getAttrFromCache(inode uint64) (*Attr, bool) {
	attr, found := m.attrCache.Get(inode)
	if found {
		return attr, true
	}
	return nil, false
}

// putAttrInCache stores an attribute in cache
func (m *grpcMeta) putAttrInCache(inode uint64, attr *Attr) {
	if attr == nil {
		return
	}
	m.attrCache.Add(inode, attr)
}

// getDirFromCache retrieves directory entries from cache
func (m *grpcMeta) getDirFromCache(inode uint64) ([]*Entry, bool) {
	entries, found := m.dirCache.Get(inode)
	if found {
		return entries, true
	}
	return nil, false
}

// putDirInCache stores directory entries in cache
func (m *grpcMeta) putDirInCache(inode uint64, entries []*Entry) {
	if entries == nil {
		return
	}
	m.dirCache.Add(inode, entries)
}

// startHeartbeat starts the heartbeat goroutine
func (m *grpcMeta) startHeartbeat() {
	ctx, cancel := context.WithCancel(context.Background())
	m.heartbeatCancel = cancel

	m.heartbeatWg.Add(1)
	go func() {
		defer m.heartbeatWg.Done()
		ticker := time.NewTicker(m.heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.doHeartbeat()
			}
		}
	}()
}

// doHeartbeat sends a heartbeat to the server and drives the hub state
// machine (stage 6): failure → offline-connected, success → online, and the
// offline window is enforced on every failed beat.
func (m *grpcMeta) doHeartbeat() {
	// OIDC session check (design 6.7): while offline-connected, an empty
	// cached bearer means the token is expired or revoked — disconnect now
	// instead of waiting for the offline timeout. Non-blocking: never
	// triggers browser auth.
	if m.tokenManager != nil && m.HubState() == HubOfflineConnected {
		if m.tokenManager.CachedBearerToken(context.Background()) == "" {
			m.disconnect("oidc token expired or revoked")
			return
		}
	}

	base := m.heartbeatBaseCtx
	if base == nil {
		base = context.Background()
	}
	ctx := m.withAuth(base)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req := &pb.FlushSessionRequest{}
	resp, err := m.client.FlushSession(ctx, req)
	if err != nil {
		logger.Debugf("Heartbeat error: %v", err)
		m.markHubOffline()
		m.checkOfflineTimeout()
		return
	}
	if resp.GetErrno() != 0 {
		logger.Debugf("Heartbeat errno: %d", resp.GetErrno())
		m.markHubOffline()
		m.checkOfflineTimeout()
		return
	}
	// Ordering (design A2): the reconnect hook (write journal replay) fires
	// only when this beat both reconnected the client and did NOT wipe its
	// keys. On a reconnect beat that also carries a generation bump the wipe
	// wins: replaying under destroyed FEKs would commit undecryptable data, so
	// the journal is left for the next mount (NewVFS replays after
	// loadAllHandles with fresh FEKs).
	reconnected := m.markHubOnline()
	wiped := m.applyPermissionGeneration(resp.GetPermissionGeneration())
	if reconnected && !wiped {
		m.hubMu.Lock()
		cb := m.onReconnect
		m.hubMu.Unlock()
		if cb != nil {
			go cb()
		}
	}
}

// applyPermissionGeneration applies the heartbeat's permission generation
// (task 7.3, FR-REV-2/7): the platform counter only increases (INCR on every
// role change), so the client wipes all key material exactly when the value
// goes up. The first successful beat initializes the baseline without wiping —
// a user with historical role changes must not lose freshly issued keys at
// mount. A 0 or lower value (KeyManager unavailable, see FlushSession) is a
// no-op: an outage must not revoke authorized users. It returns true iff this
// beat wiped the keys; doHeartbeat uses that to suppress the reconnect hook
// on the same beat (design A2).
func (m *grpcMeta) applyPermissionGeneration(gen uint64) bool {
	g := int64(gen)
	if m.lastPermGen < 0 {
		m.lastPermGen = g
		return false
	}
	if g <= m.lastPermGen {
		return false
	}
	m.lastPermGen = g
	logger.Warnf("permission generation increased to %d: wiping keys (revocation)", g)
	m.WipeKeys()
	m.hubMu.Lock()
	cb := m.onWipe
	m.hubMu.Unlock()
	if cb != nil {
		cb() // VFS.InvalidateAllKeys: stale handles fail with EIO on next use
	}
	return true
}

// Check integrity of an absolute path and repair it if asked
func (m *grpcMeta) Check(ctx Context, fpath string, opt *CheckOpt) error {
	c := m.grpcContext(ctx)
	req := &pb.CheckRequest{
		Ctx:           c,
		Fpath:         fpath,
		Repair:        opt != nil && opt.Repair,
		Recursive:     opt != nil && opt.Recursive,
		SyncDirStat:   opt != nil && opt.SyncDirStat,
		RepairDirMode: uint32(opt.RepairDirMode),
	}
	resp, err := m.client.Check(m.withSessionID(ctx), req)
	if err != nil {
		return err
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	return nil
}

// Chroot changes root to a directory specified by subdir
func (m *grpcMeta) Chroot(ctx Context, subdir string) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.ChrootRequest{
		Ctx:    c,
		Subdir: subdir,
	}
	resp, err := m.client.Chroot(m.withSessionID(ctx), req)
	if err != nil {
		return grpcStatusToErrno(err)
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	return 0
}

// grpcStatusToErrno maps a gRPC status error to an appropriate syscall.Errno.
func grpcStatusToErrno(err error) syscall.Errno {
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.PermissionDenied, codes.Unauthenticated:
			return syscall.EACCES
		case codes.NotFound:
			return syscall.ENOENT
		case codes.AlreadyExists:
			return syscall.EEXIST
		case codes.InvalidArgument:
			return syscall.EINVAL
		default:
			return syscall.EIO
		}
	}
	return syscall.EIO
}

// GetPaths returns all paths of a given inode
func (m *grpcMeta) GetPaths(ctx Context, inode Ino) []string {
	c := m.grpcContext(ctx)
	req := &pb.GetPathsRequest{
		Ctx:   c,
		Inode: uint64(inode),
	}
	resp, err := m.client.GetPaths(m.withSessionID(ctx), req)
	if err != nil {
		return nil
	}
	if resp.GetErrno() != 0 {
		return nil
	}
	return resp.Paths
}
