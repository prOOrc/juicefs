# domain-outbox

## Purpose

Публикация событий файловой системы (create/delete/move/write) через Redis Streams в Kafka: meta-слой JuiceFS пишет события в stream атомарно с meta-операцией, standalone-консьюмер пересылает их в Kafka-топик с retry и dead-letter очередью.

## ADDED Requirements

### Requirement: Event model

The system SHALL define exactly seven filesystem event types: `FileCreated`, `FileDeleted`, `FileMoved`, `FileWritten`, `DirCreated`, `DirDeleted`, `DirMoved`. Each event SHALL be serialized as a `JuiceFsEvent` JSON object with fields `type`, `timestamp`, `volume`, `uid`, `gid`, `subdir` (omitted when empty), `inode`, `parent`, `name`, `path` (omitted when empty), `old_path` (omitted when empty, set for move events), `size` (omitted when zero, set for write events), `mode` (omitted when zero, set for create events).

#### Scenario: Move event carries both paths

- **WHEN** a file or directory is renamed/moved
- **THEN** the emitted event SHALL have type `FileMoved`/`DirMoved`, `path` set to the new full path and `old_path` set to the previous full path

### Requirement: Event emission from meta operations

The `redisMeta` engine SHALL publish events for the following operations, writing each event into the same Redis pipeline as the metadata mutation: `doMknod` → `FileCreated` or `DirCreated` (with `mode & 0777`), `doUnlink` → `FileDeleted`, `doRmdir` → `DirDeleted`, `doRename` → `FileMoved` or `DirMoved`, `doWrite` → `FileWritten` (with resulting file length), `doFallocate` → `FileWritten` (with resulting file length). Event emission SHALL be a no-op when the outbox is disabled or not configured. Events for inodes at or above `TrashInode` (internal trash operations) SHALL NOT be published.

Verified-by: pkg/meta/redis_event_test.go::TestRedisRenamePublishesDirMoved
Verified-by: pkg/meta/redis_event_test.go::TestRedisRenamePublishesFileMoved
Verified-by: pkg/meta/redis_event_test.go::TestRedisRenameTopLevelDirExactPaths

#### Scenario: Rename of a directory publishes DirMoved

- **WHEN** `doRename` moves a directory entry from `(parentSrc, nameSrc)` to `(parentDst, nameDst)`
- **THEN** exactly one `DirMoved` event SHALL be added to the pipeline with the directory inode, destination parent/name and both reconstructed paths

#### Scenario: Outbox disabled suppresses all events

- **WHEN** the outbox is not enabled (`--outbox-enabled` absent)
- **THEN** meta operations SHALL complete without any XADD to the outbox stream

### Requirement: Event path reconstruction

The system SHALL reconstruct the full POSIX path for each event by walking up the parent chain (attribute lookup + directory entry scan) from the event's inode. When the parent or name cannot be resolved, the `path` field SHALL be empty rather than partial.

#### Scenario: Unresolvable path yields empty path

- **WHEN** an ancestor attribute or directory entry cannot be read during reconstruction
- **THEN** the event SHALL still be published with an empty `path` field

### Requirement: Redis stream writing

Each event SHALL be appended to the configured Redis stream (default name `juicefs:outbox`) as a single field `data` containing the JSON payload, followed by `XTRIM MAXLEN` with the configured trim length to bound the stream. The dead-letter stream name SHALL be `<stream>:dead`.

#### Scenario: Stream is bounded after each write

- **WHEN** an event is added to the stream
- **THEN** the same pipeline SHALL also trim the stream to the configured maximum length

### Requirement: Consumer group and polling

The consumer SHALL initialize a Redis consumer group on the stream (creating the stream if absent) starting from the latest message (`$`). The consume loop SHALL poll with `XREADGROUP` using consumer name `juicefs`, only-new-message id `>`, batch count 100, block timeout 1 second, driven by a 1-second ticker. Starting a second consumer on the same outbox instance SHALL return an error.

Verified-by: pkg/meta/redis_outbox_test.go::TestInitConsumerGroupNoStream

#### Scenario: Group created at latest offset

- **WHEN** the consumer group does not exist yet
- **THEN** it SHALL be created with start id `$` so that pre-existing stream messages are not replayed

### Requirement: Message processing and retry

For each consumed message the system SHALL: discard (acknowledge) messages whose `data` field is missing or not a string; increment a per-message retry counter (Redis key `<stream>:retry:<msgID>`, TTL 24 hours) when JSON unmarshaling fails or Kafka publishing fails; acknowledge the message after successful publish. When the retry counter reaches the configured maximum, the message SHALL be moved to the dead-letter queue.

#### Scenario: Publish failure increments retry counter

- **WHEN** publishing a valid event to Kafka fails
- **THEN** the retry counter for that stream message id SHALL be incremented and the message SHALL remain pending in the consumer group

#### Scenario: Retry limit moves message to dead letter

- **WHEN** the retry counter for a message reaches `MaxRetries`
- **THEN** the message payload SHALL be appended to the dead-letter stream and the original message acknowledged

### Requirement: Dead-letter queue

Messages exceeding the retry limit SHALL be written to the `<stream>:dead` stream with fields `data` (original payload), `retry_at` (RFC3339 timestamp of the move) and `msg_id` (original stream message id).

#### Scenario: Dead letter preserves original payload

- **WHEN** a message is moved to the dead-letter queue
- **THEN** the dead-letter entry SHALL contain the unmodified original `data` payload plus `retry_at` and `msg_id`

### Requirement: Event filtering

Before publishing to Kafka, each event SHALL pass through `FilterEvent` with first-match-wins rules: (1) events whose path starts with `/.minio.sys/` or `/.sys/` are dropped; (2) non-move events under `/.sys/tmp/` are dropped; (3) `FileMoved`/`DirMoved` events whose `old_path` starts with `/.sys/tmp/` are transformed into `FileCreated`/`DirCreated` (all fields preserved except `old_path`); (4) all other events pass through unchanged. Dropped and transformed events SHALL be acknowledged in the stream. These rules are temporary MinIO Gateway compatibility rules.

Verified-by: pkg/meta/event_filter_test.go::TestFilterEvent
Verified-by: pkg/meta/event_filter_test.go::TestFilterEvent_TransformPreservesFields

#### Scenario: S3 PUT via tmp rename surfaces as FileCreated

- **WHEN** a file is moved from `/.sys/tmp/...` to its final path (MinIO S3 PUT pattern)
- **THEN** the event published to Kafka SHALL have type `FileCreated` with the final path and no `old_path`

#### Scenario: MinIO internal paths are never published

- **WHEN** an event path starts with `/.minio.sys/` or `/.sys/`
- **THEN** no event SHALL be published to Kafka for it

### Requirement: Kafka publishing

Events SHALL be published to a single fixed Kafka topic via Watermill + Sarama with `WaitForAll` acks and LZ4 compression. The message name SHALL be `JuiceFsEvent`; the partition key SHALL be `<volume>:<subdir><path>`. Optional SASL authentication SHALL use SCRAM-SHA512; optional TLS SHALL accept a base64-encoded certificate or a certificate file path with an insecure-skip-verify flag. Creating a publisher without brokers or with an empty topic SHALL return an error.

#### Scenario: Partition key groups events by volume and path

- **WHEN** an event is published to Kafka
- **THEN** its partition key metadata SHALL equal the concatenation of volume, `:`, subdir and path

### Requirement: Standalone outbox consumer command

The CLI SHALL provide a standalone `juicefs outbox REDIS-URL` command that connects to Redis, initializes the consumer group, starts the consume loop publishing to Kafka, and serves an optional HTTP health endpoint. Flags and defaults: `--outbox-stream` (`juicefs:outbox`), `--group` (`meta-proxy-outbox`), `--kafka-brokers` (required, comma-separated), `--kafka-topic` (`juicefs.events`), `--kafka-user`, `--kafka-password`, `--kafka-cert`, `--kafka-cert-file`, `--kafka-insecure-skip-verify`, `--max-retries` (10), `--trim-max-len` (10000), `--health-addr` (`:9100`). The health endpoint SHALL return 200 when the consumer is running and Redis is reachable, 503 otherwise. The command SHALL stop the consumer gracefully on SIGINT/SIGTERM.

Verified-by: pkg/meta/redis_outbox_test.go::TestRedisOutboxIsHealthy

#### Scenario: Health endpoint reflects consumer state

- **WHEN** the consumer is running and Redis ping succeeds
- **THEN** `GET /health` SHALL return 200 with body `ok`; otherwise it SHALL return 503 with body `unhealthy`

### Requirement: Mount integration

The `mount` command SHALL accept `--outbox-enabled` (boolean) and `--outbox-stream` (default `juicefs:outbox`) flags. When enabled, the mounted `redisMeta` instance SHALL write events to the stream as part of meta operations; the mount process SHALL NOT run the Kafka consumer — forwarding to Kafka is performed exclusively by the standalone `outbox` command.

#### Scenario: Mount writes, standalone command forwards

- **WHEN** a volume is mounted with `--outbox-enabled` and a separate `juicefs outbox` process runs against the same Redis
- **THEN** file operations on the mount SHALL appear in the stream and be forwarded to Kafka by the standalone consumer
