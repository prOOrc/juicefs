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
	"syscall"
	"time"

	aclAPI "github.com/juicedata/juicefs/pkg/acl"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
)

// --- Data path operations for grpcMeta ---

// Read returns the list of slices on the given chunk
func (m *grpcMeta) Read(ctx Context, inode Ino, indx uint32, slices *[]Slice) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.ReadRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Indx:  indx,
	}
	resp, err := m.client.Read(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if slices != nil {
		*slices = ProtoToSlices(resp.Slices)
	}
	return 0
}

// NewSlice returns an id for new slice
func (m *grpcMeta) NewSlice(ctx Context, id *uint64) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.NewSliceRequest{
		Ctx: c,
	}
	resp, err := m.client.NewSlice(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if id != nil {
		*id = resp.GetId()
	}
	return 0
}

// Write put a slice of data on top of the given chunk
func (m *grpcMeta) Write(ctx Context, inode Ino, indx uint32, off uint32, slice Slice, mtime time.Time) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.WriteRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Indx:  indx,
		Off:   off,
		Slice: SliceToProto(slice),
		Mtime: mtime.UnixNano() / 1000,
	}
	resp, err := m.client.Write(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	m.invalidateAttrCache(uint64(inode))
	return 0
}

// InvalidateChunkCache invalidates chunk cache
func (m *grpcMeta) InvalidateChunkCache(ctx Context, inode Ino, indx uint32) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.InvalidateChunkCacheRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Indx:  indx,
	}
	resp, err := m.client.InvalidateChunkCache(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	return 0
}

// CopyFileRange copies part of a file to another one
func (m *grpcMeta) CopyFileRange(ctx Context, fin Ino, offIn uint64, fout Ino, offOut uint64, size uint64, flags uint32, copied, outLength *uint64) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.CopyFileRangeRequest{
		Ctx:    c,
		Fin:    uint64(fin),
		OffIn:  offIn,
		Fout:   uint64(fout),
		OffOut: offOut,
		Size:   size,
		Flags:  flags,
	}
	resp, err := m.client.CopyFileRange(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if copied != nil {
		*copied = resp.GetCopied()
	}
	if outLength != nil {
		*outLength = resp.GetOutLength()
	}
	m.invalidateAttrCache(uint64(fin))
	m.invalidateAttrCache(uint64(fout))
	return 0
}

// GetParents returns a map of node parents
func (m *grpcMeta) GetParents(ctx Context, inode Ino) map[Ino]int {
	c := m.grpcContext(ctx)
	req := &pb.GetParentsRequest{
		Ctx:   c,
		Inode: uint64(inode),
	}
	resp, err := m.client.GetParents(m.withSessionID(ctx), req)
	if err != nil {
		return nil
	}
	if resp.GetErrno() != 0 {
		return nil
	}
	parents := make(map[Ino]int)
	for ino, count := range resp.Parents {
		parents[Ino(ino)] = int(count)
	}
	return parents
}

// GetDirStat returns the space and inodes usage of a directory
func (m *grpcMeta) GetDirStat(ctx Context, inode Ino) (stat *dirStat, st syscall.Errno) {
	c := m.grpcContext(ctx)
	req := &pb.GetDirStatRequest{
		Ctx:   c,
		Inode: uint64(inode),
	}
	resp, err := m.client.GetDirStat(m.withSessionID(ctx), req)
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	return &dirStat{
		length: resp.Length,
		space:  resp.Space,
		inodes: resp.Inodes,
	}, 0
}

// Clone clones a file or directory
func (m *grpcMeta) Clone(ctx Context, srcParentIno, srcIno, dstParentIno Ino, dstName string, cmode uint8, cumask uint16, concurrency uint8, count, total *uint64) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.CloneRequest{
		Ctx:          c,
		SrcParentIno: uint64(srcParentIno),
		SrcIno:       uint64(srcIno),
		DstParentIno: uint64(dstParentIno),
		DstName:      dstName,
		Cmode:        uint32(cmode),
		Cumask:       uint32(cumask),
		Concurrency:  uint32(concurrency),
	}
	resp, err := m.client.Clone(m.withSessionID(ctx), req)
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

// Compact compacts chunks for specified inode
func (m *grpcMeta) Compact(ctx Context, inode Ino, concurrency int, preFunc, postFunc func()) syscall.Errno {
	if preFunc != nil {
		preFunc()
	}
	if postFunc != nil {
		defer postFunc()
	}
	c := m.grpcContext(ctx)
	req := &pb.CompactRequest{
		Ctx:         c,
		Inode:       uint64(inode),
		Concurrency: int32(concurrency),
	}
	resp, err := m.client.Compact(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// CompactAll compacts all chunks
func (m *grpcMeta) CompactAll(ctx Context, threads int, bar *utils.Bar) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.CompactAllRequest{
		Ctx:     c,
		Threads: int32(threads),
	}
	resp, err := m.client.CompactAll(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// DeleteTokens deletes tokens by IDs
func (m *grpcMeta) DeleteTokens(ctx Context, ids []uint32) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.DeleteTokensRequest{
		Ctx: c,
		Ids: ids,
	}
	resp, err := m.client.DeleteTokens(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// DumpMeta dumps metadata to writer (not supported by gRPC client)
func (m *grpcMeta) DumpMeta(w io.Writer, root Ino, threads int, keepSecret, fast, skipTrash bool) error {
	return syscall.ENOSYS
}

// DumpMetaV2 dumps metadata to writer v2 (not supported by gRPC client)
func (m *grpcMeta) DumpMetaV2(ctx Context, w io.Writer, opt *DumpOption) error {
	return syscall.ENOSYS
}

// LoadMeta loads metadata from reader (not supported by gRPC client)
func (m *grpcMeta) LoadMeta(r io.Reader) error {
	return syscall.ENOSYS
}

// Flock tries to put a lock on given file
func (m *grpcMeta) Flock(ctx Context, inode Ino, owner uint64, ltype uint32, block bool) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.FlockRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Owner: owner,
		Ltype: ltype,
		Block: block,
	}
	resp, err := m.client.Flock(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// SetFacl sets ACL on inode
func (m *grpcMeta) SetFacl(ctx Context, ino Ino, aclType uint8, n *aclAPI.Rule) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.SetFaclRequest{
		Ctx:     c,
		Ino:     uint64(ino),
		AclType: uint32(aclType),
		Rule:    toProtoACLRule(n),
	}
	resp, err := m.client.SetFacl(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// GetFacl gets ACL from inode
func (m *grpcMeta) GetFacl(ctx Context, ino Ino, aclType uint8, n *aclAPI.Rule) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.GetFaclRequest{
		Ctx:     c,
		Ino:     uint64(ino),
		AclType: uint32(aclType),
	}
	resp, err := m.client.GetFacl(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if n != nil && resp.Rule != nil {
		*n = *fromProtoACLRule(resp.Rule)
	}
	return 0
}

// StoreToken stores a token
func (m *grpcMeta) StoreToken(ctx Context, token []byte) (id uint32, st syscall.Errno) {
	c := m.grpcContext(ctx)
	req := &pb.StoreTokenRequest{
		Ctx:   c,
		Token: token,
	}
	resp, err := m.client.StoreToken(m.withSessionID(ctx), req)
	if err != nil {
		return 0, syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return 0, syscall.Errno(resp.GetErrno())
	}
	return resp.Id, 0
}

// UpdateToken updates a token
func (m *grpcMeta) UpdateToken(ctx Context, id uint32, token []byte) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.UpdateTokenRequest{
		Ctx:   c,
		Id:    id,
		Token: token,
	}
	resp, err := m.client.UpdateToken(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// LoadToken loads a token
func (m *grpcMeta) LoadToken(ctx Context, id uint32) (token []byte, st syscall.Errno) {
	c := m.grpcContext(ctx)
	req := &pb.LoadTokenRequest{
		Ctx: c,
		Id:  id,
	}
	resp, err := m.client.LoadToken(m.withSessionID(ctx), req)
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	return resp.Token, 0
}

// ListTokens lists all tokens
func (m *grpcMeta) ListTokens(ctx Context) (tokens map[uint32][]byte, st syscall.Errno) {
	c := m.grpcContext(ctx)
	req := &pb.ListTokensRequest{
		Ctx: c,
	}
	resp, err := m.client.ListTokens(m.withSessionID(ctx), req)
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	return resp.Tokens, 0
}

// GetFormat returns a copy of the current format
func (m *grpcMeta) GetFormat() Format {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.format == nil {
		return Format{}
	}
	f := *m.format
	return f
}

// GetSummary gets directory summary
func (m *grpcMeta) GetSummary(ctx Context, inode Ino, summary *Summary, recursive, strict bool) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.GetSummaryRequest{
		Ctx:       c,
		Inode:     uint64(inode),
		Recursive: recursive,
		Strict:    strict,
	}
	resp, err := m.client.GetSummary(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if summary != nil && resp.Summary != nil {
		*summary = *fromProtoSummary(resp.Summary)
	}
	return 0
}

// GetTreeSummary gets tree summary
func (m *grpcMeta) GetTreeSummary(ctx Context, root *TreeSummary, depth, topN uint8, strict bool, updateProgress func(count uint64, bytes uint64)) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.GetTreeSummaryRequest{
		Ctx:    c,
		Depth:  uint32(depth),
		TopN:   uint32(topN),
		Strict: strict,
	}
	resp, err := m.client.GetTreeSummary(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if root != nil && resp.Tree != nil {
		*root = *fromProtoTreeSummary(resp.Tree)
	}
	return 0
}

// Remove removes a directory recursively
func (m *grpcMeta) Remove(ctx Context, parent Ino, name string, skipTrash bool, numThreads int, count *uint64) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.RemoveRequest{
		Ctx:        c,
		Parent:     uint64(parent),
		Name:       name,
		SkipTrash:  skipTrash,
		NumThreads: int32(numThreads),
	}
	resp, err := m.client.Remove(m.withSessionID(ctx), req)
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

// BatchUnlink removes multiple entries
func (m *grpcMeta) BatchUnlink(ctx Context, parent Ino, entries []*Entry, count *uint64, skipCheckTrash bool) syscall.Errno {
	c := m.grpcContext(ctx)
	protoEntries := make([]*pb.ProtoEntry, len(entries))
	for i, e := range entries {
		protoEntries[i] = toProtoEntry(e)
	}
	req := &pb.BatchUnlinkRequest{
		Ctx:            c,
		Parent:         uint64(parent),
		Entries:        protoEntries,
		SkipCheckTrash: skipCheckTrash,
	}
	resp, err := m.client.BatchUnlink(m.withSessionID(ctx), req)
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

// GetXattr returns the value of extended attribute
func (m *grpcMeta) GetXattr(ctx Context, inode Ino, name string, vbuff *[]byte) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.GetXattrRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Name:  name,
	}
	resp, err := m.client.GetXattr(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if vbuff != nil {
		*vbuff = resp.Value
	}
	return 0
}

// SetXattr updates the extended attribute
func (m *grpcMeta) SetXattr(ctx Context, inode Ino, name string, value []byte, flags uint32) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.SetXattrRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Name:  name,
		Value: value,
		Flags: flags,
	}
	resp, err := m.client.SetXattr(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// RemoveXattr removes the extended attribute
func (m *grpcMeta) RemoveXattr(ctx Context, inode Ino, name string) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.RemoveXattrRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Name:  name,
	}
	resp, err := m.client.RemoveXattr(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// ListXattr returns all extended attributes
func (m *grpcMeta) ListXattr(ctx Context, inode Ino, dbuff *[]byte) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.ListXattrRequest{
		Ctx:   c,
		Inode: uint64(inode),
	}
	resp, err := m.client.ListXattr(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if dbuff != nil {
		*dbuff = resp.Names
	}
	return 0
}

// Getlk returns the current lock owner for a range on a file
func (m *grpcMeta) Getlk(ctx Context, inode Ino, owner uint64, ltype *uint32, start, end *uint64, pid *uint32) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.GetlkRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Owner: owner,
		Ltype: 0,
		Start: 0,
		End:   0,
	}
	if ltype != nil {
		req.Ltype = *ltype
	}
	if start != nil {
		req.Start = *start
	}
	if end != nil {
		req.End = *end
	}
	resp, err := m.client.Getlk(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if ltype != nil {
		*ltype = resp.Ltype
	}
	if pid != nil && resp.Pid != 0 {
		*pid = resp.Pid
	}
	return 0
}

// Setlk sets a file range lock on given file
func (m *grpcMeta) Setlk(ctx Context, inode Ino, owner uint64, block bool, ltype uint32, start, end uint64, pid uint32) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.SetlkRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Owner: owner,
		Block: block,
		Ltype: ltype,
		Start: start,
		End:   end,
		Pid:   pid,
	}
	resp, err := m.client.Setlk(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// Unlk unlocks a file range lock (not supported by gRPC client)
func (m *grpcMeta) Unlk(ctx Context, inode Ino, owner uint64, ltype uint32, start, end uint64, pid uint32) syscall.Errno {
	return syscall.ENOSYS
}

// HandleQuota handles quota operations
func (m *grpcMeta) HandleQuota(ctx Context, cmd uint8, qkey string, qtype uint32, quotas map[string]*Quota, strict, repair, create bool) error {
	c := m.grpcContext(ctx)
	protoQuotas := make(map[string]*pb.ProtoQuota, len(quotas))
	for k, v := range quotas {
		protoQuotas[k] = toProtoQuota(v)
	}
	req := &pb.HandleQuotaRequest{
		Ctx:    c,
		Cmd:    uint32(cmd),
		Qkey:   qkey,
		Qtype:  uint32(qtype),
		Quotas: protoQuotas,
		Strict: strict,
		Repair: repair,
		Create: create,
	}
	resp, err := m.client.HandleQuota(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// ScanUserGroupUsage scans user/group usage
func (m *grpcMeta) ScanUserGroupUsage(ctx Context) error {
	c := m.grpcContext(ctx)
	req := &pb.ScanUserGroupUsageRequest{
		Ctx: c,
	}
	resp, err := m.client.ScanUserGroupUsage(m.withSessionID(ctx), req)
	if err != nil {
		return err
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	return nil
}

// InitMetrics initializes metrics (not supported by gRPC client)
func (m *grpcMeta) InitMetrics(registerer prometheus.Registerer) {
	// Metrics are handled by the server
}

// InitSharedMetrics initializes shared metrics (not supported by gRPC client)
func (m *grpcMeta) InitSharedMetrics(registerer prometheus.Registerer) {
	// Metrics are handled by the server
}

// ListSlices lists slices
func (m *grpcMeta) ListSlices(ctx Context, slices map[Ino][]Slice, scanPending, delete bool, showProgress func()) syscall.Errno {
	c := m.grpcContext(ctx)
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
	req := &pb.ListSlicesRequest{
		Ctx:         c,
		Slices:      protoEntries,
		ScanPending: scanPending,
		Delete:      delete,
	}
	resp, err := m.client.ListSlices(m.withSessionID(ctx), req)
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

// LoadMetaV2 loads metadata v2 (not supported by gRPC client)
func (m *grpcMeta) LoadMetaV2(ctx Context, r io.Reader, opt *LoadOption) error {
	return syscall.ENOSYS
}

// NewDirHandler creates a directory handler for streaming directory entries
func (m *grpcMeta) NewDirHandler(ctx Context, inode Ino, plus bool, initEntries []*Entry) (DirHandler, syscall.Errno) {
	initProto := EntriesToProto(initEntries)
	req := &pb.NewDirHandlerRequest{
		Ctx:         m.grpcContext(ctx),
		Inode:       uint64(inode),
		Plus:        plus,
		InitEntries: initProto,
	}
	resp, err := m.client.NewDirHandler(m.withSessionID(ctx), req)
	if err != nil {
		logger.Errorf("NewDirHandler error: %v", err)
		return nil, syscall.EIO
	}
	if resp.Errno != 0 {
		return nil, syscall.Errno(resp.Errno)
	}
	return &grpcDirHandler{
		client: m.client,
		handle: resp.Handle,
	}, 0
}

// OnMsg adds a callback for the given message type (not supported by gRPC client)
func (m *grpcMeta) OnMsg(mtype uint32, cb MsgCallback) {
	// Message handling is done on the server
}

// OnReload registers a callback for format changes (not supported by gRPC client)
func (m *grpcMeta) OnReload(cb func(new *Format)) {
	// Format changes are handled by the server
}

// Reset cleans up all metadata (not supported by gRPC client)
func (m *grpcMeta) Reset() error {
	return syscall.ENOSYS
}

// chroot sets the root directory by inode (not supported by gRPC client)
func (m *grpcMeta) chroot(inode Ino) {
	// chroot is handled locally
}

// getBase returns the base engine (not used by grpcMeta)
func (m *grpcMeta) getBase() *baseMeta {
	return nil
}
