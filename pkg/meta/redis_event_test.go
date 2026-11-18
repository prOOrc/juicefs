//go:build !noredis
// +build !noredis

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
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestRedisMetaWithOutbox(t *testing.T, db int, stream string) *redisMeta {
	t.Helper()
	conf := testConfig()
	conf.Outbox.Enabled = true
	conf.Outbox.StreamName = stream
	conf.Outbox.ConsumerGroup = "testgroup"
	metaClient, err := newRedisMeta("redis", "127.0.0.1:6379/"+strconv.Itoa(db), conf)
	if err != nil {
		t.Skipf("create meta: %v (is Redis running?)", err)
	}
	m, ok := metaClient.(*redisMeta)
	if !ok {
		t.Fatalf("expected *redisMeta, got %T", metaClient)
	}
	t.Cleanup(func() { m.Shutdown() })
	if err := m.Reset(); err != nil {
		t.Fatalf("reset meta: %v", err)
	}
	if err := m.Init(testFormat(), true); err != nil {
		t.Fatalf("init meta: %v", err)
	}
	// The stream key is not covered by Reset (no prefix) — clear leftovers from previous runs
	if err := m.rdb.Del(context.Background(), stream).Err(); err != nil {
		t.Fatalf("del stream: %v", err)
	}
	return m
}

func readOutboxEvents(t *testing.T, m *redisMeta, stream string) []*JuiceFsEvent {
	t.Helper()
	msgs, err := m.rdb.XRange(context.Background(), stream, "-", "+").Result()
	if err != nil {
		t.Fatalf("xrange %s: %v", stream, err)
	}
	events := make([]*JuiceFsEvent, 0, len(msgs))
	for _, msg := range msgs {
		data, ok := msg.Values["data"].(string)
		if !ok {
			t.Fatalf("message %s missing data field", msg.ID)
		}
		var ev JuiceFsEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("unmarshal event from message %s: %v", msg.ID, err)
		}
		events = append(events, &ev)
	}
	return events
}

func findOutboxEvent(events []*JuiceFsEvent, typ EventType, inode Ino) *JuiceFsEvent {
	for _, ev := range events {
		if ev.Type == typ && ev.Inode == inode {
			return ev
		}
	}
	return nil
}

func TestRedisRenamePublishesDirMoved(t *testing.T) {
	m := newTestRedisMetaWithOutbox(t, 14, "test:outbox:rename")
	ctx := Background()

	var srcDir, dstDir Ino
	if st := m.Mkdir(ctx, RootInode, "src", 0777, 022, 0, &srcDir, nil); st != 0 {
		t.Fatalf("mkdir src: %s", st)
	}
	if st := m.Mkdir(ctx, RootInode, "dst", 0777, 022, 0, &dstDir, nil); st != 0 {
		t.Fatalf("mkdir dst: %s", st)
	}
	var movedDir Ino
	if st := m.Mkdir(ctx, srcDir, "dir_to_move", 0777, 022, 0, &movedDir, nil); st != 0 {
		t.Fatalf("mkdir dir_to_move: %s", st)
	}

	var ino Ino
	if st := m.Rename(ctx, srcDir, "dir_to_move", dstDir, "dir_moved", 0, &ino, nil); st != 0 {
		t.Fatalf("rename dir: %s", st)
	}

	events := readOutboxEvents(t, m, "test:outbox:rename")
	ev := findOutboxEvent(events, DirMoved, movedDir)
	if ev == nil {
		t.Fatalf("expected DirMoved event for inode %d, got: %+v", movedDir, events)
	}
	if ev.Parent != dstDir {
		t.Errorf("expected parent %d, got %d", dstDir, ev.Parent)
	}
	if ev.Name != "dir_moved" {
		t.Errorf("expected name dir_moved, got %s", ev.Name)
	}
	// Full path reconstruction depends on attr.Parent of ancestors, which Redis
	// metadata does not maintain for directories created under root — assert the
	// leaf component only (same limitation as FileMoved).
	if !strings.HasSuffix(ev.Path, "dir_moved") {
		t.Errorf("expected path ending with dir_moved, got %s", ev.Path)
	}
	if !strings.HasSuffix(ev.OldPath, "dir_to_move") {
		t.Errorf("expected old_path ending with dir_to_move, got %s", ev.OldPath)
	}
	// A directory rename must not emit FileMoved
	if f := findOutboxEvent(events, FileMoved, movedDir); f != nil {
		t.Errorf("unexpected FileMoved event for directory inode %d: %+v", movedDir, f)
	}
}

func TestRedisRenamePublishesFileMoved(t *testing.T) {
	m := newTestRedisMetaWithOutbox(t, 14, "test:outbox:rename")
	ctx := Background()

	var srcDir, dstDir Ino
	if st := m.Mkdir(ctx, RootInode, "src", 0777, 022, 0, &srcDir, nil); st != 0 {
		t.Fatalf("mkdir src: %s", st)
	}
	if st := m.Mkdir(ctx, RootInode, "dst", 0777, 022, 0, &dstDir, nil); st != 0 {
		t.Fatalf("mkdir dst: %s", st)
	}
	var file Ino
	if st := m.Mknod(ctx, srcDir, "file_to_move", TypeFile, 0644, 022, 0, "", &file, nil); st != 0 {
		t.Fatalf("mknod file_to_move: %s", st)
	}

	var ino Ino
	if st := m.Rename(ctx, srcDir, "file_to_move", dstDir, "file_moved", 0, &ino, nil); st != 0 {
		t.Fatalf("rename file: %s", st)
	}

	events := readOutboxEvents(t, m, "test:outbox:rename")
	ev := findOutboxEvent(events, FileMoved, file)
	if ev == nil {
		t.Fatalf("expected FileMoved event for inode %d, got: %+v", file, events)
	}
	if ev.Parent != dstDir {
		t.Errorf("expected parent %d, got %d", dstDir, ev.Parent)
	}
	if ev.Name != "file_moved" {
		t.Errorf("expected name file_moved, got %s", ev.Name)
	}
	// A file rename must not emit DirMoved
	if d := findOutboxEvent(events, DirMoved, file); d != nil {
		t.Errorf("unexpected DirMoved event for file inode %d: %+v", file, d)
	}
}

func TestRedisRenameTopLevelDirExactPaths(t *testing.T) {
	m := newTestRedisMetaWithOutbox(t, 14, "test:outbox:rename")
	ctx := Background()

	var topDir Ino
	if st := m.Mkdir(ctx, RootInode, "topdir", 0777, 022, 0, &topDir, nil); st != 0 {
		t.Fatalf("mkdir topdir: %s", st)
	}

	var ino Ino
	if st := m.Rename(ctx, RootInode, "topdir", RootInode, "renamed_topdir", 0, &ino, nil); st != 0 {
		t.Fatalf("rename topdir: %s", st)
	}

	events := readOutboxEvents(t, m, "test:outbox:rename")
	ev := findOutboxEvent(events, DirMoved, topDir)
	if ev == nil {
		t.Fatalf("expected DirMoved event for inode %d, got: %+v", topDir, events)
	}
	if ev.Path != "/renamed_topdir" {
		t.Errorf("expected path /renamed_topdir, got %s", ev.Path)
	}
	if ev.OldPath != "/topdir" {
		t.Errorf("expected old_path /topdir, got %s", ev.OldPath)
	}
}

func TestBatchUnlinkPublishesFileDeleted(t *testing.T) {
	m := newTestRedisMetaWithOutbox(t, 14, "test:outbox:batchunlink")
	ctx := Background()

	var parent Ino
	if st := m.Mkdir(ctx, RootInode, "batchdir", 0777, 022, 0, &parent, nil); st != 0 {
		t.Fatalf("mkdir batchdir: %s", st)
	}
	var f1, f2 Ino
	if st := m.Mknod(ctx, parent, "file1", TypeFile, 0644, 022, 0, "", &f1, nil); st != 0 {
		t.Fatalf("mknod file1: %s", st)
	}
	if st := m.Mknod(ctx, parent, "file2", TypeFile, 0644, 022, 0, "", &f2, nil); st != 0 {
		t.Fatalf("mknod file2: %s", st)
	}
	var dir Ino
	if st := m.Mkdir(ctx, parent, "subdir", 0777, 022, 0, &dir, nil); st != 0 {
		t.Fatalf("mkdir subdir: %s", st)
	}

	var entries []*Entry
	for _, e := range []struct {
		ino  Ino
		name string
	}{{f1, "file1"}, {f2, "file2"}, {dir, "subdir"}} {
		var attr Attr
		if st := m.GetAttr(ctx, e.ino, &attr); st != 0 {
			t.Fatalf("getattr %s: %s", e.name, st)
		}
		entries = append(entries, &Entry{Inode: e.ino, Name: []byte(e.name), Attr: &attr})
	}

	if st := m.BatchUnlink(ctx, parent, entries, nil, false); st != 0 {
		t.Fatalf("batch unlink: %s", st)
	}

	events := readOutboxEvents(t, m, "test:outbox:batchunlink")
	del1 := findOutboxEvent(events, FileDeleted, f1)
	del2 := findOutboxEvent(events, FileDeleted, f2)
	if del1 == nil || del2 == nil {
		t.Fatalf("expected FileDeleted events for inodes %d and %d, got: %+v", f1, f2, events)
	}
	if del1.Name != "file1" {
		t.Fatalf("expected name file1, got %s", del1.Name)
	}
	if del2.Name != "file2" {
		t.Fatalf("expected name file2, got %s", del2.Name)
	}
	// The directory entry must not produce any deletion event
	if d := findOutboxEvent(events, FileDeleted, dir); d != nil {
		t.Fatalf("unexpected FileDeleted for directory inode %d: %+v", dir, d)
	}
	if d := findOutboxEvent(events, DirDeleted, dir); d != nil {
		t.Fatalf("unexpected DirDeleted for directory inode %d: %+v", dir, d)
	}
	// mtime is filled from the pre-deletion attr
	for _, ev := range []*JuiceFsEvent{del1, del2} {
		if ev.Mtime <= 0 {
			t.Fatalf("expected positive mtime on FileDeleted for inode %d, got %d", ev.Inode, ev.Mtime)
		}
	}
}

func TestEventUUIDGenerated(t *testing.T) {
	m := newTestRedisMetaWithOutbox(t, 14, "test:outbox:uuid")
	ctx := Background()

	var ino1, ino2 Ino
	if st := m.Mknod(ctx, RootInode, "f1", TypeFile, 0644, 022, 0, "", &ino1, nil); st != 0 {
		t.Fatalf("mknod f1: %s", st)
	}
	if st := m.Mknod(ctx, RootInode, "f2", TypeFile, 0644, 022, 0, "", &ino2, nil); st != 0 {
		t.Fatalf("mknod f2: %s", st)
	}

	events := readOutboxEvents(t, m, "test:outbox:uuid")
	ev1 := findOutboxEvent(events, FileCreated, ino1)
	if ev1 == nil {
		t.Fatalf("expected FileCreated event for inode %d, got: %+v", ino1, events)
	}
	if ev1.Uuid == "" {
		t.Fatalf("expected non-empty uuid on FileCreated event for inode %d", ino1)
	}
	ev2 := findOutboxEvent(events, FileCreated, ino2)
	if ev2 == nil {
		t.Fatalf("expected FileCreated event for inode %d, got: %+v", ino2, events)
	}
	if ev2.Uuid == "" {
		t.Fatalf("expected non-empty uuid on FileCreated event for inode %d", ino2)
	}
	if ev1.Uuid == ev2.Uuid {
		t.Fatalf("expected distinct uuids across operations, got %s twice", ev1.Uuid)
	}
}

func TestWritePublishesMtime(t *testing.T) {
	m := newTestRedisMetaWithOutbox(t, 14, "test:outbox:mtime")
	ctx := Background()

	var ino Ino
	if st := m.Mknod(ctx, RootInode, "file", TypeFile, 0644, 022, 0, "", &ino, nil); st != 0 {
		t.Fatalf("mknod: %s", st)
	}

	if st := m.Write(ctx, ino, 0, 0, Slice{Id: 1, Size: 4, Len: 4}, time.Now()); st != 0 {
		t.Fatalf("write: %s", st)
	}

	events := readOutboxEvents(t, m, "test:outbox:mtime")
	ev := findOutboxEvent(events, FileWritten, ino)
	if ev == nil {
		t.Fatalf("expected FileWritten event for inode %d, got: %+v", ino, events)
	}
	if ev.Mtime <= 0 {
		t.Fatalf("expected positive mtime on FileWritten, got %d", ev.Mtime)
	}
	window := 5 * time.Second
	lower := time.Now().Add(-window).UnixNano()
	upper := time.Now().Add(window).UnixNano()
	if ev.Mtime < lower || ev.Mtime > upper {
		t.Fatalf("mtime %d outside reasonable window [%d, %d]", ev.Mtime, lower, upper)
	}
	// Cross-check against the inode attr after the write
	var attr Attr
	if st := m.GetAttr(ctx, ino, &attr); st != 0 {
		t.Fatalf("getattr: %s", st)
	}
	if want := attr.Mtime*1_000_000_000 + int64(attr.Mtimensec); ev.Mtime != want {
		t.Fatalf("expected mtime %d (from attr), got %d", want, ev.Mtime)
	}
}

func TestRenamePublishesS3MetaSnapshot(t *testing.T) {
	m := newTestRedisMetaWithOutbox(t, 14, "test:outbox:s3meta")
	ctx := Background()
	metaJSON := []byte(`{"x-amz-meta-x-agio-sync":"task-1"}`)

	// Plain rename of a file with s3-meta: event carries the snapshot
	var ino1 Ino
	if st := m.Mknod(ctx, RootInode, "obj1", TypeFile, 0644, 022, 0, "", &ino1, nil); st != 0 {
		t.Fatalf("mknod obj1: %s", st)
	}
	if st := m.SetXattr(ctx, ino1, s3MetaXattr, metaJSON, 0); st != 0 {
		t.Fatalf("setxattr s3-meta: %s", st)
	}
	if st := m.Rename(ctx, RootInode, "obj1", RootInode, "final1", 0, nil, nil); st != 0 {
		t.Fatalf("rename obj1: %s", st)
	}

	events := readOutboxEvents(t, m, "test:outbox:s3meta")
	ev1 := findOutboxEvent(events, FileMoved, ino1)
	if ev1 == nil {
		t.Fatalf("expected FileMoved event for inode %d, got: %+v", ino1, events)
	}
	if got := ev1.Xattrs[s3MetaXattr]; got != string(metaJSON) {
		t.Fatalf("expected xattrs[%s] %s, got %q (xattrs: %v)", s3MetaXattr, metaJSON, got, ev1.Xattrs)
	}

	// Gateway PUT pattern: rename from /.sys/tmp/... transforms into
	// FileCreated via FilterEvent with the snapshot preserved
	var tmpDir Ino
	if st := m.Mkdir(ctx, RootInode, ".sys", 0777, 022, 0, &tmpDir, nil); st != 0 {
		t.Fatalf("mkdir .sys: %s", st)
	}
	var d09 Ino
	if st := m.Mkdir(ctx, tmpDir, "tmp", 0777, 022, 0, &d09, nil); st != 0 {
		t.Fatalf("mkdir .sys/tmp: %s", st)
	}
	var ino2 Ino
	if st := m.Mknod(ctx, d09, "tmp-obj", TypeFile, 0644, 022, 0, "", &ino2, nil); st != 0 {
		t.Fatalf("mknod tmp-obj: %s", st)
	}
	if st := m.SetXattr(ctx, ino2, s3MetaXattr, metaJSON, 0); st != 0 {
		t.Fatalf("setxattr s3-meta on tmp-obj: %s", st)
	}
	if st := m.Rename(ctx, d09, "tmp-obj", RootInode, "final2", 0, nil, nil); st != 0 {
		t.Fatalf("rename tmp-obj: %s", st)
	}

	events = readOutboxEvents(t, m, "test:outbox:s3meta")
	ev2 := findOutboxEvent(events, FileMoved, ino2)
	if ev2 == nil {
		t.Fatalf("expected FileMoved event for inode %d, got: %+v", ino2, events)
	}
	if !strings.HasPrefix(ev2.OldPath, "/.sys/tmp/") {
		t.Fatalf("expected old_path under /.sys/tmp/, got %s", ev2.OldPath)
	}
	transformed := FilterEvent(ev2)
	if transformed == nil {
		t.Fatal("expected transformed event, got nil")
	}
	if transformed.Type != FileCreated {
		t.Fatalf("expected FileCreated, got %s", transformed.Type)
	}
	if transformed.OldPath != "" {
		t.Fatalf("expected empty old_path after transform, got %s", transformed.OldPath)
	}
	if got := transformed.Xattrs[s3MetaXattr]; got != string(metaJSON) {
		t.Fatalf("transformed event lost xattrs[%s]: expected %s, got %q", s3MetaXattr, metaJSON, got)
	}
}

func TestSetXattrEmitsNoEvent(t *testing.T) {
	m := newTestRedisMetaWithOutbox(t, 14, "test:outbox:setxattr")
	ctx := Background()

	var ino Ino
	if st := m.Mknod(ctx, RootInode, "file", TypeFile, 0644, 022, 0, "", &ino, nil); st != 0 {
		t.Fatalf("mknod: %s", st)
	}

	before := len(readOutboxEvents(t, m, "test:outbox:setxattr"))

	if st := m.SetXattr(ctx, ino, s3MetaXattr, []byte(`{"x-amz-meta-x-agio-sync":"task-1"}`), 0); st != 0 {
		t.Fatalf("setxattr s3-meta: %s", st)
	}
	if st := m.RemoveXattr(ctx, ino, s3MetaXattr); st != 0 {
		t.Fatalf("removexattr s3-meta: %s", st)
	}

	after := len(readOutboxEvents(t, m, "test:outbox:setxattr"))
	if after != before {
		t.Fatalf("expected no new events from xattr ops, before=%d after=%d", before, after)
	}
}

func TestEventJSONBackwardCompatibility(t *testing.T) {
	// (a) A JuiceFsEvent without extension fields marshals/unmarshals with zero
	// extension fields
	legacy := JuiceFsEvent{
		Type:      FileMoved,
		Timestamp: time.Unix(1767225600, 0).UTC(),
		Volume:    "vol",
		Uid:       1000,
		Gid:       1000,
		Inode:     42,
		Parent:    1,
		Name:      "n",
	}
	data, err := json.Marshal(&legacy)
	if err != nil {
		t.Fatalf("marshal legacy event: %v", err)
	}
	var ev JuiceFsEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal legacy event: %v", err)
	}
	if ev.Type != legacy.Type || ev.Inode != legacy.Inode || ev.Volume != legacy.Volume {
		t.Fatalf("unexpected event after roundtrip: %+v", ev)
	}
	if ev.Uuid != "" || ev.Mtime != 0 || ev.Xattrs != nil {
		t.Fatalf("expected zero extension fields, got uuid=%q mtime=%d xattrs=%v", ev.Uuid, ev.Mtime, ev.Xattrs)
	}
	// Extension fields must be omitted from JSON when zero
	for _, field := range []string{`"uuid"`, `"mtime"`, `"xattrs"`} {
		if strings.Contains(string(data), field) {
			t.Fatalf("expected %s omitted in zero-valued event JSON: %s", field, data)
		}
	}

	// (b) JSON produced by an old producer (no extension fields) unmarshals
	// into the extended struct without error
	oldEvent := struct {
		Type      EventType `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Volume    string    `json:"volume"`
		Uid       uint32    `json:"uid"`
		Gid       uint32    `json:"gid"`
		Inode     Ino       `json:"inode"`
		Parent    Ino       `json:"parent"`
		Name      string    `json:"name"`
	}{
		Type:      FileCreated,
		Timestamp: legacy.Timestamp,
		Volume:    "vol",
		Uid:       1,
		Gid:       1,
		Inode:     7,
		Parent:    1,
		Name:      "f",
	}
	oldData, err := json.Marshal(&oldEvent)
	if err != nil {
		t.Fatalf("marshal old-struct event: %v", err)
	}
	var ev2 JuiceFsEvent
	if err := json.Unmarshal(oldData, &ev2); err != nil {
		t.Fatalf("unmarshal old-struct event: %v", err)
	}
	if ev2.Type != oldEvent.Type || ev2.Inode != oldEvent.Inode || ev2.Name != oldEvent.Name || ev2.Uid != oldEvent.Uid {
		t.Fatalf("unexpected event from old JSON: %+v", ev2)
	}
	if ev2.Uuid != "" || ev2.Mtime != 0 || ev2.Xattrs != nil {
		t.Fatalf("expected zero extension fields, got uuid=%q mtime=%d xattrs=%v", ev2.Uuid, ev2.Mtime, ev2.Xattrs)
	}

	// (c) Extended JSON round-trips through the extended struct
	full := JuiceFsEvent{
		Type:      FileWritten,
		Timestamp: legacy.Timestamp,
		Volume:    "vol",
		Inode:     9,
		Parent:    1,
		Name:      "w",
		Size:      8,
		Uuid:      "0198a0b4-6d5e-7cc3-9f21-3b6d1e8a2c77",
		Mtime:     legacy.Timestamp.UnixNano(),
		Xattrs:    map[string]string{s3MetaXattr: `{"x-amz-meta-x-agio-sync":"task-1"}`},
	}
	fullData, err := json.Marshal(&full)
	if err != nil {
		t.Fatalf("marshal full event: %v", err)
	}
	var ev3 JuiceFsEvent
	if err := json.Unmarshal(fullData, &ev3); err != nil {
		t.Fatalf("unmarshal full event: %v", err)
	}
	if ev3.Uuid != full.Uuid || ev3.Mtime != full.Mtime || ev3.Size != full.Size {
		t.Fatalf("extension fields lost after roundtrip: %+v", ev3)
	}
	if got := ev3.Xattrs[s3MetaXattr]; got != full.Xattrs[s3MetaXattr] {
		t.Fatalf("expected xattrs[%s] %s, got %q", s3MetaXattr, full.Xattrs[s3MetaXattr], got)
	}
}
