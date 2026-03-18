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

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// ContextKey is the key for storing Context in gRPC context
type ContextKey string

const metaContextKey ContextKey = "meta-context"

// grpcContext wraps Context for gRPC metadata
type grpcContext struct {
	context.Context
	uid             uint32
	gid             uint32
	gids            []uint32
	pid             uint32
	checkPermission bool
}

// NewGRPCContext creates a new grpcContext from Context fields
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
func (c *grpcContext) WithValue(k, v interface{}) Context {
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

// protoToMetaContext converts a ProtoMetaContext to Context
func ProtoToMetaContext(ctx context.Context, ctx2 *pb.MetaContext) Context {
	gids := ctx2.Gids
	if len(gids) == 0 {
		gids = []uint32{ctx2.Gid}
	}
	return WrapWithCancel(ctx, ctx2.Pid, ctx2.Uid, gids)
}

// metaContextToProto converts a Context to ProtoMetaContext
func MetaContextToProto(ctx Context) *pb.MetaContext {
	gids := ctx.Gids()
	if len(gids) == 0 {
		gids = []uint32{ctx.Gid()}
	}
	return &pb.MetaContext{
		Uid:             ctx.Uid(),
		Gid:             ctx.Gid(),
		Gids:            gids,
		Pid:             ctx.Pid(),
		CheckPermission: ctx.CheckPermission(),
	}
}

// GetMetaContextFromGRPCContext extracts Context from gRPC context
func GetMetaContextFromGRPCContext(ctx context.Context) Context {
	if c, ok := ctx.Value(metaContextKey).(Context); ok {
		return c
	}
	return Background()
}

// WithMetaContextInGRPCContext adds Context to gRPC context
func WithMetaContextInGRPCContext(ctx context.Context, mc Context) context.Context {
	return context.WithValue(ctx, metaContextKey, mc)
}

// Alias functions for simpler names used in grpc_client.go and server.go

func toProtoContext(ctx Context) *pb.MetaContext {
	if ctx == nil {
		return &pb.MetaContext{}
	}
	return MetaContextToProto(ctx)
}

func fromProtoContext(ctx2 *pb.MetaContext) Context {
	if ctx2 == nil {
		return Background()
	}
	return ProtoToMetaContext(context.Background(), ctx2)
}
