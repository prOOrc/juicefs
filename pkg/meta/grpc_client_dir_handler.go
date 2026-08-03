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

// grpcDirHandler implements DirHandler interface for gRPC client
type grpcDirHandler struct {
	meta   *grpcMeta
	client pb.MetaServiceClient
	handle *pb.DirHandlerHandle
}

// authCtx returns the context with auth headers, or the original if meta is nil.
func (h *grpcDirHandler) authCtx(ctx context.Context) context.Context {
	if h.meta != nil {
		return h.meta.withAuth(ctx)
	}
	return ctx
}

// List returns directory entries starting from offset
func (h *grpcDirHandler) List(ctx Context, offset int) ([]*Entry, syscall.Errno) {
	req := &pb.DirHandlerListRequest{
		Handle: h.handle,
		Offset: int32(offset),
	}
	resp, err := h.client.DirHandlerList(h.authCtx(ctx), req)
	if err != nil {
		if h.meta != nil && isUnauthenticated(err) && h.meta.tryReauthenticate(context.Background()) {
			resp, err = h.client.DirHandlerList(h.authCtx(ctx), req)
		}
		if err != nil {
			logger.Errorf("DirHandlerList error: %v", err)
			return nil, syscall.EIO
		}
	}
	if resp.Errno != 0 {
		return nil, syscall.Errno(resp.Errno)
	}
	return ProtoToEntries(resp.Entries), 0
}

// Insert adds an entry to the directory handler
func (h *grpcDirHandler) Insert(inode Ino, name string, attr *Attr) {
	req := &pb.DirHandlerInsertRequest{
		Handle: h.handle,
		Inode:  uint64(inode),
		Name:   name,
		Attr:   AttrToProto(attr),
	}
	resp, err := h.client.DirHandlerInsert(h.authCtx(context.Background()), req)
	if err != nil {
		logger.Errorf("DirHandlerInsert error: %v", err)
		return
	}
	if resp.Errno != 0 {
		logger.Errorf("DirHandlerInsert errno: %d", resp.Errno)
	}
}

// Delete removes an entry from the directory handler
func (h *grpcDirHandler) Delete(name string) {
	req := &pb.DirHandlerDeleteRequest{
		Handle: h.handle,
		Name:   name,
	}
	resp, err := h.client.DirHandlerDelete(h.authCtx(context.Background()), req)
	if err != nil {
		logger.Errorf("DirHandlerDelete error: %v", err)
		return
	}
	if resp.Errno != 0 {
		logger.Errorf("DirHandlerDelete errno: %d", resp.Errno)
	}
}

// Read is not implemented for gRPC DirHandler
func (h *grpcDirHandler) Read(offset int) {
	// Not supported by gRPC client
}

// Close closes the directory handler
func (h *grpcDirHandler) Close() {
	req := &pb.DirHandlerCloseRequest{
		Handle: h.handle,
	}
	resp, err := h.client.DirHandlerClose(h.authCtx(context.Background()), req)
	if err != nil {
		logger.Errorf("DirHandlerClose error: %v", err)
		return
	}
	if resp.Errno != 0 {
		logger.Errorf("DirHandlerClose errno: %d", resp.Errno)
	}
}
