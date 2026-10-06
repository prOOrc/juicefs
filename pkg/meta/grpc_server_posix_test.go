/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package meta

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// TestProxyMknodAuthzModeSkipsPosix (scenarios: "Foreign-uid client writes
// into stored-owned directory with authz allowed" / "Without authz the
// behavior is unchanged"): a client carrying a POSIX uid that differs from
// the stored owner of a 0755 directory is denied by the engine-level POSIX
// check without authz mode and succeeds with authz mode, where the authz
// interceptor is the only access gate. The new inode is stored with the
// client-carried uid.
func TestProxyMknodAuthzModeSkipsPosix(t *testing.T) {
	if os.Getenv("SKIP_NON_CORE") == "true" {
		t.Skipf("skip non-core test")
	}
	const db = 15 // free slot of the local 16-db Redis (0,1,10-14 taken by other suites)
	const clientUid = 1000

	rdb := redis.NewClient(&redis.Options{Addr: encTestRedisAddr(), DB: db})
	require.NoError(t, rdb.Ping(Background()).Err(), "Redis must be running at "+encTestRedisAddr())
	require.NoError(t, rdb.FlushDB(Background()).Err())
	t.Cleanup(func() { _ = rdb.Close() })

	m, err := newRedisMeta("redis", fmt.Sprintf("%s/%d", encTestRedisAddr(), db), testConfig())
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Shutdown() })
	rm := m.(*redisMeta)
	format := &Format{Name: "posix-gate-test", UUID: "posix-gate-test-uuid", DirStats: true}
	require.NoError(t, rm.Init(format, true))
	_, err = rm.Load(true)
	require.NoError(t, err)

	server := NewMetaProxyServer(rm, 0)
	root := &pb.MetaContext{Uid: 0, Gid: 0, Gids: []uint32{0}}
	client := &pb.MetaContext{Uid: clientUid, Gid: clientUid, Gids: []uint32{clientUid}}

	// Stored parent: /acl owned by uid 0, mode 0755.
	mresp, err := server.Mkdir(context.Background(), &pb.MkdirRequest{
		Ctx: root, Parent: uint64(RootInode), Name: "acl", Mode: 0755,
	})
	require.NoError(t, err)
	require.Zero(t, mresp.Errno, "root Mkdir setup")
	parent := mresp.Inode

	aresp, err := server.GetAttr(context.Background(), &pb.GetAttrRequest{Ctx: root, Inode: parent})
	require.NoError(t, err)
	require.Zero(t, aresp.Errno)
	require.Equal(t, uint32(0), aresp.Attr.Uid, "stored owner must be uid 0")
	require.Equal(t, uint32(0755), uint32(aresp.Attr.Mode)&0777)

	mknod := func() *pb.MknodResponse {
		resp, err := server.Mknod(context.Background(), &pb.MknodRequest{
			Ctx: client, Parent: parent, Name: "f.bin", Type: uint32(TypeFile),
			Mode: 0644,
		})
		require.NoError(t, err)
		return resp
	}

	// Without authz mode: engine-level POSIX check denies the foreign uid.
	require.False(t, server.authzMode)
	resp := mknod()
	require.Equal(t, syscall.EACCES, syscall.Errno(resp.Errno),
		"stored-POSIX must deny uid %d in a 0755 dir owned by 0 without authz mode", clientUid)

	// With authz mode: the POSIX gate is skipped, the create succeeds and the
	// inode is stored with the client-carried uid.
	server.SetAuthzMode(true)
	resp = mknod()
	require.Zero(t, resp.Errno, "authz mode must skip the stored-POSIX gate")
	require.NotZero(t, resp.Inode)
	require.Equal(t, uint32(clientUid), resp.Attr.Uid, "new inode stored with the client-carried uid")

	aresp, err = server.GetAttr(context.Background(), &pb.GetAttrRequest{Ctx: root, Inode: resp.Inode})
	require.NoError(t, err)
	require.Zero(t, aresp.Errno)
	require.Equal(t, uint32(clientUid), aresp.Attr.Uid, "stored uid must reflect the carried uid, not a rewritten one")
}
