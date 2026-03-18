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
	"time"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/utils"
)

// BatchUnlink removes multiple entries
func (c *GRPCClient) BatchUnlink(ctx Context, parent Ino, entries []*Entry, count *uint64, skipCheckTrash bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	protoEntries := make([]*pb.ProtoEntry, 0, len(entries))
	for _, e := range entries {
		protoEntries = append(protoEntries, toProtoEntry(e))
	}

	resp, err := c.client.BatchUnlink(grpcCtx, &pb.BatchUnlinkRequest{
		Ctx:            toProtoContext(ctx),
		Parent:         uint64(parent),
		Entries:        protoEntries,
		SkipCheckTrash: skipCheckTrash,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if count != nil {
		*count = resp.GetCount()
	}
	return 0
}

// Remove removes a directory recursively
func (c *GRPCClient) Remove(ctx Context, parent Ino, name string, skipTrash bool, numThreads int, count *uint64) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Remove(grpcCtx, &pb.RemoveRequest{
		Ctx:        toProtoContext(ctx),
		Parent:     uint64(parent),
		Name:       name,
		SkipTrash:  skipTrash,
		NumThreads: int32(numThreads),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if count != nil {
		*count = resp.GetCount()
	}
	return 0
}

// GetSummary gets directory summary
func (c *GRPCClient) GetSummary(ctx Context, ino Ino, summary *Summary, recursive, strict bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetSummary(grpcCtx, &pb.GetSummaryRequest{
		Ctx:       toProtoContext(ctx),
		Inode:     uint64(ino),
		Recursive: recursive,
		Strict:    strict,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if summary != nil {
		*summary = *fromProtoSummary(resp.GetSummary())
	}
	return 0
}

// GetTreeSummary gets tree summary
func (c *GRPCClient) GetTreeSummary(ctx Context, root *TreeSummary, depth, topN uint8, strict bool, updateProgress func(count uint64, bytes uint64)) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetTreeSummary(grpcCtx, &pb.GetTreeSummaryRequest{
		Ctx:    toProtoContext(ctx),
		Inode:  0,
		Depth:  uint32(depth),
		TopN:   uint32(topN),
		Strict: strict,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if root != nil {
		*root = *fromProtoTreeSummary(resp.GetTree())
	}
	return 0
}

// Clone clones a file
func (c *GRPCClient) Clone(ctx Context, srcParentIno, srcIno, dstParentIno Ino, dstName string, cmode uint8, cumask uint16, concurrency uint8, count, total *uint64) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Clone(grpcCtx, &pb.CloneRequest{
		Ctx:          toProtoContext(ctx),
		SrcParentIno: uint64(srcParentIno),
		SrcIno:       uint64(srcIno),
		DstParentIno: uint64(dstParentIno),
		DstName:      dstName,
		Cmode:        uint32(cmode),
		Cumask:       uint32(cumask),
		Concurrency:  uint32(concurrency),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if count != nil {
		*count = resp.GetCount()
	}
	if total != nil {
		*total = resp.GetTotal()
	}
	return 0
}

// GetPaths gets paths for an inode
func (c *GRPCClient) GetPaths(ctx Context, ino Ino) []string {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetPaths(grpcCtx, &pb.GetPathsRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
	})
	if err != nil || resp == nil {
		return nil
	}
	if resp.GetErrno() != 0 {
		return nil
	}
	return resp.GetPaths()
}

// Check checks filesystem consistency
func (c *GRPCClient) Check(ctx Context, fpath string, opt *CheckOpt) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	repair := false
	recursive := false
	syncDirStat := false
	repairDirMode := uint32(0)
	if opt != nil {
		repair = opt.Repair
		recursive = opt.Recursive
		syncDirStat = opt.SyncDirStat
		repairDirMode = uint32(opt.RepairDirMode)
	}

	resp, err := c.client.Check(grpcCtx, &pb.CheckRequest{
		Ctx:           toProtoContext(ctx),
		Fpath:         fpath,
		Repair:        repair,
		Recursive:     recursive,
		SyncDirStat:   syncDirStat,
		RepairDirMode: repairDirMode,
	})
	if err != nil {
		return err
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	return nil
}

// CompactAll compacts all files
func (c *GRPCClient) CompactAll(ctx Context, threads int, bar *utils.Bar) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CompactAll(grpcCtx, &pb.CompactAllRequest{
		Ctx:     toProtoContext(ctx),
		Threads: int32(threads),
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// Compact compacts a file
func (c *GRPCClient) Compact(ctx Context, ino Ino, concurrency int, preFunc, postFunc func()) syscall.Errno {
	if preFunc != nil {
		preFunc()
	}
	defer func() {
		if postFunc != nil {
			postFunc()
		}
	}()

	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Compact(grpcCtx, &pb.CompactRequest{
		Ctx:         toProtoContext(ctx),
		Inode:       uint64(ino),
		Concurrency: int32(concurrency),
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// ListSlices lists slices
func (c *GRPCClient) ListSlices(ctx Context, slices map[Ino][]Slice, scanPending, delete bool, showProgress func()) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	protoEntries := make([]*pb.SliceMapEntry, 0, len(slices))
	for ino, sls := range slices {
		protoSlices := make([]*pb.ProtoSlice, 0, len(sls))
		for _, s := range sls {
			protoSlices = append(protoSlices, &pb.ProtoSlice{
				Id:   s.Id,
				Size: s.Size,
				Off:  s.Off,
				Len:  s.Len,
			})
		}
		protoEntries = append(protoEntries, &pb.SliceMapEntry{
			Inode:  uint64(ino),
			Slices: protoSlices,
		})
	}

	resp, err := c.client.ListSlices(grpcCtx, &pb.ListSlicesRequest{
		Ctx:         toProtoContext(ctx),
		Slices:      protoEntries,
		ScanPending: scanPending,
		Delete:      delete,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}

	if showProgress != nil {
		showProgress()
	}
	return 0
}

// HandleQuota handles quota
func (c *GRPCClient) HandleQuota(ctx Context, cmd uint8, dpath string, uid, gid uint32, quotas map[string]*Quota, strict, repair, create bool) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	protoQuotas := make(map[string]*pb.ProtoQuota, len(quotas))
	for k, v := range quotas {
		protoQuotas[k] = toProtoQuota(v)
	}

	resp, err := c.client.HandleQuota(grpcCtx, &pb.HandleQuotaRequest{
		Ctx:    toProtoContext(ctx),
		Cmd:    uint32(cmd),
		Dpath:  dpath,
		Uid:    uid,
		Gid:    gid,
		Quotas: protoQuotas,
		Strict: strict,
		Repair: repair,
		Create: create,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// ScanUserGroupUsage scans user/group usage
func (c *GRPCClient) ScanUserGroupUsage(ctx Context) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ScanUserGroupUsage(grpcCtx, &pb.ScanUserGroupUsageRequest{
		Ctx: toProtoContext(ctx),
	})
	if err != nil {
		return err
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	return nil
}

// Chroot changes the root directory
func (c *GRPCClient) Chroot(ctx Context, subdir string) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Chroot(grpcCtx, &pb.ChrootRequest{
		Ctx:    toProtoContext(ctx),
		Subdir: subdir,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// CleanupTrashBefore cleans up trash before a timestamp
func (c *GRPCClient) CleanupTrashBefore(ctx Context, edge time.Time, increProgress func(int), stats *CleanupTrashStats) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CleanupTrashBefore(grpcCtx, &pb.CleanupTrashBeforeRequest{
		Ctx:  toProtoContext(ctx),
		Edge: edge.Unix(),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if stats != nil {
		stats.DeletedFiles = resp.GetDeletedFiles()
	}
	return 0
}

// CleanupDetachedNodesBefore cleans up detached nodes before a timestamp
func (c *GRPCClient) CleanupDetachedNodesBefore(ctx Context, edge time.Time, increProgress func()) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CleanupDetachedNodesBefore(grpcCtx, &pb.CleanupDetachedNodesBeforeRequest{
		Ctx:  toProtoContext(ctx),
		Edge: edge.Unix(),
	})
	if err == nil && resp != nil {
		_ = resp.GetErrno()
	}
}

// ScanDeletedObject scans deleted objects
func (c *GRPCClient) ScanDeletedObject(ctx Context, tss trashSliceScan, pss pendingSliceScan, tfs trashFileScan, pfs pendingFileScan) error {
	// Not implemented - returns ENOSYS
	return syscall.ENOSYS
}
