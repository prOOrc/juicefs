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

// Client-side Create FEK handling (stage 11, decision B3): the response FEK
// flows into attr.Fek and the FEK LRU (so the very first write session is
// encrypted); an older proxy without the response FEK falls back to
// ResolveFileKey on a TLS channel only.

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// createFekClient answers Create with a canned response and counts
// ResolveFileKey calls (the fallback path). Any other call panics on the
// embedded nil client.
type createFekClient struct {
	pb.MetaServiceClient

	createResp  *pb.CreateResponse
	resolveResp *pb.ResolveFileKeyResponse
	resolveErr  error
	resolves    int
}

func (c *createFekClient) Create(ctx context.Context, req *pb.CreateRequest, opts ...grpc.CallOption) (*pb.CreateResponse, error) {
	return c.createResp, nil
}

func (c *createFekClient) ResolveFileKey(ctx context.Context, req *pb.ResolveFileKeyRequest, opts ...grpc.CallOption) (*pb.ResolveFileKeyResponse, error) {
	c.resolves++
	if c.resolveErr != nil {
		return nil, c.resolveErr
	}
	return c.resolveResp, nil
}

// newCreateFekMeta builds a grpcMeta around the stub client with the caches
// the Create path touches.
func newCreateFekMeta(c pb.MetaServiceClient, tlsOn bool) *grpcMeta {
	return &grpcMeta{
		client:    c,
		fekCache:  expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		attrCache: expirable.NewLRU[uint64, *Attr](10, nil, time.Minute),
		dirCache:  expirable.NewLRU[uint64, []*Entry](10, nil, time.Minute),
		tlsOn:     tlsOn,
	}
}

// TestCreateUsesResponseFek (decision B3): a proxy that delivers the plaintext
// FEK in CreateResponse fills attr.Fek and the FEK LRU without any extra RPC —
// the writer starts encrypted from the very first write.
func TestCreateUsesResponseFek(t *testing.T) {
	fek := randomBytes(t, fekSize)
	c := &createFekClient{createResp: &pb.CreateResponse{
		Errno: 0, Inode: 7,
		Attr: AttrToProto(&Attr{Typ: TypeFile, Encrypted: true, DriveFileID: "df-1", FekVersion: 1}),
		Fek:  fek, FekVersion: 1,
	}}
	m := newCreateFekMeta(c, true)

	var inode Ino
	var attr Attr
	st := m.Create(Background(), RootInode, "f.exr", 0o644, 0o22, syscall.O_CREAT|syscall.O_EXCL, &inode, &attr)
	require.Equal(t, syscall.Errno(0), st)
	require.Equal(t, Ino(7), inode)
	require.Equal(t, fek, attr.Fek, "attr.Fek must carry the response FEK (writer.fek)")
	require.EqualValues(t, 1, attr.FekVersion)
	require.Equal(t, 0, c.resolves, "no fallback RPC when the response carries the FEK")

	e, ok := m.fekCache.Get(7)
	require.True(t, ok, "the response FEK must be cached for the Open cache-hit path")
	require.Equal(t, fek, e.fek)
	require.EqualValues(t, 1, e.version)
}

// TestCreateFallbackResolveFileKey (decision B3): an older proxy sends no fek —
// on a TLS channel the client resolves the FEK itself (and caches it); over a
// plaintext channel there is no fallback (the writer fails closed instead).
func TestCreateFallbackResolveFileKey(t *testing.T) {
	fek := randomBytes(t, fekSize)
	encryptedResp := &pb.CreateResponse{
		Errno: 0, Inode: 8,
		Attr: AttrToProto(&Attr{Typ: TypeFile, Encrypted: true, DriveFileID: "df-2", FekVersion: 1}),
	}

	t.Run("tls channel resolves the fek", func(t *testing.T) {
		c := &createFekClient{
			createResp:  encryptedResp,
			resolveResp: &pb.ResolveFileKeyResponse{Errno: 0, Encrypted: true, Fek: fek, FekVersion: 1, DriveFileId: "df-2"},
		}
		m := newCreateFekMeta(c, true)

		var inode Ino
		var attr Attr
		st := m.Create(Background(), RootInode, "old.exr", 0o644, 0o22, syscall.O_CREAT, &inode, &attr)
		require.Equal(t, syscall.Errno(0), st)
		require.Equal(t, 1, c.resolves, "the fallback must call ResolveFileKey")
		require.Equal(t, fek, attr.Fek)
		require.EqualValues(t, 1, attr.FekVersion)

		e, ok := m.fekCache.Get(8)
		require.True(t, ok, "ResolveFileKey caches the key")
		require.Equal(t, fek, e.fek)
	})

	t.Run("plaintext channel stays keyless", func(t *testing.T) {
		c := &createFekClient{createResp: encryptedResp}
		m := newCreateFekMeta(c, false)

		var inode Ino
		var attr Attr
		st := m.Create(Background(), RootInode, "old.exr", 0o644, 0o22, syscall.O_CREAT, &inode, &attr)
		require.Equal(t, syscall.Errno(0), st)
		require.Equal(t, 0, c.resolves, "no FEK over a plaintext channel")
		require.Nil(t, attr.Fek, "the writer fails closed (EIO) without a FEK")
	})

	t.Run("resolve failure keeps the create successful", func(t *testing.T) {
		c := &createFekClient{
			createResp: encryptedResp,
			resolveErr: syscall.EACCES,
		}
		m := newCreateFekMeta(c, true)

		var inode Ino
		var attr Attr
		st := m.Create(Background(), RootInode, "old.exr", 0o644, 0o22, syscall.O_CREAT, &inode, &attr)
		require.Equal(t, syscall.Errno(0), st, "a failed fallback must not fail the POSIX create")
		require.Nil(t, attr.Fek)
	})
}
