# Delta: domain-outbox

## MODIFIED Requirements

### Requirement: Event model

The system SHALL define exactly seven filesystem event types (`FileCreated`, `FileDeleted`,
`FileMoved`, `FileWritten`, `DirCreated`, `DirDeleted`, `DirMoved`) and serialize each as a
`JuiceFsEvent` JSON object with fields `type`, `timestamp`, `volume`, `uid`, `gid`, `subdir`
(omitted when empty), `inode`, `parent`, `name`, `path` (omitted when empty), `old_path`
(omitted when empty, set for move events), `size` (omitted when zero, set for write events),
`mode` (omitted when zero, set for create events), plus extension fields: `uuid` (string, RFC 4122,
omitted when empty), `mtime` (int64, Unix nanoseconds, omitted when zero), `xattrs`
(map[string]string, omitted when empty).

Поле `xattrs` SHALL содержать только ключи из whitelist-константы (текущий состав: единственный
ключ `s3-meta`); полный дамп xattr inode в событии запрещён.

#### Scenario: Move event carries both paths

- **WHEN** a file or directory is renamed/moved
- **THEN** the emitted event SHALL have type `FileMoved`/`DirMoved`, `path` set to the new full path and `old_path` set to the previous full path

#### Scenario: Move event carries both paths and metadata snapshot

- **WHEN** консюмер разбирает событие `FileMoved`/`DirMoved`, эмитированное при включённом outbox
  и наличии у inode xattr `s3-meta`
- **THEN** JSON содержит `path` и `old_path`, а также `xattrs` с ключом `s3-meta` и исходным
  значением

#### Scenario: Legacy JSON unmarshals with zero extension fields

- **WHEN** консюмер разбирает JSON события без полей `uuid`/`mtime`/`xattrs` (событие, записанное
  до этого change)
- **THEN** unmarshal завершается успешно, поля расширения принимают нулевые значения, обработка
  продолжается

#### Scenario: Extended JSON is ignored by old consumers

- **WHEN** потребитель, собранный до этого change, получает JSON с полями `uuid`/`mtime`/`xattrs`
- **THEN** неизвестные поля игнорируются (поведение encoding/json), обработка не прерывается

Verified-by: pkg/meta/redis_event_test.go::TestEventJSONBackwardCompatibility

### Requirement: Event emission from meta operations

The `redisMeta` engine SHALL publish events for the following operations, writing each event into
the same Redis pipeline as the metadata mutation: `doMknod` → `FileCreated` or `DirCreated`
(with `mode & 0777`), `doUnlink` → `FileDeleted`, `doRmdir` → `DirDeleted`, `doRename` →
`FileMoved` or `DirMoved`, `doWrite`/`doFallocate` → `FileWritten` (with resulting file length).
Event emission SHALL be a no-op when the outbox is disabled or not configured. Events for inodes
at or above `TrashInode` SHALL NOT be published.

Эмиссия каждого события SHALL заполнять `uuid` (генерация `uuid.New`) и `mtime` (Unix nanoseconds,
вычисляемый из `Attr.Mtime` — секунды — и `Attr.Mtimensec`). Эмиссия Moved SHALL включать снимок
xattr `s3-meta` (whitelist), читаемый внеконвейерным чтением в том же стиле, что
`reconstructPath`. `doSetXattr`/`doRemoveXattr` событий НЕ эмитируют; новые типы событий для
xattr-операций не вводятся.

#### Scenario: Rename of a directory publishes DirMoved

- **WHEN** `doRename` moves a directory entry from `(parentSrc, nameSrc)` to `(parentDst, nameDst)`
- **THEN** exactly one `DirMoved` event SHALL be added to the pipeline with the directory inode, destination parent/name and both reconstructed paths

#### Scenario: Outbox disabled suppresses all events

- **WHEN** the outbox is not enabled (`--outbox-enabled` absent)
- **THEN** meta operations SHALL complete without any XADD to the outbox stream

#### Scenario: UUID is generated for every emitted event

- **WHEN** выполняется любая из перечисленных meta-операций при включённом outbox
- **THEN** событие в стриме содержит непустой `uuid`

#### Scenario: Write event carries mtime in nanoseconds

- **WHEN** `doWrite` выполняется при включённом outbox
- **THEN** событие `FileWritten` содержит `mtime`, равный Unix-времени записи в наносекундах
  (`Attr.Mtime`×10⁹ + `Attr.Mtimensec`)

#### Scenario: Rename publishes s3-meta snapshot

- **WHEN** `doRename` выполняется при включённом outbox и inode-источник имеет xattr `s3-meta`
- **THEN** эмитированное событие Moved содержит `xattrs["s3-meta"]` с исходным значением, а
  трансформация FilterEvent moved-from-tmp → Created сохраняет `xattrs`

#### Scenario: No s3-meta means omitted xattrs

- **WHEN** `doRename` выполняется при включённом outbox и xattr `s3-meta` у inode отсутствует
- **THEN** поле `xattrs` в JSON события отсутствует (omitempty)

#### Scenario: Xattr operations emit nothing

- **WHEN** выполняются `doSetXattr` или `doRemoveXattr`
- **THEN** в outbox-стрим не добавляется ни одного события

Verified-by: pkg/meta/redis_event_test.go::TestRedisRenamePublishesDirMoved
Verified-by: pkg/meta/redis_event_test.go::TestRedisRenamePublishesFileMoved
Verified-by: pkg/meta/redis_event_test.go::TestRedisRenameTopLevelDirExactPaths
Verified-by: pkg/meta/redis_event_test.go::TestEventUUIDGenerated
Verified-by: pkg/meta/redis_event_test.go::TestWritePublishesMtime
Verified-by: pkg/meta/redis_event_test.go::TestRenamePublishesS3MetaSnapshot
Verified-by: pkg/meta/redis_event_test.go::TestSetXattrEmitsNoEvent

### Requirement: Consumer group and polling

The consumer SHALL poll with `XREADGROUP` using consumer name `juicefs`, only-new-message id `>`,
batch count 100, block timeout 1 second, driven by a 1-second ticker; a consumer group SHALL be
initialized (creating the stream if absent) starting from the latest message (`$`); starting a
second consumer on the same outbox instance SHALL return an error.

Цикл консюмера SHALL дополнительно периодически выполнять `XAutoClaim` с `MinIdle` из флага
`--reclaim-min-idle` (default 60s) и передавать reclaimed-сообщения в общий путь обработки
(`processMessage`).

#### Scenario: Group created at latest offset

- **WHEN** the consumer group does not exist yet
- **THEN** it SHALL be created with start id `$` so that pre-existing stream messages are not replayed

#### Scenario: Stale pending message is reclaimed

- **WHEN** сообщение находится в PEL дольше `--reclaim-min-idle` (например, консюмер упал между
  чтением и ack)
- **THEN** оно забирается `XAutoClaim` и обрабатывается заново тем же путём, что новые сообщения

#### Scenario: Messages survive consumer restart

- **WHEN** процесс консюмера перезапускается, оставив необработанные сообщения в PEL
- **THEN** после старта эти сообщения reclam'ятся и обрабатываются (at-least-once)

Verified-by: pkg/meta/redis_outbox_test.go::TestInitConsumerGroupNoStream
Verified-by: pkg/meta/redis_outbox_test.go::TestReclaimPendingMessages

### Requirement: Message processing and retry

For each consumed message the system SHALL: discard (acknowledge) messages whose `data` field is
missing or not a string; increment a per-message retry counter (Redis key `<stream>:retry:<msgID>`,
TTL 24 hours) when JSON unmarshaling fails or sink publishing fails; acknowledge the message after
successful publish. When the retry counter reaches the configured maximum, the message SHALL be
moved to the dead-letter queue. В s3-режиме XACK также выполняется после успешной записи события в
спилл-буфер (ack-after-spill). `moveToDeadQueue` SHALL после XADD в стрим `<stream>:dead`
подтверждать исходное сообщение (XACK), исключая повторную обработку из PEL.

#### Scenario: Publish failure increments retry counter

- **WHEN** publishing a valid event to the sink fails
- **THEN** the retry counter for that stream message id SHALL be incremented and the message SHALL remain pending in the consumer group

#### Scenario: Retry limit moves message to dead letter

- **WHEN** the retry counter for a message reaches `MaxRetries`
- **THEN** the message payload SHALL be moved to the dead-letter queue

#### Scenario: Spill substitutes for sink on failure

- **WHEN** публикация в sink завершилась ошибкой и событие успешно записано в спилл-буфер
- **THEN** сообщение подтверждается (XACK), а событие присутствует в спилл-каталоге

#### Scenario: Dead-letter move acks the original

- **WHEN** счётчик ретраев сообщения достигает `MaxRetries`
- **THEN** payload дописывается в `<stream>:dead` с полями `data`, `retry_at`, `msg_id` И исходное
  сообщение подтверждается (XACK)

Verified-by: pkg/meta/s3_sink_test.go::TestSpillOnSinkUnavailable
Verified-by: pkg/meta/redis_outbox_test.go::TestMoveToDeadQueueAcksOriginal

### Requirement: Standalone outbox consumer command

The CLI SHALL provide a standalone `juicefs outbox REDIS-URL` command that connects to Redis,
initializes the consumer group, starts the consume loop publishing to the selected sink, and
serves an optional HTTP health endpoint. Existing flags and defaults are preserved:
`--outbox-stream` (`juicefs:outbox`), `--group` (`meta-proxy-outbox`), `--kafka-brokers`
(required, comma-separated), `--kafka-topic` (`juicefs.events`), `--kafka-user`,
`--kafka-password`, `--kafka-cert`, `--kafka-cert-file`, `--kafka-insecure-skip-verify`,
`--max-retries` (10), `--trim-max-len` (10000), `--health-addr` (`:9100`). Команда SHALL
поддерживать флаг `--sink kafka|s3`
(default `kafka`, env `JFS_OUTBOX_SINK`) и набор `--s3-endpoint`, `--s3-bucket`,
`--s3-access-key`, `--s3-secret-key`, `--s3-session-token`, `--s3-prefix` (default `events/`),
`--s3-region`, `--s3-storage` (default `minio`), `--s3-spill-dir` (default пусто = спилл
выключен), `--s3-spill-max-size` (default 1073741824 байт) с env-алиасами `JFS_S3_*`, и флаг
`--reclaim-min-idle` (default 60s, env `JFS_RECLAIM_MIN_IDLE`).

Валидация на старте: `sink=kafka` → `--kafka-brokers` обязателен; `sink=s3` → `--s3-endpoint` и
`--s3-bucket` обязательны, kafka-флаги не требуются; значение `--sink` вне {kafka, s3} → ошибка
старта. Хранилище конструируется через `object.CreateStorage` с bucket в path endpoint'а.

Health endpoint: kafka-режим — семантика без изменений (200 при running и успешном Redis PING,
503 иначе). S3-режим — 200 при running, успешном Redis PING и отсутствии backpressure; 503 —
иначе. The command SHALL stop the consumer gracefully on SIGINT/SIGTERM; graceful shutdown не
удаляет спилл-данные.

#### Scenario: Health endpoint reflects consumer state

- **WHEN** the consumer is running and Redis ping succeeds (kafka mode, unchanged semantics)
- **THEN** `GET /health` SHALL return 200 with body `ok`; otherwise it SHALL return 503 with body `unhealthy`

#### Scenario: Default invocation behaves as kafka mode

- **WHEN** команда запускается без `--sink`
- **THEN** поведение идентично режиму kafka, включая обязательность `--kafka-brokers`

#### Scenario: S3 mode requires endpoint and bucket

- **WHEN** команда запускается с `--sink s3` без `--s3-endpoint` или без `--s3-bucket`
- **THEN** команда завершается ошибкой валидации до начала консюмирования

#### Scenario: Unknown sink value is rejected

- **WHEN** `--sink` принимает значение вне {kafka, s3}
- **THEN** команда завершается ошибкой старта

#### Scenario: Backpressure flips health to 503

- **WHEN** s3-режим находится в backpressure (спилл-лимит исчерпан, sink недоступен)
- **THEN** `GET /health` возвращает 503; после восстановления sink и дренажа спилла — 200

Verified-by: pkg/meta/redis_outbox_test.go::TestRedisOutboxIsHealthy
Verified-by: cmd/outbox_test.go::TestValidateSinkFlags

### Requirement: Mount integration

The `mount` command SHALL accept `--outbox-enabled` (boolean) and `--outbox-stream` (default
`juicefs:outbox`) flags. When enabled, the mounted `redisMeta` instance SHALL write events to
the stream as part of meta operations; the mount process SHALL NOT run the Kafka consumer —
forwarding to the sink is performed exclusively by the standalone `outbox` command.

The `gateway` command SHALL accept the same `--outbox-enabled` and `--outbox-stream` flags; when
enabled, the gateway's `redisMeta` instance SHALL write events to the stream for operations
performed through the S3 gateway (PUT с метаданными `X-Amz-Meta-*`, DELETE, move). Consumer
forwarding logic из mount-интеграции применяется без изменений.

#### Scenario: Mount writes, standalone command forwards

- **WHEN** a volume is mounted with `--outbox-enabled` and a separate `juicefs outbox` process runs against the same Redis
- **THEN** file operations on the mount SHALL appear in the stream and be forwarded to the sink by the standalone consumer

#### Scenario: Gateway PUT with user metadata emits events

- **WHEN** gateway запущен с `--outbox-enabled` и через S3 API выполняется PUT с заголовками
  `X-Amz-Meta-*`
- **THEN** rename tmp → final path в gateway-процессе эмитит событие с `xattrs["s3-meta"]`,
  и standalone-консюмер доставляет его в sink

## ADDED Requirements

### Requirement: S3 object sink

The system SHALL provide an S3 object sink implementing the `EventSink` interface
(`Publish(ctx, *JuiceFsEvent) error`): каждое отфильтрованное событие записывается одним
JSON-объектом (`JuiceFsEvent` as-is); ключ объекта —
`<prefix>/<YYYY-MM-DD>/<HH>/<uuid>.json`, где `<prefix>` — значение `--s3-prefix` без
завершающего слэша (default `events/`), а дата и час вычисляются из `timestamp` события в
UTC. Если событие не содержит `uuid` (legacy-окно апгрейда),
sink SHALL сгенерировать uuid до первой попытки доставки этого сообщения. Ошибки
`ObjectStorage.Put` возвращаются вызывающему; процесс не завершается.

#### Scenario: Object key format

- **WHEN** S3Sink публикует событие с uuid и timestamp
- **THEN** ключ объекта соответствует `^events/\d{4}-\d{2}-\d{2}/\d{2}/[0-9a-f-]{36}\.json$`
  (для default prefix `events/`), а тело — валидный JSON `JuiceFsEvent`

#### Scenario: UUID generated for legacy events

- **WHEN** событие без `uuid` доходит до S3Sink
- **THEN** ключ объекта содержит сгенерированный uuid, уникальный для разных сообщений

#### Scenario: Sink error does not kill the process

- **WHEN** `ObjectStorage.Put` возвращает ошибку
- **THEN** `Publish` возвращает ошибку вызывающему, процесс продолжает работать

Verified-by: pkg/meta/s3_sink_test.go::TestS3SinkKeyFormat
Verified-by: pkg/meta/s3_sink_test.go::TestS3SinkGeneratesUUIDWhenAbsent
Verified-by: pkg/meta/s3_sink_test.go::TestS3SinkPutErrorReturned

### Requirement: S3 spill and backpressure

При недоступности sink и включённом `--s3-spill-dir` событие SHALL дописываться одной
JSONL-строкой в открытый чанк спилл-каталога, после чего сообщение подтверждается
(ack-after-spill: событие либо целиком в чанке и ack'нуто, либо не записано и остаётся в PEL;
оборванный хвост при крае отбрасывается дрением с WARN и ределиверится). Новый чанк создаётся
атомарно (tmp+rename); чанк ротируется при достижении порога ротации. Суммарный размер спилла
ограничен `--s3-spill-max-size`. При
исчерпании лимита sink SHALL переходить в backpressure: консюмер перестаёт читать новые
сообщения, выполнять reclaim-проход и подтверждать сообщения, `/health` возвращает 503, в лог
пишется ERROR, процесс не завершается, события не отбрасываются. После восстановления sink спилл
SHALL дренироваться в S3 (FIFO по
файлам; дренаж попытками из цикла консюмера — каждый тик при непустом спилле и после каждого
успешного Publish; позиция частичного дренажа отслеживается курсором, при неудаче чанк не
перезаписывается; после рестарта курсор сбрасывается и дренаж идёт с начала чанка — повторная
запись уже дренированных объектов идемпотентна по uuid-ключу). Backpressure SHALL сниматься
только фактом успешного Put: при пустом спилле попытка дренажа выполняет probe-Put объекта
`<prefix>/.probe/<uuid>.json` (вне date-префикса — не является событием, игнорируется
drainer'ом, чистится lifecycle-правилом префикса); флаг backpressure не снимается
«пустым» дренажем без доказательства работоспособности sink. При выключенном спилле
недоставка оставляет сообщение в PEL с ретраями (поллинг/reclaim), без завершения процесса.

#### Scenario: Spill on sink unavailability

- **WHEN** `Put` завершается ошибкой и спилл включён
- **THEN** событие появляется в спилл-каталоге как валидная JSONL-строка, сообщение ack'ается

#### Scenario: Spill disabled keeps message pending

- **WHEN** `Put` завершается ошибкой и `--s3-spill-dir` пуст
- **THEN** сообщение остаётся в PEL и ретраится, процесс продолжает работать

#### Scenario: Overflow triggers backpressure

- **WHEN** размер спилла достигает `--s3-spill-max-size` при недоступном sink
- **THEN** sink входит в backpressure, `/health` возвращает 503, процесс не завершается

#### Scenario: Recovery drains spill

- **WHEN** sink восстанавливается после периода спилла
- **THEN** спилл-файлы дренируются в объекты с исходными ключами и backpressure снимается

Verified-by: pkg/meta/s3_sink_test.go::TestSpillOnSinkUnavailable
Verified-by: pkg/meta/s3_sink_test.go::TestSpillOverflowBackpressure
Verified-by: pkg/meta/s3_sink_test.go::TestSpillDrainAfterRecovery
