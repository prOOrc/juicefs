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
	"sync"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// dirHandler implements meta.DirHandler interface
type dirHandler struct {
	handleID uint64
	client   pb.MetaServiceClient
	mu       sync.Mutex
}

// NewDirHandler creates a new directory handler
func (c *Client) NewDirHandler(ctx meta.Context, ino meta.Ino, plus bool, initEntries []*meta.Entry) (meta.DirHandler, syscall.Errno) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	protoEntries := make([]*pb.ProtoEntry, 0, len(initEntries))
	for _, e := range initEntries {
		protoEntries = append(protoEntries, toProtoEntry(e))
	}

	resp, err := c.client.NewDirHandler(grpcCtx, &pb.NewDirHandlerRequest{
		Ctx:         toProtoContext(ctx),
		Inode:       uint64(ino),
		Plus:        plus,
		InitEntries: protoEntries,
	})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}

	return &dirHandler{
		handleID: resp.GetHandle().GetHandleId(),
		client:   c.client,
	}, 0
}

// List lists directory entries
func (h *dirHandler) List(ctx meta.Context, offset int) ([]*meta.Entry, syscall.Errno) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h.mu.Lock()
	defer h.mu.Unlock()

	resp, err := h.client.DirHandlerList(grpcCtx, &pb.DirHandlerListRequest{
		Handle: &pb.DirHandlerHandle{HandleId: h.handleID},
		Offset: int32(offset),
	})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}

	entries := make([]*meta.Entry, 0, len(resp.GetEntries()))
	for _, e := range resp.GetEntries() {
		entries = append(entries, fromProtoEntry(e))
	}
	return entries, 0
}

// Insert inserts a directory entry
func (h *dirHandler) Insert(ino meta.Ino, name string, attr *meta.Attr) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := h.client.DirHandlerInsert(grpcCtx, &pb.DirHandlerInsertRequest{
		Handle: &pb.DirHandlerHandle{HandleId: h.handleID},
		Inode:  uint64(ino),
		Name:   name,
		Attr:   toProtoAttr(attr),
	})
	_ = err // Ignore errors for now
}

// Delete deletes a directory entry
func (h *dirHandler) Delete(name string) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := h.client.DirHandlerDelete(grpcCtx, &pb.DirHandlerDeleteRequest{
		Handle: &pb.DirHandlerHandle{HandleId: h.handleID},
		Name:   name,
	})
	_ = err // Ignore errors for now
}

// Close closes the directory handler
func (h *dirHandler) Close() {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := h.client.DirHandlerClose(grpcCtx, &pb.DirHandlerCloseRequest{
		Handle: &pb.DirHandlerHandle{HandleId: h.handleID},
	})
	_ = err // Ignore errors for now
}

// Read reads directory entries (not implemented)
func (h *dirHandler) Read(offset int) {
	// Not implemented
}
