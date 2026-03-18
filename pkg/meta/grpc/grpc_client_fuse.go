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
	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// --- Core FUSE operations ---

// StatFS gets filesystem stats
func (c *Client) StatFS(ctx meta.Context, ino meta.Ino, totalspace, availspace, iused, iavail *uint64) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.StatFS(grpcCtx, &pb.StatFSRequest{
		Ctx: toProtoContext(ctx),
		Ino: uint64(ino),
	})
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

// Lookup looks up a directory entry
func (c *Client) Lookup(ctx meta.Context, parent meta.Ino, name string, inode *meta.Ino, attr *meta.Attr, checkPerm bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Lookup(grpcCtx, &pb.LookupRequest{
		Ctx:             toProtoContext(ctx),
		Parent:          uint64(parent),
		Name:            name,
		CheckPermission: checkPerm,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = meta.Ino(resp.GetInode())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// Resolve resolves a path
func (c *Client) Resolve(ctx meta.Context, parent meta.Ino, path string, inode *meta.Ino, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Resolve(grpcCtx, &pb.ResolveRequest{
		Ctx:    toProtoContext(ctx),
		Parent: uint64(parent),
		Path:   path,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = meta.Ino(resp.GetInode())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// Access checks access permissions
func (c *Client) Access(ctx meta.Context, ino meta.Ino, mode uint8, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Access(grpcCtx, &pb.AccessRequest{
		Ctx:      toProtoContext(ctx),
		Inode:    uint64(ino),
		Modemask: uint32(mode),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// GetAttr gets file attributes
func (c *Client) GetAttr(ctx meta.Context, ino meta.Ino, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetAttr(grpcCtx, &pb.GetAttrRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// SetAttr sets file attributes
func (c *Client) SetAttr(ctx meta.Context, ino meta.Ino, set uint16, sggidclearmode uint8, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	req := &pb.SetAttrRequest{
		Ctx:            toProtoContext(ctx),
		Inode:          uint64(ino),
		Set:            uint32(set),
		Sggidclearmode: uint32(sggidclearmode),
		Attr:           toProtoAttr(attr),
	}
	resp, err := c.client.SetAttr(grpcCtx, req)
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// CheckSetAttr checks if attributes can be set
func (c *Client) CheckSetAttr(ctx meta.Context, ino meta.Ino, set uint16, attr meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CheckSetAttr(grpcCtx, &pb.CheckSetAttrRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Set:   uint32(set),
		Attr:  toProtoAttr(&attr),
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// Mknod creates a special file
func (c *Client) Mknod(ctx meta.Context, parent meta.Ino, name string, typ uint8, mode, cumask uint16, rdev uint32, fpath string, inode *meta.Ino, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Mknod(grpcCtx, &pb.MknodRequest{
		Ctx:    toProtoContext(ctx),
		Parent: uint64(parent),
		Name:   name,
		Type:   uint32(typ),
		Mode:   uint32(mode),
		Cumask: uint32(cumask),
		Rdev:   rdev,
		Path:   fpath,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = meta.Ino(resp.GetInode())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// Mkdir creates a directory
func (c *Client) Mkdir(ctx meta.Context, parent meta.Ino, name string, mode, cumask uint16, copysgid uint8, inode *meta.Ino, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Mkdir(grpcCtx, &pb.MkdirRequest{
		Ctx:      toProtoContext(ctx),
		Parent:   uint64(parent),
		Name:     name,
		Mode:     uint32(mode),
		Cumask:   uint32(cumask),
		Copysgid: uint32(copysgid),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = meta.Ino(resp.GetInode())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// Create creates a file
func (c *Client) Create(ctx meta.Context, parent meta.Ino, name string, mode, cumask uint16, flags uint32, inode *meta.Ino, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Create(grpcCtx, &pb.CreateRequest{
		Ctx:    toProtoContext(ctx),
		Parent: uint64(parent),
		Name:   name,
		Mode:   uint32(mode),
		Cumask: uint32(cumask),
		Flags:  flags,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = meta.Ino(resp.GetInode())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// Open opens a file
func (c *Client) Open(ctx meta.Context, ino meta.Ino, flags uint32, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Open(grpcCtx, &pb.OpenRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Flags: flags,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// Close closes a file
func (c *Client) Close(ctx meta.Context, ino meta.Ino) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Close(grpcCtx, &pb.CloseRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// Unlink removes a file
func (c *Client) Unlink(ctx meta.Context, parent meta.Ino, name string, skipCheckTrash ...bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	skip := false
	if len(skipCheckTrash) > 0 {
		skip = skipCheckTrash[0]
	}

	resp, err := c.client.Unlink(grpcCtx, &pb.UnlinkRequest{
		Ctx:            toProtoContext(ctx),
		Parent:         uint64(parent),
		Name:           name,
		SkipCheckTrash: skip,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// Rmdir removes a directory
func (c *Client) Rmdir(ctx meta.Context, parent meta.Ino, name string, skipCheckTrash ...bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	skip := false
	if len(skipCheckTrash) > 0 {
		skip = skipCheckTrash[0]
	}

	resp, err := c.client.Rmdir(grpcCtx, &pb.RmdirRequest{
		Ctx:            toProtoContext(ctx),
		Parent:         uint64(parent),
		Name:           name,
		SkipCheckTrash: skip,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// Rename renames a file/directory
func (c *Client) Rename(ctx meta.Context, srcParent meta.Ino, srcName string, dstParent meta.Ino, dstName string, flags uint32, inode *meta.Ino, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Rename(grpcCtx, &pb.RenameRequest{
		Ctx:       toProtoContext(ctx),
		ParentSrc: uint64(srcParent),
		NameSrc:   srcName,
		ParentDst: uint64(dstParent),
		NameDst:   dstName,
		Flags:     flags,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = meta.Ino(resp.GetInode())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// Link creates a hard link
func (c *Client) Link(ctx meta.Context, srcIno, parent meta.Ino, name string, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Link(grpcCtx, &pb.LinkRequest{
		Ctx:      toProtoContext(ctx),
		InodeSrc: uint64(srcIno),
		Parent:   uint64(parent),
		Name:     name,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// Symlink creates a symbolic link
func (c *Client) Symlink(ctx meta.Context, parent meta.Ino, name, path string, inode *meta.Ino, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Symlink(grpcCtx, &pb.SymlinkRequest{
		Ctx:    toProtoContext(ctx),
		Parent: uint64(parent),
		Name:   name,
		Path:   path,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if inode != nil {
		*inode = meta.Ino(resp.GetInode())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// ReadLink reads a symbolic link
func (c *Client) ReadLink(ctx meta.Context, ino meta.Ino, path *[]byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ReadLink(grpcCtx, &pb.ReadLinkRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if path != nil {
		*path = resp.GetPath()
	}
	return 0
}

// Truncate truncates a file
func (c *Client) Truncate(ctx meta.Context, ino meta.Ino, flags uint8, length uint64, attr *meta.Attr, skipPermCheck bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Truncate(grpcCtx, &pb.TruncateRequest{
		Ctx:           toProtoContext(ctx),
		Inode:         uint64(ino),
		Flags:         uint32(flags),
		Length:        length,
		SkipPermCheck: skipPermCheck,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if attr != nil {
		*attr = *fromProtoAttr(resp.GetAttr())
	}
	return 0
}

// Fallocate allocates file space
func (c *Client) Fallocate(ctx meta.Context, ino meta.Ino, mode uint8, off, size uint64, length *uint64) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Fallocate(grpcCtx, &pb.FallocateRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Mode:  uint32(mode),
		Off:   off,
		Size:  size,
	})
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

// Readdir reads directory entries
func (c *Client) Readdir(ctx meta.Context, ino meta.Ino, wantAttr uint8, entries *[]*meta.Entry) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Readdir(grpcCtx, &pb.ReaddirRequest{
		Ctx:      toProtoContext(ctx),
		Inode:    uint64(ino),
		Wantattr: uint32(wantAttr),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	result := make([]*meta.Entry, 0, len(resp.GetEntries()))
	for _, e := range resp.GetEntries() {
		result = append(result, fromProtoEntry(e))
	}
	if entries != nil {
		*entries = result
	}
	return 0
}

// Read reads file slices
func (c *Client) Read(ctx meta.Context, ino meta.Ino, indx uint32, slices *[]meta.Slice) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Read(grpcCtx, &pb.ReadRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Indx:  indx,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	result := make([]meta.Slice, 0, len(resp.GetSlices()))
	for _, s := range resp.GetSlices() {
		result = append(result, meta.Slice{
			Id:   s.Id,
			Size: s.Size,
			Off:  s.Off,
			Len:  s.Len,
		})
	}
	if slices != nil {
		*slices = result
	}
	return 0
}

// Write writes file slices
func (c *Client) Write(ctx meta.Context, ino meta.Ino, indx, off uint32, slice meta.Slice, mtime time.Time) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Write(grpcCtx, &pb.WriteRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Indx:  indx,
		Off:   off,
		Slice: &pb.ProtoSlice{
			Id:   slice.Id,
			Size: slice.Size,
			Off:  slice.Off,
			Len:  slice.Len,
		},
		Mtime: mtime.Unix(),
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// NewSlice creates a new slice
func (c *Client) NewSlice(ctx meta.Context, id *uint64) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.NewSlice(grpcCtx, &pb.NewSliceRequest{
		Ctx: toProtoContext(ctx),
	})
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

// InvalidateChunkCache invalidates chunk cache
func (c *Client) InvalidateChunkCache(ctx meta.Context, ino meta.Ino, indx uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.InvalidateChunkCache(grpcCtx, &pb.InvalidateChunkCacheRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Indx:  indx,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// CopyFileRange copies file range
func (c *Client) CopyFileRange(ctx meta.Context, fin, offIn, fout, offOut, size uint64, flags uint32, copied, outLength *uint64) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CopyFileRange(grpcCtx, &pb.CopyFileRangeRequest{
		Ctx:    toProtoContext(ctx),
		Fin:    fin,
		OffIn:  offIn,
		Fout:   fout,
		OffOut: offOut,
		Size:   size,
		Flags:  flags,
	})
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
	return 0
}
