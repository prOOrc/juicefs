# Proposal: implement-encrypt

## Why

Подсистема шифрования agio Drive (SRS-001 v2.2, Per-File FEK + Per-Chunk CEK) полностью спроектирована и зафиксирована в Source of Truth (`openspec/specs/domain-encrypt`, инвентаризация `inventory-encrypt` от 2026-08-30), но не реализована: данные в S3 хранятся в plaintext, ключей CEK/FEK/KEK нет, render-ноды и user-клиенты не имеют криптографической изоляции. Это блокирует требования аудита (SOC 2, MPAA TPN) и конкурентный паритет с LucidLink (стратегические решения S1/S4). Реализация запускается сейчас: контракт стабилен, межэтапные форматы зафиксированы (мастер-план §4), декомпозиция на 10 этапов готова.

## What Changes

Реализация капабилити `domain-encrypt` по 10 этапам в двух репозиториях — форк JuiceFS (`agio-drive-v2`) и agio-platform (`feature/drive-v2`). Кратко по этапам (детали — tasks.md, код-уровень — stage-планы):

- **Stage 1 (platform):** KMS/Secret Manager-порты + Yandex-реализации, `CompanyKEKService`, PG-таблицы `drive_company_crypto_key`/`drive_key_access_log`, `IdentityResolver` (валидация UUID), gRPC-сервис `DriveKeyManagerService` (`CreateFileKey`/`GetFileFEK`/`GetBulkFileFEK`/`FetchCompanyKEK`/`ProvisionCompanyKEK`; ротация — stubs до stage 7), аудит выдачи ключей.
- **Stage 2 (форк):** крипто-примитивы AGDF/AGCK/AGFK, slice-запись с `wrapped_cek`, crypto-поля `Attr` + transient `Fek`, `Format.EncryptionEnabled/KEKVersion`, `ChunkStore.NewReaderWithKey/NewWriterWithKey`, ciphertext-only local cache, VFS plumbing (FEK в handle, CEK per open file), предусловия версионирования FR-VER-1/4.
- **Stage 3 (оба):** proto-расширения MetaService (`OpenResponse.fek/fek_version/encrypted`, `CreateRequest.drive_file_id`, `Format.encryption_enabled/kek_version`), `redisMeta.SetFileCrypto`, KeyManager-клиент форка, интеграция в Meta Proxy (Create/Open + rollback + FEK LRU 100k/15 мин), UUID-валидация identity на proxy.
- **Stage 4 (форк):** render-клиент — команда `juicefs render-mount`, декоратор `renderMeta` (прямой Redis+S3, Company KEK из `FetchCompanyKEK` по IAM ноды, локальный FEK unwrap, LRU 100k/1h, aggressive caching, chroot company prefix).
- **Stage 5 (форк):** zero-copy Clone/CopyFileRange через CEK re-wrap (`RewrapSlices`), Compaction с CEK (`fileKeyResolver`, новый CEK под FEK), RPC `ResolveFileKey`, предусловия FR-VER-2/3.
- **Stage 6 (форк):** no residuality — `MemClear`/mlock, state machine online → offline-connected → disconnected, write journal с replay, logout/OIDC-expiry/timeout триггеры обнуления ключей.
- **Stage 7 (оба):** revocation + STS — permission generation (Redis counter + `FlushSessionResponse.permission_generation`), STS-провайдер platform (prefix-scoped S3-политики) + `stsRefresher` форка, FEK rotation (`RotateFileKey`, re-wrap без перешифровки S3), offboarding-batch `RotateFileKeysByPaths`.
- **Stage 8 (форк):** миграция legacy-данных — `ReencryptChunk`, команда `juicefs reencrypt` (идемпотентная, возобновляемая, rate-limited), `enable-encryption` для существующего тома, CEK rotation как режим `--file --rotate-cek`.
- **Stage 9 (оба):** тестовая матрица SRS §18 — cross-repo known-answer векторы форматов, интеграционный suite (`make test.enc.integration`), нагрузочные (FR-TEST-22..25), security-сценарии (FR-TEST-26..30), AC-чеклист.
- **Stage 10 (оба + инфраструктура):** production rollout — per-company KMS keys + auto-rotation, encrypted Redis backups + restore-тест, аудит ≥12 мес + anomaly detection, метрики/алерты, runbooks, audit package (SOC 2 / MPAA TPN prep), rollout/rollback-план.

**BREAKING:** расширение slice-записи (опциональный AGCK-хвост) — старые читатели падают на новых записях; rollout требует обновления всех клиентов до включения шифрования на томе (S2: upstream-совместимость не требуется).

## Capabilities

### New Capabilities

(нет — капабилити уже зафиксирована инвентаризацией `inventory-encrypt`)

### Modified Capabilities

- `domain-encrypt`: требование «gRPC API surface» дополняется RPC `RotateFileKeysByPaths` (batch FEK rotation по путям для offboarding, решение 7.7 stage-плана) — контракт, который форк реализует в stage 7 и который отсутствует в baseline-спеке. Остальные требования baseline реализуются без изменения формулировок.

## Non-goals

- Не реализовывать версионирование, снепшоты и корзину (SRS §9): только криптографические предусловия FR-VER-1…4 (хуки + `fek_version`).
- Не реализовывать zero-knowledge режим (SRS §13, FR-ZK) — опциональный premium, вне MVP.
- Не описывать и не менять agio-platform как капабилити этого репозитория: KeyManager, KMS/Secret Manager/STS-адаптеры, PG-таблицы реализуются в репозитории agio-platform (этапы 1/7); здесь фиксируется только потребляемый gRPC-контракт `agio.platform.drive.crypto.v1.DriveKeyManagerService`.
- Не менять upstream-поведение JuiceFS и не поддерживать совместимость с upstream (S2); целевой metadata-бэкенд — Redis, SQL/KV-ветки трогаются только если ломается компиляция общего кода.
- Не шифровать метаданные (имена файлов в Redis остаются видимыми) и не менять authz-модель `domain-meta-proxy`/`domain-outbox`.
- Не делать де-энкрипцию (обратную миграцию) — rollback задокументирован как «остановить создание новых зашифрованных файлов», полное откатывание шифрования = отдельный проект.

## Related Requirements

- SRS-001 (review, Final draft v2.2): Подсистема шифрования agio Drive — `specs/srs/SRS-001-agio-drive-encryption.md`. Ссылки по разделам (стабильных REQ-* ID у SRS нет до approved): §4 (иерархия ключей и форматы), §5 (хранение в Redis), §6 (user path), §7 (render path), §8 (операции Clone/CopyFileRange/Compaction), §9 (предусловия версионирования), §10 (offline/no-residuality), §11 (revocation), §12 (ротация), §14 (NFR), §15 (gRPC API), §16 (миграция), §17 (модификации), §18 (тестирование), §19 (план внедрения), §22 (Acceptance Criteria).
- `domain-encrypt` (Source of Truth): `openspec/specs/domain-encrypt/spec.md` — целевой контракт капабилити, созданный инвентаризацией `inventory-encrypt` (archived 2026-08-30); этот change реализует его требования.
- Мастер-план шифрования (рабочий документ): `.qwen/plans/agio-drive-encrypt-master.md` — межэтапные контракты §4.1–4.8 (бинарные форматы, поля метаданных, proto-расширения, параметры кэшей, решения D1–D12), порядок этапов §3, AC→этапы §7, статусная таблица §8.
- Stage-планы 01–10 (рабочие документы, код-уровневая декомпозиция): `.qwen/plans/stage-01-foundation.md` … `stage-10-production-rollout.md`; запускные промпты — `.qwen/prompts/agio-drive-encrypt-stage-01.md` … `stage-10.md`.
- ADR-001 (accepted): Гибридный spec-driven workflow — процесс, которому следует этот change.
- BRD для agio Drive отсутствует (см. `specs/index.md`) — бизнес-контекст живёт в архитектурном плане v13 (`.qwen/plans/agio-drive-full-plan-v13.md`); требования берутся из SRS-001 напрямую.

## Impact

- **Форк JuiceFS (`agio-drive-v2`):** `pkg/meta/` (slice, attr, config, redis_fek, render_meta, keymanager client, proto pb/, grpc_server/client, authz_interceptor), `pkg/chunk/` (cek_encrypt, cached_store, chunk interface), `pkg/vfs/` (handle, reader, writer, compact, write_journal), `pkg/utils/` (memclr), `cmd/` (render_mount, reencrypt, sts_refresher, meta_proxy, mount), `tests/` (load, security, acceptance), Makefile (test.enc.integration).
- **agio-platform (`feature/drive-v2`):** `src/application/authz/proto/key_manager.proto` + generated code, `src/application/authz/service/key_manager_service.go` + `fek_crypto.go`, `src/internal/drive/infrastructure/adapters/` (kms_yandex, secret_manager_yandex, company_kek, identity_resolver, sts_aws/sts_yc), `src/internal/drive/application/ports/crypto.go`+`sts.go`, PG-миграции 000190/000191 + SQLBoiler, `src/api/iam_interceptor.go`, wire/config, CLI `keymanager`.
- **Инфраструктура:** KMS master keys (per-company), Secret Manager (Company KEK), STS (замена статических S3-credentials — обязательное условие реального отзыва, R8), encrypted Redis backups, audit storage ≥12 мес.
- **API:** расширение `MetaService` proto (форк) и новый сервис `DriveKeyManagerService` (platform); формат slice-записи в Redis получает опциональный хвост (см. BREAKING выше).
- **Зависимости:** YC SDK (KMS/Secret Manager/IAM), AWS SDK (STS, альтернативный провайдер), `golang.org/x/sys/unix` (mlock) — проверить наличие в vendor/go.mod при реализации.
