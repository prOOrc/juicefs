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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"

	"github.com/juicedata/juicefs/pkg/object"
)

// s3SpillChunkSuffix is the file extension of spill chunks
const s3SpillChunkSuffix = ".jsonl"

// s3SpillChunkRotateSize is the spill chunk rotation threshold: a new chunk is
// started once the current one reaches this size, bounding the drain's read
// memory to one chunk
const s3SpillChunkRotateSize = 64 << 20

// S3SinkConfig holds the configuration of the S3 object sink
type S3SinkConfig struct {
	// Prefix of object keys, default "events/"; stored without trailing slashes
	Prefix string
	// SpillDir buffers undelivered events when the storage is unavailable;
	// empty means spill is disabled and failed publishes return the error
	SpillDir string
	// SpillMaxSize limits the total spill size in bytes; must be positive
	// when spill is enabled
	SpillMaxSize int64
}

// S3Sink delivers events to an object storage bucket, one JSON object per
// event. When the storage is unavailable and spill is configured, events are
// buffered in local JSONL chunks (ack-after-spill) and drained FIFO once the
// storage recovers. Exhausting the spill limit flips the sink into
// backpressure (Health), letting the consumer stop reading and drain instead.
// S3Sink has no internal goroutines or tickers: draining is driven by
// Publish success, the outbox consumer loop during backpressure, and a single
// best-effort pass at construction when chunks are left from a previous run.
type S3Sink struct {
	storage      object.ObjectStorage
	prefix       string // stored without trailing slashes
	spillDir     string // empty = spill disabled
	spillMaxSize int64
	rotateSize   int64 // spill chunk rotation threshold

	mu           sync.Mutex
	spillSize    int64          // total size of spill chunks on disk
	backpressure bool           // set when the spill limit is exhausted
	drainCursor  map[string]int // chunk file name → count of already drained lines
}

// NewS3Sink creates an S3 sink. If spill chunks are left in SpillDir from a
// previous run, one best-effort drain is attempted.
func NewS3Sink(cfg S3SinkConfig, storage object.ObjectStorage) (*S3Sink, error) {
	if storage == nil {
		return nil, errors.New("s3 sink: storage must not be nil")
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix == "" {
		return nil, errors.New("s3 sink: prefix must not be empty")
	}
	if cfg.SpillDir != "" && cfg.SpillMaxSize <= 0 {
		return nil, errors.New("s3 sink: spill max size must be positive when spill is enabled")
	}
	s := &S3Sink{
		storage:      storage,
		prefix:       prefix,
		spillDir:     cfg.SpillDir,
		spillMaxSize: cfg.SpillMaxSize,
		rotateSize:   s3SpillChunkRotateSize,
		drainCursor:  make(map[string]int),
	}
	if s.spillDir != "" {
		if err := os.MkdirAll(s.spillDir, 0755); err != nil {
			return nil, errors.Wrap(err, "create spill dir")
		}
		s.mu.Lock()
		chunks := s.listSpillChunksLocked()
		s.mu.Unlock()
		if len(chunks) > 0 {
			logger.Infof("s3 sink: %d spill chunk(s) left from a previous run, draining (best-effort)", len(chunks))
			// Best-effort: startup must not fail and must not raise backpressure
			_ = s.TryDrain(context.Background())
		}
	}
	return s, nil
}

// Publish delivers the event as a single JSON object with key
// <prefix>/<YYYY-MM-DD>/<HH>/<uuid>.json (date and hour taken from the event
// timestamp in UTC). Events without a uuid (legacy producers) get one before
// the first delivery attempt, making retries stable. On storage failure with
// spill enabled the event is appended to a spill chunk and Publish returns
// nil (ack-after-spill); with the spill limit exhausted the sink enters
// backpressure and the error is returned.
func (s *S3Sink) Publish(ctx context.Context, event *JuiceFsEvent) error {
	if event.Uuid == "" {
		event.Uuid = uuid.New().String()
	}
	body, err := json.Marshal(event)
	if err != nil {
		return errors.Wrap(err, "marshal event")
	}
	key := s.objectKey(event)
	if err := s.storage.Put(ctx, key, bytes.NewReader(body)); err != nil {
		if s.spillDir == "" {
			return errors.Wrap(err, "put event object")
		}
		return s.spill(body, err)
	}
	// Success — try to drain spill accumulated while the storage was down;
	// drain errors are logged but must not fail the published event
	if err := s.TryDrain(ctx); err != nil {
		logger.Warnf("s3 sink: drain after publish: %v", err)
	}
	return nil
}

// Health returns an error iff the sink is in backpressure
func (s *S3Sink) Health() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backpressure {
		return errors.Errorf("s3 sink: spill limit of %d bytes reached, backpressure active", s.spillMaxSize)
	}
	return nil
}

// TryDrain drains spill chunks FIFO into the storage. Draining stops at the
// first storage failure and resumes from an in-memory cursor on the next
// call — the chunk file is left untouched. A fully drained chunk is removed.
// A fresh cursor (e.g. after a restart) drains from the chunk start, which
// re-delivers already-drained events idempotently: the object key is the
// event uuid, so redelivery overwrites the same object. Unparsable lines are
// dropped with a warning so they never block the drain. Returns nil when
// spill is disabled or empty. Backpressure is cleared only by proof that the
// storage works again: at least one event delivered by a fully successful
// drain, or a successful probe put when the buffer is empty.
func (s *S3Sink) TryDrain(ctx context.Context) error {
	if s.spillDir == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chunks := s.listSpillChunksLocked()
	// Forget cursors of chunks that no longer exist on disk
	live := make(map[string]bool, len(chunks))
	for _, chunk := range chunks {
		live[filepath.Base(chunk)] = true
	}
	for name := range s.drainCursor {
		if !live[name] {
			delete(s.drainCursor, name)
		}
	}
	if len(chunks) == 0 {
		// Empty buffer: without evidence of a working storage the flag must
		// not clear — an unconditional clear would toggle /health 503/200
		// every tick while the sink is still dead (e2e regression)
		if s.backpressure {
			return s.probeLocked(ctx)
		}
		return nil
	}
	var drained int
	for _, chunk := range chunks {
		n, err := s.drainChunkLocked(ctx, chunk)
		drained += n
		if err != nil {
			// Attempt failed — backpressure (if set) stays: the sink has not
			// proven healthy; the cursor keeps the progress for the next tick
			return err
		}
	}
	if drained > 0 {
		// At least one event reached the storage — recovery confirmed
		s.backpressure = false
	}
	return nil
}

// probeLocked verifies the storage with a single empty put when the spill
// buffer is empty and backpressure is set. The probe key lives outside the
// date prefixes (events/<date>/<hour>/) that drainers list, so it is never
// mistaken for an event, and the events/ lifecycle policy cleans it up.
func (s *S3Sink) probeLocked(ctx context.Context) error {
	key := fmt.Sprintf("%s/.probe/%s.json", s.prefix, uuid.NewString())
	if err := s.storage.Put(ctx, key, bytes.NewReader(nil)); err != nil {
		return errors.Wrap(err, "probe put")
	}
	logger.Infof("s3 sink: sink recovered (probe put ok)")
	s.backpressure = false
	return nil
}

// objectKey builds the object key for the event from its timestamp and uuid
func (s *S3Sink) objectKey(event *JuiceFsEvent) string {
	ts := event.Timestamp.UTC()
	return fmt.Sprintf("%s/%s/%s/%s.json", s.prefix, ts.Format("2006-01-02"), ts.Format("15"), event.Uuid)
}

// spill appends the marshaled event to the spill buffer; entering
// backpressure and returning the cause when the size limit is exhausted
func (s *S3Sink) spill(body []byte, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := make([]byte, 0, len(body)+1)
	line = append(line, body...)
	line = append(line, '\n')
	if s.spillSize+int64(len(line)) > s.spillMaxSize {
		s.backpressure = true
		logger.Errorf("s3 sink: spill limit of %d bytes reached, entering backpressure (cause: %v)", s.spillMaxSize, cause)
		return cause
	}
	if err := s.appendToCurrentChunkLocked(line); err != nil {
		logger.Errorf("s3 sink: spill write failed: %v", err)
		return err
	}
	s.refreshSpillSizeLocked()
	return nil
}

// appendToCurrentChunkLocked appends the line to the newest chunk, creating
// one atomically (tmp + rename) when the buffer is empty or the current chunk
// has reached the rotation threshold
func (s *S3Sink) appendToCurrentChunkLocked(line []byte) error {
	chunks := s.listSpillChunksLocked()
	if len(chunks) > 0 {
		path := chunks[len(chunks)-1]
		// Rotate: start a new chunk once the current one reached the threshold
		if info, err := os.Stat(path); err == nil && info.Size() < s.rotateSize {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
			if err != nil {
				return errors.Wrapf(err, "open spill chunk %s", filepath.Base(path))
			}
			_, werr := f.Write(line)
			cerr := f.Close()
			if werr != nil {
				return errors.Wrapf(werr, "append to spill chunk %s", filepath.Base(path))
			}
			if cerr != nil {
				return errors.Wrapf(cerr, "close spill chunk %s", filepath.Base(path))
			}
			return nil
		}
	}
	// Zero-padded nanosecond timestamp keeps lexicographic order = FIFO
	path := filepath.Join(s.spillDir, fmt.Sprintf("%020d-%s.jsonl", time.Now().UnixNano(), uuid.NewString()))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, line, 0644); err != nil {
		return errors.Wrap(err, "write spill tmp file")
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return errors.Wrap(err, "publish spill chunk")
	}
	return nil
}

// drainChunkLocked publishes the event lines of one chunk, resuming from the
// per-chunk cursor. The chunk file is never rewritten: on the first storage
// failure draining stops and the cursor keeps the progress for the next
// attempt; a fully drained chunk (cursor reached the last line) is removed.
// Returns the number of successfully delivered lines.
func (s *S3Sink) drainChunkLocked(ctx context.Context, path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, errors.Wrapf(err, "read spill chunk %s", filepath.Base(path))
	}
	name := filepath.Base(path)
	var lines []string
	for _, raw := range strings.Split(string(data), "\n") {
		if line := strings.TrimSpace(raw); line != "" {
			lines = append(lines, line)
		}
	}
	// A fresh cursor (e.g. after a restart) drains from the chunk start:
	// already-drained events are re-published under the same uuid object
	// key, which makes redelivery idempotent
	drained := s.drainCursor[name]
	for i := drained; i < len(lines); i++ {
		var event JuiceFsEvent
		if err := json.Unmarshal([]byte(lines[i]), &event); err != nil {
			// Corrupt line — drop it so it never blocks the drain
			logger.Warnf("s3 sink: dropping unparsable spill line %d in %s: %v", i, name, err)
			s.drainCursor[name] = i + 1
			continue
		}
		key := s.objectKey(&event)
		if err := s.storage.Put(ctx, key, bytes.NewReader([]byte(lines[i]))); err != nil {
			// Stop at the first failure; the progress made so far stays in
			// the cursor and the next drain resumes from this line
			return drained, errors.Wrapf(err, "put %s", key)
		}
		drained = i + 1
		s.drainCursor[name] = i + 1
	}
	// Fully drained (possibly only unparsable lines) — remove the chunk
	if err := os.Remove(path); err != nil {
		return drained, errors.Wrapf(err, "remove drained spill chunk %s", filepath.Base(path))
	}
	delete(s.drainCursor, name)
	s.refreshSpillSizeLocked()
	return drained, nil
}

// listSpillChunksLocked returns spill chunk paths sorted by name (FIFO)
func (s *S3Sink) listSpillChunksLocked() []string {
	matches, err := filepath.Glob(filepath.Join(s.spillDir, "*"+s3SpillChunkSuffix))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

// refreshSpillSizeLocked recomputes the total spill size from the files
func (s *S3Sink) refreshSpillSizeLocked() {
	entries, err := os.ReadDir(s.spillDir)
	if err != nil {
		return
	}
	var total int64
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), s3SpillChunkSuffix) {
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	s.spillSize = total
}
