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
	"io"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *MetaProxyServer) DumpMeta(req *pb.DumpMetaRequest, stream pb.MetaService_DumpMetaServer) error {
	pr, pw := io.Pipe()
	go func() {
		_ = s.meta.DumpMeta(pw, Ino(req.Root), int(req.Threads), req.KeepSecret, req.Fast, req.SkipTrash)
		pw.Close()
	}()
	buf := make([]byte, 64*1024)
	for {
		n, readErr := pr.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&pb.DumpMetaChunk{Data: buf[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func (s *MetaProxyServer) LoadMeta(stream pb.MetaService_LoadMetaServer) error {
	pr, pw := io.Pipe()
	go func() {
		for {
			chunk, recvErr := stream.Recv()
			if recvErr == io.EOF {
				pw.Close()
				return
			}
			if recvErr != nil {
				pw.CloseWithError(recvErr)
				return
			}
			_, writeErr := pw.Write(chunk.Data)
			if writeErr != nil {
				pw.CloseWithError(writeErr)
				return
			}
		}
	}()
	err := s.meta.LoadMeta(pr)
	pw.Close()
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return stream.SendAndClose(&pb.LoadMetaResponse{Errno: 0})
}

func (s *MetaProxyServer) DumpMetaV2(req *pb.DumpMetaV2Request, stream pb.MetaService_DumpMetaV2Server) error {
	mctx := s.metaCtx(stream.Context(), req.Ctx)
	opt := &DumpOption{
		KeepSecret: req.KeepSecret,
		Threads:    int(req.Threads),
	}
	pr, pw := io.Pipe()
	go func() {
		_ = s.meta.DumpMetaV2(mctx, pw, opt)
		pw.Close()
	}()
	buf := make([]byte, 64*1024)
	for {
		n, readErr := pr.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&pb.DumpMetaV2Chunk{Data: buf[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func (s *MetaProxyServer) LoadMetaV2(stream pb.MetaService_LoadMetaV2Server) error {
	pr, pw := io.Pipe()
	go func() {
		for {
			chunk, recvErr := stream.Recv()
			if recvErr == io.EOF {
				pw.Close()
				return
			}
			if recvErr != nil {
				pw.CloseWithError(recvErr)
				return
			}
			_, writeErr := pw.Write(chunk.Data)
			if writeErr != nil {
				pw.CloseWithError(writeErr)
				return
			}
		}
	}()
	opt := &LoadOption{Threads: 10}
	err := s.meta.LoadMetaV2(Background(), pr, opt)
	pw.Close()
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return stream.SendAndClose(&pb.LoadMetaV2Response{Errno: 0})
}
