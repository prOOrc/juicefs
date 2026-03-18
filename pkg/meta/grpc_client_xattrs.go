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

// --- Xattrs operations ---

// GetXattr gets extended attribute
func (c *GRPCClient) GetXattr(ctx Context, ino Ino, name string, vbuff *[]byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetXattr(grpcCtx, &pb.GetXattrRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Name:  name,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if vbuff != nil {
		*vbuff = resp.GetValue()
	}
	return 0
}

// SetXattr sets extended attribute
func (c *GRPCClient) SetXattr(ctx Context, ino Ino, name string, value []byte, flags uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.SetXattr(grpcCtx, &pb.SetXattrRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Name:  name,
		Value: value,
		Flags: flags,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// RemoveXattr removes extended attribute
func (c *GRPCClient) RemoveXattr(ctx Context, ino Ino, name string) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.RemoveXattr(grpcCtx, &pb.RemoveXattrRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Name:  name,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// ListXattr lists extended attributes
func (c *GRPCClient) ListXattr(ctx Context, ino Ino, names *[]byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ListXattr(grpcCtx, &pb.ListXattrRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if names != nil {
		*names = resp.GetNames()
	}
	return 0
}
