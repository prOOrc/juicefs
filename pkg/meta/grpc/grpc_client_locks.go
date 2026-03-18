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

// --- Locks operations ---

// Flock manages file locks
func (c *Client) Flock(ctx meta.Context, ino meta.Ino, owner uint64, ltype uint32, block bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Flock(grpcCtx, &FlockRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Owner: owner,
		Ltype: ltype,
		Block: block,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// Getlk gets lock information
func (c *Client) Getlk(ctx meta.Context, ino meta.Ino, owner uint64, ltype uint32, start, end uint64) (ltypeOut uint32, startOut uint64, endOut uint64, pid uint32, errno syscall.Errno) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Getlk(grpcCtx, &GetlkRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Owner: owner,
		Ltype: ltype,
		Start: start,
		End:   end,
	})
	if err != nil {
		return 0, 0, 0, 0, syscall.EIO
	}
	return resp.GetLtype(), resp.GetStart(), resp.GetEnd(), resp.GetPid(), syscall.Errno(resp.GetErrno())
}

// Setlk sets a lock
func (c *Client) Setlk(ctx meta.Context, ino meta.Ino, owner uint64, block bool, ltype uint32, start, end uint64, pid uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Setlk(grpcCtx, &SetlkRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Owner: owner,
		Block: block,
		Ltype: ltype,
		Start: start,
		End:   end,
		Pid:   pid,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}
