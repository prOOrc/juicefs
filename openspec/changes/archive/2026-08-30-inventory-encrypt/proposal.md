# Proposal: inventory-encrypt

## Why

Подсистема шифрования agio Drive (SRS-001 v2.2, Per-File FEK + Per-Chunk CEK) полностью спроектирована — SRS Final draft, мастер-план с межэтапными контрактами и 10 stage-планов — но не описана в Source of Truth, и ни один этап не начат (мастер-план §8: все «не начат»). До запуска per-stage implementation changes нужен машиночитаемый контракт капабилити: иерархия ключей, бинарные форматы AGFK/AGCK/AGDF, crypto-поля метаданных Redis, user/render-пути, операции Clone/CopyFileRange/Compaction (CEK re-wrap), offline-connected + no residuality, revocation + STS, ротация ключей, миграция legacy-данных. Это база для per-stage changes (stage-01…10) и защита от дрейфа форматов между двумя репозиториями (форк JuiceFS + agio-platform).

Методология: целевой контракт извлекается из SRS-001 v2.2 + мастер-план §4 (межэтапные контракты, решения D1–D12) + stage-планы 01–10; фактическое состояние кода (as-is) верифицировано grep-ами и зафиксировано в design.md. В отличие от инвентаризации `domain-meta-proxy` (as-is, код реализован), это инвентаризация **запланированной** подсистемы (greenfield): требования описывают целевое поведение, которое per-stage changes будут реализовывать.

## What Changes

- Новый capability spec `domain-encrypt`: целевой контракт подсистемы шифрования со стороны форка — crypto-примитивы и форматы (AGFK/AGCK/AGDF), расширение slice-записи и `Attr`, ChunkStore с ключами, VFS plumbing (FEK в handle, CEK per open file), интеграция Meta Proxy с KeyManager (Create/Open, rollback, FEK LRU), render-клиент (прямой Redis+S3, Company KEK), CEK re-wrap для Clone/CopyFileRange/Compaction, offline-connected + no residuality (memclr/mlock/журнал записей), revocation + STS refresher, ротация, миграция legacy-данных — плюс gRPC-контракт `DriveKeyManagerService`, который форк потребляет.
- Продакшен-код не меняется: это инвентаризация, не фича.

## Capabilities

### New Capabilities

- `domain-encrypt`: подсистема шифрования agio Drive (Per-File FEK + Per-Chunk CEK) — иерархия ключей и форматы обёрток, хранение wrapped-ключей в метаданных Redis, user path через Meta Proxy + KeyManager (authz-gated выдача FEK), render path с прямым Redis+S3 и Company KEK, zero-copy Clone/Compaction через CEK re-wrap, encrypted local cache + offline-connected режим, отзыв доступа + STS, ротация ключей, миграция нешифрованных данных.

### Modified Capabilities

- (нет)

## Non-goals

- Не реализовывать шифрование: per-stage changes (stage-01…10) создаются отдельно поверх этого инвентаря, каждый со ссылкой на `domain-encrypt`.
- Не описывать agio-platform как капабилити этого репозитория: KeyManager, KMS/Secret Manager-адаптеры, STS-провайдер, PG-таблицы живут в репозитории agio-platform; здесь фиксируется только gRPC-контракт `agio.platform.drive.crypto.v1.DriveKeyManagerService`, который форк потребляет (тот же паттерн, что `AuthzService` в `domain-meta-proxy`).
- Не описывать upstream JuiceFS (meta-движки, vfs, chunk) — только AGIO-специфичные расширения.
- Не менять поведение существующего кода; не трогать капабилити `domain-meta-proxy` и `domain-outbox`.
- Не проектировать версионирование/снепшоты/корзину (SRS §9): только криптографические предусловия FR-VER-1…4.
- Zero-knowledge режим (SRS §13, FR-ZK) не входит в MVP и не включается в спеку.

## Related Requirements

- SRS-001 (review, Final draft v2.2): Подсистема шифрования agio Drive — `specs/srs/SRS-001-agio-drive-encryption.md`. Ссылки по разделам (стабильных REQ-* ID у SRS нет до approved): §4 (иерархия ключей и форматы), §5 (хранение в Redis), §6 (user path), §7 (render path), §8 (операции), §9 (предусловия версионирования), §10 (offline/no-residuality), §11 (revocation), §12 (ротация), §14 (NFR), §15 (gRPC API), §16 (миграция), §18 (тестирование), §22 (Acceptance Criteria).
- Мастер-план шифрования (рабочий документ): `.qwen/plans/agio-drive-encrypt-master.md` — межэтапные контракты §4.1–4.8 (бинарные форматы, поля метаданных, proto-расширения, параметры кэшей, решения D1–D12), обзор этапов §3, статусная таблица §8 (все этапы «не начат»).
- Stage-планы 01–10 (рабочие документы): `.qwen/plans/stage-01-foundation.md` … `stage-10-production-rollout.md` — декомпозиция SRS-001 по этапам реализации; каждый stage — кандидат на отдельный OpenSpec change.
- ADR-001 (accepted): Гибридный spec-driven workflow — процесс, которому следует эта инвентаризация.
- BRD для agio Drive отсутствует — бизнес-контекст живёт в архитектурном плане v13 (`.qwen/plans/agio-drive-full-plan-v13.md`); инвентаризация создаёт первый формальный слой требований по капабилити шифрования.

## Impact

- `openspec/specs/domain-encrypt/` — новый capability spec (после archive).
- На код, API и зависимости влияния нет: change только документационный (инвентаризация).
- Последующие per-stage changes (stage-01…10) ссылаются на `domain-encrypt` как на контракт; отклонения от SRS, зафиксированные в stage-планах (решения 1.3, 3.1, 7.1, 7.7), отражаются в design.md → Known Deviations.
