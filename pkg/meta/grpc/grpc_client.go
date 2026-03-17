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
	"io"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	aclAPI "github.com/juicedata/juicefs/pkg/acl"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/utils"
)

var gLogger = utils.GetLogger("juicefs")

// Client implements meta.Meta interface using gRPC
type Client struct {
	client MetaServiceClient
	conn   *grpc.ClientConn
	addr   string
	opts   *Options
}

// Options for gRPC client
type Options struct {
	Timeout     time.Duration
	MaxRetries  int
	DialOptions []grpc.DialOption
}

// DefaultOptions returns default client options
func DefaultOptions() *Options {
	return &Options{
		Timeout: 30 * time.Second,
	}
}

// NewClient creates a new gRPC client
func NewClient(addr string, opts *Options) (*Client, error) {
	if opts == nil {
		opts = DefaultOptions()
	}

	dialOpts := opts.DialOptions
	if len(dialOpts) == 0 {
		dialOpts = []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		}
	}

	conn, err := grpc.Dial(addr, dialOpts...)
	if err != nil {
		return nil, err
	}

	c := &Client{
		client: NewMetaServiceClient(conn),
		conn:   conn,
		addr:   addr,
		opts:   opts,
	}

	return c, nil
}

// CloseConn closes the gRPC connection
func (c *Client) CloseConn() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Name returns the name of the meta backend
func (c *Client) Name() string {
	return "grpc"
}

// getBase returns nil - this client implements Meta directly without baseMeta
func (c *Client) getBase() interface{} {
	return nil
}

// chroot changes the root directory
func (c *Client) chroot(ino meta.Ino) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	_, err := c.client.Chroot(ctx, &ChrootRequest{
		Ctx:    toProtoContext(nil),
		Subdir: "",
	})
	if err != nil {
		return err
	}
	return nil
}

// --- Meta interface implementation ---

// Init initializes the volume
func (c *Client) Init(format *meta.Format, force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	req := &InitRequest{
		Format: toProtoFormat(format),
		Force:  force,
	}
	resp, err := c.client.Init(ctx, req)
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// Load loads the volume
func (c *Client) Load(checkVersion bool) (*meta.Format, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Load(ctx, &LoadRequest{
		CheckVersion: checkVersion,
	})
	if err != nil {
		return nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	return fromProtoFormat(resp.GetFormat()), nil
}

// NewSession creates a new session
func (c *Client) NewSession(record bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.NewSession(ctx, &NewSessionRequest{
		Record: record,
	})
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// CloseSession closes a session
func (c *Client) CloseSession() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CloseSession(ctx, &CloseSessionRequest{})
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// FlushSession flushes session data
func (c *Client) FlushSession() {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.FlushSession(ctx, &FlushSessionRequest{})
	if err == nil && resp != nil {
		_ = resp.GetErrno()
	}
}

// Shutdown shuts down the volume
func (c *Client) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Shutdown(ctx, &ShutdownRequest{})
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// Reset resets the volume
func (c *Client) Reset() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Reset(ctx, &ResetRequest{})
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// GetFormat gets the volume format
func (c *Client) GetFormat() (*meta.Format, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetFormat(ctx, &GetFormatRequest{})
	if err != nil {
		return nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	return fromProtoFormat(resp.GetFormat()), nil
}

// GetSession gets session info
func (c *Client) GetSession(sid uint64, detail bool) (*meta.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetSession(ctx, &GetSessionRequest{
		Sid:    sid,
		Detail: detail,
	})
	if err != nil {
		return nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	return fromProtoSession(resp.GetSession()), nil
}

// ListSessions lists all sessions
func (c *Client) ListSessions() ([]*meta.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ListSessions(ctx, &ListSessionsRequest{})
	if err != nil {
		return nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	sessions := make([]*meta.Session, 0, len(resp.GetSessions()))
	for _, s := range resp.GetSessions() {
		sessions = append(sessions, fromProtoSession(s))
	}
	return sessions, nil
}

// CleanStaleSessions cleans stale sessions
func (c *Client) CleanStaleSessions(ctx meta.Context) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.CleanStaleSessions(grpcCtx, &CleanStaleSessionsRequest{
		Ctx: toProtoContext(ctx),
	})
	if err == nil && resp != nil {
		_ = resp.GetErrno()
	}
}

// StatFS gets filesystem stats
func (c *Client) StatFS(ctx meta.Context, ino meta.Ino, totalspace, availspace, iused, iavail *uint64) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.StatFS(grpcCtx, &StatFSRequest{
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

	resp, err := c.client.Lookup(grpcCtx, &LookupRequest{
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

	resp, err := c.client.Resolve(grpcCtx, &ResolveRequest{
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

	resp, err := c.client.Access(grpcCtx, &AccessRequest{
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

	resp, err := c.client.GetAttr(grpcCtx, &GetAttrRequest{
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

	req := &SetAttrRequest{
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

	resp, err := c.client.CheckSetAttr(grpcCtx, &CheckSetAttrRequest{
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

// Truncate truncates a file
func (c *Client) Truncate(ctx meta.Context, ino meta.Ino, flags uint8, length uint64, attr *meta.Attr, skipPermCheck bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Truncate(grpcCtx, &TruncateRequest{
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

	resp, err := c.client.Fallocate(grpcCtx, &FallocateRequest{
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

// ReadLink reads a symbolic link
func (c *Client) ReadLink(ctx meta.Context, ino meta.Ino, path *[]byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ReadLink(grpcCtx, &ReadLinkRequest{
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

// Symlink creates a symbolic link
func (c *Client) Symlink(ctx meta.Context, parent meta.Ino, name, path string, inode *meta.Ino, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Symlink(grpcCtx, &SymlinkRequest{
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

// Mknod creates a special file
func (c *Client) Mknod(ctx meta.Context, parent meta.Ino, name string, typ uint8, mode, cumask uint16, rdev uint32, fpath string, inode *meta.Ino, attr *meta.Attr) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Mknod(grpcCtx, &MknodRequest{
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

	resp, err := c.client.Mkdir(grpcCtx, &MkdirRequest{
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

	resp, err := c.client.Create(grpcCtx, &CreateRequest{
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

	resp, err := c.client.Open(grpcCtx, &OpenRequest{
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

	resp, err := c.client.Close(grpcCtx, &CloseRequest{
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

	resp, err := c.client.Unlink(grpcCtx, &UnlinkRequest{
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

// Rmdir removes a directory
func (c *Client) Rmdir(ctx meta.Context, parent meta.Ino, name string, skipCheckTrash ...bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	skip := false
	if len(skipCheckTrash) > 0 {
		skip = skipCheckTrash[0]
	}

	resp, err := c.client.Rmdir(grpcCtx, &RmdirRequest{
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

	resp, err := c.client.Rename(grpcCtx, &RenameRequest{
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

	resp, err := c.client.Link(grpcCtx, &LinkRequest{
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

// Readdir reads directory entries
func (c *Client) Readdir(ctx meta.Context, ino meta.Ino, wantAttr uint8, entries *[]*meta.Entry) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Readdir(grpcCtx, &ReaddirRequest{
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

	resp, err := c.client.Read(grpcCtx, &ReadRequest{
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

	resp, err := c.client.Write(grpcCtx, &WriteRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Indx:  indx,
		Off:   off,
		Slice: &ProtoSlice{
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

	resp, err := c.client.NewSlice(grpcCtx, &NewSliceRequest{
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

	resp, err := c.client.InvalidateChunkCache(grpcCtx, &InvalidateChunkCacheRequest{
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

	resp, err := c.client.CopyFileRange(grpcCtx, &CopyFileRangeRequest{
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

// Flock manages file locks
func (c *Client) Flock(ctx meta.Context, ino meta.Ino, owner uint64, ltype uint32, block bool) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Flock(grpcCtx, &FlockRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Owner: owner,
		Ltype: ltype,
		Block: block,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// Getlk gets lock information
func (c *Client) Getlk(ctx meta.Context, ino meta.Ino, owner uint64, ltype uint32, start, end uint64) (ltypeOut uint32, startOut uint64, endOut uint64, pid uint32, errno syscall.Errno) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Getlk(grpcCtx, &GetlkRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Owner: owner,
		Ltype: ltype,
		Start: start,
		End:   end,
	})
	if err != nil {
		return 0, 0, 0, 0, syscall.EIO
	}
	return resp.GetLtype(), resp.GetStart(), resp.GetEnd(), resp.GetPid(), syscall.Errno(resp.GetErrno())
}

// Setlk sets a lock
func (c *Client) Setlk(ctx meta.Context, ino meta.Ino, owner uint64, block bool, ltype uint32, start, end uint64, pid uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.Setlk(grpcCtx, &SetlkRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Owner: owner,
		Block: block,
		Ltype: ltype,
		Start: start,
		End:   end,
		Pid:   pid,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// ListLocks lists locks
func (c *Client) ListLocks(ctx context.Context, ino meta.Ino) ([]meta.PLockItem, []meta.FLockItem, error) {
	grpcCtx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ListLocks(grpcCtx, &ListLocksRequest{
		Ctx:   toProtoContext(nil),
		Inode: uint64(ino),
	})
	if err != nil {
		return nil, nil, err
	}
	if resp.GetErrno() != 0 {
		return nil, nil, syscall.Errno(resp.GetErrno())
	}
	// Simplified - return empty slices since plockRecord is unexported
	return nil, nil, nil
}

// GetXattr gets extended attribute
func (c *Client) GetXattr(ctx meta.Context, ino meta.Ino, name string, vbuff *[]byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetXattr(grpcCtx, &GetXattrRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Name:  name,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if vbuff != nil {
		*vbuff = resp.GetValue()
	}
	return 0
}

// SetXattr sets extended attribute
func (c *Client) SetXattr(ctx meta.Context, ino meta.Ino, name string, value []byte, flags uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.SetXattr(grpcCtx, &SetXattrRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Name:  name,
		Value: value,
		Flags: flags,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// RemoveXattr removes extended attribute
func (c *Client) RemoveXattr(ctx meta.Context, ino meta.Ino, name string) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.RemoveXattr(grpcCtx, &RemoveXattrRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
		Name:  name,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// ListXattr lists extended attributes
func (c *Client) ListXattr(ctx meta.Context, ino meta.Ino, names *[]byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ListXattr(grpcCtx, &ListXattrRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if names != nil {
		*names = resp.GetNames()
	}
	return 0
}

// GetParents gets parent inodes
func (c *Client) GetParents(ctx meta.Context, ino meta.Ino) map[meta.Ino]int {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetParents(grpcCtx, &GetParentsRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
	})
	if err != nil || resp == nil {
		return nil
	}
	if resp.GetErrno() != 0 {
		return nil
	}
	parents := make(map[meta.Ino]int, len(resp.GetParents()))
	for k, v := range resp.GetParents() {
		parents[meta.Ino(k)] = int(v)
	}
	return parents
}

// GetDirStat gets directory statistics
func (c *Client) GetDirStat(ctx meta.Context, ino meta.Ino) (stat interface{}, st syscall.Errno) {
	// Not implemented - dirStat is unexported
	return nil, syscall.ENOSYS
}

// SetFacl sets ACL
func (c *Client) SetFacl(ctx meta.Context, ino meta.Ino, aclType uint32, rule *aclAPI.Rule) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.SetFacl(grpcCtx, &SetFaclRequest{
		Ctx:     toProtoContext(ctx),
		Ino:     uint64(ino),
		AclType: aclType,
		Rule:    toProtoACLRule(rule),
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// GetFacl gets ACL
func (c *Client) GetFacl(ctx meta.Context, ino meta.Ino, aclType uint32, rule *aclAPI.Rule) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetFacl(grpcCtx, &GetFaclRequest{
		Ctx:     toProtoContext(ctx),
		Ino:     uint64(ino),
		AclType: aclType,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if rule != nil {
		*rule = *fromProtoACLRule(resp.GetRule())
	}
	return 0
}

// StoreToken stores a token
func (c *Client) StoreToken(ctx meta.Context, token []byte, id *uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.StoreToken(grpcCtx, &StoreTokenRequest{
		Ctx:   toProtoContext(ctx),
		Token: token,
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

// UpdateToken updates a token
func (c *Client) UpdateToken(ctx meta.Context, id uint32, token []byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.UpdateToken(grpcCtx, &UpdateTokenRequest{
		Ctx:   toProtoContext(ctx),
		Id:    id,
		Token: token,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// LoadToken loads a token
func (c *Client) LoadToken(ctx meta.Context, id uint32, token *[]byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.LoadToken(grpcCtx, &LoadTokenRequest{
		Ctx: toProtoContext(ctx),
		Id:  id,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if token != nil {
		*token = resp.GetToken()
	}
	return 0
}

// DeleteTokens deletes tokens
func (c *Client) DeleteTokens(ctx meta.Context, ids []uint32) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.DeleteTokens(grpcCtx, &DeleteTokensRequest{
		Ctx: toProtoContext(ctx),
		Ids: ids,
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// ListTokens lists tokens
func (c *Client) ListTokens(ctx meta.Context, tokens *map[uint32][]byte) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.ListTokens(grpcCtx, &ListTokensRequest{
		Ctx: toProtoContext(ctx),
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	result := make(map[uint32][]byte, len(resp.GetTokens()))
	for k, v := range resp.GetTokens() {
		result[k] = v
	}
	if tokens != nil {
		*tokens = result
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

// --- DirHandler implementation ---

type dirHandler struct {
	handleID uint64
	client   MetaServiceClient
	mu       sync.Mutex
}

// NewDirHandler creates a new directory handler
func (c *Client) NewDirHandler(ctx meta.Context, ino meta.Ino, plus bool, initEntries []*meta.Entry) (meta.DirHandler, syscall.Errno) {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	protoEntries := make([]*ProtoEntry, 0, len(initEntries))
	for _, e := range initEntries {
		protoEntries = append(protoEntries, toProtoEntry(e))
	}

	resp, err := c.client.NewDirHandler(grpcCtx, &NewDirHandlerRequest{
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

	resp, err := h.client.DirHandlerList(grpcCtx, &DirHandlerListRequest{
		Handle: &DirHandlerHandle{HandleId: h.handleID},
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

	_, err := h.client.DirHandlerInsert(grpcCtx, &DirHandlerInsertRequest{
		Handle: &DirHandlerHandle{HandleId: h.handleID},
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

	_, err := h.client.DirHandlerDelete(grpcCtx, &DirHandlerDeleteRequest{
		Handle: &DirHandlerHandle{HandleId: h.handleID},
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

	_, err := h.client.DirHandlerClose(grpcCtx, &DirHandlerCloseRequest{
		Handle: &DirHandlerHandle{HandleId: h.handleID},
	})
	_ = err // Ignore errors for now
}

// Read reads directory entries (not implemented)
func (h *dirHandler) Read(offset int) {
	// Not implemented
}

// --- Streaming Dump/Load ---

// DumpMeta dumps metadata
func (c *Client) DumpMeta(root meta.Ino, threads int32, keepSecret, fast, skipTrash bool) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		stream, err := c.client.DumpMeta(grpcCtx, &DumpMetaRequest{
			Root:       uint64(root),
			Threads:    threads,
			KeepSecret: keepSecret,
			Fast:       fast,
			SkipTrash:  skipTrash,
		})
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(chunk.GetData()); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()
	return pr, nil
}

// LoadMeta loads metadata
func (c *Client) LoadMeta(rc io.ReadCloser) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stream, err := c.client.LoadMeta(grpcCtx)
	if err != nil {
		return err
	}

	buf := make([]byte, 64*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if err := stream.Send(&LoadMetaChunk{Data: buf[:n]}); err != nil {
				return err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}

// DumpMetaV2 dumps metadata v2
func (c *Client) DumpMetaV2(ctx meta.Context, keepSecret bool, threads int32) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		stream, err := c.client.DumpMetaV2(grpcCtx, &DumpMetaV2Request{
			Ctx:        toProtoContext(ctx),
			KeepSecret: keepSecret,
			Threads:    threads,
		})
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(chunk.GetData()); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()
	return pr, nil
}

// LoadMetaV2 loads metadata v2
func (c *Client) LoadMetaV2(ctx meta.Context, rc io.ReadCloser) error {
	grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stream, err := c.client.LoadMetaV2(grpcCtx)
	if err != nil {
		return err
	}

	buf := make([]byte, 64*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if err := stream.Send(&LoadMetaV2Chunk{Data: buf[:n]}); err != nil {
				return err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	return syscall.Errno(resp.GetErrno())
}
