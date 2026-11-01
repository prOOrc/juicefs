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
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTreeMeta is an in-memory engine for recovery tests: a tree of inodes
// with parents and child names, mirroring what GetAttr/Readdir answer on a
// real engine. Unstubbed Meta methods are never called by pathResolver.
type fakeTreeMeta struct {
	Meta
	mu       sync.Mutex
	attrs    map[Ino]*Attr          // ino → attr (Parent, Typ)
	names    map[Ino]map[string]Ino // parent → name → child ino
	getErr   map[Ino]syscall.Errno  // GetAttr error injection per ino
	rdErr    map[Ino]syscall.Errno  // Readdir error injection per parent
	gate     chan struct{}          // if set, GetAttr(gatedIno) blocks until closed
	gatedIno Ino
	calls    recoveryEngineCallCounts // counters for singleflight assertions
}

type recoveryEngineCallCounts struct {
	getAttr  map[Ino]*int32
	readdir  map[Ino]*int32
	getAttrT *int32 // total
}

func newFakeTreeMeta() *fakeTreeMeta {
	m := &fakeTreeMeta{
		attrs:  map[Ino]*Attr{RootInode: {Typ: TypeDirectory, Parent: RootInode}},
		names:  map[Ino]map[string]Ino{RootInode: {}},
		getErr: map[Ino]syscall.Errno{},
		rdErr:  map[Ino]syscall.Errno{},
	}
	total := new(int32)
	m.calls = recoveryEngineCallCounts{
		getAttr:  map[Ino]*int32{},
		readdir:  map[Ino]*int32{},
		getAttrT: total,
	}
	return m
}

// mkdir adds a directory inode under parent with the given name.
func (m *fakeTreeMeta) mkdir(parent Ino, name string, ino Ino) *fakeTreeMeta {
	return m.add(parent, name, ino, TypeDirectory)
}

// create adds a file inode under parent with the given name.
func (m *fakeTreeMeta) create(parent Ino, name string, ino Ino) *fakeTreeMeta {
	return m.add(parent, name, ino, TypeFile)
}

func (m *fakeTreeMeta) add(parent Ino, name string, ino Ino, typ uint8) *fakeTreeMeta {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attrs[ino] = &Attr{Typ: typ, Parent: parent}
	if m.names[parent] == nil {
		m.names[parent] = map[string]Ino{}
	}
	m.names[parent][name] = ino
	return m
}

func (m *fakeTreeMeta) failGetAttr(ino Ino, st syscall.Errno) *fakeTreeMeta {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getErr[ino] = st
	return m
}

func (m *fakeTreeMeta) failReaddir(parent Ino, st syscall.Errno) *fakeTreeMeta {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rdErr[parent] = st
	return m
}

func (m *fakeTreeMeta) GetAttr(ctx Context, inode Ino, attr *Attr) syscall.Errno {
	atomic.AddInt32(m.calls.getAttrT, 1)
	m.mu.Lock()
	c := m.calls.getAttr[inode]
	if c == nil {
		c = new(int32)
		m.calls.getAttr[inode] = c
	}
	m.mu.Unlock()
	atomic.AddInt32(c, 1)

	if m.gate != nil && inode == m.gatedIno {
		<-m.gate // singleflight test: hold the leader's walk in flight
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.getErr[inode]; ok {
		return st
	}
	a, ok := m.attrs[inode]
	if !ok {
		return syscall.ENOENT
	}
	*attr = *a
	return 0
}

func (m *fakeTreeMeta) Readdir(ctx Context, inode Ino, plus uint8, entries *[]*Entry) syscall.Errno {
	m.mu.Lock()
	c := m.calls.readdir[inode]
	if c == nil {
		c = new(int32)
		m.calls.readdir[inode] = c
	}
	m.mu.Unlock()
	atomic.AddInt32(c, 1)

	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.rdErr[inode]; ok {
		return st
	}
	children, ok := m.names[inode]
	if !ok {
		if _, isDir := m.attrs[inode]; !isDir {
			return syscall.ENOTDIR
		}
		*entries = nil
		return 0
	}
	// baseMeta.Readdir prepends "." and ".." — include them so the resolver is
	// exercised against the real listing shape.
	*entries = []*Entry{
		{Inode: inode, Name: []byte("."), Attr: &Attr{Typ: TypeDirectory}},
		{Inode: m.attrs[inode].Parent, Name: []byte(".."), Attr: &Attr{Typ: TypeDirectory}},
	}
	for name, ino := range children {
		*entries = append(*entries, &Entry{Inode: ino, Name: []byte(name), Attr: &Attr{Typ: m.attrs[ino].Typ}})
	}
	return 0
}

func (m *fakeTreeMeta) getAttrCalls(ino Ino) int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.calls.getAttr[ino]; ok {
		return atomic.LoadInt32(c)
	}
	return 0
}

func (m *fakeTreeMeta) readdirCalls(parent Ino) int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.calls.readdir[parent]; ok {
		return atomic.LoadInt32(c)
	}
	return 0
}

func TestRecovery_LinearChain(t *testing.T) {
	engine := newFakeTreeMeta().
		mkdir(RootInode, "a", 10).
		mkdir(10, "b", 11).
		mkdir(11, "c", 12).
		create(12, "f.exr", 13)
	cache := NewInodePathCache(0)
	r := newPathResolver(engine, cache)

	p, resolved, gone := r.Resolve(context.Background(), 13)
	assert.True(t, resolved)
	assert.False(t, gone)
	assert.Equal(t, "/a/b/c/f.exr", p)

	// Directories are cached (and returned) in the dir convention with a
	// trailing slash, same as withDirSlash at the handler cache sites.
	p, resolved, gone = r.Resolve(context.Background(), 12)
	assert.True(t, resolved)
	assert.False(t, gone)
	assert.Equal(t, "/a/b/c/", p)

	// The whole ancestor chain is warmed (first-seen-wins Set), directories in
	// the dir convention with a trailing slash.
	assert.Equal(t, "/a/", cache.Get(10))
	assert.Equal(t, "/a/b/", cache.Get(11))
	assert.Equal(t, "/a/b/c/", cache.Get(12))
}

func TestRecovery_TreeSharedAncestors(t *testing.T) {
	engine := newFakeTreeMeta().
		mkdir(RootInode, "a", 10).
		mkdir(10, "b", 11).
		create(11, "x", 20).
		create(11, "y", 21)
	cache := NewInodePathCache(0)
	r := newPathResolver(engine, cache)

	p, resolved, _ := r.Resolve(context.Background(), 20)
	require.True(t, resolved)
	assert.Equal(t, "/a/b/x", p)

	readdirB := engine.readdirCalls(11)
	p, resolved, _ = r.Resolve(context.Background(), 21)
	require.True(t, resolved)
	assert.Equal(t, "/a/b/y", p)
	// The shared parent's listing was fetched only once; the sibling resolved
	// from the warmed cache without another walk.
	assert.Equal(t, int32(1), readdirB, "shared ancestor must be walked once")
	assert.Equal(t, "/a/b/y", cache.Get(21))
}

func TestRecovery_RootShortCircuit(t *testing.T) {
	r := newPathResolver(newFakeTreeMeta(), NewInodePathCache(0))
	p, resolved, gone := r.Resolve(context.Background(), RootInode)
	assert.True(t, resolved)
	assert.False(t, gone)
	assert.Equal(t, "/", p)
}

func TestRecovery_CycleFailsClosed(t *testing.T) {
	engine := newFakeTreeMeta().
		mkdir(RootInode, "a", 10).
		mkdir(10, "b", 11)
	// Corrupt the chain: 10's parent points back into the cycle below it.
	engine.attrs[10].Parent = 11
	r := newPathResolver(engine, NewInodePathCache(0))

	p, resolved, gone := r.Resolve(context.Background(), 11)
	assert.False(t, resolved, "cycle must be unrecoverable, not an error")
	assert.False(t, gone)
	assert.Equal(t, "", p)
}

func TestRecovery_DepthCapFailsClosed(t *testing.T) {
	engine := newFakeTreeMeta()
	ino := Ino(RootInode)
	for i := 1; i <= maxRecoverDepth+5; i++ {
		next := Ino(i + 1)
		engine.mkdir(ino, fmt.Sprintf("d%d", i), next)
		engine.attrs[next].Parent = ino
		ino = next
	}
	r := newPathResolver(engine, NewInodePathCache(0))

	deep := Ino(maxRecoverDepth + 6)
	p, resolved, gone := r.Resolve(context.Background(), deep)
	assert.False(t, resolved, "beyond maxRecoverDepth must be unrecoverable")
	assert.False(t, gone)
	assert.Equal(t, "", p)
}

func TestRecovery_MissingInodeIsGone(t *testing.T) {
	engine := newFakeTreeMeta()
	r := newPathResolver(engine, NewInodePathCache(0))

	// Leaf ENOENT (I7): the engine says the inode does not exist.
	_, resolved, gone := r.Resolve(context.Background(), 999)
	assert.False(t, resolved)
	assert.True(t, gone)

	// Mid-chain ENOENT is gone as well: the path cannot exist.
	engine.mkdir(RootInode, "a", 10).
		mkdir(10, "b", 11).
		create(11, "f", 12)
	engine.failGetAttr(10, syscall.ENOENT)
	_, resolved, gone = r.Resolve(context.Background(), 12)
	assert.False(t, resolved)
	assert.True(t, gone)
}

func TestRecovery_EngineErrorFailsClosed(t *testing.T) {
	engine := newFakeTreeMeta().
		mkdir(RootInode, "a", 10).
		create(10, "f", 11)
	r := newPathResolver(engine, NewInodePathCache(0))

	t.Run("getattr error", func(t *testing.T) {
		engine.failGetAttr(10, syscall.EIO)
		_, resolved, gone := r.Resolve(context.Background(), 11)
		assert.False(t, resolved)
		assert.False(t, gone, "engine error is uncertainty, not ENOENT")
	})

	t.Run("readdir error", func(t *testing.T) {
		engine2 := newFakeTreeMeta().
			mkdir(RootInode, "a", 10).
			create(10, "f", 11)
		engine2.failReaddir(10, syscall.EIO)
		r2 := newPathResolver(engine2, NewInodePathCache(0))
		_, resolved, gone := r2.Resolve(context.Background(), 11)
		assert.False(t, resolved)
		assert.False(t, gone)
	})
}

func TestRecovery_DanglingEntry(t *testing.T) {
	// attr.Parent says 10, but 10's listing has no entry for the inode: the
	// node is deleted — gone, so the client gets an honest ENOENT.
	engine := newFakeTreeMeta().
		mkdir(RootInode, "a", 10)
	engine.attrs[11] = &Attr{Typ: TypeFile, Parent: 10} // not in 10's listing
	r := newPathResolver(engine, NewInodePathCache(0))

	_, resolved, gone := r.Resolve(context.Background(), 11)
	assert.False(t, resolved)
	assert.True(t, gone)
}

func TestRecovery_ConcurrentMissesSingleWalk(t *testing.T) {
	engine := newFakeTreeMeta().
		mkdir(RootInode, "a", 10).
		mkdir(10, "b", 11).
		create(11, "f", 12)
	cache := NewInodePathCache(0)
	r := newPathResolver(engine, cache)

	// Hold the leader's walk in flight until every goroutine is in Resolve:
	// then all misses must share one reconstruction (design I5). The gate
	// makes the singleflight property deterministic instead of timing luck.
	gate := make(chan struct{})
	engine.gate = gate
	engine.gatedIno = 12

	const n = 16
	var wg sync.WaitGroup
	results := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, resolved, _ := r.Resolve(context.Background(), 12)
			if resolved {
				results[i] = p
			}
		}(i)
	}
	// The leader's GetAttr(12) starts and blocks inside the gate.
	require.Eventually(t, func() bool {
		return engine.getAttrCalls(12) == 1
	}, 5*time.Second, time.Millisecond)
	close(gate)
	wg.Wait()

	for i, p := range results {
		require.Equal(t, "/a/b/f", p, "goroutine %d must see the recovered path", i)
	}
	assert.Equal(t, int32(1), engine.getAttrCalls(12), "singleflight must collapse concurrent misses into one walk")
	assert.Equal(t, int32(1), engine.readdirCalls(11))
	assert.Equal(t, int32(1), engine.getAttrCalls(11))
}

func TestRecovery_Invalidate(t *testing.T) {
	engine := newFakeTreeMeta().
		mkdir(RootInode, "a", 10).
		create(10, "f", 11)
	cache := NewInodePathCache(0)
	cache.Set(11, "/a/f")
	r := newPathResolver(engine, cache)

	p, resolved, _ := r.Resolve(context.Background(), 11)
	require.True(t, resolved)
	require.Equal(t, "/a/f", p)

	r.Invalidate(11)
	assert.Equal(t, "", cache.Get(11), "Invalidate must drop the mapping")

	// Recovery rebuilds it from the engine.
	p, resolved, _ = r.Resolve(context.Background(), 11)
	require.True(t, resolved)
	assert.Equal(t, "/a/f", p)

	// Root is pinned: Invalidate never drops it.
	r.Invalidate(RootInode)
	assert.Equal(t, "/", cache.Get(RootInode))
}

func TestRecovery_PopulatesAncestorsFirstSeenWins(t *testing.T) {
	engine := newFakeTreeMeta().
		mkdir(RootInode, "a", 10).
		create(10, "f", 11)
	cache := NewInodePathCache(0)
	// 10 already cached under a different path (hardlink first-seen): the
	// warm-up must not overwrite it.
	cache.Set(10, "/elsewhere/")
	r := newPathResolver(engine, cache)

	p, resolved, _ := r.Resolve(context.Background(), 11)
	require.True(t, resolved)
	assert.Equal(t, "/a/f", p)
	assert.Equal(t, "/elsewhere/", cache.Get(10), "first-seen-wins: existing mapping stays")
}
