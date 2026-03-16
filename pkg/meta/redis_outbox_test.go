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
	"net/url"
	"testing"

	"github.com/redis/go-redis/v9"
)

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

func TestCreateRedisOptionsNilConf(t *testing.T) {
	result, err := createRedisOptions("redis://localhost:6379/0", nil)
	if err != nil {
		t.Fatalf("createRedisOptions with nil conf returned error: %v", err)
	}
	if result.Options.MaxRetries != -1 {
		t.Fatalf("expected MaxRetries=-1 with nil conf, got %d", result.Options.MaxRetries)
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

	// After stripping, createRedisOptions must succeed
	_, err = createRedisOptions(u.String(), nil)
	if err != nil {
		t.Fatalf("createRedisOptions failed after stripping route-read: %v", err)
	}
}
