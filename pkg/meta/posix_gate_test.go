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

	"github.com/stretchr/testify/require"
)

// TestAccessCheckPermissionGate: baseMeta.Access is the single engine-level
// POSIX gate shared by all engines (Redis/SQL/KV call-sites). With
// CheckPermission()==false it must return 0 without touching the engine (a
// zero baseMeta has a nil engine — any access would panic); the uid==0
// bypass is unchanged.
func TestAccessCheckPermissionGate(t *testing.T) {
	m := &baseMeta{} // en == nil: engine access would panic — the gate must prevent it

	// authz-mode context (proxy): POSIX check skipped for a non-root uid.
	ctx := WrapWithCancelSkipPermCheck(context.Background(), 1, 1000, []uint32{1000})
	require.Zero(t, m.Access(ctx, 1, MODE_MASK_W, nil),
		"CheckPermission()==false must short-circuit the POSIX gate")

	// uid==0 bypass unchanged (pre-existing semantics).
	root := NewContext(1, 0, []uint32{0})
	require.Zero(t, m.Access(root, 1, MODE_MASK_W, nil))

	// WithValue preserves the skip flag through the shallow copy.
	vctx := ctx.WithValue("k", "v")
	require.Zero(t, m.Access(vctx, 1, MODE_MASK_W, nil))
}
