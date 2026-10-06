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

package fuse

import (
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/vfs"
)

func newOwnerOverrideFS(t *testing.T) *fileSystem {
	t.Helper()
	metaConf := meta.DefaultConf()
	metaConf.MountPoint = t.TempDir()
	m := meta.NewClient("memkv://", metaConf)
	format := &meta.Format{
		Name:        "test",
		UUID:        uuid.New().String(),
		Storage:     "mem",
		BlockSize:   4096,
		Compression: "lz4",
		DirStats:    true,
	}
	if err := m.Init(format, true); err != nil {
		t.Fatalf("init meta: %s", err)
	}
	chunkConf := chunk.Config{
		BlockSize:   format.BlockSize * 1024,
		Compress:    format.Compression,
		MaxUpload:   2,
		MaxDownload: 200,
		BufferSize:  30 << 20,
		CacheSize:   10 << 20,
		CacheDir:    "memory",
	}
	blob, err := object.CreateStorage("mem", "", "", "", "")
	if err != nil {
		t.Fatalf("create storage: %s", err)
	}
	registry := prometheus.NewRegistry()
	store := chunk.NewCachedStore(blob, chunkConf, registry)
	conf := &vfs.Config{
		Meta:            metaConf,
		Format:          *format,
		Chunk:           &chunkConf,
		FuseOpts:        &vfs.FuseOptions{},
		AttrTimeout:     time.Second,
		EntryTimeout:    time.Second,
		DirEntryTimeout: time.Second,
		OwnerOverride:   &vfs.AnonymousAccount{Uid: 2000, Gid: 2000},
	}
	v := vfs.NewVFS(conf, m, store, prometheus.WrapRegistererWithPrefix("juicefs_", registry), registry)
	return &fileSystem{RawFileSystem: fuse.NewDefaultRawFileSystem(), conf: conf, v: v}
}

func TestOwnerOverrideLookupConsistency(t *testing.T) {
	fs := newOwnerOverrideFS(t)
	// gids must be non-empty: meta wrapContext.Gid() reads gids[0]
	ctx := vfs.NewLogContext(meta.NewContext(10, 10, []uint32{10}))
	de, e := fs.v.Mkdir(ctx, 1, "d1", 0755, 0)
	if e != 0 {
		t.Fatalf("mkdir d1: %s", e)
	}
	f1, e := fs.v.Mknod(ctx, de.Inode, "f1", 0644|syscall.S_IFREG, 0, 0)
	if e != 0 {
		t.Fatalf("mknod d1/f1: %s", e)
	}

	header := &fuse.InHeader{NodeId: 1}
	header.Uid = 5000
	header.Gid = 5000
	header.Pid = 1

	// lookup каталога
	var entryOut fuse.EntryOut
	if st := fs.Lookup(nil, header, "d1", &entryOut); st != fuse.OK {
		t.Fatalf("lookup d1: %v", st)
	}
	if entryOut.Attr.Uid != 2000 || entryOut.Attr.Gid != 2000 {
		t.Fatalf("lookup d1 presents %d/%d", entryOut.Attr.Uid, entryOut.Attr.Gid)
	}

	// getattr того же inode
	var attrOut fuse.AttrOut
	attrIn := &fuse.GetAttrIn{}
	attrIn.InHeader.NodeId = uint64(de.Inode)
	attrIn.InHeader.Uid = 5000
	attrIn.InHeader.Gid = 5000
	attrIn.InHeader.Pid = 1
	if st := fs.GetAttr(nil, attrIn, &attrOut); st != fuse.OK {
		t.Fatalf("getattr d1: %v", st)
	}
	if attrOut.Attr.Uid != 2000 || attrOut.Attr.Gid != 2000 {
		t.Fatalf("getattr d1 presents %d/%d", attrOut.Attr.Uid, attrOut.Attr.Gid)
	}

	// согласованность Lookup vs GetAttr
	if entryOut.Attr.Uid != attrOut.Attr.Uid || entryOut.Attr.Gid != attrOut.Attr.Gid {
		t.Fatalf("lookup %d/%d != getattr %d/%d", entryOut.Attr.Uid, entryOut.Attr.Gid, attrOut.Attr.Uid, attrOut.Attr.Gid)
	}

	// readdir-plus: атрибуты листинга согласованы
	openIn := &fuse.OpenIn{}
	openIn.InHeader.NodeId = uint64(de.Inode)
	openIn.InHeader.Uid = 5000
	openIn.InHeader.Gid = 5000
	var openOut fuse.OpenOut
	if st := fs.OpenDir(nil, openIn, &openOut); st != fuse.OK {
		t.Fatalf("opendir d1: %v", st)
	}
	buf := make([]byte, 64<<10)
	readIn := &fuse.ReadIn{}
	readIn.InHeader.NodeId = uint64(de.Inode)
	readIn.InHeader.Uid = 5000
	readIn.InHeader.Gid = 5000
	readIn.InHeader.Pid = 1
	readIn.Fh = openOut.Fh
	if st := fs.ReadDirPlus(nil, readIn, fuse.NewDirEntryList(buf, 0)); st != fuse.OK {
		t.Fatalf("readdirplus d1: %v", st)
	}
	eo := findDirEntry(buf, "f1")
	if eo == nil {
		t.Fatal("readdirplus did not return \"f1\"")
	}
	if eo.Attr.Uid != 2000 || eo.Attr.Gid != 2000 {
		t.Fatalf("readdirplus entry f1 presents %d/%d", eo.Attr.Uid, eo.Attr.Gid)
	}
	if eo.Attr.Uid != attrOut.Attr.Uid || eo.Attr.Gid != attrOut.Attr.Gid {
		t.Fatal("readdirplus attrs are not consistent with lookup/getattr")
	}

	// regression for the ModifiedSince refresh branch: a write to f1 after
	// Opendir marks the inode as modified, so the next ReadDirPlus (whose
	// ctx.start is the dir handle's readAt taken before the write) re-reads
	// attr from meta — the presented ownership must still be overridden.
	if _, fh, e := fs.v.Open(ctx, f1.Inode, syscall.O_RDWR); e != 0 {
		t.Fatalf("open f1: %s", e)
	} else if e = fs.v.Write(ctx, f1.Inode, []byte("x"), 0, fh); e != 0 {
		t.Fatalf("write f1: %s", e)
	}
	readIn.Offset = 2 // skip "." and "..": reuses the dir handle, so ctx.start stays before the write
	buf2 := make([]byte, 64<<10)
	if st := fs.ReadDirPlus(nil, readIn, fuse.NewDirEntryList(buf2, 0)); st != fuse.OK {
		t.Fatalf("readdirplus d1 after write: %v", st)
	}
	eo2 := findDirEntry(buf2, "f1")
	if eo2 == nil {
		t.Fatal("readdirplus after write did not return \"f1\"")
	}
	if eo2.Attr.Uid != 2000 || eo2.Attr.Gid != 2000 {
		t.Fatalf("readdirplus entry f1 after write presents %d/%d", eo2.Attr.Uid, eo2.Attr.Gid)
	}
}

// findDirEntry parses a raw readdirplus buffer (layout of one record:
// EntryOut, _Dirent{Ino, Off uint64; NameLen, Typ uint32}, name padded to 8;
// dirHandler issues "." and ".." before real entries) and returns the
// EntryOut of the named entry, or nil when it is absent.
func findDirEntry(buf []byte, want string) *fuse.EntryOut {
	entryOutSize := int(unsafe.Sizeof(fuse.EntryOut{}))
	type direntHeader struct {
		Ino     uint64
		Off     uint64
		NameLen uint32
		Typ     uint32
	}
	for off := 0; off+entryOutSize+24 <= len(buf); {
		e := (*fuse.EntryOut)(unsafe.Pointer(&buf[off]))
		dh := (*direntHeader)(unsafe.Pointer(&buf[off+entryOutSize]))
		name := string(buf[off+entryOutSize+24 : off+entryOutSize+24+int(dh.NameLen)])
		if name == want {
			return e
		}
		off += entryOutSize + 24 + int(dh.NameLen) + (8-int(dh.NameLen)&7)&7
	}
	return nil
}
