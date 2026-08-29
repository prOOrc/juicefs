# Design: baseline-inventory-outbox

## Architectural Context

Outbox — AGIO-расширение `redisMeta` (build tag `!noredis`): события файловой системы пишутся в Redis stream атомарно с meta-операцией (один pipeline), затем standalone-процесс `juicefs outbox` пересылает их в Kafka. Потребители Kafka (например, agio-platform) строят path-based проекции (`drive_object`).

Схема потока:

```text
meta operation (doMknod/doRename/doUnlink/doRmdir/doWrite/doFallocate)
  → publish*ToPipe → RedisOutbox.AddEventToPipe
    → XADD <stream> data=<json> + XTRIM MAXLEN   [один pipeline с meta-мутацией]
      → (standalone) juicefs outbox: XREADGROUP → FilterEvent → WatermillKafkaPublisher
        → Kafka topic juicefs.events (partition key = volume:subdir+path)
          → retry: <stream>:retry:<msgID> (INCR, TTL 24h) → <stream>:dead после MaxRetries
```

## Component Map

| Компонент | Файл | Роль |
|---|---|---|
| `EventType`, `JuiceFsEvent` | `pkg/meta/events.go` | 7 типов событий, JSON-модель |
| `publish*ToPipe`, `reconstructPath` | `pkg/meta/redis_event.go` | Генерация событий в pipeline meta-операции; восстановление полного пути |
| `RedisOutbox` | `pkg/meta/redis_outbox.go` | XADD/XTRIM, consumer group, consume loop, retry, dead-letter |
| `FilterEvent`, `isMinioInternal`, `isTmpOperation`, `isMovedFromTmp` | `pkg/meta/event_filter.go` | Фильтрация/трансформация перед Kafka (временные MinIO-правила) |
| `WatermillKafkaPublisher`, `KafkaConfig`, `JSONMarshaler` | `pkg/meta/watermill_kafka.go` | Публикация в Kafka (Watermill + Sarama), SASL SCRAM-SHA512, TLS, partition key |
| `cmdOutbox`, `outboxConsumer` | `cmd/outbox.go` | Standalone-консьюмер: флаги, health endpoint, graceful shutdown |
| `OutboxConfig` | `pkg/meta/config.go` | Enabled, StreamName, ConsumerGroup, KafkaBrokers, KafkaTopic, MaxRetries, TrimMaxLen |
| Флаги mount | `cmd/flags.go`, `cmd/mount.go` | `--outbox-enabled`, `--outbox-stream` |
| Поле `redisMeta.outbox` | `pkg/meta/redis.go:98,375` | Инстанс outbox в meta-движке |

## Execution Flow

1. **Запись (mount):** meta-операция в `redisMeta` вызывает `publish*ToPipe(ctx, pipe, …)` внутри своего pipeline; `publishEventToPipe` пропускает событие при disabled outbox или `Inode >= TrashInode`, заполняет `Timestamp`/`Volume`/`Uid`/`Gid`/`Subdir`, `AddEventToPipe` добавляет XADD + XTRIM. Pipeline исполняется атомарно с meta-мутацией.
2. **Чтение (standalone):** `consumeLoop` (ticker 1s) → `XREADGROUP group=<group> consumer=juicefs ">" COUNT 100 BLOCK 1s`.
3. **Обработка сообщения:** нет/не-string `data` → warn + XACK; unmarshal error → retry counter; `FilterEvent` → nil (отфильтровано) → XACK; `publisher.Publish` error → retry counter; успех → XACK.
4. **Retry/DLQ:** `INCR <stream>:retry:<msgID>` + `EXPIRE 24h`; при `count >= MaxRetries` — `XADD <stream>:dead` (data, retry_at, msg_id) + XACK оригинала.

## Invariants

- Событие появляется в stream только если meta-мутация закоммичена (один pipeline) — нет «событий в будущее».
- Consumer group создаётся с `$`: сообщения, записанные до старта консьюмера, не переигрываются.
- Stream ограничен XTRIM MAXLEN (default 10000) — память Redis не растёт без bound.
- `deleteMessage` = только XACK (удаление из PEL), физическое удаление из stream делает XTRIM.
- Outbox существует только для `redisMeta`; SQL/KV движки событий не генерируют.

## Decisions & Rationale

- **Redis Streams как outbox, а не прямой Kafka из mount:** mount не зависит от доступности Kafka; доставка at-least-once через consumer group + retry + DLQ.
- **Один pipeline с meta-мутацией:** атомарность «состояние + событие» без транзакций поверх Redis.
- **Standalone-консьюмер вместо вшитого в mount:** независимый масштабирование/рестарт форвардера; health endpoint для оркестрации.
- **Partition key `volume:subdir+path`:** события одного файла/объёма попадают в одну партицию — порядок per-path у потребителя.
- **Временные MinIO-фильтры в `FilterEvent`:** совместимость с текущим MinIO Gateway (tmp-rename паттерн S3 PUT); помечены «remove when migrating to Meta Proxy».

## Integration Points

- **Kafka:** топик `juicefs.events` (настраивается), SASL SCRAM-SHA512 / TLS опциональны.
- **Redis:** тот же инстанс, что и metadata; stream `juicefs:outbox`, group `meta-proxy-outbox`, DLQ `juicefs:outbox:dead`, retry-ключи `juicefs:outbox:retry:*`.
- **Потребители:** agio-platform (проекции drive) — внешняя система, в этом репозитории не описана.
- **Документация:** `docs/en/deployment/outbox.md` (флаги, примеры).

## Known Deviations

As-is отклонения, зафиксированные как кандидаты на fix-change:

1. **Нет событий по дочерним объектам при move/rename директории** — `doRename` двигает поддерево атомарно, публикуется только одно `DirMoved`. Path-based проекции потребителя дрифтуют для вложенных объектов. Кандидат: ADR-002 (draft) → change по domain-outbox.
2. **Retry без backoff и без повторного чтения** — при ошибке publish сообщение остаётся в PEL, но consume loop читает только новые сообщения (`">"`); pending-сообщения не перечитываются, retry counter инкрементируется только при повторной обработке того же сообщения (которая в текущем цикле не происходит). Фактически retry/DLQ срабатывают лишь при редких повторных чтениях. Кандидат на fix-change.
3. **`AddEventToPipe` молча дропает событие при ошибке marshal** (только log) — meta-мутация закоммитится без события.
4. **`reconstructPath` делает O(глубина) Redis-чтений + полный `doReaddir` каждого предка** внутри pipeline meta-операции — деградация latency на глубоких деревьях; при неудаче путь пуст (потребитель получает событие без path).
5. **`XTRIM MAXLEN` точный (не `~`)** — O(N) трим на каждый XADD при длинном stream.
6. **Consumer name жёстко `juicefs`** — все реплики консьюмера делят один PEL-контекст; масштабирование консьюмеров ограничено.

## Open Questions

- [ ] Фикс retry-механики (перечитывание PEL) — отдельный change после ADR-002?
- [ ] Удаление временных MinIO-фильтров при миграции на Meta Proxy — в какой change?
- [ ] Нужен ли outbox для SQL/KV движков (parity) или Redis-only осознанный scope?
