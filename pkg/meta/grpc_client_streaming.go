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
	"io"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// DumpMeta dumps metadata
func (c *GRPCClient) DumpMeta(w io.Writer, root Ino, threads int, keepSecret, fast, skipTrash bool) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stream, err := c.client.DumpMeta(grpcCtx, &pb.DumpMetaRequest{
		Root:       uint64(root),
		Threads:    int32(threads),
		KeepSecret: keepSecret,
		Fast:       fast,
		SkipTrash:  skipTrash,
	})
	if err != nil {
		return err
	}

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if _, err := w.Write(chunk.GetData()); err != nil {
			return err
		}
	}
	return nil
}

// LoadMeta loads metadata
func (c *GRPCClient) LoadMeta(r io.Reader) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stream, err := c.client.LoadMeta(grpcCtx)
	if err != nil {
		return err
	}

	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&pb.LoadMetaChunk{Data: buf[:n]}); err != nil {
				return err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// DumpMetaV2 dumps metadata v2
func (c *GRPCClient) DumpMetaV2(ctx Context, w io.Writer, opt *DumpOption) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stream, err := c.client.DumpMetaV2(grpcCtx, &pb.DumpMetaV2Request{
		Ctx:        toProtoContext(ctx),
		KeepSecret: opt.KeepSecret,
		Threads:    int32(opt.Threads),
	})
	if err != nil {
		return err
	}

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if _, err := w.Write(chunk.GetData()); err != nil {
			return err
		}
	}
	return nil
}

// LoadMetaV2 loads metadata v2
func (c *GRPCClient) LoadMetaV2(ctx Context, r io.Reader, opt *LoadOption) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stream, err := c.client.LoadMetaV2(grpcCtx)
	if err != nil {
		return err
	}

	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&pb.LoadMetaV2Chunk{Data: buf[:n]}); err != nil {
				return err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}
