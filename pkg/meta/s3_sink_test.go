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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/object"
)

// mockPut is a successful object write recorded by mockStorage
type mockPut struct {
	key  string
	body []byte
}

// mockStorage is a test ObjectStorage: only Put is implemented, everything
// else is provided by the embedded nil interface (never called by the tests)
type mockStorage struct {
	object.ObjectStorage

	mu       sync.Mutex
	attempts int
	failAll  error        // non-nil → every Put fails
	failAt   map[int]bool // 1-based attempt numbers that fail once
	puts     []mockPut
}

func (m *mockStorage) Put(ctx context.Context, key string, in io.Reader, getters ...object.AttrGetter) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts++
	if m.failAll != nil {
		return m.failAll
	}
	if m.failAt[m.attempts] {
		return errors.New("mock storage put failure")
	}
	body, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	m.puts = append(m.puts, mockPut{key: key, body: body})
	return nil
}

func (m *mockStorage) setFailAll(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failAll = err
}

// setFailAt makes the n-th next Put attempt fail (attempts are counted from
// the last resetAttempts)
func (m *mockStorage) setFailAt(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failAt = map[int]bool{n: true}
}

func (m *mockStorage) resetAttempts() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts = 0
}

func (m *mockStorage) recorded() []mockPut {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mockPut(nil), m.puts...)
}

// spillChunks returns the spill chunk paths sorted by name and their lines
func spillChunks(dir string) ([]string, [][]string, error) {
	chunks, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	var all [][]string
	for _, c := range chunks {
		data, err := os.ReadFile(c)
		if err != nil {
			return nil, nil, err
		}
		var lines []string
		for _, raw := range strings.Split(string(data), "\n") {
			if line := strings.TrimSpace(raw); line != "" {
				lines = append(lines, line)
			}
		}
		all = append(all, lines)
	}
	return chunks, all, nil
}

// expectedKey mirrors S3Sink.objectKey for test assertions
func expectedKey(prefix string, ts time.Time, uuid string) string {
	utc := ts.UTC()
	return fmt.Sprintf("%s/%s/%s/%s.json", strings.Trim(prefix, "/"), utc.Format("2006-01-02"), utc.Format("15"), uuid)
}

func TestS3SinkKeyFormat(t *testing.T) {
	mock := &mockStorage{}
	sink, err := NewS3Sink(S3SinkConfig{Prefix: "events/"}, mock)
	if err != nil {
		t.Fatalf("new s3 sink: %v", err)
	}

	event := &JuiceFsEvent{
		Type:      FileCreated,
		Timestamp: time.Date(2026, 11, 19, 3, 4, 5, 0, time.UTC),
		Volume:    "test-vol",
		Inode:     42,
		Parent:    1,
		Name:      "file",
		Uuid:      "0198a0b4-6d5e-7cc3-9f21-3b6d1e8a2c77",
	}
	if err := sink.Publish(context.Background(), event); err != nil {
		t.Fatalf("publish: %v", err)
	}

	puts := mock.recorded()
	if len(puts) != 1 {
		t.Fatalf("expected 1 object, got %d", len(puts))
	}
	key := puts[0].key
	re := regexp.MustCompile(`^events/\d{4}-\d{2}-\d{2}/\d{2}/[0-9a-f-]{36}\.json$`)
	if !re.MatchString(key) {
		t.Fatalf("key %q does not match expected format", key)
	}
	if want := expectedKey("events/", event.Timestamp, event.Uuid); key != want {
		t.Fatalf("expected key %s, got %s", want, key)
	}

	var got JuiceFsEvent
	if err := json.Unmarshal(puts[0].body, &got); err != nil {
		t.Fatalf("unmarshal object body: %v", err)
	}
	if got.Type != FileCreated || got.Uuid != event.Uuid {
		t.Fatalf("unexpected body: type=%s uuid=%s", got.Type, got.Uuid)
	}
}

func TestS3SinkGeneratesUUIDWhenAbsent(t *testing.T) {
	mock := &mockStorage{}
	sink, err := NewS3Sink(S3SinkConfig{Prefix: "events/"}, mock)
	if err != nil {
		t.Fatalf("new s3 sink: %v", err)
	}

	ts := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	first := &JuiceFsEvent{Type: FileCreated, Timestamp: ts, Inode: 1, Name: "first"}
	second := &JuiceFsEvent{Type: FileCreated, Timestamp: ts, Inode: 2, Name: "second"}
	if err := sink.Publish(context.Background(), first); err != nil {
		t.Fatalf("publish first: %v", err)
	}
	if err := sink.Publish(context.Background(), second); err != nil {
		t.Fatalf("publish second: %v", err)
	}

	puts := mock.recorded()
	if len(puts) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(puts))
	}
	keyOf := func(body []byte) string {
		var ev JuiceFsEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		return ev.Uuid
	}
	firstUUID := keyOf(puts[0].body)
	secondUUID := keyOf(puts[1].body)
	if firstUUID == "" || secondUUID == "" {
		t.Fatalf("expected generated uuids, got %q and %q", firstUUID, secondUUID)
	}
	if firstUUID == secondUUID {
		t.Fatalf("expected distinct generated uuids, got %q twice", firstUUID)
	}
	if puts[0].key != expectedKey("events/", ts, firstUUID) || puts[1].key != expectedKey("events/", ts, secondUUID) {
		t.Fatalf("keys must carry the generated uuids: %s, %s", puts[0].key, puts[1].key)
	}

	// An event with a preset uuid keeps it
	fixed := &JuiceFsEvent{Type: FileCreated, Timestamp: ts, Inode: 3, Name: "third", Uuid: "11111111-2222-3333-4444-555555555555"}
	if err := sink.Publish(context.Background(), fixed); err != nil {
		t.Fatalf("publish third: %v", err)
	}
	puts = mock.recorded()
	if len(puts) != 3 {
		t.Fatalf("expected 3 objects, got %d", len(puts))
	}
	if want := expectedKey("events/", ts, fixed.Uuid); puts[2].key != want {
		t.Fatalf("expected key %s for preset uuid, got %s", want, puts[2].key)
	}
}

func TestS3SinkPutErrorReturned(t *testing.T) {
	mock := &mockStorage{}
	sink, err := NewS3Sink(S3SinkConfig{Prefix: "events/"}, mock)
	if err != nil {
		t.Fatalf("new s3 sink: %v", err)
	}
	mock.setFailAll(errors.New("storage down"))

	err = sink.Publish(context.Background(), &JuiceFsEvent{Type: FileCreated, Timestamp: time.Now(), Inode: 1, Name: "f"})
	if err == nil {
		t.Fatal("expected the put error to be returned when spill is disabled")
	}
	if err := sink.Health(); err != nil {
		t.Fatalf("expected no backpressure, got %v", err)
	}
	if puts := mock.recorded(); len(puts) != 0 {
		t.Fatalf("expected no recorded objects, got %d", len(puts))
	}
}

func TestSpillOnSinkUnavailable(t *testing.T) {
	mock := &mockStorage{}
	dir := t.TempDir()
	sink, err := NewS3Sink(S3SinkConfig{Prefix: "events/", SpillDir: dir, SpillMaxSize: 1 << 30}, mock)
	if err != nil {
		t.Fatalf("new s3 sink: %v", err)
	}
	mock.setFailAll(errors.New("storage down"))

	event := &JuiceFsEvent{Type: FileCreated, Timestamp: time.Now(), Inode: 77, Name: "spilled"}
	if err := sink.Publish(context.Background(), event); err != nil {
		t.Fatalf("expected spill to swallow the put error, got %v", err)
	}
	if err := sink.Health(); err != nil {
		t.Fatalf("expected no backpressure for a single small event, got %v", err)
	}

	chunks, chunkLines, err := spillChunks(dir)
	if err != nil {
		t.Fatalf("read spill dir: %v", err)
	}
	if len(chunks) != 1 || len(chunkLines) != 1 || len(chunkLines[0]) != 1 {
		t.Fatalf("expected 1 chunk with 1 line, got %d chunks: %v", len(chunks), chunkLines)
	}
	var got JuiceFsEvent
	if err := json.Unmarshal([]byte(chunkLines[0][0]), &got); err != nil {
		t.Fatalf("spill line is not a valid JuiceFsEvent JSON: %v", err)
	}
	if got.Inode != 77 || got.Name != "spilled" || got.Uuid == "" {
		t.Fatalf("unexpected spilled event: %+v", got)
	}
}

func TestSpillOverflowBackpressure(t *testing.T) {
	mock := &mockStorage{}
	dir := t.TempDir()
	sink, err := NewS3Sink(S3SinkConfig{Prefix: "events/", SpillDir: dir, SpillMaxSize: 1}, mock)
	if err != nil {
		t.Fatalf("new s3 sink: %v", err)
	}
	mock.setFailAll(errors.New("storage down"))

	err = sink.Publish(context.Background(), &JuiceFsEvent{Type: FileCreated, Timestamp: time.Now(), Inode: 1, Name: "f"})
	if err == nil {
		t.Fatal("expected the put error when the spill limit is exhausted")
	}
	if err := sink.Health(); err == nil {
		t.Fatal("expected backpressure after spill overflow")
	}
	if chunks, _, err := spillChunks(dir); err != nil || len(chunks) != 0 {
		t.Fatalf("expected empty spill after overflow, got %v (%v)", chunks, err)
	}

	// Recovery: drain on an empty spill probes the storage and clears
	// backpressure on success
	mock.setFailAll(nil)
	if err := sink.TryDrain(context.Background()); err != nil {
		t.Fatalf("drain on empty spill: %v", err)
	}
	if err := sink.Health(); err != nil {
		t.Fatalf("expected backpressure to be cleared after the probe put, got %v", err)
	}
	puts := mock.recorded()
	if len(puts) != 1 || !strings.Contains(puts[0].key, "/.probe/") {
		t.Fatalf("expected exactly 1 probe object, got %v", puts)
	}

	// The sink works again — a fresh event is delivered as an object
	recovered := &JuiceFsEvent{Type: FileCreated, Timestamp: time.Now(), Inode: 9, Name: "after-recovery"}
	if err := sink.Publish(context.Background(), recovered); err != nil {
		t.Fatalf("publish after recovery: %v", err)
	}
	puts = mock.recorded()
	if len(puts) != 2 {
		t.Fatalf("expected probe + event objects, got %d", len(puts))
	}
	if want := expectedKey("events/", recovered.Timestamp, recovered.Uuid); puts[1].key != want {
		t.Fatalf("expected key %s, got %s", want, puts[1].key)
	}
}

func TestBackpressureClearedOnlyBySuccessfulPut(t *testing.T) {
	mock := &mockStorage{}
	dir := t.TempDir()
	sink, err := NewS3Sink(S3SinkConfig{Prefix: "events/", SpillDir: dir, SpillMaxSize: 1}, mock)
	if err != nil {
		t.Fatalf("new s3 sink: %v", err)
	}
	mock.setFailAll(errors.New("storage down"))

	// Overflow: the spill limit (1 byte) is below any event line
	err = sink.Publish(context.Background(), &JuiceFsEvent{Type: FileCreated, Timestamp: time.Now(), Inode: 1, Name: "f"})
	if err == nil {
		t.Fatal("expected the put error when the spill limit is exhausted")
	}
	if err := sink.Health(); err == nil {
		t.Fatal("expected backpressure after spill overflow")
	}

	// Drain attempt on an EMPTY spill with a dead storage must not clear
	// the flag (e2e regression: /health toggled 503/200 every tick)
	if err := sink.TryDrain(context.Background()); err == nil {
		t.Fatal("expected the probe put to fail while the storage is down")
	}
	if err := sink.Health(); err == nil {
		t.Fatal("expected backpressure to persist while the probe put fails")
	}
	if puts := mock.recorded(); len(puts) != 0 {
		t.Fatalf("expected no recorded objects from failed probes, got %d", len(puts))
	}

	// Recovery: the probe put succeeds and clears backpressure
	mock.setFailAll(nil)
	if err := sink.TryDrain(context.Background()); err != nil {
		t.Fatalf("drain on empty spill after recovery: %v", err)
	}
	if err := sink.Health(); err != nil {
		t.Fatalf("expected backpressure cleared after the probe put, got %v", err)
	}
	puts := mock.recorded()
	if len(puts) != 1 {
		t.Fatalf("expected exactly 1 probe object, got %d", len(puts))
	}
	if !strings.Contains(puts[0].key, "/.probe/") {
		t.Fatalf("expected a probe object key, got %s", puts[0].key)
	}
}

func TestTryDrainNoOpWhenHealthyAndEmpty(t *testing.T) {
	mock := &mockStorage{}
	sink, err := NewS3Sink(S3SinkConfig{Prefix: "events/", SpillDir: t.TempDir(), SpillMaxSize: 1 << 30}, mock)
	if err != nil {
		t.Fatalf("new s3 sink: %v", err)
	}

	if err := sink.TryDrain(context.Background()); err != nil {
		t.Fatalf("drain on empty spill without backpressure: %v", err)
	}
	if err := sink.Health(); err != nil {
		t.Fatalf("expected no backpressure, got %v", err)
	}
	if puts := mock.recorded(); len(puts) != 0 {
		t.Fatalf("expected no probe without backpressure, got %d objects", len(puts))
	}
}

func TestSpillChunkRotation(t *testing.T) {
	mock := &mockStorage{}
	dir := t.TempDir()
	sink, err := NewS3Sink(S3SinkConfig{Prefix: "events/", SpillDir: dir, SpillMaxSize: 1 << 30}, mock)
	if err != nil {
		t.Fatalf("new s3 sink: %v", err)
	}
	// Rotation threshold below one line: every append starts a new chunk
	sink.mu.Lock()
	sink.rotateSize = 1
	sink.mu.Unlock()

	mock.setFailAll(errors.New("storage down"))
	events := []*JuiceFsEvent{
		{Type: FileCreated, Timestamp: time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC), Inode: 1, Name: "r1", Uuid: "aaaaaaaa-1111-2222-3333-444444444444"},
		{Type: FileWritten, Timestamp: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC), Inode: 2, Name: "r2", Uuid: "bbbbbbbb-1111-2222-3333-444444444444"},
		{Type: FileDeleted, Timestamp: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC), Inode: 3, Name: "r3", Uuid: "cccccccc-1111-2222-3333-444444444444"},
	}
	for i, event := range events {
		if err := sink.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish %d during outage: %v", i+1, err)
		}
	}

	// Each spilled event got its own chunk, still ordered FIFO by name
	chunks, chunkLines, err := spillChunks(dir)
	if err != nil {
		t.Fatalf("read spill dir: %v", err)
	}
	if len(chunks) != len(events) {
		t.Fatalf("expected %d chunks after rotation, got %d", len(events), len(chunks))
	}
	for i, lines := range chunkLines {
		if len(lines) != 1 {
			t.Fatalf("expected 1 line in chunk %d, got %d", i, len(lines))
		}
		var got JuiceFsEvent
		if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
			t.Fatalf("unmarshal chunk %d line: %v", i, err)
		}
		if got.Uuid != events[i].Uuid {
			t.Fatalf("expected FIFO order: chunk %d carries uuid %s, want %s", i, got.Uuid, events[i].Uuid)
		}
	}

	// All rotated chunks drain FIFO
	mock.setFailAll(nil)
	if err := sink.TryDrain(context.Background()); err != nil {
		t.Fatalf("drain after recovery: %v", err)
	}
	if chunks, _, err := spillChunks(dir); err != nil || len(chunks) != 0 {
		t.Fatalf("expected empty spill after full drain, got %v (%v)", chunks, err)
	}
	if puts := mock.recorded(); len(puts) != len(events) {
		t.Fatalf("expected %d drained objects, got %d", len(events), len(puts))
	}
}

func TestSpillDrainResumesAfterRestart(t *testing.T) {
	mock := &mockStorage{}
	dir := t.TempDir()
	sink, err := NewS3Sink(S3SinkConfig{Prefix: "events/", SpillDir: dir, SpillMaxSize: 1 << 30}, mock)
	if err != nil {
		t.Fatalf("new s3 sink: %v", err)
	}

	events := []*JuiceFsEvent{
		{Type: FileCreated, Timestamp: time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC), Inode: 11, Name: "p1", Uuid: "dddddddd-1111-2222-3333-444444444444"},
		{Type: FileDeleted, Timestamp: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), Inode: 12, Name: "p2", Uuid: "eeeeeeee-1111-2222-3333-444444444444"},
	}
	mock.setFailAll(errors.New("storage down"))
	for i, event := range events {
		if err := sink.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish %d during outage: %v", i+1, err)
		}
	}

	// Simulate a restart: a fresh S3Sink over the same spill dir with a
	// working storage — its constructor drains the leftovers best-effort
	// from the chunk start (idempotent overwrites by uuid object key)
	restartedMock := &mockStorage{}
	restarted, err := NewS3Sink(S3SinkConfig{Prefix: "events/", SpillDir: dir, SpillMaxSize: 1 << 30}, restartedMock)
	if err != nil {
		t.Fatalf("restart s3 sink: %v", err)
	}
	if chunks, _, err := spillChunks(dir); err != nil || len(chunks) != 0 {
		t.Fatalf("expected startup drain to empty the spill, got %v (%v)", chunks, err)
	}
	if err := restarted.Health(); err != nil {
		t.Fatalf("expected no backpressure after startup drain, got %v", err)
	}
	puts := restartedMock.recorded()
	if len(puts) != len(events) {
		t.Fatalf("expected %d drained objects after restart, got %d", len(events), len(puts))
	}
	for i, event := range events {
		if want := expectedKey("events/", event.Timestamp, event.Uuid); puts[i].key != want {
			t.Fatalf("expected object %d key %s, got %s", i, want, puts[i].key)
		}
	}
}

func TestSpillDrainAfterRecovery(t *testing.T) {
	mock := &mockStorage{}
	dir := t.TempDir()
	sink, err := NewS3Sink(S3SinkConfig{Prefix: "events/", SpillDir: dir, SpillMaxSize: 1 << 30}, mock)
	if err != nil {
		t.Fatalf("new s3 sink: %v", err)
	}

	events := []*JuiceFsEvent{
		{Type: FileCreated, Timestamp: time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC), Inode: 1, Name: "e1", Uuid: "aaaaaaaa-1111-2222-3333-444444444444"},
		{Type: FileWritten, Timestamp: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC), Inode: 2, Name: "e2", Uuid: "bbbbbbbb-1111-2222-3333-444444444444"},
		{Type: FileDeleted, Timestamp: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC), Inode: 3, Name: "e3", Uuid: "cccccccc-1111-2222-3333-444444444444"},
	}
	mock.setFailAll(errors.New("storage down"))
	for i, event := range events {
		if err := sink.Publish(context.Background(), event); err != nil {
			t.Fatalf("publish %d during outage: %v", i+1, err)
		}
	}
	if err := sink.Health(); err != nil {
		t.Fatalf("expected no backpressure while under the limit, got %v", err)
	}
	chunks, chunkLines, err := spillChunks(dir)
	if err != nil {
		t.Fatalf("read spill dir: %v", err)
	}
	if len(chunks) != 1 || len(chunkLines[0]) != 3 {
		t.Fatalf("expected 1 chunk with 3 spilled lines, got %d chunks / %d lines", len(chunks), len(chunkLines[0]))
	}
	if info, err := os.Stat(chunks[0]); err != nil || info.Size() == 0 {
		t.Fatalf("expected non-empty spill chunk, got size error %v", err)
	}

	// Partial drain: the first object lands, the second fails — draining
	// stops, the chunk file stays untouched, no backpressure is raised
	mock.setFailAll(nil)
	mock.resetAttempts()
	mock.setFailAt(2)
	if err := sink.TryDrain(context.Background()); err == nil {
		t.Fatal("expected a drain error when the second object fails")
	}
	if err := sink.Health(); err != nil {
		t.Fatalf("expected no backpressure from a failed drain under the limit, got %v", err)
	}
	chunks, chunkLines, err = spillChunks(dir)
	if err != nil {
		t.Fatalf("read spill dir after partial drain: %v", err)
	}
	if len(chunks) != 1 || len(chunkLines[0]) != 3 {
		t.Fatalf("expected the chunk untouched with 3 lines, got %d chunks / %d lines", len(chunks), len(chunkLines[0]))
	}
	if puts := mock.recorded(); len(puts) != 1 {
		t.Fatalf("expected 1 drained object before the failure, got %d", len(puts))
	}

	// Full recovery: draining resumes from the cursor and delivers the
	// remaining lines FIFO with their original keys
	mock.setFailAt(0)
	mock.resetAttempts()
	if err := sink.TryDrain(context.Background()); err != nil {
		t.Fatalf("drain after recovery: %v", err)
	}
	if chunks, _, err := spillChunks(dir); err != nil || len(chunks) != 0 {
		t.Fatalf("expected empty spill after full drain, got %v (%v)", chunks, err)
	}
	if err := sink.Health(); err != nil {
		t.Fatalf("expected no backpressure after full drain, got %v", err)
	}

	puts := mock.recorded()
	if len(puts) != 3 {
		t.Fatalf("expected 3 drained objects in total, got %d", len(puts))
	}
	wantKeys := map[string]bool{}
	wantUUIDs := map[string]string{}
	for _, event := range events {
		key := expectedKey("events/", event.Timestamp, event.Uuid)
		wantKeys[key] = true
		wantUUIDs[key] = event.Uuid
	}
	for _, put := range puts {
		if !wantKeys[put.key] {
			t.Fatalf("unexpected object key %s", put.key)
		}
		var ev JuiceFsEvent
		if err := json.Unmarshal(put.body, &ev); err != nil {
			t.Fatalf("unmarshal drained object body: %v", err)
		}
		if wantUUIDs[put.key] != ev.Uuid {
			t.Fatalf("object %s carries uuid %q, want %q", put.key, ev.Uuid, wantUUIDs[put.key])
		}
	}
}
