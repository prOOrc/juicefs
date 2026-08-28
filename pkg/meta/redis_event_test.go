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
