# Design: fix-outbox-stop-consumer-race

## Architectural Context

Outbox — redis-only подсистема: `RedisOutbox` (pkg/meta/redis_outbox.go) — единственная
реализация потребительского цикла, граница «outbox — redis-only» существует в AGENTS.md
(metadata-engine parity не применим). `consumeLoop` — единственная горутина, запускаемая
`StartConsumer`; единственный продакшен-вызывающий `StopConsumer` — cmd/outbox.go
(`outbox.StopConsumer()` на SIGINT/SIGTERM, cmd/outbox.go:356), после чего процесс выходит и
выполняются deferred-закрытия sink. Изменение локально в pkg/meta/redis_outbox.go и его тестах.

## Component Map

- `RedisOutbox` (pkg/meta/redis_outbox.go) — поля `mu`, `running`, `ctx`, `cancel` (существуют),
  новое поле `done chan struct{}`.
- `StartConsumer` / `StopConsumer` / `consumeLoop` — существующие методы, точка изменения.
- `IsHealthy` — чтение `running` под `mu` + Redis PING; не меняется.
- `pollAndPublish` / `reclaimPending` / `processMessage` / `tryDrainSink` — внутренности цикла;
  не меняются.
- Моки тестов `captureSink`, `failingSink` (pkg/meta/redis_outbox_test.go) — существуют; новый
  мок `blockingSink` с каналами синхронизации.

## Execution Flow

Shutdown: SIGINT → `StopConsumer`: под `mu` — `cancel()`, `running=false`, локальная ссылка на
`done`; вне `mu` — `<-done`. Горутина цикла: текущая итерация завершает
`pollAndPublish`/`reclaimPending`/`TryDrain` (Publish получает отменённый ctx → ошибка →
`addRetryCount`), следующий `select` видит `ctx.Done()` → `defer close(done)` срабатывает →
`StopConsumer` возвращает управление → вызывающий закрывает sink и выходит.
Restart: `StartConsumer` после `StopConsumer` создаёт новый `done` и горутину — старая
гарантированно завершена (ожидание в `StopConsumer`).

## Invariants

- at-least-once: сообщение, чей `Publish` не завершился до остановки, остаётся в PEL.
- Ровно один активный цикл на экземпляр: guard `running` + ожидание done при остановке.
- `mu` не удерживается во время ожидания done (нет взаимоблокировки с `IsHealthy`/`StartConsumer`).
- Поведение poll/reclaim/спилл/backpressure/health не меняется.

## Decisions & Rationale

- D1: done-канал вместо `sync.WaitGroup` — ровно один воркер; канал даёт точку синхронизации
  «цикл вышел» без счётчика и повторно используется при рестарте (пересоздаётся в
  `StartConsumer`).
- D2: ожидание вне `mu` — `StopConsumer` не блокирует `IsHealthy`/повторный `StartConsumer`
  на время ожидания; `cancel()` под `mu` гарантирует, что `running=false` виден до начала
  ожидания.
- D3: без grace-дренажа в паблиш: `Publish` с отменённым ctx возвращает ошибку, `addRetryCount`
  с отменённым ctx не инкрементирует счётчик (команды Redis не выполняются), сообщение остаётся
  в PEL → redelivery при следующем старте (сценарий «Messages survive consumer restart»).
- D4: redis-only, parity не применим: redis_outbox.go — redis-специфичный код outbox
  (существующая граница «outbox — redis-only»).
- D5: cmd/outbox.go не меняется: единственный вызывающий уже делает `StopConsumer` перед
  выходом, а sink закрывается через defer после возврата — после фикса порядок становится
  гонко-безопасным без правок.

## Integration Points

- cmd/outbox.go:356 `outbox.StopConsumer()` — сигнатура и порядок вызовов не меняются.
- `/health` (cmd/outbox.go) — `IsHealthy()` без изменений.
- pkg/meta/events.go, pkg/meta/redis_event.go, pkg/meta/s3_sink.go, pkg/meta/event_filter.go —
  не затрагиваются; контракт события и доставки не меняется.

## Open Questions

- Нет.
