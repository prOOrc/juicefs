# Proposal: fix-outbox-stop-consumer-race

## Why

`RedisOutbox.StopConsumer` (pkg/meta/redis_outbox.go) отменяет контекст цикла и сбрасывает
`running`, но не ждёт фактического завершения горутины `consumeLoop` — возврат происходит
немедленно. Вызывающий код (cmd/outbox.go: остановка по SIGINT/SIGTERM) после `StopConsumer`
выходит из процесса и закрывает sink (`defer publisher.Close()`), пока горутина цикла может ещё
находиться внутри `sink.Publish` / `processMessage`. Следствия: (1) публикация в уже закрываемый
sink при shutdown — гонка publish-vs-Close (для kafka-паблишера Watermill поведение при
конкурентном Close с Publish не определено); (2) быстрый Stop→Start на том же экземпляре
запускает второй цикл, пока первый ещё тикает — двойной консюмер одной группы. Потерь данных
нет: незавершённое сообщение остаётся в PEL и обрабатывается следующим стартом
(at-least-once), поэтому дефект не проявлялся в e2e, где shutdown редок.

## What Changes

- `RedisOutbox` получает done-канал: `StartConsumer` создаёт его перед запуском горутины,
  `consumeLoop` закрывает его при выходе (`defer close`).
- `StopConsumer` отменяет контекст, сбрасывает `running`, ждёт закрытия done-канала (вне
  блокировки `mu`) и только после этого возвращает управление и пишет лог остановки.
- Конфигурация, флаги, семантика доставки (at-least-once, retry/DLQ, спилл, backpressure,
  health) — без изменений.

## Capabilities

- domain-outbox (MODIFIED: Requirement «Consumer group and polling» — семантика остановки и
  рестарта консюмера)

## Non-goals

- Не делается drain-in-flight при остановке: недоведённое до ack сообщение остаётся в PEL и
  обрабатывается следующим стартом (existing at-least-once semantics).
- Не меняется cmd/outbox.go: порядок «StopConsumer → defer publisher.Close() → выход» уже
  корректен, как только StopConsumer дождётся цикла.
- Не трогаются pkg/meta/redis_event.go, pkg/meta/s3_sink.go, pkg/meta/event_filter.go —
  эмиссия и доставка вне скоупа.
- Не вводится таймаут ожидания цикла: `XREADGROUP` ограничен Block 1s, а `TryDrain`/`Publish`
  уважают отменённый контекст — ожидание дольше секунд невозможно по построению.

## Related Requirements

Бизнес-документов (BRD/SRS/ADR) для outbox в specs/index.md нет: изменение закрывает дефект
конкурентной семантики жизненного цикла консюмера, поведение доставки не меняется, поэтому
ссылок по ID нет (смежный ADR — нет).

## Impact

- pkg/meta/redis_outbox.go — структура `RedisOutbox` (+done), `StartConsumer`,
  `StopConsumer`, `consumeLoop`.
- pkg/meta/redis_outbox_test.go — новые тесты (`blockingSink`,
  TestStopConsumerWaitsForLoopExit, TestRestartAfterStopSingleLoop).
- Cross-repo: agio-cloud docker/juicefs — bump `JUICEFS_REV` на новый HEAD agio-drive-v2 и
  новый `JUICEFS_IMAGE_TAG` (пересборка juicefs-image-build); пин CRSS change 0a
  (`var.hk_outbox_image_tag`) должен ссылаться на новый тег вместо
  `agio-v1.4.2_20261119a_bf4bd204a`. Кросс-репо-контракт события (JuiceFsEvent JSON,
  объект-ключи `events/<date>/<hour>/<uuid>.json`) не меняется.
