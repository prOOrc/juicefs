# Design: add-outbox-s3-sink

## Architectural Context

Форк juicefs (ветка `agio-drive-v2`): outbox-конвейер = redisMeta эмитит события в Redis Stream
атомарно с meta-операцией (`AddEventToPipe`: XADD поле `data` + XTRIM MAXLEN), standalone-команда
`juicefs outbox` (cmd/outbox.go) через `RedisOutbox.StartConsumer` читает `XREADGROUP` и публикует
в Kafka через `NewWatermillKafkaPublisherWithConfig` (pkg/meta/watermill_kafka.go). Точка
расширения — интерфейс `KafkaPublisher.Publish(ctx, *JuiceFsEvent)` (pkg/meta/redis_outbox.go:32-34).

CRSS change 0c: добавить S3 bucket-sink (события = JSON-объекты под `events/`), починить доставку
(at-least-once: reclaim PEL, спилл-буфер с backpressure) и расширить контракт события
(uuid/mtime/xattrs) как кросс-репо договор с drainer (agio-platform change 2). Мотивация — в
proposal.md (раздел Why), здесь не повторяется.

Граница мета-движков: эмиссия существует только в redisMeta (pkg/meta/redis_event.go, build-tag
`!noredis`); SQL/KV движки события не эмитят. Это существующее ограничение, данным change'ом оно
сознательно НЕ расширяется — parity Redis/SQL/KV для outbox не требуется (outbox-спека исторически
redis-only).

## Component Map

| Компонент | Файл | Изменение |
|---|---|---|
| `cmdOutbox` / `outboxConsumer` | cmd/outbox.go | флаг `--sink` + `--s3-*` + `--reclaim-min-idle` с env-алиасами, валидация режима (kafka-brokers теряет `Required: true` в urfave/cli-определении, валидируется по режиму), ветвление конструктора sink (строка :191), health через `SinkHealthChecker` |
| `EventSink` (rename из `KafkaPublisher`), `RedisOutbox`, `consumeLoop`, `pollAndPublish`, `processMessage`, `moveToDeadQueue` | pkg/meta/redis_outbox.go | rename интерфейса; reclaim-проход `XAutoClaim`; ack после Put или спилла (логика ack — в `processMessage`, исход — из возвращённой ошибки sink'а). XACK оригинала в `moveToDeadQueue` уже реализован (вызов deleteMessage/XAck) — поведение фиксируется требованием и новым тестом, код не меняется |
| `S3Sink`, `S3SinkConfig`, `NewS3Sink`, `SinkHealthChecker` | pkg/meta/s3_sink.go | NEW: Put объекта, именование ключей, uuid-шим для legacy-событий, спилл (JSONL, tmp+rename, лимит), дренаж FIFO, backpressure, `Health() error` |
| `JuiceFsEvent` | pkg/meta/events.go | поля `uuid string`, `mtime int64`, `xattrs map[string]string` (все omitempty) |
| `publishEventToPipe`, `publishFileMovedToPipe`, `publishDirMovedToPipe` (+ остальные хелперы) | pkg/meta/redis_event.go | генерация uuid; mtime из `Attr.Mtime`/`Attr.Mtimensec`; общий хелпер снимка `s3-meta` (whitelist), вызываемый из обоих Moved-хелперов |
| точки эмиссии | pkg/meta/redis.go (doRename :2787/:2789, doWrite :3226, doFallocate :1372, doMknod, doUnlink, doRmdir) | проброс Attr в точки эмиссии для mtime; снятие xattr — внутри Moved-хелперов |
| `FilterEvent` | pkg/meta/event_filter.go | правила фильтрации и семантика не меняются; литерал трансформации moved-from-tmp → Created выравнивается с SoT («all fields preserved except `old_path`»): перенос `Size` (сейчас теряется — дрейф от SoT) и новых полей `uuid`/`mtime`/`xattrs` |
| `WatermillKafkaPublisher` | pkg/meta/watermill_kafka.go | без изменений (реализует EventSink неявно, имя метода Publish совпадает) |
| object-слой | pkg/object (CreateStorage, minio-драйвер) | без изменений; использование по идиоме cmd/sync.go:425-443 (bucket в path endpoint'а) |
| доки | docs/en/deployment/outbox.md | режим s3, флаги/env, именование объектов, спилл, backpressure, health, схема потока |
| e2e | docker-compose.outbox-s3-test.yml | NEW: redis + minio (network_mode: host, конвенция docker-compose.test.yml) |

## Execution Flow

S3-режим (`juicefs outbox --sink s3 REDIS-URL`):

1. `cmdOutbox` парсит флаги/env → валидация (s3: `--s3-endpoint`+`--s3-bucket` required; kafka:
   `--kafka-brokers` required; неизвестный `--sink` → ошибка старта).
2. Конструирование: `meta.CreateRedisClient(redisURL)` + Ping; хранилище =
   `object.CreateStorage(cfg.Storage, endpoint+"/"+bucket, ak, sk, token)` (bucket в path, как в
   cmd/sync.go:425-443; `--s3-region` прокидывается установкой `MINIO_REGION` для конструктора);
   `meta.NewS3Sink(cfg, storage)`; `outbox.InitConsumerGroup(ctx)`; `outbox.StartConsumer(sink)`;
   HTTP health-сервер.
3. `consumeLoop` (тик 1s): `pollAndPublish` (`XREADGROUP >`, Count 100, Block 1s) + reclaim-проход
   `XAutoClaim` (MinIdle = `--reclaim-min-idle`, Count 100) → оба потока в общий `processMessage`.
4. `processMessage`: unmarshal `data` → `FilterEvent` (nil → XACK, discard) → `sink.Publish`.
5. `S3Sink.Publish`: ключ `prefix/YYYY-MM-DD/HH/uuid.json` (UTC по `event.Timestamp`; для
   legacy-событий без uuid — генерация до первой попытки) → `storage.Put(ctx, key, JSON)`:
   - успех → попытка дренажа спилла (если есть) → nil → XACK;
   - ошибка Put + спилл включён и лимит не исчерпан → атомарная запись JSONL-чанка (tmp+rename),
     учёт размера → nil → XACK (ack-after-spill);
   - ошибка Put + спилл выключен или переполнен → error → сообщение остаётся в PEL,
     retry-счётчик растёт (reclaim вернёт сообщение), при исчерпании `MaxRetries` → DLQ + XACK.
6. Backpressure: флаг в `S3Sink` → `Health()` возвращает error → `/health` 503; consumeLoop
   останавливает и poll новых сообщений, и reclaim-проход (PEL не расходуется — сообщения не
   уезжают в DLQ стеллажом), но продолжает попытки дренажа спилла; восстановление Put → дренаж
   FIFO (частичный дренаж переписывает остаток) → снятие backpressure → 200.
7. SIGINT/SIGTERM: закрыть HTTP, `StopConsumer`; спилл-файлы остаются на диске и дренируются при
   следующем старте.

Kafka-режим: идентичен текущему, кроме: reclaim-проход добавлен (at-least-once становится
фактической); `moveToDeadQueue` — XACK оригинала фиксируется тестом (уже реализован);
`--kafka-brokers` валидируется в cmd (без `Required: true`); health-семантика не меняется.

## Invariants

1. At-least-once end-to-end; дубликаты возможны и сходятся: uuid-ключ объекта
   (overwrite-идемпотентность Put) + drainer latest-wins по (volume, path|inode) с `timestamp`
   как tiebreak.
2. Ошибки sink никогда не завершают процесс: в sink-пути нет panic/os.Exit (урок alpha:
   недоступность Kafka уложила релей в crash-loop).
3. `FilterEvent` выполняется до любого sink; правила фильтрации и семантика трансформации
   (moved-from-tmp → Created: все поля кроме `old_path` сохраняются — включая новые
   uuid/mtime/xattrs) не меняются; литерал трансформации выравнивается с SoT (сейчас `Size`
   не переносится — дрейф, фиксируется этим change).
4. JSON-контракт совместим в обе стороны: старый mount (без uuid/mtime/xattrs) читается новым
   outbox; расширенный JSON игнорируется неизвестные-поля-толерантными старыми потребителями
   (encoding/json).
5. `xattrs` в событии содержит только ключи whitelist (сейчас `s3-meta`); полный дамп xattr
   inode запрещён — защита от утечки ключевого материала шифрования (реализуемого change'ом
   implement-encrypt) в events-бакет.
6. Ack только после стойкого исхода: успешный Put, успешная запись спилла или перенос в DLQ.
7. Спилл переживает рестарт процесса; дренаж при следующем запуске.
8. Эмиссия — только redisMeta; SQL/KV не эмитят (сознательная граница).
9. В events-бакет попадают пользовательские метаданные объектов (X-Amz-Meta-*) — доступ к бакету
   ограничивается на стороне инфры (0a), это эксплуатационное требование, не кодовое.

## Decisions & Rationale

- **D1. Селектор `--sink kafka|s3`, default `kafka`.** Полная обратная совместимость CLI:
  существующие запуски без флага не меняют поведение. Параллельный флаг-набор `--s3-*` с env
  `JFS_S3_*` соответствует конвенции `JFS_KAFKA_*`/`JFS_OUTBOX_*`. Валидация — fail-fast на
  старте (конфиг-ошибка ≠ runtime-недоступность). `kafka-brokers` теряет `Required: true` в
  urfave/cli-определении (cmd/outbox.go:70-75) — валидируется по режиму (ручная проверка пустой
  строки уже есть на cmd/outbox.go:139-142, обобщается на оба режима).
- **D2. Контракт события: JSON `JuiceFsEvent` as-is.** Добавляются три omitempty-поля: `uuid`
  (генерация `uuid.New` в `publishEventToPipe`, продюсерская сторона — событие в стриме сразу
  несёт uuid), `mtime` (**Unix nanoseconds**, вычисляется из `Attr.Mtime` — секунды (redis.go:
  `attr.Mtime = now.Unix()`) — и `Attr.Mtimensec`), `xattrs map[string]string`. Поле `type` НЕ
  переименовывается в `op`: drainer получает JSON as-is, маппинг type→op — его ответственность.
  Вторая схема = второй источник рассинхронизации.
- **D3. xattr — вариант «снимок при rename», whitelist `s3-meta`.** Gateway-PUT всегда идёт
  tmp→rename (gateway.go putObject :925: `applyObjTaggingFunc` ставит etag/tags/s3-meta на tmp
  ДО Rename :985; multipart finalize и CopyObject — тот же порядок), т.е. вся gateway-метаданная
  достигает потребителя через FileMoved (трансформируемый FilterEvent'ом в FileCreated с
  сохранением полей). Один общий хелпер снимка, вызываемый из `publishFileMovedToPipe`
  (redis_event.go:102) и `publishDirMovedToPipe` (:115), покрывает PUT/CopyObject/multipart.
  Отвергнуты: (а) типы событий XattrSet/XattrRemoved — gateway ставит `s3-meta` на tmp-путь
  `/.sys/tmp/`, такое событие отфильтруется, полезной нагрузки для gateway не даёт; FUSE-клиента
  setfattr сейчас нет; внутренние xattr шифрования создали бы спам и риск утечки. (б) enrichment
  в sink-процессе по inode — требует meta-доступа в CLI-процессе и гонки с переиспользованием
  inode. Безопасность: снимок — строго чтение одного ключа `s3-meta` (whitelist-константа),
  никогда не полный дамп xattr. Чтение внеконвейерное (результаты из TxPipelined недоступны до
  EXEC), тем же стилем, что `reconstructPath` (:144-225) — профиль нагрузки не меняется.
- **D4. Именование объектов.** Ключ `<prefix>/<YYYY-MM-DD>/<HH>/<uuid>.json`, где `<prefix>` —
  значение `--s3-prefix` без завершающего слэша (default `events/`), дата-час из
  `event.Timestamp` в UTC (детерминизм между хостами). Часовой префикс = водяной знак drainer'а
  через LIST, дневной = единица lifecycle 7d (0a). Ключ на базе uuid делает повторную доставку
  идемпотентной (Put перезаписывает тот же объект). Один объект = одно событие (тело —
  `JuiceFsEvent` as-is); Content-Type не нормируем (Put/AttrGetter его не несёт, drainer
  использует LIST+GET).
- **D5. Доставка.** (i) Reclaim PEL (`XAutoClaim`, клиент go-redis v9.18.1 поддерживает, сервер
  Redis ≥ 6.2) — чинит подтверждённый латентный дефект: `pollAndPublish` читал только `>`,
  XPending/XAutoClaim отсутствовали, failed-сообщение навсегда оставалось в PEL и retry/DLQ были
  недостижимы. (ii) Спилл + ack-after-spill: `--s3-spill-dir` default пусто = выключен (без
  спилла: сообщение остаётся в PEL, ретраи через reclaim, процесс жив; риск обрезки головы
  XTRIM 10000 при долгой недоступности — документирован в Risks; CRSS-деплой 0a включает спилл
  явно). При Put-fail и включённом спилле: JSONL-чанк атомарной записью (tmp+rename) → ack.
  Обоснование ack-after-spill: uuid-ключи делают повторную доставку идемпотентной; спилл выносит
  данные из Redis до того, как XTRIM срежет голову; PEL остаётся коротким. Лимит
  `--s3-spill-max-size` default 1 GiB; переполнение → backpressure (НЕ drop-oldest): консюмер
  перестаёт брать новые, health 503, ERROR в лог, процесс жив — потерянный FileDeleted = вечный
  ghost-объект в реплике, тихая потеря недопустима. Дренаж: FIFO по файлам, порядок семантически
  не нужен (latest-wins у drainer), частичный дренаж переписывает остаток. (iii) Батчинг: чтение
  стрима уже батчево (Count 100); на S3 один объект на событие — batching не вводится (упрощение
  честное: ключ-uuid закрывает дедуп, объект-в-событие максимально прост для drainer'а).
- **D6. Интерфейс и размещение.** Rename `KafkaPublisher` → `EventSink` (grep: использования
  только redis_outbox.go и cmd/outbox.go; `WatermillKafkaPublisher` реализует интерфейс неявно).
  S3-sink — `pkg/meta/s3_sink.go`: meta-пакет уже держит `JuiceFsEvent`, outbox-конфиг и тесты;
  конструктор `NewS3Sink(cfg S3SinkConfig, storage object.ObjectStorage)` с инъекцией хранилища
  для мок-тестов (pkg/object не импортирует pkg/meta — цикла нет).
- **D7. Флаги S3.** `--sink` (env `JFS_OUTBOX_SINK`), `--s3-endpoint`*, `--s3-bucket`*,
  `--s3-access-key`, `--s3-secret-key`, `--s3-session-token`, `--s3-prefix` (default `events/`),
  `--s3-region`, `--s3-storage` (default `minio`), `--s3-spill-dir` (default пусто),
  `--s3-spill-max-size` (default 1073741824), `--reclaim-min-idle` (default 60s). Driver
  `--s3-storage` default `minio` (minio-драйвер CreateStorage: path-style по умолчанию, token
  поддержан, совместим с AWS/OSS S3-API; тот же driver в enc-test-compose), `--s3-path-style` не
  вводим (bucket всегда в path endpoint'а, virtual-hosted для внутреннего бакета не нужен);
  region прокидывается установкой `MINIO_REGION` в cmd-конструкторе (документировано).
  Отсутствие required в s3-режиме → ошибка старта с именем флага.
- **D8. Health.** Optional-интерфейс `SinkHealthChecker { Health() error }`; реализует только
  S3Sink; cmd-хендлер /health комбинирует `outbox.IsHealthy()` и type-assertion sink'а. S3: 503
  при (a) Redis PING fail, (b) not running, (c) backpressure; 200 пока спилл буферизует
  (by design — раньше сигналит рост спилла: мониторинг диска — эксплуатация 0a). Kafka-режим:
  семантика не тронута — нулевая регрессия. Недоступный sink без спилла выглядит healthy (200) —
  как в kafka-режиме сегодня; алерт по логам ошибок Put (см. Risks).
- **D9. Дельты спеки.** MODIFIED: Event model, Event emission from meta operations, Consumer
  group and polling, Message processing and retry, Standalone outbox consumer command. ADDED:
  S3 object sink, S3 spill and backpressure. Без дельт: Event path reconstruction, Redis stream
  writing, Dead-letter queue (поля не меняются), Event filtering, Kafka publishing, Mount
  integration.

## Risks / Trade-offs

- **Окно потери при backpressure**: backpressure защищает процесс, а не данные — при
  заполненном спилле консюмер не ack'ает, и `XTRIM MAXLEN 10000` на продюсере срежет голову
  стрима (PEL XTRIM'ом не защищён). Спилл (ack-after-spill) держит PEL коротким; остаточное окно
  = события сверх лимита спилла → закрывается алертом на 503 и мониторингом диска спилла (0a).
- **Reclaim меняет семантику kafka-режима**: возможны повторные доставки в топик
  `juicefs.events` (at-least-once становится фактической). Потребители (в т.ч. платформенный
  консюмер juicefs-event-consolidation) должны быть идемпотентны — claim в proposal
  (agio-platform). Консюмер консолидации применяет события как дельты latest-wins — семантика
  совместима, но подтверждение на стороне потребителя остаётся за владельцем.
- **Legacy-uuid**: события, уже лежащие в стриме на момент апгрейда, не несут uuid; sink
  генерирует его на попытке — при reclaim-повторе возможен другой uuid. Покрывается
  latest-wins; после апгрейда mount'ов все события несут продюсерский uuid.
- **Спилл default-off**: осознанно (явное включение в 0a-compose); риск деплоя без спилла
  закрывается claim'ом в proposal и документацией (5.1).
- **Недоступный sink без спилла выглядит healthy** (200): совпадает с текущим поведением
  kafka-режима; алерт по частоте ошибок Put в логах — эксплуатация (0a), в коде не решается.

## Migration

Порядок внедрения (без миграции данных): merge change → bump `JUICEFS_REV` в agio-cloud →
CI `juicefs-image-build` (новый уникальный тег) → 0a пинит тег в `var.hk_outbox_image_tag` и
разворачивает HK-compose с `--sink s3` + `--s3-spill-dir`. Существующие alpha/kafka-деплойменты
не трогаются (default `kafka`); повторные доставки после reclaim для них легализуются
идемпотентностью потребителей. Откат: возврат на предыдущий образ + `--sink kafka`.

## Integration Points

- pkg/object: `CreateStorage(name, endpoint, accessKey, secretKey, token string)` (object_storage.go:197-204);
  `Put(ctx, key, in io.Reader, getters ...AttrGetter)` (interface.go:90), API идемпотентно по
  ключу (:79).
- Redis: XADD/XREADGROUP/XACK/XTRIM (существующие) + `XAUTOCLAIM` (Redis-сервер ≥ 6.2; клиент
  github.com/redis/go-redis/v9 v9.18.1 — replace на форк prOOrc/go-redis v9.18.1 — метод
  поддерживает).
- UUID: github.com/google/uuid v1.6.0 (direct dep go.mod).
- CLI/env-конвенция: `JFS_OUTBOX_SINK`, `JFS_S3_ENDPOINT`, `JFS_S3_BUCKET`, `JFS_S3_ACCESS_KEY`,
  `JFS_S3_SECRET_KEY`, `JFS_S3_SESSION_TOKEN`, `JFS_S3_PREFIX`, `JFS_S3_REGION`,
  `JFS_S3_STORAGE`, `JFS_S3_SPILL_DIR`, `JFS_S3_SPILL_MAX_SIZE`, `JFS_RECLAIM_MIN_IDLE`.
- Makefile: `make test.meta.core` (:112-113) покрывает pkg/meta (Redis-зависимые тесты skip при
  отсутствии Redis); `test.enc.integration` (:119-123) — compose Redis+MinIO (референс для e2e).

## Cross-repo contract

Стороны: этот change (продюсер в events-бакет), agio-platform change 2 (drainer `events/` →
Redpanda), agio-terraform-alibaba change 0a (бакет + lifecycle + пин образа), agio-cloud (сборка
образа).

1. **JSON события** — `JuiceFsEvent` as-is:
   `type` (7 значений File*/Dir*), `timestamp` (RFC3339, `time.Time`), `volume`, `uid`, `gid`,
   `subdir`, `inode`, `parent`, `name`, `path`, `old_path`, `size`, `mode` — существующие поля,
   опускаются при пустых/нулевых значениях; новые: `uuid` (RFC 4122 string), `mtime` (int64,
   **Unix nanoseconds**), `xattrs` (map; единственный ключ `s3-meta` → значение = JSON-документ
   метаданных gateway: карта X-Amz-Meta-* ключей + whitelist cache-control/content-disposition/
   content-type). Маппинг type→op — ответственность drainer. Дубликаты: at-least-once, порядок не
   гарантируется; drainer применяет latest-wins по (volume, path|inode) с `timestamp` как
   tiebreak; `uuid` — для дедупа точных копий.
2. **Ключи объектов**: `<prefix>/<YYYY-MM-DD>/<HH>/<uuid>.json`, `<prefix>` = значение
   `--s3-prefix` без завершающего слэша (default `events/`), UTC по `timestamp` события.
   Drainer держит водяной знак LIST'ом по часовому префиксу;
   идемпотентность повторной выгрузки — по uuid в имени (overwrite).
3. **Lifecycle**: правило 7d по префиксу `events/` — change 0a (agio-terraform-alibaba). Drainer
   обязан читать быстрее 7d; ключи дня = единица ретенции.
4. **Образ**: bump `JUICEFS_REV` (docker/juicefs/Dockerfile, agio-cloud) на HEAD
   `agio-drive-v2` после мержа → CI-джоба `juicefs-image-build` → уникальный тег
   `agio-v1.4.x_<date><letter>_<rev>`; повторная публикация тега запрещена (manifest inspect
   guard). 0a пинит через `var.hk_outbox_image_tag` (ренейм переменной — на усмотрение
   0a-пропозала). Точный ref фиксируется в tasks.md этого change на apply-фазе.
5. **S3-доступ sink'а**: креды только через флаги/env (имена выше), секреты в артефактах не
   хранятся; HK-compose (0a) передаёт `--s3-spill-dir` (спилл включён явно).

## Open Questions

1. Расширение whitelist xattr (etag/tags объектов gateway) — отложено: потребителя нет;
   добавление ключа в whitelist-константу обратно совместимо.
2. Мониторинг заполнения спилла до порога backpressure (алерт-правило) — эксплуатация 0a; в коде
   — только 503 и ERROR-лог.
