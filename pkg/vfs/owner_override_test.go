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

package vfs

import (
	"syscall"
	"testing"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/utils"
)

func TestGetAttrOwnerOverride(t *testing.T) {
	v, _ := createTestVFS(nil, "")
	// gids must be non-empty: meta wrapContext.Gid() reads gids[0]
	ctx := NewLogContext(meta.NewContext(10, 10, []uint32{10}))
	de, e := v.Mkdir(ctx, 1, "d1", 0755, 0)
	if e != 0 {
		t.Fatalf("mkdir d1: %s", e)
	}
	// on darwin children inherit the parent's gid (BSD semantics): fix up the
	// directory group (allowed for the owner) so expectations stay portable
	if _, e = v.SetAttr(ctx, de.Inode, meta.SetAttrGID, 0, 0, 0, 10, 0, 0, 0, 0, 0); e != 0 {
		t.Fatalf("setattr d1 gid: %s", e)
	}
	fe, e := v.Mknod(ctx, de.Inode, "f1", 0644|syscall.S_IFREG, 0, 0)
	if e != 0 {
		t.Fatalf("mknod d1/f1: %s", e)
	}

	// без флага — upstream-поведение: предъявляются stored uid/gid
	entry, e := v.GetAttr(ctx, fe.Inode, 0)
	if e != 0 || entry.Attr.Uid != 10 || entry.Attr.Gid != 10 {
		t.Fatalf("getattr f1 without flag: %s %d/%d", e, entry.Attr.Uid, entry.Attr.Gid)
	}
	entry, e = v.Lookup(ctx, 1, "d1")
	if e != 0 || entry.Attr.Uid != 10 || entry.Attr.Gid != 10 {
		t.Fatalf("lookup d1 without flag: %s %d/%d", e, entry.Attr.Uid, entry.Attr.Gid)
	}

	v.Conf.OwnerOverride = &AnonymousAccount{Uid: 2000, Gid: 2000}
	defer func() { v.Conf.OwnerOverride = nil }()

	// с флагом — файл, каталог и спец-узел .stats предъявляются 2000:2000
	for _, ino := range []Ino{fe.Inode, de.Inode, StatsInode} {
		entry, e = v.GetAttr(ctx, ino, 0)
		if e != 0 || entry.Attr.Uid != 2000 || entry.Attr.Gid != 2000 {
			t.Fatalf("getattr ino %d with flag: %s %d/%d", ino, e, entry.Attr.Uid, entry.Attr.Gid)
		}
	}
	entry, e = v.Lookup(ctx, de.Inode, "f1")
	if e != 0 || entry.Attr.Uid != 2000 || entry.Attr.Gid != 2000 {
		t.Fatalf("lookup f1 with flag: %s %d/%d", e, entry.Attr.Uid, entry.Attr.Gid)
	}
	entry, e = v.Lookup(ctx, 1, ".stats")
	if e != 0 || entry.Attr.Uid != 2000 || entry.Attr.Gid != 2000 {
		t.Fatalf("lookup .stats with flag: %s %d/%d", e, entry.Attr.Uid, entry.Attr.Gid)
	}

	// общий attr спец-узла не мутирован: остаётся uid/gid процесса
	n := getInternalNode(StatsInode)
	if n.attr.Uid != uint32(utils.GetCurrentUID()) || n.attr.Gid != uint32(utils.GetCurrentGID()) {
		t.Fatalf("internal node attr mutated: %d/%d", n.attr.Uid, n.attr.Gid)
	}

	// stored-атрибуты в meta неизменны
	var stored meta.Attr
	if e = v.Meta.GetAttr(ctx, fe.Inode, &stored); e != 0 || stored.Uid != 10 || stored.Gid != 10 {
		t.Fatalf("stored attrs changed: %s %d/%d", e, stored.Uid, stored.Gid)
	}
	if e = v.Meta.GetAttr(ctx, de.Inode, &stored); e != 0 || stored.Uid != 10 || stored.Gid != 10 {
		t.Fatalf("stored dir attrs changed: %s %d/%d", e, stored.Uid, stored.Gid)
	}

	// readdir: атрибуты записей каталога переопределены
	fh, e := v.Opendir(ctx, de.Inode, 0)
	if e != 0 {
		t.Fatalf("opendir d1: %s", e)
	}
	defer v.Releasedir(ctx, de.Inode, fh)
	entries, _, e := v.Readdir(ctx, de.Inode, 1024, 0, fh, true)
	if e != 0 {
		t.Fatalf("readdir d1: %s", e)
	}
	var found bool
	for _, en := range entries {
		if string(en.Name) == "f1" {
			found = true
			if en.Attr.Uid != 2000 || en.Attr.Gid != 2000 {
				t.Fatalf("readdir entry f1: %d/%d", en.Attr.Uid, en.Attr.Gid)
			}
		}
	}
	if !found {
		t.Fatal("readdir d1: entry f1 not found")
	}
}

func TestOwnerOverrideSquashComposition(t *testing.T) {
	v, _ := createTestVFS(nil, "")
	// gids must be non-empty: meta wrapContext.Gid() reads gids[0]
	// identity вызывающего, к которой мапит all-squash 1000:1000
	ctx := NewLogContext(meta.NewContext(1000, 1000, []uint32{1000}))
	de, e := v.Mkdir(ctx, 1, "d1", 0755, 0)
	if e != 0 {
		t.Fatalf("mkdir d1: %s", e)
	}
	// on darwin children inherit the parent's gid (BSD semantics): fix up the
	// directory group (allowed for the owner) so expectations stay portable
	if _, e = v.SetAttr(ctx, de.Inode, meta.SetAttrGID, 0, 0, 0, 1000, 0, 0, 0, 0, 0); e != 0 {
		t.Fatalf("setattr d1 gid: %s", e)
	}
	fe, e := v.Mknod(ctx, de.Inode, "f1", 0644|syscall.S_IFREG, 0, 0)
	if e != 0 {
		t.Fatalf("mknod f1: %s", e)
	}

	v.Conf.OwnerOverride = &AnonymousAccount{Uid: 2000, Gid: 2000}
	defer func() { v.Conf.OwnerOverride = nil }()

	entry, e := v.GetAttr(ctx, fe.Inode, 0)
	if e != 0 || entry.Attr.Uid != 2000 || entry.Attr.Gid != 2000 {
		t.Fatalf("getattr f1: %s %d/%d", e, entry.Attr.Uid, entry.Attr.Gid)
	}
	// операции остаются от 1000: stored-владелец = squash-identity
	var stored meta.Attr
	if e = v.Meta.GetAttr(ctx, fe.Inode, &stored); e != 0 || stored.Uid != 1000 || stored.Gid != 1000 {
		t.Fatalf("stored attrs: %s %d/%d", e, stored.Uid, stored.Gid)
	}

	// смена override меняет только предъявление, не идентичность операций
	v.Conf.OwnerOverride = &AnonymousAccount{Uid: 3000, Gid: 3000}
	entry, e = v.GetAttr(ctx, fe.Inode, 0)
	if e != 0 || entry.Attr.Uid != 3000 || entry.Attr.Gid != 3000 {
		t.Fatalf("getattr f1 after override change: %s %d/%d", e, entry.Attr.Uid, entry.Attr.Gid)
	}
	if e = v.Meta.GetAttr(ctx, fe.Inode, &stored); e != 0 || stored.Uid != 1000 || stored.Gid != 1000 {
		t.Fatalf("stored attrs after override change: %s %d/%d", e, stored.Uid, stored.Gid)
	}
}
