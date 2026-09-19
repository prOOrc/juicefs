# Design: implement-encrypt

## Context

См. proposal.md — Why. Фактическое состояние (точка отсчёта, проверено по ветке `agio-drive-v2`):

- **Реализовано:** gRPC Meta Proxy (`MetaProxyServer` оборачивает неизменённый `redisMeta`; клиент `grpcMeta` реализует `meta.Meta` напрямую), OIDC authn (серверный interceptor + клиентский `withAuth()` с singleflight), authz через внешний gRPC `AuthzService` agio-platform (`authz_interceptor.go`, `InodePathCache`, кэш решений TTL 30s; Company Owner bypass — на стороне platform: `auth.PermissionOwn` в `src/internal/drive/infrastructure/adapters/auth.go`). Данные НЕ проходят через proxy — только slice-метаданные; S3-credentials клиент получает из `Format` через `Load` (`NewReloadableStorage`, cmd/mount.go:462).
- **Greenfield:** CEK/FEK/KEK, KeyManager, render-клиент, STS, encrypted local cache, offline-режим, revocation-механика, миграция legacy-данных.
- **Критические ограничения (не нарушать):** (1) render-ноды — нативный FUSE с прямым Redis+S3, без proxy/OIDC/authz; (2) шифрование только на клиенте (chunk store); (3) Redis = source of truth для метаданных; (4) инвариант идентичности A6/S12: OIDC `sub` ≡ `kratos.identity_id` ≡ `user.id` (UUID), маппинга нет.
- **Контракт:** целевое поведение зафиксировано в delta-спеке этого change `specs/domain-encrypt/spec.md` (greenfield-капабилити; при архивации change она становится SoT `openspec/specs/domain-encrypt/`); межэтапные бинарные форматы и решения D1–D12 — в мастер-плане §4 (`.qwen/plans/agio-drive-encrypt-master.md`).

## Goals / Non-Goals

**Goals:**
- Реализовать все требования `domain-encrypt` (baseline + delta этого change) в двух репозиториях по 10 этапам.
- Сохранить zero-copy семантику Clone/CopyFileRange/Compaction через re-wrap CEK (операция над ключом, не над данными).
- Поддержать identity-less render-ноды как первоклассных клиентов шифрования.
- Обеспечить fail-closed поведение на любой ошибке криптографии, authz или identity.

**Non-Goals:**
- Версионирование/снепшоты/корзина — только хуки-предусловия (FR-VER-1…4).
- Zero-knowledge режим (SRS §13).
- Совместимость с upstream JuiceFS (S2); целевой metadata-бэкенд — Redis.
- Де-энкрипция (обратная миграция).

## Architectural Context

Иерархия ключей (4 уровня):

```
KMS Master Key (per-company, Yandex KMS / AWS KMS)
    │ wrap
    ▼
Company KEK (32B, Lockbox; один на компанию)
    │ wrap → AGFK blob в Redis inode attr
    ▼
FEK (32B, per file/inode; plaintext — только в RAM клиента/proxy на время запроса)
    │ wrap → AGCK blob в slice-записи Redis
    ▼
CEK (32B, per slice; plaintext — только в RAM на время операции)
    │ AES-256-GCM → AGDF blob
    ▼
Блок данных в S3 (ciphertext)
```

Планы и их роли:

| Plane | Путь | Шифрование |
|---|---|---|
| Metadata (user) | AGIO Client → Meta Proxy gRPC → Redis | `wrapped_fek`/`wrapped_cek` в метаданных; plaintext FEK только в `OpenResponse` (TLS) |
| Data (user/render) | Client → S3 напрямую | AGDF-блоки; кэш — ciphertext |
| Authz | Meta Proxy / KeyManager → agio-platform `AuthzService` → PG | authz-gated выдача FEK (Read/Edit), Company Owner bypass |
| Render | Render Client → Redis + S3 напрямую, без proxy/OIDC/PG | локальный unwrap FEK по Company KEK (из `FetchCompanyKEK` при mount) |

Два репозитория: форк JuiceFS (`/Users/i.obukhov/github/juicefs`, ветка `agio-drive-v2`) — data path, meta-интеграция, render-клиент, миграция; agio-platform (`/Users/i.obukhov/ai/agio/agio-platform`, ветка `feature/drive-v2`) — KeyManager, KMS/Lockbox/STS-адаптеры, PG-таблицы, аудит. Связь — gRPC `agio.platform.drive.crypto.v1.DriveKeyManagerService` (TLS), тот же сервер, что `AuthzService` (решение D6).

## Component Map

**Форк JuiceFS:**

| Компонент | Файл | Этап |
|---|---|---|
| Крипто-примитивы AGDF/AGCK (EncryptBlock/DecryptBlock/WrapCEK/UnwrapCEK) | `pkg/chunk/cek_encrypt.go` (новый) | 2 |
| Крипто-примитивы AGFK (WrapFEK/UnwrapFEK, `FekAAD`) | `pkg/meta/fek_crypto.go` (новый) | 2 |
| Slice-запись с `wrapped_cek` (24B base + optional AGCK tail) | `pkg/meta/slice.go`, `pkg/meta/interface.go` (`Slice.WrappedCEK`) | 2 |
| Crypto-поля `Attr` (persisted suffix) + transient `Fek` | `pkg/meta/interface.go` | 2 |
| `Format.EncryptionEnabled/KEKVersion` | `pkg/meta/config.go` | 2 |
| `ChunkStore.NewReaderWithKey/NewWriterWithKey`; `rSlice.cek`/`wSlice.cek` | `pkg/chunk/chunk.go`, `pkg/chunk/cached_store.go` | 2 |
| VFS plumbing: `handle.fek/fekVer/encrypted`, CEK-кэш в `fileReader`/`fileWriter` | `pkg/vfs/handle.go`, `reader.go`, `writer.go` | 2 |
| Версионирование-хуки: `baseMeta.fileCryptoDeletable/sliceDeletable/compactionAllowed` | `pkg/meta/base.go` | 2, 5 |
| Proto-расширения (`ProtoFileCrypto`, `ProtoSlice.wrapped_cek`, `OpenResponse.fek`, `CreateRequest.drive_file_id`, `Format.encryption_enabled/kek_version`) + конверсии | `pkg/meta/pb/*.proto`, `pkg/meta/grpc_convert.go` | 3 |
| `redisMeta.SetFileCrypto` (один Redis txn) | `pkg/meta/redis_fek.go` (новый) | 3 |
| KeyManager-клиент форка (копия proto platform + gRPC-обёртка) | `pkg/meta/keymanager_pb/`, `pkg/meta/keymanager_client.go` (новые) | 3 |
| Proxy Create/Open с KeyManager + rollback; UUID-валидация identity | `pkg/meta/grpc_server_fuse.go`, `pkg/meta/authz_interceptor.go` | 3 |
| Клиентский FEK LRU (100k/15 мин), `drive_file_id` генерация | `pkg/meta/grpc_client.go`, `grpc_client_fuse.go` | 3 |
| Render-декоратор `RenderMeta` (Open/Create/GetAttr, локальный unwrap) | `pkg/meta/render_meta.go` (новый) | 4 |
| Команда `juicefs render-mount` | `cmd/render_mount.go` (новый) | 4 |
| `redisMeta.RewrapSlices`; RPC `ResolveFileKey`; компакция с CEK (`vfs.Compact` + `doCompactChunk` аргумент `wrappedCEK`) | `pkg/meta/redis_fek.go`, `pkg/meta/pb/meta.proto`, `pkg/vfs/compact.go`, `pkg/meta/base.go` | 5 |
| Clone-оркестрация в proxy (re-wrap под FEK target) | `pkg/meta/grpc_server_fuse.go` | 5 |
| `MemClear`/`MlockPage`; state machine hub; `WipeKeys`; write journal | `pkg/utils/memclr.go`, `pkg/meta/grpc_client.go`, `pkg/vfs/vfs.go`, `pkg/vfs/write_journal.go` (новые) | 6 |
| Heartbeat generation (`FlushSessionResponse.permission_generation`); `stsRefresher`; `ReloadableStorage.SetCredentials`; RPC `RotateFileKey`/`RotateFileKeysByPaths` | `pkg/meta/pb/meta_lifecycle.proto`, `cmd/sts_refresher.go` (новый), `cmd/mount.go`, `pkg/meta/grpc_server_fuse.go` | 7 |
| `baseMeta.ReencryptChunk`; команды `juicefs reencrypt`, `enable-encryption` | `pkg/meta/base.go`, `cmd/reencrypt.go`, `cmd/reencrypt_worker.go` (новые) | 8 |
| Тестовая инфраструктура: known-answer векторы, compose-среда, load harness, security-скрипты, AC-чеклист | `pkg/agio/testcrypto/vectors.json`, `docker-compose.enc-test.yml`, `tests/load/`, `tests/security/`, `tests/acceptance.md` (новые) | 9 |
| Метрики, алерты, runbooks, audit package | `docs/ops/`, `docs/security/` (новые) | 10 |

**agio-platform:**

| Компонент | Файл | Этап |
|---|---|---|
| Порты: `KMS`, `SecretStore`, `CompanyKEKService`, `IdentityResolver`, `KeyAccessLogger` | `src/internal/drive/application/ports/crypto.go` (новый) | 1 |
| Yandex KMS / Lockbox-адаптеры + fake для тестов | `src/internal/drive/infrastructure/adapters/kms_yandex.go`, `lockbox_yandex.go`, `crypto_fake.go` (новые) | 1 |
| `CompanyKEKService` (GetKEK с RAM-LRU TTL 5 мин, ProvisionKEK) | `src/internal/drive/infrastructure/adapters/company_kek.go` (новый) | 1 |
| PG-миграции `drive_company_crypto_key`, `drive_key_access_log` + SQLBoiler | `src/infrastructure/db/migrations/000203*`, `000204*` (следующие свободные номера; актуальный максимум в platform — 000202) | 1 |
| Proto `DriveKeyManagerService` + generated code | `src/application/authz/proto/key_manager.proto` (новый) | 1, 7 |
| `KeyManagerService` (CreateFileKey/GetFileFEK/GetBulkFileFEK/FetchCompanyKEK/ProvisionCompanyKEK; singleflight, аудит) + AGFK-хелперы | `src/application/authz/service/key_manager_service.go`, `fek_crypto.go` (новые) | 1 |
| IAM-interceptor для `FetchCompanyKEK` | `src/api/iam_interceptor.go` (новый) | 1 |
| Permission generation (INCR `drivepermgen:{userID}` при SetRole/DeleteRole/ClearRoles + `Invalidate` кэша); RPC `GetPermissionGeneration`, `GetSTSCredentials`; STS-провайдер (AWS/YC) | `src/internal/drive/infrastructure/adapters/auth.go`, `sts_aws.go`, `sts_yc.go` (новые), `src/internal/drive/application/ports/sts.go` | 7 |
| `GenerateRotatedFileKey`; CLI `keymanager` (provision, rotate-user-keys) | `src/application/authz/service/key_manager_service.go`, `src/cmd/keymanager.go` (новый) | 7 |

## Execution Flow

**1. User Create (зашифрованный том).** Client → proxy `Create` (с client-generated `drive_file_id`) → `redisMeta.Create` → proxy: `extractUserIDFromOIDC` (UUID-валидация, fail-closed EACCES) → KeyManager `CreateFileKey` (authz Write на path; FEK = CSPRNG 32B; `wrapped_fek = WrapFEK(KEK, FEK, AAD)`) → `redisMeta.SetFileCrypto` (один txn: GET attr → mutate crypto-поля → SET). Ошибка любого шага после создания inode → `Unlink` (rollback, FR-USR-2). Клиент получает FEK в ответе и шифрует блоки CEK-ами.

**2. User Open.** Authz interceptor проверяет Read/Edit на каждом Open (всегда). Proxy: если `attr.Encrypted` и `OpenRequest.cached_fek_version != attr.FekVersion` → KeyManager `GetFileFEK` (authz Read/Edit, singleflight, аудит) → plaintext FEK в `OpenResponse.fek` (TLS only). При совпадении версий — KeyManager не вызывается, клиент берёт FEK из LRU (NFR-PERF-1). `GetAttr`/`Readdir`/`Lookup` несут только `wrapped_fek`.

**3. Data path (read/write).** Read: `fileReader` для каждого slice с `WrappedCEK` → `UnwrapCEK(fek, ...)` один раз (CEK-кэш per open file) → `store.NewReaderWithKey(sliceID, size, cek)` → блок из кэша/S3 → `DecryptBlock` (единая точка после получения данных). Write: `sliceWriter` генерирует CEK (CSPRNG) → `store.NewWriterWithKey` → `EncryptBlock` перед upload/staging → кэш и S3 получают ciphertext; при commit `WrappedCEK = WrapCEK(fek, cek, ...)` в slice-записи. Legacy (без CEK/без magic) — passthrough без изменений.

**4. Render mount.** `juicefs render-mount META_URL MP --company-id ...` → прямой `redisMeta` (subdir = `companies/{code}`, chroot) → один gRPC-вызов `FetchCompanyKEK` (IAM ноды, TLS 1.3) → KEK в RAM (mlock) → декоратор `RenderMeta`: Open — FEK из LRU (100k/1h) или локальный `UnwrapFEK`; Create — локальная генерация FEK + `SetFileCrypto`. Без OIDC/proxy/authz/PG. Чужая компания → unwrap fail → EIO.

**5. Clone (zero-copy).** Proxy: `GetFileFEK(src)` (authz Read) → `meta.Clone` (verbatim-копия chunk lists + refcount) → `CreateFileKey(dst)` → `SetFileCrypto(dst)` → `RewrapSlices(dst, FEK_src → FEK_dst)` (один txn на chunk list: UnwrapCEK под srcFek → WrapCEK под dstFek; legacy-записи не трогаются). S3 не переписывается. Ошибка re-wrap → `Unlink(dst)`.

**6. Compaction.** `baseMeta.compactChunk` → хук `compactionAllowed` (FR-VER-3) → `fileKeyResolver(inode)` (user mode: RPC `ResolveFileKey`; render: локальный unwrap) → ошибка → компакция пропускается (fail-closed, данные целы) → `vfs.Compact`: read source slice'ов с CEK → merge в новый slice с новым CEK → `WrapCEK(fek, newCEK)` → `doCompactChunk` атомарно заменяет chunk list.

**7. Revocation.** Platform: DeleteRole/ClearRoles → soft-delete прав + `INCR drivepermgen:{userID}` + `Invalidate` PermissionCache (TTL ≤ 30s в production). Proxy: на каждом heartbeat `FlushSession` читает generation → `FlushSessionResponse.permission_generation`. Клиент: изменение generation → `WipeKeys()` + `VFS.InvalidateAllKeys()` (memclr FEK/CEK, handles → stale EIO). Data plane: STS-токен истекает ≤ 60 мин (`stsRefresher` обновляет credentials локально, в Redis `Format` не пишет).

**8. Migration.** `juicefs reencrypt`: walk от root → для legacy-файла: генерация FEK + `SetFileCrypto(encrypted=1)` сразу (новые записи — зашифрованные) → по чанкам: read legacy slice'ов → новый slice с новым CEK → `ReencryptChunk` (атомарный swap chunk list + refcount, с проверкой неизменности list). Идемпотентность из состояния Redis: чанк «сделан» = все записи несут AGCK. Rate-limits: concurrency/IOPS/bandwidth.

## Invariants

1. **Identity (A6/S12):** OIDC `sub` ≡ `user.id` (UUID) во всех компонентах; маппинга/преобразования нет; неверный формат UUID → fail-closed (`Unauthenticated` на proxy, `InvalidArgument` в KeyManager); пользователь без записей на платформе → deny authz-слоем (FR-ID-4).
2. **Plaintext только в RAM:** CEK/FEK/Company KEK не пишутся ни в одно стойкое хранилище (Redis, PG, S3, файлы, логи, метрики, core dumps); transient `Attr.Fek` никогда не маршализуется.
3. **Fail-closed:** любая ошибка криптографии (tag mismatch, AAD mismatch, unwrap), authz или identity → отказ операции; plaintext не возвращается.
4. **Zero-copy:** Clone и FEK rotation — re-wrap CEK в метаданных; S3-объекты не переписываются. CEK rotation / миграция — единственные операции с перешифровкой данных.
5. **Legacy-совместимость (внутренняя):** 24B slice-запись и `Attr` без crypto-suffix парсятся как plaintext; mixed chunk list (legacy + encrypted записи) валиден во время миграции.
6. **0 доп. round-trips:** `wrapped_fek` читается в том же GET, что attr; `wrapped_cek` — в той же slice-записи.
7. **Один unwrap на файл за сессию:** FEK — LRU + `cached_fek_version`; CEK — кэш per open file.
8. **Nonce uniqueness:** новый случайный 12B nonce на каждую запись с тем же ключом (NFR-SEC-9).

## Decisions & Rationale

Зафиксированные решения мастер-плана §4.8 (D1–D12) + решения stage-планов. Ключевые с альтернативами:

| # | Решение | Альтернатива и почему отклонена |
|---|---|---|
| D1 | `wrapped_fek` — variable-length suffix в `Attr` (паттерн `Tier`/ACL) | xattr: +1 Redis round-trip на GetAttr (нарушает NFR-PERF-6), невидимость POSIX-утилитам хуже; отдельный key `fek/{inode}`: +N GET на каждый Open |
| D2 | `wrapped_cek` — опциональный length-prefixed AGCK-хвост в slice-записи (24B base неизменён) | Отдельный key per slice: +N round-trips на Read; новый фиксированный размер записи: ломает все существующие данные без миграции |
| D3 | CEK = per-slice («чанк» SRS ≙ slice), AAD блока = `sliceID ‖ blockIndex` | Per-chunk (логический чанк из slice'ов): slice шарится через refcount — CEK должен жить в slice-записи; per-block CEK: раздувание метаданных ×64 на чанк |
| D4 | Хуки шифрования в `cachedStore` (`rSlice.cek`/`wSlice.cek`); disk cache получает ciphertext как есть | Обёртка `ObjectStorage` (SRS §17.1 `pkg/object/fek_encrypt.go`): не покрывает writeback staging и кэш; изменения в `disk_cache.go` не нужны — он хранит те байты, которые ему дают |
| D5 | Plaintext FEK — в `OpenResponse.fek` (user) / локальный unwrap (render) → `handle.fek`; CEK per-slice при записи, wrap при commit | FEK в attr-кэше/GetAttr: plaintext в кэшируемых ответах (T6); CEK в отдельном RPC: +N round-trips |
| D6 | KeyManager — в agio-platform, тот же gRPC-сервер что `AuthzService`; без кэша FEK (только singleflight) | KeyManager в форке: дублирование authz/PG/Redis-зависимостей; кэш FEK в KeyManager: нарушение T6 («не дольше запроса») |
| D7 | Clone = verbatim-копия + post-hoc `RewrapSlices` | Re-encrypt при Clone: потеря zero-copy (FR-OP-2); inline re-wrap в `doCloneEntry`: redisMeta не знает FEK (он у proxy/KeyManager) |
| D8 | FEK для фоновой компакции — хук `baseMeta.fileKeyResolver`; `CompactChunk` msg расширяется аргументами `{fek, driveFileID, fekVersion}` | Компакция только по явному запросу: фоновая компакция — основной путь; FEK в slice-записях: plaintext в Redis (нарушение инварианта 2) |
| D9 (ред. 7.1) | Revoke-signal через heartbeat `FlushSession`: `permission_generation` из Redis platform (`drivepermgen:{userID}`), proxy опрашивает `GetPermissionGeneration` | Отдельный streaming RPC: новый канал там, где есть готовый heartbeat 12s; counter в Redis форка: proxy не имеет доступа к Redis platform — счётчик живёт в Redis platform (решение 7.1) |
| D10 | STS: user mode — RPC `GetSTSCredentials` (prefix-scoped политика); render — прямой cloud STS по IAM ноды; swap credentials локально через `ReloadableStorage.SetCredentials` | Запись STS-токенов в Redis `Format`: утечка чужих токенов всем клиентам + лишние writes (решение 7.2) |
| D11 | Identity: только UUID-валидация формата; DB-запроса на существование пользователя нет | Kratos/email fallback: не нужен при инварианте A6; проверка существования в KeyManager: дублирует authz deny (FR-ID-4) |
| D12 | KMS/Lockbox — порты + Yandex-реализации первыми, интерфейс провайдер-независимый (порт `SecretStore`; сервиса «Secret Manager» в YC нет) | AWS-first: платформа деплоится в YC; hard-coded провайдер: ломает A1 |
| 3.1 | `OpenRequest.cached_fek_version`: при совпадении с `attr.FekVersion` proxy не вызывает KeyManager | Вызов KeyManager на каждый Open: NFR-PERF-1 (≤5 мс p99) недостижим; authz interceptor при этом проверяется ВСЕГДА (отзыв прав не обходится) |
| 3.3 | Rollback Create: ошибка KeyManager/SetFileCrypto после `meta.Create` → `Unlink` + EIO | Файл «без FEK» как unusable-маркер: оставляет мусор в листинге; окно безопасно — данные не пишутся до получения FEK |
| 5.1 | `RewrapSlices` — один Redis txn на chunk list, последовательно по чанкам | Один глобальный txn на файл: блокировка/размер txn на больших файлах; частичный re-wrap при сбое → target `Unlink` (unusable), source не затрагивается |
| 6.5 | Offline-записи — append-only write journal (`<cache-dir>/writejournal/<session>.jrl`) с replay по seq при reconnect | Только staging-данные без журнала: теряется порядок метаданных (slice-записи); блокировка записей offline: нарушает NFR-OFF-1 |
| 7.7 | Offboarding — batch RPC proxy `RotateFileKeysByPaths` (rate-limited, resumable, checkpoint) | CLI platform напрямую в Redis: platform не трогает Redis форка (решение 1.1); per-file RotateFileKey в цикле: N RPC + нет rate-limit/checkpoint на стороне proxy |
| 8.3 | Идемпотентность миграции из состояния Redis (чанк «сделан» = все записи несут AGCK) | Внешний checkpoint-файл: рассинхрон при ручных изменениях; таблица прогресса в PG: evental consistency + новое хранилище |

## Integration Points

1. **Форк ↔ agio-platform (gRPC, TLS):** сервис `agio.platform.drive.crypto.v1.DriveKeyManagerService`. Форк держит копию proto в `pkg/meta/keymanager_pb/` (паттерн `authz_pb/`); синхронизация контролируется cross-repo known-answer тестами (stage 9) — расхождение форматов AGFK/AGCK/AGDF между репозиториями = критический баг.
2. **KMS / Lockbox (Yandex первыми, D12):** `WrapKey/UnwrapKey` по keyID; Company KEK — именованные секреты `drive/kek/{companyID}/v{version}` в Lockbox (folder из флага `lockbox_folder`; YC не имеет сервиса «Secret Manager»). Fake-реализации для тестов.
3. **STS:** AWS `AssumeRole` с inline session policy на company prefix (паттерн `S3AuthzHandler` platform); YC — проверить возможности IAM (Open Question). TTL ≤ 60 мин.
4. **Redis (форк):** attr suffix + slice tail (этап 2), `SetFileCrypto`/`RewrapSlices` txn (этапы 3/5), `drivepermgen:{userID}` counter в Redis platform (этап 7). Backups — внутренние бэкапы YC, retention 35 дней (stage 10, FR-REDIS-5; SSE-KMS/S3-экспорт сервисом не поддерживается).
5. **PG (platform):** `drive_company_crypto_key` (управление KEK), `drive_key_access_log` (аудит, append-only, ≥12 мес); SQLBoiler-регенерация после миграций.
6. **Known-answer векторы:** идентичный `vectors.json` в обоих репозиториях — единственная защита от дрейфа бинарных форматов.
7. **YC-инфраструктура (`agio-terraform-yc`, `agio-cloud`):** KMS master keys per-company (`yandex_kms_symmetric_key`, rotation_period — см. Open Question 5), Lockbox (именованные секреты `drive/kek/{companyID}/v{version}`; YC не имеет сервиса «Secret Manager»), IAM service accounts / STS-роли (render-ноды, user-сессии), Redis metadata backups (внутренние бэкапы YC, retention 35 дней; SSE-KMS/S3-экспорт сервисом не поддерживается — OQ1 change `drive-crypto-infra`) — Terraform в `agio-terraform-yc` (изменения трекаются в его собственном openspec, spec-driven); k8s values для новых флагов platform (`kms_key_id`, `lockbox_folder`, IAM) — `agio-cloud` (chart `platform-api`).

## Risks / Trade-offs

- [Старые читатели падают на slice-записях с AGCK-хвостом] → rollout-правило (stage 10): включить шифрование только после обновления всех клиентов; mixed-состояние данных допустимо только legacy→encrypted.
- [Горячий путь `store.load`/`upload`: overhead ≤10% (NFR-PERF-3)] → AES-GCM с AES-NI; зашифрованный путь короче legacy (без compressor); нагрузочное сравнение — stage 9 (FR-TEST-24).
- [Потеря Redis = потеря `wrapped_fek` = потеря данных (R2)] → внутренние бэкапы YC (retention 35 дней) + AOF + restore-тест (stage 10).
- [STS не реализован → отзыв доступа к S3 невозможен (R8, критический)] → stage 7 — приоритет; до STS отзыв ограничен authz+FEK-кэшами.
- [Redis load от сотен render-нод (R9)] → aggressive caching ≥60s, pipelined readdir, 0 доп. round-trips на FEK (unwrap локальный); нагрузочный тест FR-TEST-25.
- [Дрейф идентичности Kratos↔platform (R12)] → fail-closed (FR-ID-4) + daily reconciliation-мониторинг (stage 10).
- [FEK lifecycle не заложен → переделка при версионировании (R13)] → хуки FR-VER-1…4 в этапах 2/5.
- [Go GC может оставить копии ключей в heap] → `MemClear` + mlock — best-effort; дамп памяти подключённого клиента — принятая граница (SRS §3.3, T9), не заявляем больше.
- [Writeback staging: двойное шифрование] → magic-проверка AGDF перед encrypt (решение 2.5) + тест.
- [Crash recovery без FEK в `saveHandle`] → повторный `meta.Open` при восстановлении handle; fail-closed EIO до получения FEK.

## Migration Plan

1. **Per-company rollout** (stage 10): пилотная компания (test) → stage → production по одной компании (`rb-enable-company`).
2. **Предусловия per company:** все клиенты обновлены до версии с поддержкой slice-формата v2; render-ноды обновлены; STS включён; Redis metadata backups проверены restore-тестом.
3. **Включение:** `ProvisionCompanyKEK` → `juicefs enable-encryption` (идемпотентно, `Format.EncryptionEnabled=true`) → новые файлы шифруются по умолчанию; legacy читаются passthrough.
4. **Миграция legacy:** `juicejs reencrypt` (rate-limited, возобновляемая) в off-peak.
5. **Rollback:** отключить создание новых зашифрованных файлов (`EncryptionEnabled=false` для новых файлов) + чтение старых продолжается (новые клиенты читают оба формата). Полное «откатить шифрование» = де-энкрипция — отдельный проект, out of scope.

## Open Questions

1. **YC IAM: prefix-scoped S3-политики?** Если Yandex не даёт prefix-scoped STS-политики — role с минимальными правами на бакет + аудит (решение фиксируется в stage 7, не меняет спеки).
2. **Идемпотентность replay write journal:** семантика `redisMeta.doWrite` при повторном append того же slice — проверить в stage 6; fallback: replay только подтверждённых записей + явная метка в логе.
3. **Направление зависимости pkg/meta → pkg/chunk** для AGCK-примитивов (`RewrapSlices`): если `pkg/meta` не импортирует `pkg/chunk` — вынести AGCK-хелперы в общий пакет (решение фиксируется в stage 5).
4. **Доступ к «сырому» `redisMeta` из `meta.NewClient`** для декоратора `RenderMeta` — проверить в stage 4; при необходимости конструктор напрямую.
5. **Yandex KMS auto-rotation:** поведение Decrypt для старых версий ключа после ротации — проверить документацией + тестом ДО включения (stage 10); fallback — manual re-wrap KEK.
