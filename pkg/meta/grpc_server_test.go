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
	"testing"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/require"
)

// TestMetaCtxCheckPermissionAuthzMode (spec: Authz mode disables engine-level
// POSIX access checks): with authz mode on, every metaCtx — including the
// zero/nil MetaContext branch — carries CheckPermission()==false so the
// engine-level stored-POSIX gate (baseMeta.Access) is skipped; without authz
// mode the behavior is unchanged.
func TestMetaCtxCheckPermissionAuthzMode(t *testing.T) {
	mctx := &pb.MetaContext{Uid: 1000, Gid: 1000, Gids: []uint32{1000}, Pid: 42}

	s := NewMetaProxyServer(nil, 0)
	require.True(t, s.metaCtx(context.Background(), mctx).CheckPermission(),
		"without authz mode the engine-level POSIX checks stay enabled")
	require.True(t, s.metaCtx(context.Background(), nil).CheckPermission())

	s.SetAuthzMode(true)
	require.False(t, s.metaCtx(context.Background(), mctx).CheckPermission(),
		"authz mode must disable engine-level POSIX checks")
	require.False(t, s.metaCtx(context.Background(), nil).CheckPermission(),
		"the Background branch follows authz mode too")
	require.False(t, s.metaCtx(context.Background(), &pb.MetaContext{}).CheckPermission())

	// The skip survives WithValue (shallow copy of wrapContext).
	skipped := s.metaCtx(context.Background(), mctx)
	vctx := skipped.WithValue("k", "v")
	require.False(t, vctx.CheckPermission(), "WithValue must preserve the skip")
	require.Equal(t, uint32(1000), vctx.Uid())
	require.Equal(t, uint32(42), vctx.Pid())
}
