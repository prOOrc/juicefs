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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// --- Lifecycle operations for grpcMeta ---

// Init initializes the filesystem
func (m *grpcMeta) Init(format *Format, force bool) error {
	ctx := context.Background()
	req := &pb.InitRequest{
		Format: FormatToProto(*format),
		Force:  force,
	}
	resp, err := m.client.Init(ctx, req)
	if err != nil {
		return err
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	return nil
}

// Load loads the existing format
func (m *grpcMeta) Load(checkVersion bool) (*Format, error) {
	ctx := context.Background()
	req := &pb.LoadRequest{CheckVersion: checkVersion}
	resp, err := m.client.Load(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	m.mu.Lock()
	m.format = ProtoToFormat(resp.Format)
	m.mu.Unlock()
	// Create session after loading format
	if err := m.NewSession(false); err != nil {
		return nil, err
	}
	return m.format, nil
}

// NewSession creates a new session and starts heartbeat
func (m *grpcMeta) NewSession(record bool) error {
	ctx := context.Background()
	req := &pb.NewSessionRequest{Record: record}
	resp, err := m.client.NewSession(ctx, req)
	if err != nil {
		logger.Errorf("NewSession gRPC error: %v", err)
		return err
	}
	if resp.GetErrno() != 0 {
		logger.Errorf("NewSession errno: %d", resp.GetErrno())
		return syscall.Errno(resp.GetErrno())
	}
	m.sid = resp.GetSid()
	logger.Infof("NewSession succeeded, sid=%d", m.sid)
	m.startHeartbeat()
	return nil
}

// CloseSession closes the current session
func (m *grpcMeta) CloseSession() error {
	if !atomic.CompareAndSwapInt32(&m.closed, 0, 1) {
		return nil
	}

	// Stop heartbeat first with timeout
	if m.heartbeatCancel != nil {
		m.heartbeatCancel()
		done := make(chan struct{})
		go func() {
			m.heartbeatWg.Wait()
			close(done)
		}()
		select {
		case <-done:
			logger.Debugf("Heartbeat stopped")
		case <-time.After(3 * time.Second):
			logger.Warnf("Heartbeat stop timeout after 3s")
		}
	}

	// Close gRPC connection
	if m.conn != nil {
		m.conn.Close()
	}

	// Notify server session is closed
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := &pb.CloseSessionRequest{}
	resp, err := m.client.CloseSession(ctx, req)
	if err != nil {
		logger.Debugf("CloseSession gRPC error (may be expected): %v", err)
		return nil
	}
	if resp.GetErrno() != 0 {
		logger.Debugf("CloseSession errno: %d", resp.GetErrno())
	}
	return nil
}

// FlushSession flushes session state
func (m *grpcMeta) FlushSession() {
	ctx := context.Background()
	req := &pb.FlushSessionRequest{}
	resp, err := m.client.FlushSession(ctx, req)
	if err != nil {
		return
	}
	if resp.GetErrno() != 0 {
	}
}

// GetSession gets session info
func (m *grpcMeta) GetSession(sid uint64, detail bool) (*Session, error) {
	ctx := context.Background()
	req := &pb.GetSessionRequest{Sid: sid, Detail: detail}
	resp, err := m.client.GetSession(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	return ProtoToSession(resp.Session), nil
}

// ListSessions lists all sessions
func (m *grpcMeta) ListSessions() ([]*Session, error) {
	ctx := context.Background()
	req := &pb.ListSessionsRequest{}
	resp, err := m.client.ListSessions(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	sessions := make([]*Session, len(resp.Sessions))
	for i, s := range resp.Sessions {
		sessions[i] = ProtoToSession(s)
	}
	return sessions, nil
}

// CleanStaleSessions cleans stale sessions (not implemented on client)
func (m *grpcMeta) CleanStaleSessions(ctx Context) {
}

// ScanDeletedObject is not supported by gRPC client
func (m *grpcMeta) ScanDeletedObject(ctx Context, trashSliceScan trashSliceScan, pendingSliceScan pendingSliceScan, trashFileScan trashFileScan, pendingFileScan pendingFileScan) error {
	return syscall.ENOTSUP
}

// ListLocks returns all locks of a inode
func (m *grpcMeta) ListLocks(ctx context.Context, inode Ino) ([]PLockItem, []FLockItem, error) {
	return nil, nil, nil
}

// CleanupTrashBefore is not supported by gRPC client
func (m *grpcMeta) CleanupTrashBefore(ctx Context, edge time.Time, increProgress func(int), stats *CleanupTrashStats) syscall.Errno {
	return syscall.ENOTSUP
}

// CleanupDetachedNodesBefore is not supported by gRPC client
func (m *grpcMeta) CleanupDetachedNodesBefore(ctx Context, edge time.Time, increProgress func()) {
}

// ScanChangelog scans changelog entries starting from the given version
func (m *grpcMeta) ScanChangelog(ctx Context, last int64, handler func(ver int64, entry string) error) error {
	c := m.grpcContext(ctx)
	stream, err := m.client.ScanChangelog(m.withSessionID(ctx), &pb.ScanChangelogRequest{
		Ctx:  c,
		Last: last,
	})
	if err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		if resp.GetErrno() != 0 {
			return syscall.Errno(resp.GetErrno())
		}
		for _, entry := range resp.Entries {
			if err := handler(entry.Ver, entry.Data); err != nil {
				return err
			}
		}
	}
}
