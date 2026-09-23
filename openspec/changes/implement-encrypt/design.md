# Design: implement-encrypt

## Context

См. proposal.md — Why. Фактическое состояние (точка отсчёта, проверено по ветке `agio-drive-v2`):

- **Реализовано:** gRPC Meta Proxy (`MetaProxyServer` оборачивает неизменённый `redisMeta`; клиент `grpcMeta` реализует `meta.Meta` напрямую), OIDC authn (серверный interceptor + клиентский `withAuth()` с singleflight), authz через внешний gRPC `AuthzService` agio-platform (`authz_interceptor.go`, `InodePathCache`, кэш решений TTL 30s; Company Owner bypass — на стороне platform: `auth.PermissionOwn` в `src/internal/drive/infrastructure/adapters/auth.go`). Данные НЕ проходят через proxy — только slice-метаданные; S3-credentials клиент получает из `Format` через `Load` (`NewReloadableStorage`, cmd/mount.go:462).
- **Greenfield:** CEK/FEK/KEK, KeyManager, render-клиент, STS, encrypted local cache, offline-режим, revocation-механика, миграция legacy-данных.
- **Критические ограничения (не нарушать):** (1) render-ноды — нативный FUSE с прямым Redis+S3, без proxy/OIDC/authz; (2) шифрование только на клиенте (chunk store); (3) Redis = source of truth для метаданных; (4) инвариант идентичности A6/S12: OIDC `sub` ≡ `kratos.identity_id` ≡ `user.id` (UUID), маппинга нет.
- **Контракт:** целевое поведение зафиксировано в delta-спеке этого change `specs/domain-encrypt/spec.md` (greenfield-капабилити; при архивации change она становится SoT `openspec/specs/domain-encrypt/`); межэтапные бинарные форматы, proto-контракты, PG-таблицы и параметры кэшей — раздел «Межэтапные контракты» этого документа; решения D1–D12 и stage-решения — раздел «Decisions & Rationale».

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

## Межэтапные контракты (зафиксированы, не менять без ревизии SRS)

Эти интерфейсы — «швы» между этапами и репозиториями. При реализации **не отклоняться** от форматов; расхождение форматов AGFK/AGCK/AGDF между agio-platform и форком = критический баг (защита — cross-repo known-answer векторы, этап 9).

### Бинарные форматы (SRS §4.3)

Алгоритм везде: **AES-256-GCM** (NFR-SEC-6); ключи 32 байта из `crypto/rand` (NFR-SEC-7); все многобайтовые целые — big-endian; новый случайный 12B nonce на каждую запись с тем же ключом (NFR-SEC-9); при чтении tag проверяется, несовпадение → fail-closed (NFR-SEC-11). Legacy-чанки без magic читаются как plaintext (`encrypted=0`, NFR-COMPAT-1).

| Объект | Magic | Layout (порядок полей) | Размер | AAD |
|---|---|---|---|---|
| `wrapped_fek` (Redis, inode metadata) | `AGFK` | magic(4) ‖ version(1)=1 ‖ kek_version(4) ‖ nonce(12) ‖ ciphertext(32) ‖ tag(16) | 69 B | len(volume_uuid):4 ‖ volume_uuid ‖ len(company_id):4 ‖ company_id ‖ len(drive_file_id):4 ‖ drive_file_id ‖ inode(8) ‖ fek_version(4) |
| `wrapped_cek` (slice metadata) | `AGCK` | magic(4) ‖ version(1)=1 ‖ nonce(12) ‖ ciphertext(32) ‖ tag(16) | 65 B | len(drive_file_id):4 ‖ drive_file_id ‖ slice_id(8) ‖ fek_version(4) |
| Зашифрованный чанк (S3) | `AGDF` | magic(4) ‖ version(1)=1 ‖ nonce(12) ‖ ciphertext(N) ‖ tag(16) | 33+N B | slice_id(8) ‖ block_index(4) |

Строковые поля AAD — length-prefixed (u32 BE длина + UTF-8 байты), что делает кодировку однозначной. Overhead AGDF над plaintext = 33 байта (`agdfOverhead`).

### Поля метаданных в Redis (SRS §5.1)

| Поле | Где | Тип |
|---|---|---|
| `wrapped_fek` | inode metadata (Attr suffix), читается в том же round-trip что GetAttr (NFR-PERF-6) | bytes (формат AGFK) |
| `wrapped_cek` | slice metadata (расширение структуры Slice) | bytes (формат AGCK) |
| `drive_file_id` | inode metadata | UUID (client-generated, FR-USR-3) |
| `encrypted` | inode metadata | bool |
| `fek_version` | inode metadata + в AAD обёрток | uint32 |
| `crypto_alg` | inode metadata | string (`"AES-256-GCM"`) |

В Redis **не хранится**: plaintext FEK/CEK, Company KEK (SRS §5.2).

Layout Attr suffix (D1): при `Encrypted || len(WrappedFek) > 0` после существующего хвоста — `0x01 ‖ u32 len(WrappedFek) ‖ WrappedFek ‖ u32 len(DriveFileID) ‖ DriveFileID ‖ u32 FekVersion ‖ u8 len(CryptoAlg) ‖ CryptoAlg`. Transient `Attr.Fek []byte` никогда не маршализуется.

Layout slice-записи (D2): `[24B fixed: pos(4) id(8) size(4) off(4) len(4)] [u32 blobLen + AGCK blob]` — хвост опционален; 24B = legacy plaintext.

### Proto-расширения MetaService (форк, SRS §15.2)

```proto
// meta_common.proto
message ProtoFileCrypto {
  bytes  wrapped_fek = 1;
  string drive_file_id = 2;
  bool   encrypted = 3;
  int32  fek_version = 4;
  string crypto_alg = 5;
}
message ProtoAttr { /* ...существующие поля... */ ProtoFileCrypto file_crypto = <next>; }
message ProtoSlice { uint64 id=1; uint32 size=2; uint32 off=3; uint32 len=4; bytes wrapped_cek = 5; } // НОВОЕ (AGCK)
message ProtoFormat { /* ...существующие... */ bool encryption_enabled = <next>; int32 kek_version = <next>; }

// meta.proto
message CreateRequest { /* ...существующие... */ string drive_file_id = <next>; }  // client-generated UUID (FR-USR-3)
message OpenRequest  { /* ...существующие... */ uint32 cached_fek_version = <next>; } // решение 3.1
message OpenResponse { uint32 errno=1; ProtoAttr attr=2; bytes fek=<next>; int32 fek_version=<next>; bool encrypted=<next>; } // fek — TLS only (FR-API-1)

// meta_lifecycle.proto (этап 7)
message FlushSessionResponse { /* ...существующие... */ uint64 permission_generation = <next>; }

// Новые RPC (этапы 5/7):
rpc ResolveFileKey(ResolveFileKeyRequest) returns (ResolveFileKeyResponse);    // authz Read; plaintext FEK (TLS only)
rpc RotateFileKey(RotateFileKeyRequest) returns (RotateFileKeyResponse);       // admin-gated FEK rotation
rpc RotateFileKeysByPaths(RotateFileKeysByPathsRequest) returns (RotateFileKeysByPathsResponse); // admin-gated batch (решение 7.7)
```

### KeyManagerService (agio-platform, SRS §15.1)

Пакет `agio.platform.drive.crypto.v1`, сервис `DriveKeyManagerService` — тот же gRPC-сервер, что `AuthzService` (D6):

```proto
service DriveKeyManagerService {
  rpc CreateFileKey(CreateFileKeyRequest) returns (CreateFileKeyResponse);
  rpc GetFileFEK(GetFileFEKRequest) returns (GetFileFEKResponse);
  rpc GetBulkFileFEK(GetBulkFileFEKRequest) returns (GetBulkFileFEKResponse);    // до 1000 ключей
  rpc RotateFileFEK(RotateFileFEKRequest) returns (RotateFileFEKResponse);       // этап 7
  rpc RotateCompanyKEK(RotateCompanyKEKRequest) returns (RotateCompanyKEKResponse); // этап 7
  rpc FetchCompanyKEK(FetchCompanyKEKRequest) returns (FetchCompanyKEKResponse); // render-ноды, IAM-токен
  rpc ProvisionCompanyKEK(ProvisionCompanyKEKRequest) returns (ProvisionCompanyKEKResponse);
  rpc GetPermissionGeneration(GetPermissionGenerationRequest) returns (GetPermissionGenerationResponse); // этап 7
  rpc GetSTSCredentials(GetSTSCredentialsRequest) returns (GetSTSCredentialsResponse); // этап 7
  rpc GenerateRotatedFileKey(GenerateRotatedFileKeyRequest) returns (GenerateRotatedFileKeyResponse); // этап 7
}
```

Полные message-определения — tasks.md, Stage 1, шаг 6. Authz-гейтинг: `DriveAuthorizationService.CheckPermissionByPath`/`CheckBulkPermissionsByPaths` с Read/Edit, Company Owner bypass (`PermissionOwn`), fail-closed (SRS §15.3).

**Определение компании (решение 3.8):** `CreateFileKey`/`GetFileFEK` принимают полный путь клиента и `volume_name`; `company_id` клиентом **не передаётся** — platform определяет компанию из первого сегмента пути после отсечения companies prefix тома (тот же механизм, что в `AuthzService`) и резолвит код в UUID. Клиент монтирует весь facility и видит компании по authz; компания для KEK/AAD выводится из пути. `FetchCompanyKEK`/`ProvisionCompanyKEK` принимают `company_id` явно (render-нода/admin знают свою компанию).

### Таблицы PG (agio-platform, SRS §5.3)

DDL — tasks.md, Stage 1, шаг 1: `drive_company_crypto_key` (управление Company KEK: `key_purpose`, `key_version`, `kms_key_id`, `secret_ref`, `status active|retiring|retired`) и `drive_key_access_log` (аудит выдачи FEK, append-only, retention ≥ 12 мес). Таблицы `file_keys`/`subject_keys` из архитектурного плана v13 **исключены**.

### Параметры клиентских кэшей (SRS §14.2)

| Кэш | Размер | TTL |
|---|---|---|
| FEK, пользовательский клиент (`grpcMeta` LRU) | 100k | 15 мин |
| FEK, render-клиент (`RenderMeta` LRU) | 100k | 1 час |
| CEK (развёрнутые) | на время жизни открытого файла | — |
| Company KEK, процесс KeyManager (RAM-LRU) | — | 5 мин |
| Authz-решения (proxy) | — | ≤ 30 с |

### Иерархия ключей (SRS §4.1)

```
KMS Master Key → Company KEK (Lockbox) → wrapped_fek (Redis) → FEK (RAM клиента)
FEK → wrapped_cek (slice metadata) → CEK (RAM) → AES-256-GCM чанк в S3
```

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

Зафиксированные решения D1–D12 + ключевые stage-решения с альтернативами:

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
| 8.3 | Идемпотентность миграции из состояния Redis (чанк «сделан» = все записи несут AGCK) | Внешний checkpoint-файл: рассинхрон при ручных изменениях; таблица прогресса в PG: eventual consistency + новое хранилище |

### Решения Stage 1 (platform)

| # | Решение | Обоснование |
|---|---|---|
| 1.1 | KeyManager **не читает Redis и не парсит attr JuiceFS**: `wrapped_fek` приходит в запросе от proxy (proxy уже имеет attr из `meta.Open`) | Платформа не должна знать бинарный формат attr; AAD для unwrap полностью передаётся в запросе |
| 1.2 | Plaintext Company KEK кэшируется в RAM процесса KeyManager (LRU, TTL 5 мин) — это сервисный ключ, не FEK; ограничение T6 («не дольше запроса») действует на **FEK** | NFR-PERF-2 (≤100 мс p99 при промахе) недостижимо при KMS-вызове на каждый запрос |
| 1.3 | `GetBulkFileFEK` расширяется полями `paths` и `wrapped_feks` (параллельно `drive_file_ids`) — authz требует пути, а SRS §15.1 их не содержит; proxy резолвит пути из `InodePathCache` | Отклонение от SRS зафиксировано здесь; без путей authz-гейтинг невозможен |
| 1.4 | `FetchCompanyKEK` аутентифицируется YC IAM-токеном (отдельный interceptor), остальные RPC — модель доверия AuthzService (`user_id` в теле от proxy) | Render-ноды не имеют OIDC; proxy — доверенный компонент |
| 1.5 | `IdentityResolver`: **только валидация формата UUID** (FR-ID-3). Инвариант S12/A6: `sub` ≡ `kratos.identity_id` ≡ `user.id` — маппинга нет, DB-запроса на существование пользователя нет, кэш/singleflight не нужны (операция = `uuid.Parse`). Ошибка формата → `InvalidArgument` + audit deny. Пользователь, отсутствующий в платформе (не импортирован/удалён), проходит валидацию и отклоняется **authz-слоем**: нет записей прав → deny (FR-ID-4). Kratos/email fallback **не нужен** | FR-ID-1..4, FR-USR-6; остаточный риск R12 (дрейф идентичности Kratos↔platform) митигируется fail-closed + мониторингом рассинхрона (этап 10) |

### Решения Stage 2 (форк)

| # | Решение | Обоснование |
|---|---|---|
| 2.1 | CEK = per-slice (D3). Блок шифруется отдельно: каждый блок slice — свой nonce, AAD = `sliceID(8B BE) ‖ blockIndex(4B BE)` | NFR-SEC-9; slice ID из object key |
| 2.2 | Расширение `ChunkStore`: `NewReaderWithKey(id, length, key []byte) Reader`, `NewWriterWithKey(id, tierID uint8, key []byte) Writer`; `key=nil` → plaintext (legacy). Существующие `NewReader/NewWriter` делегируют с nil | Минимальное изменение интерфейса; остальные вызовы (gc, sync) не трогаются |
| 2.3 | Зашифрованный блок НЕ сжимается (compressor пропускается); legacy-блок — как сейчас | Сжатие ciphertext бессмысленно; порядок операций: legacy = compress(x), encrypted = encrypt(x) |
| 2.4 | Range-GET (`loadRange`) отключается для slice с CEK → полный блок через `store.load` | Ciphertext-блоки имеют переменный размер + nonce; range-семантика нарушена |
| 2.5 | Double-encrypt защита: `store.upload` и staging-upload проверяют magic `AGDF` — если данные уже зашифрованы, не шифруют повторно | Writeback: staging-файл = ciphertext, `uploadStagingFile` перечитывает его |
| 2.6 | CEK-кэш на время жизни открытого файла: `fileReader`/`fileWriter` держат `map[sliceID]CEK` (D5, FR-USR-10) | Глобальный CEK-кэш не нужен; FEK-кэш — в meta-слое (этап 3) |
| 2.7 | Slice-запись: `[24B fixed][uint32 blobLen + AGCK blob]` (опционально); `readSlices` парсит оба формата; 24B = legacy plaintext (D2) | Старые читатели упадут на новых записях — принято (S2), rollout: все клиенты обновить до включения шифрования |
| 2.8 | `Attr`: persisted suffix при `Encrypted \|\| len(WrappedFek)>0`: `0x01 ‖ u32 len(WrappedFek) ‖ WrappedFek ‖ u32 len(DriveFileID) ‖ DriveFileID ‖ u32 FekVersion ‖ u8 len(CryptoAlg) ‖ CryptoAlg`; transient `Fek []byte` вне Marshal (D1) | Backward compat: legacy attr без suffix читается как есть |

### Решения Stage 3 (оба)

| # | Решение | Обоснование |
|---|---|---|
| 3.2 | `SetFileCrypto` — отдельный метод `redisMeta`, НЕ в интерфейсе `Meta`; proxy делает type-assertion (`fileCryptoSetter`) | Не ломаем реализации dbMeta/kvMeta/grpcMeta; целевой бэкенд — Redis |
| 3.4 | Plaintext FEK возвращается ТОЛЬКО в `OpenResponse` (TLS); `GetAttr`/`Readdir`/`Lookup` несут только `wrapped_fek` (ciphertext) | T6; wrapped_fek без KEK бесполезен, а authz на листинг уже есть |
| 3.5 | `drive_file_id` генерируется в `grpcMeta.Create` (`uuid.New()`) и передаётся в `CreateRequest`; при `EncryptionEnabled=false` сервер игнорирует поле | FR-USR-3; просто и единообразно |
| 3.6 | KeyManager-клиент форка: `pkg/meta/keymanager_pb/` (копия proto platform + generated code, паттерн `authz_pb/`) + `keymanager_client.go`; флаги proxy `--keymanager-service`, `--keymanager-tls-*` | Паттерн authz_pb; sync между репозиториями контролируется known-answer тестами (этап 9) |
| 3.7 | **Валидация identity на Meta Proxy (FR-ID-3):** `extractUserIDFromOIDC` возвращает `(string, error)` и проверяет формат UUID; interceptor при ошибке → `codes.Unauthenticated`. Инвариант A6: `sub` ≡ `user.id`, маппинга нет — в KeyManager-запросы уходит `sub` как есть. В Create/Open проверка дублируется (defense in depth) → EACCES | FR-ID-2/3, AC-16; неверный формат = ошибка конфигурации идентичности, fail-closed на границе доверия |
| 3.8 | Компания определяется на стороне platform из пути: `CreateFileKey`/`GetFileFEK` принимают полный путь клиента + `volume_name`, без `company_id`; platform отсечёт companies prefix тома (тот же механизм, что в `AuthzService`) и резолвит первый сегмент (company code) в UUID | Клиент монтирует весь facility и видит компании по authz — на момент mount конкретная компания ему неизвестна; company code неизменен, резолв кэшируется перманентно (паттерн `resolveCompanyIDs`); `FetchCompanyKEK`/`ProvisionCompanyKEK` не меняются (render/admin знают компанию явно) |

### Решения Stage 4 (форк)

| # | Решение | Обоснование |
|---|---|---|
| 4.1 | `renderMeta` — **декоратор** над `meta.Meta` (redisMeta), а не модификация redisMeta | redisMeta остаётся чистым; декоратор перехватывает только Open/GetAttr/Create/Close |
| 4.2 | Company prefix — через существующий `--subdir companies/{company-code}` (Chroot в `mount()`) + проверка в renderMeta на Lookup/Mkdir/Create (path не выходит за prefix) | FR-RND-4 defense in depth; криптографическая изоляция — разные KEK (FR-RND-13) |
| 4.3 | FEK LRU: 100k, TTL **1 час** (FR-RND-7, NFR-PERF-5); ключ — inode; eviction → memclr | Отличается от user-клиента (15 мин) |
| 4.4 | KEK в RAM: `[]byte` + `unix.Mlock` (Linux; best-effort с log на macOS), обнуление при unmount; **никогда** не в аргументах CLI, логах, core dump (NFR-SEC-5, FR-RND-3) | mlock через `golang.org/x/sys/unix` (уже в зависимостях) |
| 4.5 | Aggressive caching (FR-RND-11): FUSE `attr_timeout`/`entry_timeout` ≥ 60s + meta cache flags; readdir — Redis pipelining (проверить текущую реализацию, при последовательных round-trips — pipeline) | R9: нагрузка на Redis от сотен нод |
| 4.6 | Render-режим НЕ выполняет per-file authz (FR-RND-5); `CheckPermission`-интерцепторов нет (прямой redisMeta) | Изоляция компанией криптографически |

### Решения Stage 5 (форк)

| # | Решение | Обоснование |
|---|---|---|
| 5.2 | Оркестрация Clone в **proxy** (user path): `GetFileFEK(src)` → `meta.Clone` → `CreateFileKey(dst)` → `SetFileCrypto(dst)` → `RewrapSlices`. Render path: та же последовательность локально (FEK из KEK) | D7; окно с «чужим» wrapped_fek у dst безопасно (клонирующий владеет FEK_src — проверено authz Read на src) |
| 5.3 | FEK для компакции — `baseMeta.fileKeyResolver` (D8): user mode → новый RPC `ResolveFileKey(inode)` (proxy: authz Read + KeyManager); render mode → локальный unwrap. Сообщение `CompactChunk` расширяется аргументами `{fek, driveFileID, fekVersion}` | Компакция триггерится из meta без user-контекста; fail-closed: resolver nil/ошибка → компакция пропускается (log), данные не повреждаются |
| 5.4 | Новый CEK компакции оборачивается под FEK на data-стороне (`vfs.Compact`) и передаётся в `doCompactChunk` новым аргументом `wrappedCEK []byte`; сигнатуры `sql.go`/`tkv.go` обновляются (передают nil — legacy) | FR-OP-7: read → unwrap CEKs → decrypt → merge → new CEK → encrypt → wrap under FEK → update slices |
| 5.5 | CopyFileRange: если путь делит slice'ы (clone-семантика) — `RewrapSlices` на диапазоне dst с FEK из handles (VFS имеет оба handle); если копирует данные — обычный read/write path с FEK handles | FR-OP-4/5; FEK обоих файлов уже в RAM (открытые файлы) |
| 5.6 | Предусловия версионирования (FR-VER-2/3): хуки `baseMeta.sliceDeletable func(id uint64) bool` (GC) и `compactionAllowed func(inode Ino) bool`; по умолчанию nil/true | Будущее версионирование поставит реальные политики; сейчас — точки расширения + тесты |

### Решения Stage 6 (форк)

| # | Решение | Обоснование |
|---|---|---|
| 6.1 | `MemClear(b []byte)` — цикл обнуления + `runtime.KeepAlive`; применяется ко всем plaintext-ключам (FEK LRU, handle.fek, CEK-кэши reader/writer, KEK render) | NFR-SEC-3: «memclr, не GC» — GC не гарантирует обнуление |
| 6.2 | mlock — только для FEK (LRU entries) и KEK (render); CEK — best-effort (короткоживущие, per-slice; mlock на каждый = overhead). Обёртка `mlockPage(p []byte) error` с metric и log при неудаче | NFR-SEC-5; практичный баланс |
| 6.3 | State machine в `grpcMeta`: `online → offlineConnected → disconnected`; переходы: RPC-ошибка транспорта → offlineConnected (запомнить время); успех → online; offlineConnected дольше `--offline-timeout` (default 15 мин, NFR-OFF-4) → disconnected + WipeKeys | NFR-OFF-1..4 |
| 6.4 | Offline-поведение: чтение открытых файлов — из кэша (FEK в RAM); новые Open — fail-closed EACCES (NFR-OFF-2); записи — в **журнал** (решение 6.5) | SRS §10.3 |
| 6.6 | Logout: control file `_JFS_LOGOUT` во внутреннем dir (паттерн `CompactPath` из `pkg/vfs/internal.go`) → VFS → `WipeKeys()` + disconnected. Re-login = повторный mount (или refresh токена, если hub вернул доступ) | Существующий механизм control files; без новых RPC |
| 6.7 | OIDC expiry: `tokenManager` — проверить callback о смерти сессии; если нет — heartbeat-проверка: `BearerToken()` вернул ошибку/expired → WipeKeys + disconnected | NFR-SEC-3 «истечение OIDC-сессии» |

> 6.5 — отсутствует в таблице (пропуск нумерации): решение о write journal offline-записей реализовано в задаче 6.3 tasks.md, см. её блок решений (формат записи, точки append/replay, идемпотентность по `doWrite`/`buildSlice`).

### Решения Stage 7 (оба)

| # | Решение | Обоснование |
|---|---|---|
| 7.1 | Permission generation: счётчик `drivepermgen:{userID}` в Redis platform; `INCR` при SetRole/DeleteRole/ClearRoles + явная `Invalidate` PermissionCache (production TTL ≤ 30s); RPC `GetPermissionGeneration(user_id)`; proxy кладёт значение в `FlushSessionResponse.permission_generation`; клиент при изменении → WipeKeys + InvalidateAllKeys | Канал — heartbeat (D9, задержка ≤12s); счётчик живёт в Redis platform, т.к. proxy не имеет к нему доступа |
| 7.2 | STS user mode: RPC `GetSTSCredentials(user_id, volume_name)` в platform → короткие S3-credentials (TTL ≤60 мин) с политикой на company prefix; форк: `sts_refresher` пересоздаёт blob **локально** (без записи credentials в Redis Format) через новый метод ReloadableStorage | FR-REV-3; запись STS-токенов в Format = утечка чужих токенов всем клиентам + лишние writes в Redis |
| 7.3 | STS render mode: прямой STS по IAM-роли ноды (AWS AssumeRole / YC SA token), без platform RPC | FR-RND-2 паттерн; изоляция per-company политикой роли |
| 7.4 | FEK rotation оркестрирует **proxy** через новый RPC `RotateFileKey(inode)`: KeyManager генерирует новый FEK (admin-gated) → proxy: GetFileFEK(old) → SetFileCrypto(new version) → RewrapSlices(old→new). Инвалидация клиентских кэшей — автоматически: `cached_fek_version` mismatch на следующем Open (этап 3) | KeyManager не трогает Redis (решение 1.1); re-wrap — операция proxy (этап 5) |
| 7.5 | CEK rotation (FR-ROT-5/6) = ре-энкрипция файла инструментом этапа 8 (`juicefs reencrypt --file <path> --rotate-cek`); в этом этапе — только фиксация интерфейса и документация | Механика идентична миграции; не дублируем код |
| 7.6 | Offboarding (FR-REV-6): CLI platform `keymanager rotate-user-keys --user <id> --company <id>` — перечисляет файлы по правам пользователя → batch через proxy RPC `RotateFileKeysByPaths` (rate-limited, возобновляемый) | РЕКОМЕНДУЕТСЯ при offboarding; автоматизация ручного процесса |

### Решения Stage 8 (форк)

| # | Решение | Обоснование |
|---|---|---|
| 8.1 | Новый публичный метод `baseMeta.ReencryptChunk(ctx, inode Ino, indx uint32, newSlice Slice)` — атомарная замена chunk list одним slice'ом (обёртка над engine-операцией по образцу `doCompactChunk`: DEL+RPush в txn, refcount: −1 на старые slice ID, +1 на новый) | Данные нового slice уже записаны в S3 до вызова; swap атомен; legacy-сlices становятся GC-кандидатами (refcount → 0) |
| 8.2 | FEK для миграции — **Company KEK локально** (паттерн render: `FetchCompanyKEK` по service identity), а не per-file KeyManager-RPC | Миграция — сервисная операция; один KEK на компанию покрывает все файлы; нет N RPC на N файлов |
| 8.4 | Стратегия чанка: прочитать все slice'ы чанка (в порядке pos) → один новый slice с новым CEK (merge, как компакция) → `ReencryptChunk` | Минимум slice'ов после миграции; совпадает с FR-OP-7 паттерном |
| 8.6 | Во время миграции файла новые записи идут **зашифрованными** (FEK уже сгенерирован и записан в attr на старте миграции файла, `encrypted=1` сразу), а чтение legacy-чанков — до их swap | FR-MIG-6: запись в новую (зашифрованную) версию, чтение старой; mixed chunk list корректен. Порядок: `SetFileCrypto` → почанковый swap |

> 8.5 — заменено 8.6: первоначальный порядок («сначала swap чанков, в конце `SetFileCrypto(encrypted=1)`») отклонён — новые записи во время миграции ушли бы как legacy.

### Решения Stage 9 (оба)

| # | Решение | Обоснование |
|---|---|---|
| 9.1 | **Cross-repo known-answer векторы**: файл `testcrypto/vectors.json` (форматы AGFK/AGCK/AGDF: KEK/FEK/CEK/plaintext/AAD → ожидаемые blob'ы) — копия в обоих репозиториях, тесты в каждом сравнивают свою реализацию с векторами | Единственная защита от дрейфа форматов между platform и форком (решения 1.1/3.6) |
| 9.2 | Интеграционная среда: `docker-compose.enc-test.yml` (форк): Redis + MinIO + in-process fake KeyManager (gRPC, в тестовом бинаре); для platform — существующий testcontainers-паттерн | Воспроизводимость без YC; fake KMS/SM уже есть (этап 1) |
| 9.3 | Нагрузочные тесты — отдельный Go-хarness `tests/load/` (не в CI по умолчанию, запуск вручную/в nightly): реальный стек из 9.2 + N goroutines-«клиентов» | FR-TEST-22..25; CI-ограничения по времени |
| 9.4 | Security-сценарии — скрипты `tests/security/*.sh` + Go-тесты: работают на среде 9.2, результат — отчёт (pass/fail по сценарию) | FR-TEST-26..30; часть сценариев (утечки) — «чёрный ящик» |
| 9.5 | AC-чеклист: `tests/acceptance.md` — таблица AC → тест(ы) → как запустить; прогон = часть Definition of Done | SRS §22, AC-1..16 |

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

### Риски по этапам

| Этап | Риск | Митигация |
|---|---|---|
| 1 | YC SDK в vendor не содержит KMS/Secret Manager сервисы | `go get` + `go mod vendor`; если нет доступа к реестру — зафиксировать и использовать только Fake до получения доступа (не блокировать этап) |
| 1 | Потеря Company KEK = потеря данных компании (R3) | ProvisionKEK идемпотентен по версии; ротация (этап 7) создаёт новую версию, старая — `retiring`; бэкапы Secret Manager — этап 10 |
| 1 | Аудит не должен ронять hot path | асинхронный batch-writer; deny — синхронно |
| 2 | `sql.go`/`tkv.go` ломаются компиляцией из-за сигнатур slice | Минимальные правки с legacy-поведением; целевой бэкенд — Redis |
| 3 | Окно между Create и SetFileCrypto (файл без FEK) | Безопасно: данные не пишутся до FEK; конкурентный reader видит пустой legacy-файл (решение 3.3) |
| 3 | InodePathCache не резолвит путь при Open → EACCES | Существующее поведение authz interceptor'а (нерезолвлённый = deny); кэш заполняется Lookup/Create/Readdir — в нормальном FUSE-флоу путь всегда резолвлен до Open |
| 3 | Дрейф proto между форком и platform | known-answer тесты (этап 9) + копия proto с комментарием «sync with agio-platform» |
| 3 | FEK в attrCache клиента (TTL 1s) — attr с wrapped_fek кэшируется | wrapped_fek — ciphertext, утечки нет; `Fek` в attr НЕ попадает в ProtoAttr → не кэшируется через GetAttr |
| 4 | Компрометация render-ноды (R4/T5) | Изоляция per-company: KEK ноды раскрывает только свою компанию; Network Policy + IAM least privilege — этап 10 |
| 4 | mlock на macOS/dev-машинах недоступен | Best-effort: log warning, продолжить (требование — для production Linux) |
| 5 | RewrapSlices на большом файле (много чанков) — долгий txn | Последовательные txn по chunk list'ам; прогресс-лог; для очень больших файлов — фоновый режим с метрикой (этап 10) |
| 5 | Частичный re-wrap при сбое (некоторые чанки переобёрнуты) | Target помечается unusable (Unlink по ошибке); источник не затрагивается |
| 5 | CopyFileRange с закрытыми файлами (fh=0, по inode) — FEK не в handle | ResolveFileKey для обоих inode (user mode) / локальный unwrap (render) |
| 6 | Replay журнала не идемпотентен | Проверить семантику doWrite; fallback — replay подтверждённых + явная метка в логе (решение 6.5) |
| 6 | mlock лимиты ядра (RLIMIT_MEMLOCK) на render-нодах | Metric + log; документация: поднять memlock limit в production (этап 10) |
| 6 | Control file logout гонится с активными IO | InvalidateAllKeys под handle-лок; IO получает EIO, ядро FUSE корректно завершает запросы |
| 7 | Group revocation: INCR generation для всех участников группы — дорого при больших группах | Batch INCR (Redis pipeline); асинхронно после commit роли |
| 7 | STS refresh упал, токен истёк → writes падают | Метрика `jfs_sts_refresh_failures_total` + алерт (этап 10); старые reads из кэша продолжают работать |
| 7 | RotateFileKey на открытом файле: клиент держит старый FEK | Per-file version check на следующем Open; для мгновенности — generation bump (опциональный флаг `--rotate-invalidate-all`) |
| 8 | Миграция большого тома — часы/дни | Rate-limits + возобновляемость; запуск в off-peak; метрики прогресса (этап 10) |
| 8 | Сбой между write нового slice и ReencryptChunk → сиротский S3-объект | GC JuiceFS находит сироты по refcount (существующая механика `gc`) — объект без ссылок будет удалён |
| 8 | Файл меняется во время миграции (новые slice'ы в чанке после нашего Read) | ReencryptChunk заменяет list целиком → потеря новых записей! **Защита:** перед swap проверить, что list не изменился с момента Read (txn с проверкой LRange-снапшота / версия чанка); при конфликте — повторить чанк. **Обязательно реализовать** |
| 8 | KEK компании ротировался во время миграции (этап 7) | `kek_version` в wrapped_fek; UnwrapFEK с версией из attr; при mismatch — ошибка + skip файла (повторить после синхронизации) |
| 9 | Нагрузочные цели не достигаются (R1: latency промаха) | Оптимизации: batch GetBulkFileFEK, увеличение кэшей, singleflight; зафиксировать measured значения — решение о trade-off за пользователем |
| 9 | Redis не выдерживает 500k RPS (R9) | Реплики/шардирование — инфраструктурное решение (этап 10); в отчёте — measured потолок |
| 9 | Stage-окружение недоступно во время этапа | Manual-чеклист переносится на этап 10; CI-часть этапа не блокируется |
| 10 | Yandex KMS auto-rotation меняет поведение Decrypt для старых версий | Проверить документацией + тестом ДО включения; fallback — manual re-wrap KEK при смене версии (rb-kek-rotation) |
| 10 | Restore-тест раскрывает, что бэкап несовместим | quarterly-прогон + алерт на провал; RTO задокументирован |
| 10 | Anomaly detection false positives блокируют легитимных пользователей (рендер-ферма читает много файлов) | Исключения по actor_type=render_node; пороги настраиваются; авто-блокировка — только alert на старте |
| 10 | Rollback «откатить шифрование» просят стейкхолдеры | Документация: де-энкрипция = отдельный проект (полная перепись данных); в 99% случаев rollback не нужен |

## Migration Plan

1. **Per-company rollout** (stage 10): пилотная компания (test) → stage → production по одной компании (`rb-enable-company`).
2. **Предусловия per company:** все клиенты обновлены до версии с поддержкой slice-формата v2; render-ноды обновлены; STS включён; Redis metadata backups проверены restore-тестом.
3. **Включение:** `ProvisionCompanyKEK` → `juicefs enable-encryption` (идемпотентно, `Format.EncryptionEnabled=true`) → новые файлы шифруются по умолчанию; legacy читаются passthrough.
4. **Миграция legacy:** `juicefs reencrypt` (rate-limited, возобновляемая) в off-peak.
5. **Rollback:** отключить создание новых зашифрованных файлов (`EncryptionEnabled=false` для новых файлов) + чтение старых продолжается (новые клиенты читают оба формата). Полное «откатить шифрование» = де-энкрипция — отдельный проект, out of scope.

## Acceptance Criteria → этапы (SRS §22)

| AC | Критерий | Проверяется в этапе |
|---|---|---|
| AC-1 | Все FR/NFR «ДОЛЖЕН» реализованы и покрыты тестами | 9 (сводная проверка) |
| AC-2 | Read на X не даёт расшифровать Y | 3, 9 |
| AC-3 | Утечка S3+Redis не раскрывает данные | 2, 9 |
| AC-4 | Render-клиент: без OIDC/proxy/PG | 4 |
| AC-5 | Cross-company доступ невозможен | 4, 9 |
| AC-6 | Legacy файлы читаются | 2, 8 |
| AC-7 | Clone zero-copy через CEK re-wrap | 5 |
| AC-8 | Logout → кэш нечитаем | 6 |
| AC-9 | Offline-connected работает | 6 |
| AC-10 | Revocation: отзыв + TTL + STS expiry | 7 |
| AC-11 | Нагрузочные тесты пройдены | 9 |
| AC-12 | Audit log на каждую выдачу FEK | 3, 9 |
| AC-13 | Threat model задокументирована (T8, T9) | 10 |
| AC-14 | Предусловия версионирования (FR-VER-1…4) | 2, 5 |
| AC-15 | Company Owner bypass через `PermissionOwn` | 3 |
| AC-16 | OIDC `sub` (UUID) используется напрямую как `user.id`; маппинга нет; fail-closed при неверном формате (FR-ID-1..4) | 1, 3, 9 |

## Open Questions

1. **YC IAM: prefix-scoped S3-политики?** Если Yandex не даёт prefix-scoped STS-политики — role с минимальными правами на бакет + аудит (решение фиксируется в stage 7, не меняет спеки).
2. **Идемпотентность replay write journal:** семантика `redisMeta.doWrite` при повторном append того же slice — проверить в stage 6; fallback: replay только подтверждённых записей + явная метка в логе.
3. **Направление зависимости pkg/meta → pkg/chunk** для AGCK-примитивов (`RewrapSlices`): если `pkg/meta` не импортирует `pkg/chunk` — вынести AGCK-хелперы в общий пакет (решение фиксируется в stage 5).
4. **Доступ к «сырому» `redisMeta` из `meta.NewClient`** для декоратора `RenderMeta` — проверить в stage 4; при необходимости конструктор напрямую.
5. **Yandex KMS auto-rotation:** поведение Decrypt для старых версий ключа после ротации — проверить документацией + тестом ДО включения (stage 10); fallback — manual re-wrap KEK.
