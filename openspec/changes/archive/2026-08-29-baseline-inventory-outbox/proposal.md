# Proposal: baseline-inventory-outbox

## What & Why

Инвентаризация (as-is) капабилити **domain-outbox** — публикации событий файловой системы через Redis Streams в Kafka. Код уже реализован на ветке `outbox` (коммиты `f9aa7473`, `b060af59`), но не описан в Source of Truth.

Цель — зафиксировать фактическое поведение в capability spec `domain-outbox`: модель событий, точки генерации в meta-операциях, семантика Redis stream (consumer group, retry, dead-letter), фильтрация, Kafka-публикация и standalone-консьюмер. Это база для дальнейших changes (включая ADR-002 — события по дочерним объектам).

Методология: код — источник правды; требования ретроактивно извлечены из реализации (as-is inventory). Отклонения, которые не имеют смысла как контракт (баги, мёртвый код), формулируются как intended contract и фиксируются в design.md → Known Deviations.

## Capabilities

- `domain-outbox` — НОВЫЙ capability: события файловой системы Redis Streams → Kafka.

## Non-goals

- Не описывать upstream JuiceFS (meta-движки SQL/KV, vfs, chunk) — только AGIO-специфичный outbox.
- Не менять поведение кода; это инвентаризация, не фича.
- Не покрывать события по дочерним объектам при move/rename/delete директории — это будущий change по ADR-002 (draft).
- Не описывать Meta Proxy (отдельный capability `domain-meta-proxy`, инвентаризируется на ветке `agio-drive-v2`).
- Не вводить outbox для SQL/KV движков — as-is он существует только для `redisMeta`.

## Related Requirements

Бизнес-документов (BRD/SRS) для outbox не существует — это AGIO-инфраструктурная фича, реализованная без предшествующей спецификации; инвентаризация создаёт первый формальный слой требований. Связанные документы:

- ADR-002 (draft): Outbox — события по дочерних объектах при move/rename/delete директории (`specs/decisions/ADR-002-outbox-child-events.md`) — основан на as-is фактах этого инвентаря.
- `docs/en/deployment/outbox.md` — операционная документация (флаги, примеры запуска).
