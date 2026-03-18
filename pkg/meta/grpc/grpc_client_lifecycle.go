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

package grpc

import (
	"context"
	"syscall"

	"github.com/juicedata/juicefs/pkg/meta"
)

// --- Lifecycle operations ---

// Init initializes the volume
func (c *Client) Init(format *meta.Format, force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	req := &InitRequest{
		Format: toProtoFormat(format),
		Force:  force,
	}
	resp, err := c.client.Init(ctx, req)
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// Load loads the volume
func (c *Client) Load(checkVersion bool) (*meta.Format, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Load(ctx, &LoadRequest{
		CheckVersion: checkVersion,
	})
	if err != nil {
		return nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	return fromProtoFormat(resp.GetFormat()), nil
}

// NewSession creates a new session
func (c *Client) NewSession(record bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.NewSession(ctx, &NewSessionRequest{
		Record: record,
	})
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// CloseSession closes a session
func (c *Client) CloseSession() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CloseSession(ctx, &CloseSessionRequest{})
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// FlushSession flushes session data
func (c *Client) FlushSession() {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.FlushSession(ctx, &FlushSessionRequest{})
	if err == nil && resp != nil {
		_ = resp.GetErrno()
	}
}

// Shutdown shuts down the volume
func (c *Client) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Shutdown(ctx, &ShutdownRequest{})
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// Reset resets the volume
func (c *Client) Reset() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Reset(ctx, &ResetRequest{})
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// GetSession gets session info
func (c *Client) GetSession(sid uint64, detail bool) (*meta.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetSession(ctx, &GetSessionRequest{
		Sid:    sid,
		Detail: detail,
	})
	if err != nil {
		return nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	return fromProtoSession(resp.GetSession()), nil
}

// ListSessions lists all sessions
func (c *Client) ListSessions() ([]*meta.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ListSessions(ctx, &ListSessionsRequest{})
	if err != nil {
		return nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	sessions := make([]*meta.Session, 0, len(resp.GetSessions()))
	for _, s := range resp.GetSessions() {
		sessions = append(sessions, fromProtoSession(s))
	}
	return sessions, nil
}

// CleanStaleSessions cleans stale sessions
func (c *Client) CleanStaleSessions(ctx meta.Context) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CleanStaleSessions(grpcCtx, &CleanStaleSessionsRequest{
		Ctx: toProtoContext(ctx),
	})
	if err == nil && resp != nil {
		_ = resp.GetErrno()
	}
}
