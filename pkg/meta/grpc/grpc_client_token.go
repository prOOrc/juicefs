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
	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// --- Token operations ---

// StoreToken stores a token
func (c *Client) StoreToken(ctx meta.Context, token []byte, id *uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.StoreToken(grpcCtx, &pb.StoreTokenRequest{
		Ctx:   toProtoContext(ctx),
		Token: token,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if id != nil {
		*id = resp.GetId()
	}
	return 0
}

// UpdateToken updates a token
func (c *Client) UpdateToken(ctx meta.Context, id uint32, token []byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.UpdateToken(grpcCtx, &pb.UpdateTokenRequest{
		Ctx:   toProtoContext(ctx),
		Id:    id,
		Token: token,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// LoadToken loads a token
func (c *Client) LoadToken(ctx meta.Context, id uint32, token *[]byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.LoadToken(grpcCtx, &pb.LoadTokenRequest{
		Ctx: toProtoContext(ctx),
		Id:  id,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if token != nil {
		*token = resp.GetToken()
	}
	return 0
}

// DeleteTokens deletes tokens
func (c *Client) DeleteTokens(ctx meta.Context, ids []uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.DeleteTokens(grpcCtx, &pb.DeleteTokensRequest{
		Ctx: toProtoContext(ctx),
		Ids: ids,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// ListTokens lists tokens
func (c *Client) ListTokens(ctx meta.Context, tokens *map[uint32][]byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ListTokens(grpcCtx, &pb.ListTokensRequest{
		Ctx: toProtoContext(ctx),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	result := make(map[uint32][]byte, len(resp.GetTokens()))
	for k, v := range resp.GetTokens() {
		result[k] = v
	}
	if tokens != nil {
		*tokens = result
	}
	return 0
}
