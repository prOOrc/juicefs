# Design: inventory-encrypt

## Architectural Context

Инвентаризация **запланированной** подсистемы шифрования agio Drive (greenfield: мастер-план §8 — все 10 этапов «не начат»). Целевой контракт из SRS-001 v2.2 + межэтапных контрактов мастер-плана §4 (форматы, решения D1–D12) + stage-планов 01–10. Подсистема — двухуровневый envelope encryption: KMS Master Key → Company KEK (Secret Manager) → FEK per file (wrapped в Redis inode) → CEK per slice (wrapped в slice-записи) → AES-256-GCM чанки в S3.

Критические факты as-is (проверено по ветке `agio-drive-v2`):

1. **Данные не проходят через Meta Proxy** — шифрование только на клиенте (chunk store). gRPC переносит slice-метаданные и (после этапа 3) plaintext FEK в `OpenResponse`.
2. **Render-ноды** — нативный FUSE с прямым Redis+S3, без proxy/OIDC/authz; шифрование должно работать для них как identity-less клиентов через Company KEK.
3. **Redis = source of truth** метаданных; PG `drive_object` — асинхронная проекция (Kafka). FEK живёт в Redis (стратегическое решение S7).
4. **Инвариант идентичности (S12/A6):** OIDC `sub` ≡ `kratos.identity_id` ≡ `user.id` (UUID); маппинга нет — подтверждено as-is: authz-интерцептор берёт userID из `idToken.Subject` без преобразования.
5. **Upstream-криптография существует, но не в mount:** `pkg/object/encrypt.go` + `Format.EncryptKey/EncryptAlgo/KeyEncrypted` (`pkg/meta/config.go:93-95`) — per-chunk AEAD со случайным DEK на том (RSA-OAEP), используется только в `sync`/`load`. С подсистемой SRS-001 не пересекается и не переиспользуется.
6. **Точки расширения подтверждены:** slice-запись фиксированная 24 байта (`sliceBytes`, `pkg/meta/slice.go:91`; `readSlices` hard-fail на другой длине); `Attr` — variable-length binary с паттерном опционального хвоста (Tier/ACL) — основа для crypto-suffix (D1); `ChunkStore.NewReader/NewWriter` (`pkg/chunk/chunk.go:41-42`) без ключей — расширяются `*WithKey` (D4); `handle` (`pkg/vfs/handle.go:32`), `fileReader`/`fileWriter`, `NewReloadableStorage` (`cmd/mount.go:462`) — существующие точки plumbing.

```text
User path:   AGIO Client (grpcMeta) → Meta Proxy gRPC → KeyManager (agio-platform, authz-gated FEK)
             данные: AGIO Client → S3 (AGDF ciphertext), локальный кэш — ciphertext
Render path: Render Client (renderMeta) → Redis (прямой) + S3 (прямой); Company KEK из Secret Manager по IAM ноды
Key plane:   KMS Master Key → Company KEK (Secret Manager) → wrapped_fek (Redis attr) → FEK (RAM клиента)
             FEK → wrapped_cek (slice-запись) → CEK (RAM) → AGDF-блок в S3
```

## Component Map

**Существующие компоненты (as-is, ветка `agio-drive-v2`):**

| Компонент | Файл | Роль в шифровании |
|---|---|---|
| `MetaProxyServer` | `pkg/meta/grpc_server.go`, `grpc_server_fuse.go` | Точка интеграции: вызовы KeyManager при Create/Open, запись `wrapped_fek` (этап 3) |
| `AuthzInterceptor`, `InodePathCache`, `cachingAuthzClient` | `pkg/meta/authz_*.go`, `inode_path_cache.go` | Authz-гейтинг Open/Create (Read/Edit); пути для KeyManager-запросов; TTL кэша ≤30s при revocation (этап 7) |
| `grpcMeta`, `withAuth`, heartbeat (`FlushSession` 12s) | `pkg/meta/grpc_client*.go` | Клиентский FEK LRU, `cached_fek_version`, revoke-signal через generation в heartbeat (этапы 3/6/7) |
| `redisMeta` | `pkg/meta/redis.go` | Целевой бэкенд: `SetFileCrypto`, `RewrapSlices`, `ReencryptChunk` (этапы 3/5/8) |
| `baseMeta` | `pkg/meta/base.go` | Хуки версионирования (`fileCryptoDeletable`, `sliceDeletable`, `compactionAllowed`, `fileKeyResolver`) (этапы 2/5) |
| `Attr`, `Format` | `pkg/meta/interface.go`, `config.go` | Crypto-suffix attr; `EncryptionEnabled`/`KEKVersion` в Format (этап 2) |
| slice-запись (`sliceBytes=24`) | `pkg/meta/slice.go` | Опциональный AGCK-хвост `wrapped_cek` (этап 2) |
| `cachedStore`, `rSlice`, `wSlice` | `pkg/chunk/cached_store.go` | Choke points шифрования: `store.load`/`loadRange`/`upload`; ciphertext-only кэш (D4, этап 2) |
| `handle`, `fileReader`, `fileWriter` | `pkg/vfs/handle.go`, `reader.go`, `writer.go` | FEK в handle (transient), CEK-кэш per open file, wrap CEK при commit (этап 2) |
| `NewReloadableStorage` | `cmd/mount.go:462` | Swap S3-credentials для STS без записи в Redis Format (этап 7) |

**Планируемые компоненты (to-be, по этапам):**

| Компонент | Файл (план) | Этап |
|---|---|---|
| `EncryptBlock/DecryptBlock/WrapCEK/UnwrapCEK`, `IsLegacyBlock` | `pkg/chunk/cek_encrypt.go` | 2 |
| `WrapFEK/UnwrapFEK`, `FekAAD` | `pkg/meta/fek_crypto.go` | 2 |
| `ChunkStore.NewReaderWithKey/NewWriterWithKey` | `pkg/chunk/chunk.go`, `cached_store.go` | 2 |
| `FileCrypto`, `SetFileCrypto`, `RewrapSlices`, `SliceCryptoAAD` | `pkg/meta/redis_fek.go` | 3, 5 |
| `KeyManagerClient`, proto-копия `keymanager_pb` | `pkg/meta/keymanager_client.go`, `keymanager_pb/` | 3 |
| `RenderMeta`, `cmdRenderMount` | `pkg/meta/render_meta.go`, `cmd/render_mount.go` | 4 |
| `MemClear/MlockPage`, `WipeKeys`, `InvalidateAllKeys`, hub state machine, `WriteJournal` | `pkg/utils/memclr.go`, `pkg/meta/grpc_client.go`, `pkg/vfs/*`, `pkg/vfs/write_journal.go` | 6 |
| `stsRefresher`, `SetCredentials` на holder'е | `cmd/sts_refresher.go`, `cmd/mount.go` | 7 |
| `ReencryptChunk`, `cmdReencrypt`, `enable-encryption` | `pkg/meta/base.go`, `cmd/reencrypt.go`, `cmd/format.go` | 8 |
| **agio-platform (другой репозиторий):** `DriveKeyManagerService`, `CompanyKEKService`, `IdentityResolver`, KMS/Secret Manager-адаптеры (Yandex), STS-провайдер, PG-таблицы `drive_company_crypto_key`/`drive_key_access_log` | `src/application/authz/...`, `src/internal/drive/...` | 1, 7 |

## Execution Flow

1. **Create (user path):** клиент генерирует `drive_file_id` (UUID) → proxy: `meta.Create` → KeyManager `CreateFileKey` (authz Write; FEK = CSPRNG 32B; `wrapped_fek = WrapFEK(KEK, FEK, AAD)`) → `SetFileCrypto` в attr inode (один Redis txn) → ошибка любого шага после Create → `Unlink` (rollback).
2. **Open (user path):** authz-интерцептор проверяет Read/Edit **всегда** → если `cached_fek_version == attr.FekVersion`: FEK из клиентского LRU, KeyManager не вызывается; иначе KeyManager `GetFileFEK` (singleflight, audit) → plaintext FEK только в `OpenResponse.fek` (TLS).
3. **Data path:** read — slice с `WrappedCEK`: `UnwrapCEK(FEK, ...)` один раз на open file → `NewReaderWithKey(sliceID, len, CEK)` → блок из S3/кэша (ciphertext) → `DecryptBlock` (AAD = sliceID ‖ blockIndex); write — новый CEK per slice (CSPRNG) → `NewWriterWithKey` → `EncryptBlock` перед upload; кэш получает ciphertext; при commit `WrappedCEK = WrapCEK(FEK, CEK, ...)` в slice-запись.
4. **Render path:** mount: прямой Redis + `FetchCompanyKEK` по IAM ноды (один gRPC-вызов) → KEK mlock в RAM → `renderMeta`-декоратор: Open — локальный `UnwrapFEK(KEK, ...)` (LRU 100k/1h), Create — локальная генерация FEK + `SetFileCrypto`; без OIDC/proxy/authz/PG.
5. **Clone:** `GetFileFEK(src)` → `meta.Clone` (verbatim-копия chunk lists) → `CreateFileKey(dst)` → `SetFileCrypto(dst)` → `RewrapSlices(dst, FEK_src → FEK_dst)`; S3 не переписывается.
6. **Revocation:** DeleteRole/ClearRoles → INCR `drivepermgen:{userID}` + инвалидация authz-кэша (TTL ≤30s) → proxy кладёт generation в `FlushSessionResponse` → клиент при изменении: `WipeKeys` + `InvalidateAllKeys`; S3 — expiry STS (≤60 мин).

## Invariants

- **Fail-closed:** любая ошибка криптографии (tag mismatch, AAD mismatch, corrupt blob), authz или identity → deny/EIO; plaintext не возвращается.
- **Plaintext только в RAM:** CEK/FEK/KEK никогда не сериализуются в Redis/PG/S3/логи/метрики; transient `Attr.Fek` вне Marshal.
- **Ciphertext at rest:** S3 и локальный disk cache содержат только `AGDF`; без ключей в RAM данные нечитаемы (no residuality конструктивно).
- **Zero-copy:** Clone и FEK-ротация — операции над ключами (re-wrap `wrapped_cek`), S3-объекты не переписываются.
- **Legacy-совместимость метаданных:** 24-байтные slice-записи и attr без crypto-suffix парсятся как plaintext; mixed chunk list (legacy + encrypted slice'ы) валиден во время миграции.
- **Идентичность:** `sub` ≡ `user.id` (UUID), маппинга нет; неверный формат UUID — fail-closed на границе доверия.
- **Redis = source of truth** crypto-метаданных; потеря `wrapped_fek` = потеря доступа к файлу (FR-REDIS-6).
- **Данные не проходят через proxy** — шифрование/дешифрование только в клиентском chunk store.

## Decisions & Rationale

Зафиксированные межэтапные решения мастер-плана §4.8 (D1–D12), которые определяют контракт:

- **D1 — `wrapped_fek` в `Attr` (variable-length suffix), не xattr:** 0 доп. Redis round-trips (NFR-PERF-6, `doGetAttr` = один GET); невидимо POSIX; работает для render-клиента с прямым Redis. Альтернатива (xattr) отклонена: +1 round-trip и отдельный код чтения.
- **D2 — `wrapped_cek` в slice-записи (24B + length-prefixed AGCK):** CEK «путешествует» со ссылкой на slice; Clone копирует записи verbatim → re-wrap = локальная metadata-операция. Альтернатива (отдельный Redis-key per slice) отклонена: N доп. round-trips при Read.
- **D3 — CEK per-slice (не per-chunk):** slice — иммутабельный data-объект JuiceFS, шарится через refcount; «чанк» SRS ≙ slice. Slice ID уже в object key → AAD без доп. состояния.
- **D4 — хуки шифрования в `cachedStore` (`rSlice.cek`/`wSlice.cek`):** единственные choke points — `store.load`/`loadRange`/`upload`; disk cache получает ciphertext как есть (изменений в `disk_cache.go` нет) → NFR-SEC-1 автоматически. Range-GET отключается для зашифрованных slice (ciphertext — переменный размер).
- **D5 — plaintext FEK только в `OpenResponse` / локальный unwrap:** FEK не хранится в proxy дольше запроса (T6); CEK-кэш = время жизни открытого файла.
- **D6 — KeyManager в agio-platform (тот же gRPC-сервер, что `AuthzService`):** переиспользует `DriveAuthorizationService`, PG, Redis; в KeyManager нет кэша FEK (T6), только singleflight.
- **D7 — Clone = verbatim + post-hoc re-wrap:** окно с «чужим» `wrapped_fek` у dst безопасно (клонирующий прошёл authz Read на src и владеет FEK_src).
- **D8 — FEK для компакции через хук `fileKeyResolver`:** компакция триггерится из meta без user-контекста; user mode → RPC `ResolveFileKey`, render mode → локальный unwrap; ошибка → компакция пропускается (fail-closed).
- **D9 (редакция этапа 7) — revoke-signal через heartbeat:** `FlushSessionResponse.permission_generation`; счётчик живёт в Redis platform, proxy опрашивает `GetPermissionGeneration` (см. Known Deviations #4).
- **D10 — STS через существующий reload-путь:** `NewReloadableStorage` уже пересоздаёт blob при смене credentials; локальный swap без записи в Redis Format.
- **D11 — Identity: только валидация формата UUID** (без DB-запроса на существование): отсутствующий пользователь отклоняется authz-слоем (нет записей прав → deny, FR-ID-4); остаточный риск R12 (дрейф Kratos↔platform) — мониторингом рассинхрона (этап 10).
- **D12 — KMS/Secret Manager: Yandex первыми**, интерфейс провайдер-независимый (платформа в YC; AWS/Vault — будущие реализации).
- **Целевой бэкенд метаданных — Redis:** семантические crypto-изменения `pkg/meta/` реализуются для `redisMeta`; SQL/KV-ветки получают минимальные правки только если ломается компиляция (с сохранением legacy-поведения). Это сознательное отклонение от parity-правила AGENTS.md, обоснованное S2 (глубокий форк) и конвенцией мастер-плана §5.7: целевая топология agio Drive — Redis.

## Integration Points

- **agio-platform:** gRPC `DriveKeyManagerService` (`agio.platform.drive.crypto.v1`) на том же сервере, что `AuthzService` (порт 9090); TLS; `FetchCompanyKEK` — отдельная IAM-аутентификация ноды, остальные RPC — модель доверия proxy (`user_id` в теле). Proto копируется в форк (`keymanager_pb/`, паттерн `authz_pb/`); синхронизация форматов — known-answer векторы (этап 9).
- **Yandex Cloud:** KMS (WrapKey/UnwrapKey), Secret Manager (Company KEK, имя `drive/kek/{companyID}/v{version}`), IAM-токен ноды (instance metadata / файл) для render и STS.
- **Ory Hydra/Kratos:** OIDC discovery + JWKS по `--oidc-issuer` (существующий механизм); инвариант `sub` ≡ `user.id`.
- **Redis:** crypto-метаданные в attr inode и slice-записях; `drivepermgen:{userID}` (platform Redis) для generation.
- **S3:** `AGDF`-объекты; STS-политики с company prefix (этап 7).

## Known Deviations

Отклонения от SRS-001, зафиксированные в stage-планах (кандидаты на учёт при ревизии SRS):

1. **`GetBulkFileFEK` расширен полями `paths` и `wrapped_feks`** (параллельно `drive_file_ids`) — SRS §15.1 их не содержит; без путей authz-гейтинг невозможен, без `wrapped_feks` KeyManager пришлось бы читать Redis (решение 1.3 этапа 1 + D6: KeyManager не читает Redis).
2. **KeyManager не читает Redis и не парсит attr JuiceFS** — `wrapped_fek` приходит в запросе от proxy, AAD передаётся полностью (решение 1.1 этапа 1).
3. **`OpenRequest.cached_fek_version`** — при совпадении с `attr.FekVersion` proxy не вызывает KeyManager (клиент берёт FEK из LRU); authz-проверка интерцептором выполняется всегда (решение 3.1 этапа 3; SRS такого механизма не описывает).
4. **Permission generation: счётчик в Redis platform, а не прямой доступ proxy** — master-план D9 исходно предполагал прямой read proxy'ем; редакция решения 7.1 этапа 7: новый RPC `GetPermissionGeneration`, proxy опрашивает его на каждом heartbeat.
5. **Offboarding через `RotateFileKeysByPaths` (proxy RPC)** — PG-проекция не хранит inode, маппинг path→inode только через Redis/proxy; CLI platform принимает пути (решение 7.7 этапа 7).
6. **CEK per-slice вместо per-chunk** — терминологическое: «чанк» SRS ≙ slice JuiceFS (D3); форматы и AAD адаптированы (slice_id, block_index).
7. **Старые читатели падают на новых slice-записях** — принято (S2: upstream-совместимость не требуется); rollout-правило этапа 10: включить шифрование только после обновления всех клиентов; mixed-состояние данных допустимо в одну сторону (legacy→encrypted, этап 8).
8. **mlock-область сужена: FEK + render KEK, CEK — best-effort** — SRS NFR-SEC-5 требует mlock для всех страниц памяти с ключами; решение 6.2 этапа 6 сужает до FEK (LRU entries) и render KEK: CEK короткоживущие и per-slice, mlock на каждый = overhead (обёртка `mlockPage` с metric и log при неудаче).
9. **Новые MetaService RPC `ResolveFileKey` и `RotateFileKey`** — SRS §15.2 (FR-API-1..3) описывает только расширения полей; stage-планы добавляют proxy-RPC: `ResolveFileKey(inode)` для фоновой компакции (решение 5.3 этапа 5, D8) и `RotateFileKey(inode)` для оркестрации FEK-ротации (решение 7.4 этапа 7). По природе — те же «добавления сверх SRS», что отклонения #1 и #4.

## Open Questions

- [ ] Направление зависимости `pkg/meta` → `pkg/chunk` для AGCK-примитивов при `RewrapSlices`: вынести в общий пакет или дублировать минимальный код — решить при реализации этапа 5 (шаг 1 подплана).
- [ ] Поддерживает ли YC IAM prefix-scoped S3-политики для STS render-нод; если нет — role с минимальными правами на бакет + аудит (риск этапа 7).
- [ ] Идемпотентность replay write journal: семантика `redisMeta.doWrite` при повторном append того же slice — проверить в этапе 6 (решение 6.5); fallback — replay только подтверждённых записей.
- [ ] Доступ к «сырому» `redisMeta` из `meta.NewClient` для `renderMeta`-декоратора — проверить в этапе 4; при необходимости прямой конструктор `NewRedisMeta`.
