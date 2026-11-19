/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
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
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// captureSink is a test EventSink that records every published event
type captureSink struct {
	mu     sync.Mutex
	events []*JuiceFsEvent
}

func (s *captureSink) Publish(ctx context.Context, event *JuiceFsEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *captureSink) collected() []*JuiceFsEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*JuiceFsEvent(nil), s.events...)
}

// failingSink is a test EventSink whose Publish always fails
type failingSink struct{}

func (s *failingSink) Publish(ctx context.Context, event *JuiceFsEvent) error {
	return errors.New("sink unavailable")
}

func TestRedisOutboxIsHealthy(t *testing.T) {
	// Not running → unhealthy
	client := redis.NewClient(&redis.Options{Addr: "localhost:0"})
	outbox := NewRedisOutbox(client, OutboxConfig{Enabled: true, StreamName: "test:outbox"})
	if outbox.IsHealthy() {
		t.Fatal("expected unhealthy when consumer not running")
	}

	// Simulate running state with unreachable Redis → unhealthy
	outbox.mu.Lock()
	outbox.running = true
	outbox.mu.Unlock()
	if outbox.IsHealthy() {
		t.Fatal("expected unhealthy when Redis is unreachable")
	}
}

func TestInitConsumerGroupNoStream(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	defer client.Close()

	stream := "test:initcg:nostream"
	group := "testgroup"
	client.Del(ctx, stream) // ensure stream doesn't exist

	outbox := NewRedisOutbox(client, OutboxConfig{
		Enabled:       true,
		StreamName:    stream,
		ConsumerGroup: group,
	})

	// InitConsumerGroup must succeed even when the stream doesn't exist
	if err := outbox.InitConsumerGroup(ctx); err != nil {
		t.Fatalf("InitConsumerGroup failed on missing stream: %v", err)
	}

	// Verify consumer group was created
	groups, err := client.XInfoGroups(ctx, stream).Result()
	if err != nil {
		t.Fatalf("XInfoGroups failed after init: %v", err)
	}
	found := false
	for _, g := range groups {
		if g.Name == group {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("consumer group %s was not created", group)
	}

	// Cleanup
	client.Del(ctx, stream)
}

func TestReclaimPendingMessages(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}

	stream := "test:outbox:reclaim"
	deadQueue := stream + ":dead"
	// Clean leftovers from previous runs (keys may survive if a previous
	// test aborted before its cleanup)
	client.Del(ctx, stream, deadQueue)

	outbox := NewRedisOutbox(client, OutboxConfig{
		Enabled:        true,
		StreamName:     stream,
		ConsumerGroup:  "testgroup",
		MaxRetries:     10,
		ReclaimMinIdle: 100 * time.Millisecond,
	})
	// Delete the keys while the client is still open, and give the consume
	// goroutine time to exit before closing the client
	t.Cleanup(func() {
		outbox.StopConsumer()
		client.Del(ctx, stream, deadQueue)
		time.Sleep(1200 * time.Millisecond)
		client.Close()
	})

	if err := outbox.InitConsumerGroup(ctx); err != nil {
		t.Fatalf("InitConsumerGroup failed: %v", err)
	}

	event := &JuiceFsEvent{
		Type:      FileCreated,
		Timestamp: time.Now(),
		Volume:    "test-vol",
		Inode:     42,
		Parent:    1,
		Name:      "reclaim-file",
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if _, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{"data": string(data)},
	}).Result(); err != nil {
		t.Fatalf("xadd: %v", err)
	}

	// Manually deliver the message into the PEL without processing it
	res, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    "testgroup",
		Consumer: "juicefs",
		Streams:  []string{stream, ">"},
		Count:    10,
	}).Result()
	if err != nil {
		t.Fatalf("xreadgroup: %v", err)
	}
	if len(res) != 1 || len(res[0].Messages) != 1 {
		t.Fatalf("expected 1 message in PEL, got %d streams / %d messages", len(res), len(res[0].Messages))
	}

	// Ensure the message idle time exceeds ReclaimMinIdle before the consumer starts
	time.Sleep(150 * time.Millisecond)

	sink := &captureSink{}
	if err := outbox.StartConsumer(sink); err != nil {
		t.Fatalf("start consumer: %v", err)
	}

	// The reclaim pass runs on the consume loop tick (poll XREADGROUP may
	// block up to 1s first, so allow a generous deadline)
	deadline := time.Now().Add(4 * time.Second)
	for len(sink.collected()) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}

	collected := sink.collected()
	if len(collected) != 1 {
		t.Fatalf("expected exactly 1 reclaimed event, got %d: %+v", len(collected), collected)
	}
	if collected[0].Type != FileCreated || collected[0].Inode != 42 {
		t.Fatalf("unexpected reclaimed event: %+v", collected[0])
	}

	pending, err := client.XPending(ctx, stream, "testgroup").Result()
	if err != nil {
		t.Fatalf("xpending: %v", err)
	}
	if pending.Count != 0 {
		t.Fatalf("expected empty PEL after reclaim, got %d pending", pending.Count)
	}
}

func TestMoveToDeadQueueAcksOriginal(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}

	stream := "test:outbox:dead"
	deadQueue := stream + ":dead"
	// Clean leftovers from previous runs (keys may survive if a previous
	// test aborted before its cleanup)
	client.Del(ctx, stream, deadQueue)

	outbox := NewRedisOutbox(client, OutboxConfig{
		Enabled:        true,
		StreamName:     stream,
		ConsumerGroup:  "testgroup",
		MaxRetries:     1,
		ReclaimMinIdle: time.Minute,
	})
	// Delete the keys while the client is still open, and give the consume
	// goroutine time to exit before closing the client
	t.Cleanup(func() {
		outbox.StopConsumer()
		client.Del(ctx, stream, deadQueue)
		time.Sleep(1200 * time.Millisecond)
		client.Close()
	})

	if err := outbox.InitConsumerGroup(ctx); err != nil {
		t.Fatalf("InitConsumerGroup failed: %v", err)
	}

	event := &JuiceFsEvent{
		Type:      FileCreated,
		Timestamp: time.Now(),
		Volume:    "test-vol",
		Inode:     7,
		Parent:    1,
		Name:      "dead-file",
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	msgID, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{"data": string(data)},
	}).Result()
	if err != nil {
		t.Fatalf("xadd: %v", err)
	}

	sink := &failingSink{}
	if err := outbox.StartConsumer(sink); err != nil {
		t.Fatalf("start consumer: %v", err)
	}

	// Wait for poll → publish failure → retry counter reaches MaxRetries →
	// dead queue move + ack of the original
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := client.XLen(ctx, deadQueue).Result(); n > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Dead queue holds the original payload
	deadMsgs, err := client.XRange(ctx, deadQueue, "-", "+").Result()
	if err != nil {
		t.Fatalf("xrange dead queue: %v", err)
	}
	if len(deadMsgs) != 1 {
		t.Fatalf("expected 1 dead-letter record, got %d", len(deadMsgs))
	}
	if v, ok := deadMsgs[0].Values["data"].(string); !ok || v != string(data) {
		t.Fatalf("dead record missing original payload: %+v", deadMsgs[0].Values)
	}
	if _, ok := deadMsgs[0].Values["retry_at"]; !ok {
		t.Fatalf("dead record missing retry_at: %+v", deadMsgs[0].Values)
	}
	if id, ok := deadMsgs[0].Values["msg_id"].(string); !ok || id != msgID {
		t.Fatalf("dead record msg_id %v, want %s", deadMsgs[0].Values["msg_id"], msgID)
	}

	// The original message was acknowledged (XACK in deleteMessage)
	pending, err := client.XPending(ctx, stream, "testgroup").Result()
	if err != nil {
		t.Fatalf("xpending: %v", err)
	}
	if pending.Count != 0 {
		t.Fatalf("expected empty PEL after dead-queue move, got %d pending", pending.Count)
	}

	// The per-message retry counter key exists (24h TTL set by addRetryCount)
	retryKey := fmt.Sprintf("%s:retry:%s", stream, msgID)
	if n, err := client.Exists(ctx, retryKey).Result(); err != nil || n != 1 {
		t.Fatalf("retry key %s missing (exists=%d, err=%v)", retryKey, n, err)
	}
}

func TestParseRedisClientOptionsDefaults(t *testing.T) {
	result, err := parseRedisClientOptions("redis://localhost:6379/0")
	if err != nil {
		t.Fatalf("parseRedisClientOptions returned error: %v", err)
	}
	if result.Options.MaxRetries != -1 {
		t.Fatalf("expected MaxRetries=-1, got %d", result.Options.MaxRetries)
	}
}

func TestRouteReadStrippedBeforeParse(t *testing.T) {
	// route-read is a JuiceFS-specific query param that must be stripped before
	// passing the URL to go-redis ParseURL. Reproduce the logic from newRedisMeta.
	uri := "redis://localhost:6379/0?route-read=random"
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("url.Parse failed: %v", err)
	}
	values := u.Query()
	query := queryMap{&values}
	routeRead := query.pop("route-read")
	u.RawQuery = values.Encode()

	if routeRead != "random" {
		t.Fatalf("expected route-read=random, got %q", routeRead)
	}

	// After stripping, parsing must succeed
	if _, err = parseRedisClientOptions(u.String()); err != nil {
		t.Fatalf("parseRedisClientOptions failed after stripping route-read: %v", err)
	}
}

// blockingSink is a test EventSink whose Publish blocks until release is
// closed, with channels to synchronize with the test
type blockingSink struct {
	enteredOnce sync.Once
	entered     chan struct{} // closed when Publish is entered
	releaseOnce sync.Once
	release     chan struct{} // Publish blocks until this is closed

	mu        sync.Mutex
	published int
}

func (s *blockingSink) Publish(ctx context.Context, event *JuiceFsEvent) error {
	s.enteredOnce.Do(func() { close(s.entered) })
	<-s.release
	s.mu.Lock()
	s.published++
	s.mu.Unlock()
	return nil
}

func (s *blockingSink) closeRelease() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func (s *blockingSink) publishedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.published
}

func TestStopConsumerWaitsForLoopExit(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}

	stream := "test:stopwait:outbox"
	// Clean leftovers from previous runs (keys may survive if a previous
	// test aborted before its cleanup)
	client.Del(ctx, stream)

	outbox := NewRedisOutbox(client, OutboxConfig{
		Enabled:       true,
		StreamName:    stream,
		ConsumerGroup: "testgroup",
		MaxRetries:    10,
		TrimMaxLen:    10000,
	})

	sink := &blockingSink{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(func() {
		// Unblock Publish in case the test aborted while it was in flight,
		// then stop the consumer (StopConsumer waits for the loop to exit)
		sink.closeRelease()
		outbox.StopConsumer()
		client.Del(ctx, stream)
		client.Close()
	})

	if err := outbox.InitConsumerGroup(ctx); err != nil {
		t.Fatalf("InitConsumerGroup failed: %v", err)
	}

	event := &JuiceFsEvent{
		Type:      FileCreated,
		Timestamp: time.Now(),
		Volume:    "test-vol",
		Inode:     11,
		Parent:    1,
		Name:      "stopwait-file",
		Path:      "/data/stopwait-file",
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if _, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{"data": string(data)},
	}).Result(); err != nil {
		t.Fatalf("xadd: %v", err)
	}

	if err := outbox.StartConsumer(sink); err != nil {
		t.Fatalf("start consumer: %v", err)
	}

	// Wait until the consume loop is inside Publish (the loop ticks every 1s,
	// so allow a generous deadline)
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish was not entered within 5s")
	}

	// StopConsumer must not return while Publish is in flight
	stopReturned := make(chan struct{})
	go func() {
		outbox.StopConsumer()
		close(stopReturned)
	}()

	select {
	case <-stopReturned:
		t.Fatal("StopConsumer returned while Publish in flight")
	case <-time.After(300 * time.Millisecond):
		// expected: StopConsumer is still waiting for the loop to exit
	}

	// Let Publish finish — StopConsumer must now return
	sink.closeRelease()

	select {
	case <-stopReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("StopConsumer did not return within 5s after Publish finished")
	}

	if sink.publishedCount() != 1 {
		t.Fatalf("expected exactly 1 published event, got %d", sink.publishedCount())
	}

	if outbox.IsHealthy() {
		t.Fatal("expected unhealthy after StopConsumer returned")
	}
}

func TestRestartAfterStopSingleLoop(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}

	stream := "test:restart:outbox"
	// Clean leftovers from previous runs (keys may survive if a previous
	// test aborted before its cleanup)
	client.Del(ctx, stream)

	outbox := NewRedisOutbox(client, OutboxConfig{
		Enabled:       true,
		StreamName:    stream,
		ConsumerGroup: "testgroup",
		MaxRetries:    10,
		TrimMaxLen:    10000,
	})
	t.Cleanup(func() {
		outbox.StopConsumer()
		client.Del(ctx, stream)
		client.Close()
	})

	if err := outbox.InitConsumerGroup(ctx); err != nil {
		t.Fatalf("InitConsumerGroup failed: %v", err)
	}

	// Two valid events must be in the stream before the first consumer starts
	for i := 1; i <= 2; i++ {
		event := &JuiceFsEvent{
			Type:      FileCreated,
			Timestamp: time.Now(),
			Volume:    "test-vol",
			Inode:     Ino(i),
			Parent:    1,
			Name:      fmt.Sprintf("restart-file-%d", i),
			Path:      fmt.Sprintf("/data/restart-file-%d", i),
		}
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if _, err := client.XAdd(ctx, &redis.XAddArgs{
			Stream: stream,
			Values: map[string]interface{}{"data": string(data)},
		}).Result(); err != nil {
			t.Fatalf("xadd: %v", err)
		}
	}

	sink := &captureSink{}

	if err := outbox.StartConsumer(sink); err != nil {
		t.Fatalf("first StartConsumer failed: %v", err)
	}

	// Both events must be delivered by the first loop
	deadline := time.After(5 * time.Second)
	for len(sink.collected()) < 2 {
		select {
		case <-deadline:
			t.Fatalf("expected 2 events within 5s, got %d", len(sink.collected()))
		case <-time.After(100 * time.Millisecond):
		}
	}

	// StopConsumer returns only after the loop goroutine has exited, so the
	// restart below is guaranteed to run a single loop
	outbox.StopConsumer()

	// Restart on the same instance must succeed
	if err := outbox.StartConsumer(sink); err != nil {
		t.Fatalf("restart StartConsumer after stop failed: %v", err)
	}

	// Wait two full consume-loop ticks: the restarted loop must not
	// redeliver the already acknowledged events (no duplicates from a
	// second loop)
	time.Sleep(2500 * time.Millisecond)
	if got := len(sink.collected()); got != 2 {
		t.Fatalf("expected exactly 2 events after restart (no duplicates), got %d", got)
	}

	// A third StartConsumer without an intermediate stop must fail
	if err := outbox.StartConsumer(sink); err == nil {
		t.Fatal("expected error from StartConsumer while consumer is already running")
	} else if err.Error() != "consumer already running" {
		t.Fatalf("unexpected StartConsumer error: %v", err)
	}

	outbox.StopConsumer()

	if outbox.IsHealthy() {
		t.Fatal("expected unhealthy after final StopConsumer")
	}
}
