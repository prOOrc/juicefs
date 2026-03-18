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

	aclAPI "github.com/juicedata/juicefs/pkg/acl"
	"github.com/juicedata/juicefs/pkg/meta"
)

func (s *MetaProxyServer) SetFacl(ctx context.Context, req *SetFaclRequest) (*SetFaclResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.SetFacl(mctx, meta.Ino(req.Ino), uint8(req.AclType), nil)
	return &SetFaclResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) GetFacl(ctx context.Context, req *GetFaclRequest) (*GetFaclResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var rule aclAPI.Rule
	errno := s.meta.GetFacl(mctx, meta.Ino(req.Ino), uint8(req.AclType), &rule)
	return &GetFaclResponse{Errno: uint32(errno)}, nil
}
