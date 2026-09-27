# Proposal: implement-encrypt

## Why

Подсистема шифрования agio Drive (SRS-001 v2.3, Per-File FEK + Per-Chunk CEK) полностью спроектирована, но не реализована: данные в S3 хранятся в plaintext, ключей CEK/FEK/KEK нет, render-ноды и user-клиенты не имеют криптографической изоляции. Целевой контракт капабилити `domain-encrypt` зафиксирован в delta-спеке этого change (`specs/domain-encrypt/spec.md`, 17 требований; перенесён из отозванной ветки `inventory-encrypt` — greenfield-капабилити не может находиться в SoT до реализации и попадёт в SoT при архивации этого change). Это блокирует требования аудита (SOC 2, MPAA TPN) и конкурентный паритет с LucidLink (стратегические решения S1/S4). Реализация запускается сейчас: контракт стабилен, межэтапные форматы зафиксированы в design.md («Межэтапные контракты»), декомпозиция на 10 этапов готова (tasks.md).

## What Changes

Реализация капабилити `domain-encrypt` по 10 этапам в двух кодовых репозиториях — форк JuiceFS (`agio-drive-v2`) и agio-platform (`feature/drive-v2`); stage 10 дополнительно затрагивает инфраструктурные репозитории `agio-terraform-yc` (Terraform: KMS/Lockbox/IAM/Redis-backups) и `agio-cloud` (k8s values/secrets). Кратко по этапам (детали и код-уровневая декомпозиция — tasks.md; решения — design.md):

- **Stage 1 (platform):** KMS/Lockbox-порты + Yandex-реализации, `CompanyKEKService`, PG-таблицы `drive_company_crypto_key`/`drive_key_access_log`, `IdentityResolver` (валидация UUID), gRPC-сервис `DriveKeyManagerService` (`CreateFileKey`/`GetFileFEK`/`GetBulkFileFEK`/`FetchCompanyKEK`/`ProvisionCompanyKEK`; ротация — stubs до stage 7), аудит выдачи ключей.
- **Stage 2 (форк):** крипто-примитивы AGDF/AGCK/AGFK, slice-запись с `wrapped_cek`, crypto-поля `Attr` + transient `Fek`, `Format.EncryptionEnabled/KEKVersion`, `ChunkStore.NewReaderWithKey/NewWriterWithKey`, ciphertext-only local cache, VFS plumbing (FEK в handle, CEK per open file), предусловия версионирования FR-VER-1/4.
- **Stage 3 (оба):** proto-расширения MetaService (`OpenResponse.fek/fek_version/encrypted`, `CreateRequest.drive_file_id`, `Format.encryption_enabled/kek_version`), `redisMeta.SetFileCrypto`, KeyManager-клиент форка, интеграция в Meta Proxy (Create/Open + rollback + FEK LRU 100k/15 мин), UUID-валидация identity на proxy.
- **Stage 4 (форк):** render-клиент — команда `juicefs render-mount`, декоратор `renderMeta` (прямой Redis+S3, Company KEK из `FetchCompanyKEK` по IAM ноды, локальный FEK unwrap, LRU 100k/1h, aggressive caching, chroot company prefix).
- **Stage 5 (форк):** zero-copy Clone/CopyFileRange через CEK re-wrap (`RewrapSlices`), Compaction с CEK (`fileKeyResolver`, новый CEK под FEK), RPC `ResolveFileKey`, предусловия FR-VER-2/3.
- **Stage 6 (форк):** no residuality — `MemClear`/mlock, state machine online → offline-connected → disconnected, write journal с replay, logout/OIDC-expiry/timeout триггеры обнуления ключей.
- **Stage 7 (оба):** revocation + STS — permission generation (Redis counter + `FlushSessionResponse.permission_generation`), STS-провайдер platform (prefix-scoped S3-политики) + `stsRefresher` форка, FEK rotation (`RotateFileKey`, re-wrap без перешифровки S3), offboarding-batch `RotateFileKeysByPaths`. **Дополнение (2026-09-27, закрытие FR-REV-3 — SRS v2.4, ADR-003):** tasks 7.8–7.12 — platform-issued YC STS (`sts.yandexcloud.net`, policy `<volume>/*`), RPC `GetNodeSTSCredentials` (IAM-токен ноды), STS pass-through Meta Proxy для пользовательских mount'ов, удаление ephemeral-провайдера; design.md A6 — контракт.
- **Stage 8 (форк):** миграция legacy-данных — `ReencryptChunk`, команда `juicefs reencrypt` (идемпотентная, возобновляемая, rate-limited), `enable-encryption` для существующего тома, CEK rotation как режим `--file --rotate-cek`.
- **Stage 9 (оба):** тестовая матрица SRS §18 — cross-repo known-answer векторы форматов, интеграционный suite (`make test.enc.integration`), нагрузочные (FR-TEST-22..25), security-сценарии (FR-TEST-26..30), AC-чеклист.
- **Stage 10 (оба + инфраструктура):** production rollout — per-company KMS keys + auto-rotation, Redis metadata backups (внутренние бэкапы YC, retention 35 дней) + restore-тест, аудит ≥12 мес + anomaly detection, метрики/алерты, runbooks, audit package (SOC 2 / MPAA TPN prep), rollout/rollback-план.

**BREAKING:** расширение slice-записи (опциональный AGCK-хвост) — старые читатели падают на новых записях; rollout требует обновления всех клиентов до включения шифрования на томе (S2: upstream-совместимость не требуется).

## Capabilities

### New Capabilities

- `domain-encrypt`: greenfield-капабилити — в SoT её нет. Полный целевой контракт (17 требований, включая «Offboarding batch FEK rotation» с RPC `RotateFileKeysByPaths` — решение 7.7, design.md) зафиксирован в delta-спеке этого change; при архивации она становится Source of Truth.

### Modified Capabilities

- `domain-meta-proxy`: change расширяет gRPC-поверхность Meta Proxy — новые RPC `ResolveFileKey` (authz Read), `RotateFileKey` / `RotateFileKeysByPaths` (org-admin gated), `GetSTSCredentials` pass-through (STS-выдача для пользовательских mount'ов, task 7.10 — `user_id` из сессии proxy), крипто-поля на существующих сообщениях (`OpenResponse.fek/fek_version/encrypted`, `CreateRequest.drive_file_id`, `FlushSessionResponse.permission_generation`, `Format.encryption_enabled/kek_version`, `ProtoSlice.wrapped_cek`). Delta: `specs/domain-meta-proxy/spec.md` (MODIFIED: gRPC service surface, Authorization interceptor). Без шифрования (`EncryptionEnabled=false`) поведение капабилити не меняется.

## Non-goals

- Не реализовывать версионирование, снепшоты и корзину (SRS §9): только криптографические предусловия FR-VER-1…4 (хуки + `fek_version`).
- Не реализовывать zero-knowledge режим (SRS §13, FR-ZK) — опциональный premium, вне MVP.
- Не описывать и не менять agio-platform как капабилити этого репозитория: KeyManager, KMS/Lockbox/STS-адаптеры, PG-таблицы реализуются в репозитории agio-platform (этапы 1/7); здесь фиксируется только потребляемый gRPC-контракт `agio.platform.drive.crypto.v1.DriveKeyManagerService`.
- Не менять upstream-поведение JuiceFS и не поддерживать совместимость с upstream (S2); целевой metadata-бэкенд — Redis, SQL/KV-ветки трогаются только если ломается компиляция общего кода.
- Не шифровать метаданные (имена файлов в Redis остаются видимыми) и не менять authz-модель `domain-meta-proxy`/`domain-outbox`.
- Не делать де-энкрипцию (обратную миграцию) — rollback задокументирован как «остановить создание новых зашифрованных файлов», полное откатывание шифрования = отдельный проект.
- Не решать FR-ADMIN-2 (SRS §2.4): выделение административных RPC (`Init`, `GetFormat`, `Compact*`, `Quota`, `DumpMeta`, `Remove`) в отдельный привилегированный клиент или их удаление из публичного gRPC-интерфейса — отдельный hardening-change, трекается вне этого proposal; здесь админ-RPC остаются за `CheckOrganizationAdmin` (текущее поведение `domain-meta-proxy`).

## Related Requirements

- SRS-001 (review, Final draft v2.3): Подсистема шифрования agio Drive — `specs/srs/SRS-001-agio-drive-encryption.md`. Трассировка идёт по стабильным ID `FR-*`/`NFR-*` (схема зафиксирована с v2.3 — `specs/README.md`, `specs/index.md`) и по разделам: §4 (иерархия ключей и форматы), §5 (хранение в Redis), §6 (user path), §7 (render path), §8 (операции Clone/CopyFileRange/Compaction), §9 (предусловия версионирования), §10 (offline/no-residuality), §11 (revocation), §12 (ротация), §14 (NFR), §15 (gRPC API), §16 (миграция), §17 (модификации), §18 (тестирование), §19 (план внедрения), §22 (Acceptance Criteria).
- `domain-encrypt` (целевой контракт): `specs/domain-encrypt/spec.md` (delta этого change) — 17 требований; контент перенесён с ветки `inventory-encrypt` (2026-08-30), отозванной, т.к. greenfield-капабилити не может находиться в SoT до реализации. При архивации этого change delta станет `openspec/specs/domain-encrypt/`.
- ADR-001 (accepted): Гибридный spec-driven workflow — процесс, которому следует этот change.
- BRD для agio Drive отсутствует (см. `specs/index.md`) — требования берутся из SRS-001 напрямую; бизнес-контекст и стратегические решения (S1–S4) зафиксированы в design.md (Context).

## Impact

- **Форк JuiceFS (`agio-drive-v2`):** `pkg/meta/` (slice, attr, config, redis_fek, render_meta, keymanager client, proto pb/, grpc_server/client, authz_interceptor), `pkg/chunk/` (cek_encrypt, cached_store, chunk interface), `pkg/vfs/` (handle, reader, writer, compact, write_journal), `pkg/utils/` (memclr), `cmd/` (render_mount, reencrypt, sts_refresher, meta_proxy, mount), `tests/` (load, security, acceptance), Makefile (test.enc.integration).
- **agio-platform (`feature/drive-v2`):** `src/application/authz/proto/key_manager.proto` + generated code, `src/application/authz/service/key_manager_service.go` + `fek_crypto.go`, `src/internal/drive/infrastructure/adapters/` (kms_yandex, lockbox_yandex, company_kek, identity_resolver, sts_aws/sts_yc), `src/internal/drive/application/ports/crypto.go`+`sts.go`, PG-миграции 000203/000204 (следующие свободные; актуальный максимум в platform — 000202) + SQLBoiler, `src/api/iam_interceptor.go`, wire/config, CLI `keymanager`.
- **Инфраструктура (`agio-terraform-yc` + `agio-cloud`):** KMS master keys per-company (`yandex_kms_symmetric_key` + rotation_period), Lockbox (Company KEK — именованные секреты; YC не имеет сервиса «Secret Manager»), IAM/STS (замена статических S3-credentials — обязательное условие реального отзыва, R8), Redis metadata backups (внутренние бэкапы YC, retention 35 дней; SSE-KMS/S3-экспорт сервисом не поддерживается — OQ1 change `drive-crypto-infra`), audit storage ≥12 мес. Terraform-ресурсы — в `agio-terraform-yc` (изменения трекаются в его собственном openspec); k8s values для новых флагов platform (`kms_key_id`, `lockbox_folder`, IAM) — в `agio-cloud` (chart `platform-api`).
- **API:** расширение `MetaService` proto (форк) и новый сервис `DriveKeyManagerService` (platform); формат slice-записи в Redis получает опциональный хвост (см. BREAKING выше).
- **Зависимости:** YC SDK (KMS/Lockbox/IAM), AWS SDK (STS, альтернативный провайдер), `golang.org/x/sys/unix` (mlock) — проверить наличие в vendor/go.mod при реализации.
