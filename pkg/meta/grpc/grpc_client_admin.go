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

// BatchUnlink removes multiple entries
func (c *Client) BatchUnlink(ctx meta.Context, parent meta.Ino, entries []*meta.Entry, count *uint64, skipCheckTrash bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	protoEntries := make([]*ProtoEntry, 0, len(entries))
	for _, e := range entries {
		protoEntries = append(protoEntries, toProtoEntry(e))
	}

	resp, err := c.client.BatchUnlink(grpcCtx, &BatchUnlinkRequest{
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
func (c *Client) Remove(ctx meta.Context, parent meta.Ino, name string, skipTrash bool, numThreads int32, count *uint64) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Remove(grpcCtx, &RemoveRequest{
		Ctx:        toProtoContext(ctx),
		Parent:     uint64(parent),
		Name:       name,
		SkipTrash:  skipTrash,
		NumThreads: numThreads,
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
func (c *Client) GetSummary(ctx meta.Context, ino meta.Ino, recursive, strict bool, summary *meta.Summary) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetSummary(grpcCtx, &GetSummaryRequest{
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
func (c *Client) GetTreeSummary(ctx meta.Context, ino meta.Ino, depth, topN uint32, strict bool, tree *meta.TreeSummary) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetTreeSummary(grpcCtx, &GetTreeSummaryRequest{
		Ctx:    toProtoContext(ctx),
		Inode:  uint64(ino),
		Depth:  depth,
		TopN:   topN,
		Strict: strict,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if tree != nil {
		*tree = *fromProtoTreeSummary(resp.GetTree())
	}
	return 0
}

// Clone clones a file
func (c *Client) Clone(ctx meta.Context, srcParentIno, srcIno, dstParentIno meta.Ino, dstName string, cmode, cumask, concurrency uint32, count, total *uint64) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Clone(grpcCtx, &CloneRequest{
		Ctx:          toProtoContext(ctx),
		SrcParentIno: uint64(srcParentIno),
		SrcIno:       uint64(srcIno),
		DstParentIno: uint64(dstParentIno),
		DstName:      dstName,
		Cmode:        cmode,
		Cumask:       cumask,
		Concurrency:  concurrency,
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
func (c *Client) GetPaths(ctx meta.Context, ino meta.Ino, paths *[]string) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetPaths(grpcCtx, &GetPathsRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if paths != nil {
		*paths = resp.GetPaths()
	}
	return 0
}

// Check checks filesystem consistency
func (c *Client) Check(ctx meta.Context, fpath string, repair, recursive, syncDirStat bool, repairDirMode uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Check(grpcCtx, &CheckRequest{
		Ctx:           toProtoContext(ctx),
		Fpath:         fpath,
		Repair:        repair,
		Recursive:     recursive,
		SyncDirStat:   syncDirStat,
		RepairDirMode: repairDirMode,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// CompactAll compacts all files
func (c *Client) CompactAll(ctx meta.Context, threads int32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CompactAll(grpcCtx, &CompactAllRequest{
		Ctx:     toProtoContext(ctx),
		Threads: threads,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// Compact compacts a file
func (c *Client) Compact(ctx meta.Context, ino meta.Ino, concurrency int32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Compact(grpcCtx, &CompactRequest{
		Ctx:         toProtoContext(ctx),
		Inode:       uint64(ino),
		Concurrency: concurrency,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// ListSlices lists slices
func (c *Client) ListSlices(ctx meta.Context, slices map[meta.Ino][]meta.Slice, scanPending, delete bool, result *map[meta.Ino][]meta.Slice) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	protoEntries := make([]*SliceMapEntry, 0, len(slices))
	for ino, sls := range slices {
		protoSlices := make([]*ProtoSlice, 0, len(sls))
		for _, s := range sls {
			protoSlices = append(protoSlices, &ProtoSlice{
				Id:   s.Id,
				Size: s.Size,
				Off:  s.Off,
				Len:  s.Len,
			})
		}
		protoEntries = append(protoEntries, &SliceMapEntry{
			Inode:  uint64(ino),
			Slices: protoSlices,
		})
	}

	resp, err := c.client.ListSlices(grpcCtx, &ListSlicesRequest{
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

	out := make(map[meta.Ino][]meta.Slice)
	for _, e := range resp.GetSlices() {
		sls := make([]meta.Slice, 0, len(e.GetSlices()))
		for _, s := range e.GetSlices() {
			sls = append(sls, meta.Slice{
				Id:   s.Id,
				Size: s.Size,
				Off:  s.Off,
				Len:  s.Len,
			})
		}
		out[meta.Ino(e.GetInode())] = sls
	}
	if result != nil {
		*result = out
	}
	return 0
}

// HandleQuota handles quota
func (c *Client) HandleQuota(ctx meta.Context, cmd uint32, dpath string, uid, gid uint32, quotas map[string]*meta.Quota, strict, repair, create bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	protoQuotas := make(map[string]*ProtoQuota, len(quotas))
	for k, v := range quotas {
		protoQuotas[k] = toProtoQuota(v)
	}

	resp, err := c.client.HandleQuota(grpcCtx, &HandleQuotaRequest{
		Ctx:    toProtoContext(ctx),
		Cmd:    cmd,
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
func (c *Client) ScanUserGroupUsage(ctx meta.Context) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ScanUserGroupUsage(grpcCtx, &ScanUserGroupUsageRequest{
		Ctx: toProtoContext(ctx),
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// Chroot changes the root directory
func (c *Client) Chroot(ctx meta.Context, subdir string) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Chroot(grpcCtx, &ChrootRequest{
		Ctx:    toProtoContext(ctx),
		Subdir: subdir,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// CleanupTrashBefore cleans up trash before a timestamp
func (c *Client) CleanupTrashBefore(ctx meta.Context, edge time.Time, increProgress func(int), stats *meta.CleanupTrashStats) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CleanupTrashBefore(grpcCtx, &CleanupTrashBeforeRequest{
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
func (c *Client) CleanupDetachedNodesBefore(ctx meta.Context, edge time.Time, increProgress func()) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CleanupDetachedNodesBefore(grpcCtx, &CleanupDetachedNodesBeforeRequest{
		Ctx:  toProtoContext(ctx),
		Edge: edge.Unix(),
	})
	if err == nil && resp != nil {
		_ = resp.GetErrno()
	}
}

// ScanDeletedObject scans deleted objects
func (c *Client) ScanDeletedObject(ctx meta.Context, tss interface{}, pss interface{}, tfs interface{}, pfs interface{}) error {
	// Not implemented - returns ENOSYS
	return syscall.ENOSYS
}
