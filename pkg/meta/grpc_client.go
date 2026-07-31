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
}

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

// withSessionID adds session ID to gRPC metadata
func (m *grpcMeta) withSessionID(ctx context.Context) context.Context {
	if m.sid == 0 {
		return ctx
	}
	md := metadata.Pairs("x-session-id", fmt.Sprintf("%d", m.sid))
	return metadata.NewOutgoingContext(ctx, md)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
