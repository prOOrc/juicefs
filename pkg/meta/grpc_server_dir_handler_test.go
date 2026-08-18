/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
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
	"testing"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// mockAuthzMeta implements Meta for DirHandler authz tests. Unstubbed methods
// panic (embedded nil interface), which surfaces any unexpected call.
type mockAuthzMeta struct {
	Meta
	entries []*Entry
}

// Readdir mirrors baseMeta.Readdir: prepends "." and ".." before the real entries.
func (m *mockAuthzMeta) Readdir(ctx Context, inode Ino, plus uint8, entries *[]*Entry) syscall.Errno {
	*entries = []*Entry{
		{Inode: inode, Name: []byte("."), Attr: &Attr{Typ: TypeDirectory}},
		{Inode: inode, Name: []byte(".."), Attr: &Attr{Typ: TypeDirectory}},
	}
	*entries = append(*entries, m.entries...)
	return 0
}

// NewDirHandler mirrors baseMeta.NewDirHandler: appends "." and ".." to the
// init entries and wraps a real dirHandler with a single-batch fetcher.
func (m *mockAuthzMeta) NewDirHandler(ctx Context, inode Ino, plus bool, initEntries []*Entry) (DirHandler, syscall.Errno) {
	initEntries = append(initEntries,
		&Entry{Inode: inode, Name: []byte("."), Attr: &Attr{Typ: TypeDirectory}},
		&Entry{Inode: inode, Name: []byte(".."), Attr: &Attr{Typ: TypeDirectory}},
	)
	return &dirHandler{
		inode:       inode,
		plus:        plus,
		initEntries: initEntries,
		batchNum:    128,
		fetcher: func(ctx Context, ino Ino, cursor interface{}, offset, limit int, plus bool) (interface{}, []*Entry, error) {
			if offset == 0 {
				return "end", m.entries, nil
			}
			return "end", nil, nil
		},
	}, 0
}

var rootEntries = []*Entry{
	{Inode: 100, Name: []byte(".minio.sys"), Attr: &Attr{Typ: TypeDirectory}},
	{Inode: 101, Name: []byte("companies"), Attr: &Attr{Typ: TypeDirectory}},
	{Inode: 102, Name: []byte("company"), Attr: &Attr{Typ: TypeDirectory}},
	{Inode: 103, Name: []byte("programs"), Attr: &Attr{Typ: TypeDirectory}},
}

// newAuthzTestServer builds a MetaProxyServer with authz enabled and the given
// root entries. Auth expectations are set on the returned mock client.
func newAuthzTestServer(t *testing.T, entries []*Entry) (*MetaProxyServer, *mockAuthzClient) {
	t.Helper()
	server := NewMetaProxyServer(&mockAuthzMeta{entries: entries}, 0)
	client := new(mockAuthzClient)
	server.SetAuthzInterceptor(newTestInterceptor(client, server.InodePathCache(), "user-1"))
	return server, client
}

// listUntilEOF simulates the FUSE kernel readdir protocol: keep calling List
// with an offset that counts the entries actually received, until an empty
// response. Returns the full stream of entry names in delivery order.
func listUntilEOF(t *testing.T, s *MetaProxyServer, handle *pb.DirHandlerHandle) []string {
	t.Helper()
	var stream []string
	off := 0
	for i := 0; i < 100; i++ {
		resp, err := s.DirHandlerList(context.Background(), &pb.DirHandlerListRequest{Handle: handle, Offset: int32(off)})
		assert.NoError(t, err)
		assert.EqualValues(t, 0, resp.Errno)
		if len(resp.Entries) == 0 {
			return stream
		}
		for _, e := range resp.Entries {
			stream = append(stream, string(e.Name))
		}
		off += len(resp.Entries)
	}
	t.Fatal("readdir did not terminate within 100 calls")
	return nil
}

func newTestHandle(t *testing.T, s *MetaProxyServer) *pb.DirHandlerHandle {
	t.Helper()
	resp, err := s.NewDirHandler(context.Background(), &pb.NewDirHandlerRequest{Inode: uint64(RootInode)})
	assert.NoError(t, err)
	assert.EqualValues(t, 0, resp.Errno)
	return resp.Handle
}

// TestDirHandlerList_Authz_NoDuplicateEntries is a regression test for the
// offset desync: with post-filtering on the DirHandler's own cursor, the
// kernel's filtered-count offsets re-served earlier entries (e.g. "companies"
// appeared once per init-entry + batch slice). The listing must deliver each
// allowed entry exactly once and then EOF.
func TestDirHandlerList_Authz_NoDuplicateEntries(t *testing.T) {
	server, client := newAuthzTestServer(t, rootEntries)
	client.On("CheckBulkPermissions", mock.Anything, "user-1",
		[]string{"/.minio.sys/", "/companies/", "/company/", "/programs/"}, AuthzPermissionView).
		Return([]bool{false, true, false, false}, nil).Once()

	handle := newTestHandle(t, server)
	stream := listUntilEOF(t, server, handle)

	assert.Equal(t, []string{"companies"}, stream, "each allowed entry must appear exactly once")
	client.AssertExpectations(t)
}

// TestDirHandlerList_Authz_AllDenied returns an empty stream when no entry is
// viewable, and terminates immediately.
func TestDirHandlerList_Authz_AllDenied(t *testing.T) {
	server, client := newAuthzTestServer(t, rootEntries)
	client.On("CheckBulkPermissions", mock.Anything, "user-1",
		mock.Anything, AuthzPermissionView).
		Return([]bool{false, false, false, false}, nil).Once()

	handle := newTestHandle(t, server)
	stream := listUntilEOF(t, server, handle)

	assert.Empty(t, stream)
	client.AssertExpectations(t)
}

// TestDirHandlerList_NoAuthz_Unchanged verifies the non-authz path still
// streams every entry (including "." and ".." from init entries) exactly once.
func TestDirHandlerList_NoAuthz_Unchanged(t *testing.T) {
	server := NewMetaProxyServer(&mockAuthzMeta{entries: rootEntries}, 0)
	handle := newTestHandle(t, server)

	stream := listUntilEOF(t, server, handle)

	assert.Equal(t, []string{".", "..", ".minio.sys", "companies", "company", "programs"}, stream)
}
