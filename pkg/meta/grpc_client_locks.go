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
	"syscall"

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// --- Locks operations ---

// Flock manages file locks
func (c *GRPCClient) Flock(ctx Context, ino Ino, owner uint64, ltype uint32, block bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Flock(grpcCtx, &pb.FlockRequest{
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
func (c *GRPCClient) Getlk(ctx Context, ino Ino, owner uint64, ltype *uint32, start, end *uint64, pid *uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Getlk(grpcCtx, &pb.GetlkRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Owner: owner,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if ltype != nil {
		*ltype = resp.GetLtype()
	}
	if start != nil {
		*start = resp.GetStart()
	}
	if end != nil {
		*end = resp.GetEnd()
	}
	if pid != nil {
		*pid = resp.GetPid()
	}
	return 0
}

// Setlk sets a lock
func (c *GRPCClient) Setlk(ctx Context, ino Ino, owner uint64, block bool, ltype uint32, start, end uint64, pid uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Setlk(grpcCtx, &pb.SetlkRequest{
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

// ListLocks returns all locks of a inode
func (c *GRPCClient) ListLocks(ctx context.Context, inode Ino) ([]PLockItem, []FLockItem, error) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	metaCtx := NewGRPCContext(ctx, 0, 0, 0, nil, false)
	resp, err := c.client.ListLocks(grpcCtx, &pb.ListLocksRequest{
		Ctx:   toProtoContext(metaCtx),
		Inode: uint64(inode),
	})
	if err != nil {
		return nil, nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, nil, syscall.Errno(resp.GetErrno())
	}

	plocks := make([]PLockItem, 0, len(resp.GetPlocks()))
	for _, p := range resp.GetPlocks() {
		plocks = append(plocks, PLockItem{
			ownerKey: ownerKey{
				Sid:   p.GetOwner() >> 32,
				Owner: uint64(uint32(p.GetOwner())),
			},
			plockRecord: plockRecord{
				Type:  p.GetRecords()[0].GetType(),
				Pid:   p.GetRecords()[0].GetPid(),
				Start: p.GetRecords()[0].GetStart(),
				End:   p.GetRecords()[0].GetEnd(),
			},
		})
	}

	flocks := make([]FLockItem, 0, len(resp.GetFlocks()))
	for _, f := range resp.GetFlocks() {
		flocks = append(flocks, FLockItem{
			ownerKey: ownerKey{
				Sid:   f.GetOwner() >> 32,
				Owner: uint64(uint32(f.GetOwner())),
			},
			Type: f.GetLtype(),
		})
	}

	return plocks, flocks, nil
}
