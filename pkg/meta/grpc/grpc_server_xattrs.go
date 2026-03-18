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

	"github.com/juicedata/juicefs/pkg/meta"
)

func (s *MetaProxyServer) GetXattr(ctx context.Context, req *GetXattrRequest) (*GetXattrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var value []byte
	errno := s.meta.GetXattr(mctx, meta.Ino(req.Inode), req.Name, &value)
	return &GetXattrResponse{
		Errno: uint32(errno),
		Value: value,
	}, nil
}

func (s *MetaProxyServer) SetXattr(ctx context.Context, req *SetXattrRequest) (*SetXattrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.SetXattr(mctx, meta.Ino(req.Inode), req.Name, req.Value, uint32(req.Flags))
	return &SetXattrResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) RemoveXattr(ctx context.Context, req *RemoveXattrRequest) (*RemoveXattrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.RemoveXattr(mctx, meta.Ino(req.Inode), req.Name)
	return &RemoveXattrResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ListXattr(ctx context.Context, req *ListXattrRequest) (*ListXattrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var names []byte
	errno := s.meta.ListXattr(mctx, meta.Ino(req.Inode), &names)
	return &ListXattrResponse{
		Errno: uint32(errno),
		Names: names,
	}, nil
}
