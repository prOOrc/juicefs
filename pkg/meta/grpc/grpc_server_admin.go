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
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
)

func (s *MetaProxyServer) GetFormat(ctx context.Context, req *GetFormatRequest) (*GetFormatResponse, error) {
	format := s.meta.GetFormat()
	return &GetFormatResponse{
		Errno:  0,
		Format: FormatToProto(format),
	}, nil
}

func (s *MetaProxyServer) Remove(ctx context.Context, req *RemoveRequest) (*RemoveResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var count uint64
	errno := s.meta.Remove(mctx, meta.Ino(req.Parent), req.Name, req.SkipTrash, int(req.NumThreads), &count)
	return &RemoveResponse{
		Errno: uint32(errno),
		Count: count,
	}, nil
}

func (s *MetaProxyServer) BatchUnlink(ctx context.Context, req *BatchUnlinkRequest) (*BatchUnlinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	entries := ProtoToEntries(req.Entries)
	var count uint64
	errno := s.meta.BatchUnlink(mctx, meta.Ino(req.Parent), entries, &count, req.SkipCheckTrash)
	return &BatchUnlinkResponse{
		Errno: uint32(errno),
		Count: count,
	}, nil
}

func (s *MetaProxyServer) GetSummary(ctx context.Context, req *GetSummaryRequest) (*GetSummaryResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var summary meta.Summary
	errno := s.meta.GetSummary(mctx, meta.Ino(req.Inode), &summary, req.Recursive, req.Strict)
	return &GetSummaryResponse{
		Errno:   uint32(errno),
		Summary: SummaryToProto(&summary),
	}, nil
}

func (s *MetaProxyServer) GetTreeSummary(ctx context.Context, req *GetTreeSummaryRequest) (*GetTreeSummaryResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var tree meta.TreeSummary
	errno := s.meta.GetTreeSummary(mctx, &tree, uint8(req.Depth), uint8(req.TopN), req.Strict, func(uint64, uint64) {})
	return &GetTreeSummaryResponse{
		Errno: uint32(errno),
		Tree:  TreeSummaryToProto(&tree),
	}, nil
}

func (s *MetaProxyServer) Clone(ctx context.Context, req *CloneRequest) (*CloneResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var count, total uint64
	errno := s.meta.Clone(mctx, meta.Ino(req.SrcParentIno), meta.Ino(req.SrcIno),
		meta.Ino(req.DstParentIno), req.DstName, uint8(req.Cmode), uint16(req.Cumask),
		uint8(req.Concurrency), &count, &total)
	return &CloneResponse{
		Errno: uint32(errno),
		Count: count,
		Total: total,
	}, nil
}

func (s *MetaProxyServer) GetPaths(ctx context.Context, req *GetPathsRequest) (*GetPathsResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	paths := s.meta.GetPaths(mctx, meta.Ino(req.Inode))
	return &GetPathsResponse{
		Errno: 0,
		Paths: paths,
	}, nil
}

func (s *MetaProxyServer) Check(ctx context.Context, req *CheckRequest) (*CheckResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	opt := &meta.CheckOpt{
		Repair:        req.Repair,
		Recursive:     req.Recursive,
		SyncDirStat:   req.SyncDirStat,
		RepairDirMode: uint16(req.RepairDirMode),
	}
	err := s.meta.Check(mctx, req.Fpath, opt)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &CheckResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) CompactAll(ctx context.Context, req *CompactAllRequest) (*CompactAllResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.CompactAll(mctx, int(req.Threads), nil)
	return &CompactAllResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Compact(ctx context.Context, req *CompactRequest) (*CompactResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Compact(mctx, meta.Ino(req.Inode), int(req.Concurrency), nil, nil)
	return &CompactResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ListSlices(ctx context.Context, req *ListSlicesRequest) (*ListSlicesResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	slices := ProtoToSliceMap(req.Slices)
	errno := s.meta.ListSlices(mctx, slices, req.ScanPending, req.Delete, func() {})
	return &ListSlicesResponse{
		Errno:  uint32(errno),
		Slices: SliceMapToProto(slices),
	}, nil
}

func (s *MetaProxyServer) HandleQuota(ctx context.Context, req *HandleQuotaRequest) (*HandleQuotaResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	quotas := make(map[string]*meta.Quota)
	for k, v := range req.Quotas {
		quotas[k] = ProtoToQuota(v)
	}
	err := s.meta.HandleQuota(mctx, uint8(req.Cmd), req.Dpath, req.Uid, req.Gid, quotas,
		req.Strict, req.Repair, req.Create)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &HandleQuotaResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ScanUserGroupUsage(ctx context.Context, req *ScanUserGroupUsageRequest) (*ScanUserGroupUsageResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	err := s.meta.ScanUserGroupUsage(mctx)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &ScanUserGroupUsageResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Chroot(ctx context.Context, req *ChrootRequest) (*ChrootResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Chroot(mctx, req.Subdir)
	return &ChrootResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) CleanupTrashBefore(ctx context.Context, req *CleanupTrashBeforeRequest) (*CleanupTrashBeforeResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var stats meta.CleanupTrashStats
	errno := s.meta.CleanupTrashBefore(mctx, time.Unix(req.Edge, 0), func(int) {}, &stats)
	return &CleanupTrashBeforeResponse{
		Errno:        uint32(errno),
		DeletedFiles: stats.DeletedFiles,
	}, nil
}

func (s *MetaProxyServer) CleanupDetachedNodesBefore(ctx context.Context, req *CleanupDetachedNodesBeforeRequest) (*CleanupDetachedNodesBeforeResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	s.meta.CleanupDetachedNodesBefore(mctx, time.Unix(req.Edge, 0), func() {})
	return &CleanupDetachedNodesBeforeResponse{Errno: 0}, nil
}
