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
	"io"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// DumpMeta dumps metadata
func (c *Client) DumpMeta(root meta.Ino, threads int32, keepSecret, fast, skipTrash bool) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		stream, err := c.client.DumpMeta(grpcCtx, &pb.DumpMetaRequest{
			Root:       uint64(root),
			Threads:    threads,
			KeepSecret: keepSecret,
			Fast:       fast,
			SkipTrash:  skipTrash,
		})
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(chunk.GetData()); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()
	return pr, nil
}

// LoadMeta loads metadata
func (c *Client) LoadMeta(rc io.ReadCloser) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stream, err := c.client.LoadMeta(grpcCtx)
	if err != nil {
		return err
	}

	buf := make([]byte, 64*1024)
	for {
		n, err := rc.Read(buf)
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
func (c *Client) DumpMetaV2(ctx meta.Context, keepSecret bool, threads int32) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		stream, err := c.client.DumpMetaV2(grpcCtx, &pb.DumpMetaV2Request{
			Ctx:        toProtoContext(ctx),
			KeepSecret: keepSecret,
			Threads:    threads,
		})
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(chunk.GetData()); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()
	return pr, nil
}

// LoadMetaV2 loads metadata v2
func (c *Client) LoadMetaV2(ctx meta.Context, rc io.ReadCloser) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stream, err := c.client.LoadMetaV2(grpcCtx)
	if err != nil {
		return err
	}

	buf := make([]byte, 64*1024)
	for {
		n, err := rc.Read(buf)
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
