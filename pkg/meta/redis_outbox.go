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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
)

// KafkaPublisher interface for publishing events to Kafka
type KafkaPublisher interface {
	Publish(ctx context.Context, event *JuiceFsEvent) error
}

// RedisOutbox manages Redis Streams for publishing filesystem events
type RedisOutbox struct {
	client    redis.UniversalClient
	config    OutboxConfig
	stream    string
	group     string
	deadQueue string
	mu        sync.Mutex
	running   bool
	ctx       context.Context
	cancel    context.CancelFunc
}

// NewRedisOutbox creates a new outbox instance
func NewRedisOutbox(client redis.UniversalClient, cfg OutboxConfig) *RedisOutbox {
	return &RedisOutbox{
		client:    client,
		config:    cfg,
		stream:    cfg.StreamName,
		group:     cfg.ConsumerGroup,
		deadQueue: cfg.StreamName + ":dead",
	}
}

// Enabled returns true if outbox is enabled
func (o *RedisOutbox) Enabled() bool {
	return o != nil && o.config.Enabled
}

// AddEvent adds an event to the Redis stream
func (o *RedisOutbox) AddEvent(ctx context.Context, event *JuiceFsEvent) error {
	if !o.Enabled() {
		return nil
	}

	data, err := json.Marshal(event)
	if err != nil {
		return errors.Wrap(err, "marshal event")
	}

	// XADD with XTRIM to keep stream bounded
	pipe := o.client.Pipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: o.stream,
		Values: map[string]interface{}{
			"data": string(data),
		},
	})
	pipe.XTrimMaxLen(ctx, o.stream, o.config.TrimMaxLen)
	_, err = pipe.Exec(ctx)
	return err
}

// AddEventToPipe adds an event to the provided pipeline for transactional execution
func (o *RedisOutbox) AddEventToPipe(ctx context.Context, pipe redis.Pipeliner, event *JuiceFsEvent) {
	if !o.Enabled() {
		return
	}

	data, err := json.Marshal(event)
	if err != nil {
		logger.Errorf("marshal event error: %v", err)
		return
	}

	// Add XADD to pipeline
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: o.stream,
		Values: map[string]interface{}{
			"data": string(data),
		},
	})
	// Add XTRIM to keep stream bounded
	pipe.XTrimMaxLen(ctx, o.stream, o.config.TrimMaxLen)
}

// InitConsumerGroup initializes the consumer group if it doesn't exist
func (o *RedisOutbox) InitConsumerGroup(ctx context.Context) error {
	if !o.Enabled() {
		return nil
	}

	// Check if group exists
	info, err := o.client.XInfoGroups(ctx, o.stream).Result()
	if err != nil && !strings.Contains(err.Error(), "no such key") {
		return errors.Wrap(err, "xinfo groups")
	}

	// Create group if it doesn't exist
	groupExists := false
	for _, g := range info {
		if g.Name == o.group {
			groupExists = true
			break
		}
	}

	if !groupExists {
		// XGROUP CREATE with MKSTREAM to create stream if not exists
		err = o.client.XGroupCreateMkStream(ctx, o.stream, o.group, "$").Err()
		if err != nil && err != redis.Nil {
			return errors.Wrap(err, "xgroup create")
		}
		logger.Infof("Created consumer group %s on stream %s", o.group, o.stream)
	}

	return nil
}

// StartConsumer starts the consumer that reads from the stream and publishes to Kafka
func (o *RedisOutbox) StartConsumer(publisher KafkaPublisher) error {
	if !o.Enabled() {
		return nil
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if o.running {
		return errors.New("consumer already running")
	}

	o.ctx, o.cancel = context.WithCancel(context.Background())
	o.running = true

	go o.consumeLoop(publisher)
	logger.Infof("Outbox consumer started for stream %s", o.stream)

	return nil
}

// IsHealthy returns true if the consumer is running and Redis is reachable
func (o *RedisOutbox) IsHealthy() bool {
	o.mu.Lock()
	running := o.running
	o.mu.Unlock()
	if !running {
		return false
	}
	return o.client.Ping(context.Background()).Err() == nil
}

// StopConsumer stops the consumer
func (o *RedisOutbox) StopConsumer() {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.running {
		return
	}

	if o.cancel != nil {
		o.cancel()
	}
	o.running = false
	logger.Info("Outbox consumer stopped")
}

// consumeLoop reads messages from the stream and publishes them
func (o *RedisOutbox) consumeLoop(publisher KafkaPublisher) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-o.ctx.Done():
			return
		case <-ticker.C:
			o.pollAndPublish(o.ctx, publisher)
		}
	}
}

// pollAndPublish polls the stream for new messages and publishes them
func (o *RedisOutbox) pollAndPublish(ctx context.Context, publisher KafkaPublisher) {
	// XREADGROUP with BLOCK 1000ms
	msgs, err := o.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    o.group,
		Consumer: "juicefs",
		Streams:  []string{o.stream, ">"},
		Count:    100,
		Block:    time.Second,
	}).Result()

	if err != nil {
		if err != redis.Nil {
			logger.Errorf("xreadgroup error: %v", err)
		}
		return
	}

	if len(msgs) == 0 || len(msgs[0].Messages) == 0 {
		return
	}

	for _, msg := range msgs[0].Messages {
		o.processMessage(ctx, publisher, msg)
	}
}

// processMessage processes a single message from the stream
func (o *RedisOutbox) processMessage(ctx context.Context, publisher KafkaPublisher, msg redis.XMessage) {
	var event JuiceFsEvent
	dataField := msg.Values["data"]
	if dataField == nil {
		logger.Warnf("message %s missing data field", msg.ID)
		o.deleteMessage(ctx, msg.ID)
		return
	}

	data, ok := dataField.(string)
	if !ok {
		logger.Warnf("message %s data field is not string", msg.ID)
		o.deleteMessage(ctx, msg.ID)
		return
	}

	if err := json.Unmarshal([]byte(data), &event); err != nil {
		logger.Errorf("unmarshal event error: %v", err)
		o.addRetryCount(ctx, msg.ID)
		return
	}

	// Apply filter rules
	filtered := FilterEvent(&event)
	if filtered == nil {
		// Event filtered out — XACK to remove from PEL
		o.deleteMessage(ctx, msg.ID)
		return
	}

	// Publish filtered/transformed event to Kafka
	if err := publisher.Publish(ctx, filtered); err != nil {
		logger.Errorf("publish to kafka error: %v", err)
		o.addRetryCount(ctx, msg.ID)
		return
	}

	// Acknowledge the message
	o.deleteMessage(ctx, msg.ID)
}

// addRetryCount increments the retry count for a message
func (o *RedisOutbox) addRetryCount(ctx context.Context, msgID string) {
	key := fmt.Sprintf("%s:retry:%s", o.stream, msgID)
	count := o.client.Incr(ctx, key).Val()
	o.client.Expire(ctx, key, 24*time.Hour)

	logger.Warnf("message %s failed, retry count: %d", msgID, count)

	if count >= int64(o.config.MaxRetries) {
		o.moveToDeadQueue(ctx, msgID)
	}
}

// moveToDeadQueue moves a message to the dead letter queue
func (o *RedisOutbox) moveToDeadQueue(ctx context.Context, msgID string) {
	// Get the message data
	msgs, err := o.client.XRange(ctx, o.stream, msgID, msgID).Result()
	if err != nil || len(msgs) == 0 {
		logger.Errorf("failed to get message %s for dead queue: %v", msgID, err)
		return
	}

	// Add to dead queue
	err = o.client.XAdd(ctx, &redis.XAddArgs{
		Stream: o.deadQueue,
		Values: map[string]interface{}{
			"data":     msgs[0].Values["data"],
			"retry_at": time.Now().Format(time.RFC3339),
			"msg_id":   msgID,
		},
	}).Err()
	if err != nil {
		logger.Errorf("failed to add message %s to dead queue: %v", msgID, err)
	} else {
		logger.Warnf("message %s moved to dead queue after %d retries", msgID, o.config.MaxRetries)
	}

	// Delete from original stream
	o.deleteMessage(ctx, msgID)
}

// deleteMessage acknowledges and deletes a message from the stream
func (o *RedisOutbox) deleteMessage(ctx context.Context, msgID string) {
	// Acknowledge the message
	_, err := o.client.XAck(ctx, o.stream, o.group, msgID).Result()
	if err != nil {
		logger.Errorf("xack error for message %s: %v", msgID, err)
	}
}
