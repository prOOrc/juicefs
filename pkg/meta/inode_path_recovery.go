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
	"path"
	"strconv"
	"syscall"

	"github.com/juicedata/juicefs/pkg/utils"
	"golang.org/x/sync/singleflight"
)

// maxRecoverDepth caps parent-chain reconstruction (design I6): a deeper chain
// is unrecoverable and fails closed.
const maxRecoverDepth = 64

var recoveryLogger = utils.GetLogger("juicefs-recovery")

// PathRecovery reconstructs inode→path mappings after a proxy restart or cache
// eviction (fix-meta-proxy-session-churn, design D1). The inode→path cache is a
// derived structure; the metadata engine (attr.Parent) is the source of truth.
//
// INVARIANT (design I1, task 2.1): implementations read the engine DIRECTLY,
// never through the gRPC MetaService — the authz interceptor hangs on the
// gRPC path only, so recovery cannot recurse into itself. No client identity
// participates: recovery reconstructs a server-side fact and never grants
// access; its result is only an input to the ordinary authz check (I2), and
// any failure denies exactly as before the fix (I3).
type PathRecovery interface {
	// Resolve returns the path of ino. resolved=false means unrecoverable
	// (engine error, cycle, depth cap) — callers must fail closed; gone=true
	// means the engine reported the inode (or an ancestor) as nonexistent —
	// callers answer ENOENT instead of denying (I7).
	Resolve(ctx context.Context, ino Ino) (path string, resolved bool, gone bool)
	// Invalidate drops the cached path of one inode (post-mutation hygiene,
	// design D5). Descendants are not tracked: their stale paths heal through
	// recovery on next access.
	Invalidate(ino Ino)
}

type pathResolver struct {
	meta  Meta
	cache *InodePathCache
	group singleflight.Group // one in-flight reconstruction per inode (I5)
}

func newPathResolver(m Meta, cache *InodePathCache) *pathResolver {
	return &pathResolver{meta: m, cache: cache}
}

// Resolve implements PathRecovery.
func (r *pathResolver) Resolve(ctx context.Context, ino Ino) (string, bool, bool) {
	if ino == RootInode {
		return "/", true, false
	}
	if p := r.cache.Get(ino); p != "" {
		return p, true, false
	}
	v, _, _ := r.group.Do(strconv.FormatUint(uint64(ino), 10), func() (interface{}, error) {
		return r.recover(ctx, ino), nil
	})
	res, _ := v.(recoverResult)
	// A concurrent winner may have warmed the whole ancestor chain into the
	// cache even when this ino itself stayed unresolved; prefer a fresh hit.
	if !res.resolved {
		if p := r.cache.Get(ino); p != "" {
			return p, true, false
		}
	}
	return res.path, res.resolved, res.gone
}

type recoverResult struct {
	path     string
	resolved bool
	gone     bool
}

// recover climbs the parent chain to the root (same shape as baseMeta.GetPaths,
// pkg/meta/base.go): GetAttr names the parent, readdir of the parent names the
// inode. On success the whole ancestor chain is warmed first-seen-wins.
func (r *pathResolver) recover(ctx context.Context, ino Ino) recoverResult {
	// Engine-level POSIX checks stay off, matching authz-mode request contexts
	// (metaCtx with WrapWithCancelSkipPermCheck): the authz interceptor is the
	// only access gate on the proxy.
	c := WrapWithCancelSkipPermCheck(ctx, 0, 0, []uint32{0})
	defer c.Cancel()

	var attr Attr
	if st := r.meta.GetAttr(c, ino, &attr); st != 0 {
		if st == syscall.ENOENT {
			return recoverResult{gone: true}
		}
		// Engine error is uncertainty: fail closed with a logged reason (I3).
		recoveryLogger.Debugf("path recovery: getattr inode %d: %s", ino, st)
		return recoverResult{}
	}

	type node struct {
		ino  Ino
		name string
		attr Attr
	}
	chain := make([]node, 0, 8) // target → ... → root's child
	visited := map[Ino]struct{}{ino: {}}
	cur := ino
	for cur != RootInode {
		if len(chain) >= maxRecoverDepth {
			recoveryLogger.Debugf("path recovery: depth cap %d exceeded at inode %d", maxRecoverDepth, cur)
			return recoverResult{} // depth cap (I6)
		}
		parent := attr.Parent
		if parent == 0 {
			recoveryLogger.Debugf("path recovery: inode %d has no parent", cur)
			return recoverResult{}
		}
		if _, dup := visited[parent]; dup {
			recoveryLogger.Debugf("path recovery: parent cycle at inode %d (parent %d already visited)", cur, parent)
			return recoverResult{} // parent cycle (I6)
		}
		var entries []*Entry
		if st := r.meta.Readdir(c, parent, 0, &entries); st != 0 {
			if st == syscall.ENOENT {
				return recoverResult{gone: true}
			}
			recoveryLogger.Debugf("path recovery: readdir parent %d of inode %d: %s", parent, cur, st)
			return recoverResult{}
		}
		var name string
		for _, e := range entries {
			if e.Inode == cur {
				name = string(e.Name)
				break
			}
		}
		if name == "" {
			// The parent lists no entry for the inode: the node is dangling
			// (deleted) — an honest ENOENT for the client, not a deny (I7).
			return recoverResult{gone: true}
		}
		chain = append(chain, node{ino: cur, name: name, attr: attr})
		visited[parent] = struct{}{}
		cur = parent
		if cur == RootInode {
			break
		}
		if st := r.meta.GetAttr(c, cur, &attr); st != 0 {
			if st == syscall.ENOENT {
				return recoverResult{gone: true}
			}
			recoveryLogger.Debugf("path recovery: getattr inode %d (mid-chain): %s", cur, st)
			return recoverResult{}
		}
	}

	// Build root→target and warm every ancestor first-seen-wins (I5): sibling
	// lookups in the same directory then hit the cache without their own walk.
	parts := make([]string, 0, len(chain)+1)
	parts = append(parts, "/")
	target := chain[0]
	for i := len(chain) - 1; i >= 0; i-- {
		parts = append(parts, chain[i].name)
		p := path.Join(parts...)
		r.cache.Set(chain[i].ino, withDirSlash(p, &chain[i].attr))
	}
	return recoverResult{path: withDirSlash(path.Join(parts...), &target.attr), resolved: true}
}

// Invalidate implements PathRecovery.
func (r *pathResolver) Invalidate(ino Ino) {
	if ino == RootInode {
		return // root is pinned
	}
	r.cache.Unset(ino)
}
