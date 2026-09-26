# Реестр спецификаций

Этот файл является реестром всех документов человеческого слоя (`specs/`).

Обновляй этот файл при:

- создании нового документа;
- изменении статуса;
- переименовании;
- архивации;
- замене документа новым.

## Правила

- Используй стабильные ID: `BRD-*`, `SRS-*`, `ADR-*`.
- Не меняй исторические ID без причины.
- Формат даты: `YYYY-MM-DD`.
- В колонке `Source` указывай родительский документ или требование.
- Типы документов: `BRD`, `SRS`, `ADR`.

## Реестр

| ID | Type | Title | Status | Path | Source | Updated |
|---|---|---|---|---|---|---|
| SRS-001 | SRS | Подсистема шифрования agio Drive (Per-File FEK + Per-Chunk CEK) | review (Final draft v2.3) | specs/srs/SRS-001-agio-drive-encryption.md | - | 2026-09-26 |
| ADR-001 | ADR | Гибридный spec-driven workflow (BRD/SRS + OpenSpec) | accepted | specs/decisions/ADR-001-hybrid-spec-workflow.md | - | 2026-09-26 |
| ADR-002 | ADR | Outbox: события по дочерним объектам при move/rename/delete директории | draft | specs/decisions/ADR-002-outbox-child-events.md | - | 2026-08-29 |

## Рабочие документы (вне жизненного цикла BRD/SRS/ADR)

| Документ | Роль | Path | Updated |
|---|---|---|---|
| agio Drive — архитектурный план v13 | Черновик общего плана AGIO Drive v2 (контекст, целевая архитектура, этапы). Не является BRD/SRS; источник для будущих BRD-001 и per-stage changes. | .qwen/plans/agio-drive-full-plan-v13.md | 2026-08 |
| Stage-планы шифрования 01–10 | Декомпозиция SRS-001 по этапам реализации; каждый stage — кандидат на отдельный OpenSpec change. Для крупных фич допускается единый change со стадийной декомпозицией в tasks.md (см. ADR-001): шифрование реализуется единым change `implement-encrypt`. | .qwen/plans/stage-01-foundation.md … stage-10-production-rollout.md | 2026-08 |

## Замечания

- **BRD для agio Drive отсутствует.** Бизнес-контекст сейчас живёт в архитектурном плане v13. До создания BRD-001 OpenSpec changes ссылаются на SRS-001 и ADR напрямую; в proposal.md отсутствие BRD объясняется явно.
- **SRS-001 использует фактическую схему ID** `FR-*`/`NFR-*` (например, `FR-USR-1`, `NFR-SEC-1`); схема `REQ-*`/`SEC-*` в нём не используется. Схема зафиксирована как стабильная с v2.3 (см. `specs/README.md`). Ссылки в changes допускаются как по ID, так и по разделам (например, `SRS-001#§6.2`).
- Исторические версии SRS шифрования (v1.0, v2.0, v2.1 — в `.qwen/specs/`; v2.2 — в истории git `specs/srs/`) остаются как история; актуальная — только v2.3 в `specs/srs/`.

## Связь с OpenSpec

Каждый OpenSpec change должен ссылаться на документы из этого реестра (раздел "Related Requirements" в proposal.md).

Проверка связи:

```text
SRS-001 → openspec/changes/<change-name>/proposal.md
ADR-002 → openspec/changes/<change-name>/proposal.md
```
