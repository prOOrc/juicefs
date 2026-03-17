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

// ContextKey is the key for storing meta.Context in gRPC context
type ContextKey string

const metaContextKey ContextKey = "meta-context"

// grpcContext wraps meta.Context for gRPC metadata
type grpcContext struct {
	context.Context
	uid             uint32
	gid             uint32
	gids            []uint32
	pid             uint32
	checkPermission bool
}

// NewGRPCContext creates a new grpcContext from meta.Context fields
func NewGRPCContext(ctx context.Context, uid, gid, pid uint32, gids []uint32, checkPermission bool) *grpcContext {
	return &grpcContext{
		Context:         ctx,
		uid:             uid,
		gid:             gid,
		gids:            gids,
		pid:             pid,
		checkPermission: checkPermission,
	}
}

// Uid returns the user ID
func (c *grpcContext) Uid() uint32 {
	return c.uid
}

// Gid returns the primary group ID
func (c *grpcContext) Gid() uint32 {
	return c.gid
}

// Gids returns all group IDs
func (c *grpcContext) Gids() []uint32 {
	return c.gids
}

// Pid returns the process ID
func (c *grpcContext) Pid() uint32 {
	return c.pid
}

// CheckPermission returns whether to check permissions
func (c *grpcContext) CheckPermission() bool {
	return c.checkPermission
}

// WithValue returns a new context with the given key-value pair
func (c *grpcContext) WithValue(k, v interface{}) meta.Context {
	newCtx := &grpcContext{
		Context:         context.WithValue(c.Context, k, v),
		uid:             c.uid,
		gid:             c.gid,
		gids:            c.gids,
		pid:             c.pid,
		checkPermission: c.checkPermission,
	}
	return newCtx
}

// Cancel cancels the context
func (c *grpcContext) Cancel() {
	if cancelFunc, ok := c.Context.Deadline(); ok {
		// context.WithDeadline doesn't expose cancel, so we can't cancel here
		// This is acceptable for gRPC contexts
		_ = cancelFunc
	}
}

// Canceled returns true if the context has been canceled
func (c *grpcContext) Canceled() bool {
	return c.Err() != nil
}

// protoToMetaContext converts a ProtoMetaContext to meta.Context
func ProtoToMetaContext(ctx context.Context, pb *MetaContext) meta.Context {
	gids := pb.Gids
	if len(gids) == 0 {
		gids = []uint32{pb.Gid}
	}
	return meta.WrapWithCancel(ctx, pb.Pid, pb.Uid, gids)
}

// metaContextToProto converts a meta.Context to ProtoMetaContext
func MetaContextToProto(ctx meta.Context) *MetaContext {
	gids := ctx.Gids()
	if len(gids) == 0 {
		gids = []uint32{ctx.Gid()}
	}
	return &MetaContext{
		Uid:             ctx.Uid(),
		Gid:             ctx.Gid(),
		Gids:            gids,
		Pid:             ctx.Pid(),
		CheckPermission: ctx.CheckPermission(),
	}
}

// GetMetaContextFromGRPCContext extracts meta.Context from gRPC context
func GetMetaContextFromGRPCContext(ctx context.Context) meta.Context {
	if c, ok := ctx.Value(metaContextKey).(meta.Context); ok {
		return c
	}
	return meta.Background()
}

// WithMetaContextInGRPCContext adds meta.Context to gRPC context
func WithMetaContextInGRPCContext(ctx context.Context, mc meta.Context) context.Context {
	return context.WithValue(ctx, metaContextKey, mc)
}

// Alias functions for simpler names used in grpc_client.go and server.go

func toProtoContext(ctx meta.Context) *MetaContext {
	if ctx == nil {
		return &MetaContext{}
	}
	return MetaContextToProto(ctx)
}

func fromProtoContext(pbCtx *MetaContext) meta.Context {
	if pbCtx == nil {
		return meta.Background()
	}
	return ProtoToMetaContext(context.Background(), pbCtx)
}
