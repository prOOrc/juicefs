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
	"syscall"

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// --- Core FUSE operations for grpcMeta ---

// StatFS returns summary statistics of a volume
func (m *grpcMeta) StatFS(ctx Context, ino Ino, totalspace, availspace, iused, iavail *uint64) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.StatFSRequest{
		Ctx: c,
		Ino: uint64(ino),
	}
	resp, err := m.client.StatFS(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if totalspace != nil {
		*totalspace = resp.GetTotalSpace()
	}
	if availspace != nil {
		*availspace = resp.GetAvailSpace()
	}
	if iused != nil {
		*iused = resp.GetIused()
	}
	if iavail != nil {
		*iavail = resp.GetIavail()
	}
	return 0
}

// Access checks the access permission on given inode
func (m *grpcMeta) Access(ctx Context, inode Ino, modemask uint8, attr *Attr) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.AccessRequest{
		Ctx:      c,
		Inode:    uint64(inode),
		Modemask: uint32(modemask),
	}
	resp, err := m.client.Access(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
	}
	return 0
}

// Lookup returns the inode and attributes for the given entry in a directory
func (m *grpcMeta) Lookup(ctx Context, parent Ino, name string, inode *Ino, attr *Attr, checkPerm bool) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.LookupRequest{
		Ctx:             c,
		Parent:          uint64(parent),
		Name:            name,
		CheckPermission: checkPerm,
	}
	resp, err := m.client.Lookup(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = Ino(resp.GetInode())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(resp.GetInode(), attr)
	}
	return 0
}

// Resolve fetches the inode and attributes for an entry identified by the given path
func (m *grpcMeta) Resolve(ctx Context, parent Ino, path string, inode *Ino, attr *Attr, force bool) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.ResolveRequest{
		Ctx:    c,
		Parent: uint64(parent),
		Path:   path,
		Force:  force,
	}
	resp, err := m.client.Resolve(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = Ino(resp.GetInode())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(resp.GetInode(), attr)
	}
	return 0
}

// GetAttr returns the attributes for given node (with caching)
func (m *grpcMeta) GetAttr(ctx Context, inode Ino, attr *Attr) syscall.Errno {
	// Check cache first
	if cachedAttr, found := m.getAttrFromCache(uint64(inode)); found {
		if attr != nil {
			*attr = *cachedAttr
		}
		return 0
	}

	c := m.grpcContext(ctx)
	req := &pb.GetAttrRequest{
		Ctx:   c,
		Inode: uint64(inode),
	}
	resp, err := m.client.GetAttr(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(uint64(inode), attr)
	}
	return 0
}

// SetAttr updates the attributes for given node
func (m *grpcMeta) SetAttr(ctx Context, inode Ino, set uint16, sggidclearmode uint8, attr *Attr) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.SetAttrRequest{
		Ctx:            c,
		Inode:          uint64(inode),
		Set:            uint32(set),
		Sggidclearmode: uint32(sggidclearmode),
	}
	if attr != nil {
		req.Attr = AttrToProto(attr)
	}
	resp, err := m.client.SetAttr(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	// Invalidate cache on mutation
	m.invalidateAttrCache(uint64(inode))
	return 0
}

// CheckSetAttr checks if setting attr is allowed
func (m *grpcMeta) CheckSetAttr(ctx Context, inode Ino, set uint16, attr Attr) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.CheckSetAttrRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Set:   uint32(set),
		Attr:  AttrToProto(&attr),
	}
	resp, err := m.client.CheckSetAttr(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	return 0
}

// Mknod creates a node in a directory
func (m *grpcMeta) Mknod(ctx Context, parent Ino, name string, _type uint8, mode uint16, cumask uint16, rdev uint32, linkpath string, inode *Ino, attr *Attr) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.MknodRequest{
		Ctx:    c,
		Parent: uint64(parent),
		Name:   name,
		Type:   uint32(_type),
		Mode:   uint32(mode),
		Cumask: uint32(cumask),
		Rdev:   rdev,
	}
	resp, err := m.client.Mknod(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = Ino(resp.GetInode())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(resp.GetInode(), attr)
	}
	m.invalidateDirCache(uint64(parent))
	return 0
}

// Mkdir creates a sub-directory
func (m *grpcMeta) Mkdir(ctx Context, parent Ino, name string, mode uint16, cumask uint16, copysgid uint8, inode *Ino, attr *Attr) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.MkdirRequest{
		Ctx:      c,
		Parent:   uint64(parent),
		Name:     name,
		Mode:     uint32(mode),
		Cumask:   uint32(cumask),
		Copysgid: uint32(copysgid),
	}
	resp, err := m.client.Mkdir(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = Ino(resp.GetInode())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(resp.GetInode(), attr)
	}
	m.invalidateDirCache(uint64(parent))
	return 0
}

// Create creates a file in a directory
func (m *grpcMeta) Create(ctx Context, parent Ino, name string, mode uint16, cumask uint16, flags uint32, inode *Ino, attr *Attr) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.CreateRequest{
		Ctx:    c,
		Parent: uint64(parent),
		Name:   name,
		Mode:   uint32(mode),
		Cumask: uint32(cumask),
		Flags:  flags,
	}
	resp, err := m.client.Create(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = Ino(resp.GetInode())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(resp.GetInode(), attr)
	}
	m.invalidateDirCache(uint64(parent))
	return 0
}

// Open checks permission on a node and track it as open
func (m *grpcMeta) Open(ctx Context, inode Ino, flags uint32, attr *Attr) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.OpenRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Flags: flags,
	}
	resp, err := m.client.Open(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(uint64(inode), attr)
	}
	return 0
}

// Close a file
func (m *grpcMeta) Close(ctx Context, inode Ino) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.CloseRequest{
		Ctx:   c,
		Inode: uint64(inode),
	}
	resp, err := m.client.Close(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	return 0
}

// Unlink removes a file entry from a directory
func (m *grpcMeta) Unlink(ctx Context, parent Ino, name string, skipCheckTrash ...bool) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.UnlinkRequest{
		Ctx:    c,
		Parent: uint64(parent),
		Name:   name,
	}
	resp, err := m.client.Unlink(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	m.invalidateDirCache(uint64(parent))
	return 0
}

// Rmdir removes an empty sub-directory
func (m *grpcMeta) Rmdir(ctx Context, parent Ino, name string, skipCheckTrash ...bool) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.RmdirRequest{
		Ctx:    c,
		Parent: uint64(parent),
		Name:   name,
	}
	resp, err := m.client.Rmdir(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	m.invalidateDirCache(uint64(parent))
	return 0
}

// Rename moves an entry from a source directory to another
func (m *grpcMeta) Rename(ctx Context, parentSrc Ino, nameSrc string, parentDst Ino, nameDst string, flags uint32, inode *Ino, attr *Attr) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.RenameRequest{
		Ctx:       c,
		ParentSrc: uint64(parentSrc),
		NameSrc:   nameSrc,
		ParentDst: uint64(parentDst),
		NameDst:   nameDst,
		Flags:     flags,
	}
	resp, err := m.client.Rename(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = Ino(resp.GetInode())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(resp.GetInode(), attr)
	}
	m.invalidateDirCache(uint64(parentSrc))
	m.invalidateDirCache(uint64(parentDst))
	return 0
}

// Link creates an entry for node
func (m *grpcMeta) Link(ctx Context, inodeSrc, parent Ino, name string, attr *Attr) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.LinkRequest{
		Ctx:      c,
		InodeSrc: uint64(inodeSrc),
		Parent:   uint64(parent),
		Name:     name,
	}
	resp, err := m.client.Link(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(uint64(inodeSrc), attr)
	}
	m.invalidateDirCache(uint64(parent))
	return 0
}

// Symlink creates a symlink in a directory
func (m *grpcMeta) Symlink(ctx Context, parent Ino, name string, path string, inode *Ino, attr *Attr) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.SymlinkRequest{
		Ctx:    c,
		Parent: uint64(parent),
		Name:   name,
		Path:   path,
	}
	resp, err := m.client.Symlink(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = Ino(resp.GetInode())
	}
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(resp.GetInode(), attr)
	}
	m.invalidateDirCache(uint64(parent))
	return 0
}

// ReadLink returns the target of a symlink
func (m *grpcMeta) ReadLink(ctx Context, inode Ino, path *[]byte) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.ReadLinkRequest{
		Ctx:   c,
		Inode: uint64(inode),
	}
	resp, err := m.client.ReadLink(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if path != nil {
		*path = resp.Path
	}
	return 0
}

// Truncate changes the length for given file
func (m *grpcMeta) Truncate(ctx Context, inode Ino, flags uint8, attrlength uint64, attr *Attr, skipPermCheck bool) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.TruncateRequest{
		Ctx:           c,
		Inode:         uint64(inode),
		Flags:         uint32(flags),
		Length:        attrlength,
		SkipPermCheck: skipPermCheck,
	}
	resp, err := m.client.Truncate(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	m.invalidateAttrCache(uint64(inode))
	if attr != nil && resp.Attr != nil {
		*attr = *ProtoToAttr(resp.Attr)
		m.putAttrInCache(uint64(inode), attr)
	}
	return 0
}

// Fallocate preallocate given space for given file
func (m *grpcMeta) Fallocate(ctx Context, inode Ino, mode uint8, off uint64, size uint64, length *uint64) syscall.Errno {
	c := m.grpcContext(ctx)
	req := &pb.FallocateRequest{
		Ctx:   c,
		Inode: uint64(inode),
		Mode:  uint32(mode),
		Off:   off,
		Size:  size,
	}
	resp, err := m.client.Fallocate(m.withSessionID(ctx), req)
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if length != nil {
		*length = resp.GetLength()
	}
	return 0
}

// Readdir returns all entries for given directory (with caching)
func (m *grpcMeta) Readdir(ctx Context, inode Ino, wantattr uint8, entries *[]*Entry) syscall.Errno {
	// Check cache first (only if we want attributes)
	if wantattr != 0 {
		if cachedEntries, found := m.getDirFromCache(uint64(inode)); found {
			if entries != nil {
				*entries = cachedEntries
			}
			return 0
		}
	}

	c := m.grpcContext(ctx)
	req := &pb.ReaddirRequest{
		Ctx:      c,
		Inode:    uint64(inode),
		Wantattr: uint32(wantattr),
	}
	logger.Debugf("Readdir called for inode=%d, sid=%d", inode, m.sid)
	resp, err := m.client.Readdir(m.withSessionID(ctx), req)
	if err != nil {
		logger.Errorf("Readdir gRPC error: %v", err)
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		logger.Errorf("Readdir errno: %d", resp.GetErrno())
		return syscall.Errno(resp.GetErrno())
	}
	if entries != nil {
		*entries = ProtoToEntries(resp.Entries)
		if wantattr != 0 {
			m.putDirInCache(uint64(inode), *entries)
		}
	}
	logger.Debugf("Readdir succeeded, entries=%d", len(resp.Entries))
	return 0
}
