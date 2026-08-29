# Tasks: baseline-inventory-outbox

Инвентаризационный change: задачи — исследование кода, написание spec, верификация утверждений. Продакшен-код не меняется.

## 1. Research

- [x] Прочитать модель событий и точки генерации: `pkg/meta/events.go`, `pkg/meta/redis_event.go`, call sites в `pkg/meta/redis.go` (doMknod, doUnlink, doRmdir, doRename, doWrite, doFallocate).
- [x] Прочитать stream-механику: `pkg/meta/redis_outbox.go` (XADD/XTRIM, consumer group, consume loop, retry, DLQ).
- [x] Прочитать фильтрацию и Kafka: `pkg/meta/event_filter.go`, `pkg/meta/watermill_kafka.go`.
- [x] Прочитать CLI и интеграцию с mount: `cmd/outbox.go`, `cmd/flags.go`, `cmd/mount.go`, `pkg/meta/config.go` (OutboxConfig).
- Проверка: `grep -n "publish.*ToPipe" pkg/meta/redis.go` — 6 call sites в ожидаемых функциях.

## 2. Spec

- [x] Написать delta-спеку `specs/domain-outbox/spec.md`: 11 требований (event model, emission, path reconstruction, stream writing, consumer group/polling, retry, DLQ, filtering, Kafka publishing, standalone command, mount integration).
- [x] Verified-by строки только для требований с существующими тестами.

## 3. Verification

- [x] Прогнать outbox-тесты: `go test ./pkg/meta/ -run 'TestRedisOutbox|TestInitConsumerGroup|TestRedisRename|TestFilterEvent' -count=1` — PASS.
- [x] Spot-check grep-ами: 7 EventType констант; consumer name `juicefs`; Count 100 / Block 1s; retry TTL 24h; DLQ suffix `:dead`; partition key `Volume+":"+Subdir+Path`; дефолты флагов (`juicefs:outbox`, `meta-proxy-outbox`, `juicefs.events`, max-retries 10, trim-max-len 10000, health `:9100`).
- [x] Подтвердить Known Deviations по коду: нет перечитывания PEL (только `">"`), marshal error в `AddEventToPipe` — только log, `XTRIM MAXLEN` без `~`.

## Definition of Done

- [x] `go build ./...` — PASS.
- [x] `go vet ./pkg/meta/...` — PASS (предсуществующие предупреждения вне scope не вносить).
- [x] `go test ./pkg/meta/ -run 'TestRedisOutbox|TestInitConsumerGroup|TestRedisRename|TestFilterEvent' -count=1` — PASS.
- [x] `openspec validate baseline-inventory-outbox` — PASS.
