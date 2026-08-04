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
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	// Cache defaults
	defaultAttrCacheSize     = 100_000
	defaultDirCacheSize      = 5_000
	defaultAttrCacheTTL      = time.Second
	defaultDirCacheTTL       = time.Second
	defaultHeartbeatInterval = 12 * time.Second
)

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
	// Cache TTLs
	attrCacheTTL time.Duration
	dirCacheTTL  time.Duration

	// Heartbeat
	heartbeatInterval time.Duration
	heartbeatCancel   context.CancelFunc
	heartbeatWg       sync.WaitGroup

	// OIDC (nil if not configured)
	oidcConfig   *oidc.Config
	tokenManager tokenProvider // interface for testability
	authGroup    singleflight.Group // coalesces concurrent token requests
}

// tokenProvider is the minimal interface needed by withAuth and Shutdown.
// Implemented by *oidc.TokenManager.
type tokenProvider interface {
	BearerToken(ctx context.Context) string
	Stop()
}

var _ tokenProvider = (*oidc.TokenManager)(nil)

var _ Meta = (*grpcMeta)(nil)

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

	u.RawQuery = values.Encode()
	addr = u.Host

	// Create gRPC connection
	conn, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}

	client := pb.NewMetaServiceClient(conn)

	// Create caches
	attrCache := expirable.NewLRU[uint64, *Attr](attrCacheSize, nil, attrCacheTTL)
	dirCache := expirable.NewLRU[uint64, []*Entry](dirCacheSize, nil, dirCacheTTL)

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
		attrCacheTTL:      attrCacheTTL,
		dirCacheTTL:       dirCacheTTL,
		heartbeatInterval: heartbeatInterval,
		oidcConfig:        oidcCfg,
		tokenManager:      tm,
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
	return nil
}

// withAuth adds session ID and OIDC bearer token (if configured) to gRPC metadata.
// Uses singleflight to coalesce concurrent token requests into one auth flow.
func (m *grpcMeta) withAuth(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
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

// doHeartbeat sends a heartbeat to the server
func (m *grpcMeta) doHeartbeat() {
	ctx := m.withAuth(context.Background())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req := &pb.FlushSessionRequest{}
	resp, err := m.client.FlushSession(ctx, req)
	if err != nil {
		logger.Debugf("Heartbeat error: %v", err)
		return
	}
	if resp.GetErrno() != 0 {
		logger.Debugf("Heartbeat errno: %d", resp.GetErrno())
	}
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
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	return 0
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
