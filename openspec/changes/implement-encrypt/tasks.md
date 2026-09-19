# Tasks: implement-encrypt

Код-уровневые детали (сигнатуры, тела функций, тесты) — в stage-планах `.qwen/plans/stage-01-foundation.md` … `stage-10-production-rollout.md`; контракт — `openspec/specs/domain-encrypt/spec.md`; решения D1–D12 — design.md. Порядок: 1 → 2 → 3 → (4 ∥ 5 ∥ 6) → 7 → 8 → 9 → 10; в рамках одной сессии — последовательно. Коммиты: префикс `encrypt-stage-N:` по логическим шагам.

## 1. Stage 1 — Фундамент (agio-platform, ветка feature/drive-v2)

Детали: `.qwen/plans/stage-01-foundation.md`.

- [ ] 1.1 PG-миграции `drive_company_crypto_key` и `drive_key_access_log` + регенерация SQLBoiler. Файлы: `src/infrastructure/db/migrations/000190_add_drive_company_crypto_key.{up,down}.sql`, `000191_add_drive_key_access_log.{up,down}.sql`, `src/infrastructure/boiler/*` (generated). Проверка: `migrate up`/`migrate down` на локальном PG; `go build ./...` в `src/`.
- [ ] 1.2 Порты криптографии (`KMS`, `SecretManager`, `CompanyKEKService`, `IdentityResolver`, `KeyAccessLogger`). Файл: `src/internal/drive/application/ports/crypto.go`. Проверка: `go build ./...`.
- [ ] 1.3 Yandex KMS / Secret Manager-адаптеры + fake-реализации для тестов (проверить наличие `services/kms`/`secrets` в vendor, при отсутствии — добавить зависимость). Файлы: `src/internal/drive/infrastructure/adapters/kms_yandex.go`, `secret_manager_yandex.go`, `crypto_fake.go`. Проверка: `go test ./internal/drive/infrastructure/adapters/... -run 'TestFakeKMS|TestFakeSecretManager'`.
- [ ] 1.4 `CompanyKEKService`: GetKEK (RAM-LRU TTL 5 мин, fail-closed при unwrap), ProvisionKEK (CSPRNG, KMS-wrap, Secret Manager, PG-строка). Файл: `src/internal/drive/infrastructure/adapters/company_kek.go`. Проверка: unit-тесты round-trip GetKEK/ProvisionKEK (fake KMS/SM + testcontainers PG).
- [ ] 1.5 `IdentityResolver` — только UUID-валидация формата (FR-ID-3), без DB-запросов. Файл: `src/internal/drive/infrastructure/adapters/identity_resolver.go`. Проверка: unit-тесты валидный/невалидный UUID.
- [ ] 1.6 Proto `DriveKeyManagerService` + генерация кода (дополнить `generate_protos.sh`). Файлы: `src/application/authz/proto/key_manager.proto`, generated `.pb.go`. Проверка: `go build ./...`; сгенерированный код закоммичен.
- [ ] 1.7 `KeyManagerService`: `CreateFileKey`/`GetFileFEK`/`GetBulkFileFEK` (полностью), `FetchCompanyKEK`/`ProvisionCompanyKEK` (полностью), `RotateFileFEK`/`RotateCompanyKEK` (stubs `Unimplemented` до stage 7); singleflight по FEK; аудит каждой выдачи; AGFK-хелперы формата. Файлы: `src/application/authz/service/key_manager_service.go`, `fek_crypto.go`. Проверка: unit-тесты deny/allow/bulk/singleflight (паттерн `authz_service_test.go`): `go test ./application/authz/...`.
- [ ] 1.8 IAM-interceptor для `FetchCompanyKEK` (YC IAM-токен; остальные методы — модель доверия proxy). Файл: `src/api/iam_interceptor.go`. Проверка: unit-тест верификации/отклонения токена.
- [ ] 1.9 Wire + config + регистрация сервиса в gRPC-сервере (дефолты `kms_key_id`, `secret_manager_folder`, `keymanager_kek_cache_ttl`). Файлы: `src/config/config.go`, `src/internal/drive/infrastructure/adapters/wire.go`, `src/application/authz/wire.go`, `src/api/grpc_server.go`. Проверка: `go build ./...`; сервер стартует с флагами.
- [ ] 1.10 Репозитории аудита и ключей (append-only; асинхронный batch INSERT, deny-записи синхронно). Файлы: `src/internal/drive/infrastructure/persistence/repositories/company_crypto_key_repository.go`, `key_access_log_repository.go`. Проверка: unit-тесты Insert/GetActive/NextVersion.

## 2. Stage 2 — Data path (форк, ветка agio-drive-v2)

Детали: `.qwen/plans/stage-02-data-path.md`. Предусловие: форматы AGFK/AGCK/AGDF зафиксированы (мастер-план §4.1).

- [ ] 2.1 Крипто-примитивы AGDF/AGCK: `EncryptBlock`/`DecryptBlock`/`IsLegacyBlock`/`WrapCEK`/`UnwrapCEK` (AES-256-GCM, nonce на запись, AAD = sliceID‖blockIndex / driveFileID‖sliceID‖fekVersion). Файл: `pkg/chunk/cek_encrypt.go`. Проверка: `go test ./pkg/chunk/ -run 'TestEncryptBlock|TestDecryptBlock|TestWrapCEK|TestUnwrapCEK'` (round-trip, tamper → fail-closed, nonce uniqueness, AAD mismatch).
- [ ] 2.2 AGFK-примитивы форка (`WrapFEK`/`UnwrapFEK`, `FekAAD`) + known-answer векторы из stage 1. Файл: `pkg/meta/fek_crypto.go`. Проверка: `go test ./pkg/meta/ -run 'TestWrapFEK|TestUnwrapFEK'`.
- [ ] 2.3 Slice-запись с `wrapped_cek`: 24B base + optional length-prefixed AGCK tail; `readSlices` парсит оба формата; обновление call-sites в `redis.go`. Файлы: `pkg/meta/slice.go`, `pkg/meta/interface.go` (`Slice.WrappedCEK`), `pkg/meta/redis.go`. Проверка: `go test ./pkg/meta/ -run 'TestSlice|TestMarshalSlice'` — legacy 24B байт-в-байт как до изменения.
- [ ] 2.4 Crypto-поля `Attr` (persisted suffix, transient `Fek` вне Marshal) + `Format.EncryptionEnabled/KEKVersion`. Файлы: `pkg/meta/interface.go`, `pkg/meta/config.go`. Проверка: `go test ./pkg/meta/ -run 'TestAttr|TestFormat'` — `Marshal(Attr{Fek: x})` == `Marshal(Attr{})` по crypto-части; legacy marshal не изменился.
- [ ] 2.5 `ChunkStore.NewReaderWithKey/NewWriterWithKey`; `rSlice.cek`/`wSlice.cek`; decrypt в единой точке после load (кэш/S3), encrypt перед upload/staging; range-GET отключён для CEK-slice; без compressor для ciphertext; double-encrypt guard по magic AGDF. Файлы: `pkg/chunk/chunk.go`, `pkg/chunk/cached_store.go`. Проверка: `go test ./pkg/chunk/ -run 'TestEncryptedStore|TestLegacyStore'` — S3 и disk cache содержат только ciphertext (NFR-SEC-1); legacy (key=nil) не изменился.
- [ ] 2.6 VFS plumbing: `handle.fek/fekVer/encrypted` (не сериализуется в saveHandle), CEK-кэш per open file в `fileReader`/`fileWriter`, wrap CEK при commit. Файлы: `pkg/vfs/handle.go`, `pkg/vfs/reader.go`, `pkg/vfs/writer.go`. Проверка: `go test ./pkg/vfs/ -run 'TestFileWriter_WrapsCEKUnderFEK|TestFileReader_UnwrapsCEKOnce'`.
- [ ] 2.7 Предусловия версионирования FR-VER-1/4: хук `baseMeta.fileCryptoDeletable` + точки вызова в GC; `fek_version` end-to-end. Файлы: `pkg/meta/base.go`, `pkg/meta/redis.go`. Проверка: `go test ./pkg/meta/ -run 'TestFileCryptoDeletable|TestCanDeleteFileCrypto'`.
- [ ] 2.8 Регрессия этапа. Проверка: `go build ./... && go vet ./pkg/...`; `make test.meta.core && make test.pkg` зелёные; `go fmt -l pkg/ cmd/` пусто.

## 3. Stage 3 — Meta + KeyManager (оба репозитория)

Детали: `.qwen/plans/stage-03-meta-keymanager.md`. Предусловие: этапы 1–2.

- [ ] 3.1 Proto-расширения форка (`ProtoFileCrypto`, `ProtoSlice.wrapped_cek`, `OpenResponse.fek/fek_version/encrypted`, `CreateRequest.drive_file_id`, `Format.encryption_enabled/kek_version`) + конверсии proto↔Go. Файлы: `pkg/meta/pb/meta.proto`, `pkg/meta/pb/meta_common.proto`, generated, `pkg/meta/grpc_convert.go`. Проверка: `go build ./...`; `go test ./pkg/meta/ -run 'TestAttrToProto|TestSliceToProto|TestFormatToProto'` (round-trip + legacy).
- [ ] 3.2 `redisMeta.SetFileCrypto` — один Redis txn (GET attr → mutate crypto-поля → SET); интерфейс `fileCryptoSetter` (type-assertion, не в `Meta`). Файл: `pkg/meta/redis_fek.go`. Проверка: интеграционный тест с реальным Redis: `go test -run 'TestSetFileCrypto' ./pkg/meta/` (гейт `make test.meta.non-core`).
- [ ] 3.3 KeyManager-клиент форка: копия proto platform + gRPC-обёртка (TLS по флагам). Файлы: `pkg/meta/keymanager_pb/key_manager.proto`, generated, `pkg/meta/keymanager_client.go`. Проверка: `go build ./...`; комментарий «sync with agio-platform» в копии proto.
- [ ] 3.4 Proxy Create/Open с KeyManager: FEK при Create + rollback `Unlink` (FR-USR-2), FEK при Open только при `cached_fek_version != attr.FekVersion`, plaintext FEK только в `OpenResponse`; UUID-валидация identity (`extractUserIDFromOIDC` → `(string, error)`, interceptor → `Unauthenticated`). Файлы: `pkg/meta/grpc_server_fuse.go`, `pkg/meta/authz_interceptor.go`. Проверка: интеграционные тесты `TestEncryptedFullCycle`, `TestCreateRollback`, `TestUserWithoutPermission_Denied`, `TestNonUUIDSub_Unauthenticated` (Redis + MinIO + fake KeyManager).
- [ ] 3.5 Клиент: FEK LRU 100k/TTL 15 мин в `grpcMeta`, `drive_file_id = uuid.New()` при Create, кэш-хит Open через `cached_fek_version`. Файлы: `pkg/meta/grpc_client.go`, `pkg/meta/grpc_client_fuse.go`. Проверка: `go test -run 'TestFekCache|TestEncrypted' ./pkg/meta/` — второй Open не вызывает KeyManager (counter).
- [ ] 3.6 Wiring proxy: флаги `--keymanager-service`, `--keymanager-tls-*`; warning при отсутствии. Файл: `cmd/meta_proxy.go`. Проверка: `go build ./...`; `./juicefs meta-proxy --help` показывает флаги.
- [ ] 3.7 Интеграционный suite этапа (Redis + MinIO + in-process fake KeyManager): полный цикл, deny, owner bypass, legacy volume без KeyManager-вызовов, cache-hit. Проверка: `make test.meta.non-core` зелёный; `go test -run 'TestEncrypted|TestFekCache|TestOwnerBypass|TestLegacyVolume' ./pkg/meta/`.

## 4. Stage 4 — Render-клиент (форк)

Детали: `.qwen/plans/stage-04-render-client.md`. Предусловие: этап 3.

- [ ] 4.1 Декоратор `RenderMeta` над `meta.Meta`: Open (FEK из LRU 100k/1h или локальный `UnwrapFEK`, чужая компания → EIO), Create (локальная генерация FEK + `SetFileCrypto` + rollback), остальные методы — делегирование. Файл: `pkg/meta/render_meta.go`. Проверка: `go test ./pkg/meta/ -run 'TestRenderMeta'` (unwrap, cross-company fail-closed, create, TTL).
- [ ] 4.2 Команда `juicefs render-mount`: прямой Redis (subdir = company prefix), `FetchCompanyKEK` по IAM ноды (TLS 1.3; KEK никогда через CLI), mlock KEK + обнуление при unmount. Файл: `cmd/render_mount.go`. Проверка: `go build ./...`; `./juicefs render-mount --help`; интеграционный `TestRenderFullCycle` без OIDC/proxy/PG (AC-4).
- [ ] 4.3 Aggressive caching (attr/entry timeout ≥ 60s) + pipelined readdir для render-режима. Файлы: `cmd/render_mount.go`, `pkg/meta/redis.go` (pipeline-ветка Readdir при необходимости). Проверка: unit/integration-тест таймаутов; проверка round-trips readdir (1 на директорию).
- [ ] 4.4 Интеграционные тесты: render читает/пишет файл user-клиента; cross-company → EIO + chroot. Проверка: `go test -run 'TestRender' ./pkg/meta/` (Redis + MinIO) зелёный.

## 5. Stage 5 — Clone/CopyFileRange/Compaction (форк)

Детали: `.qwen/plans/stage-05-clone-compaction.md`. Предусловие: этап 2 (этап 4 не блокирует).

- [ ] 5.1 `redisMeta.RewrapSlices` — re-wrap всех AGCK в chunk lists dst из-под srcFek в-под dstFek, один txn на chunk list; legacy-записи не трогаются; решение по направлению зависимости pkg/meta→pkg/chunk для AGCK-примитивов (при необходимости — вынос в общий пакет). Файлы: `pkg/meta/redis_fek.go`, при необходимости новый общий пакет. Проверка: `go test -run 'TestRewrapSlices' ./pkg/meta/` (round-trip, legacy untouched, fail-closed на ошибке unwrap).
- [ ] 5.2 RPC `ResolveFileKey` (authz Read; FEK в ответе + запись в клиентский LRU). Файлы: `pkg/meta/pb/meta.proto`, `pkg/meta/grpc_server_fuse.go`, `pkg/meta/grpc_client_fuse.go`. Проверка: интеграционный тест resolve + deny без прав.
- [ ] 5.3 Компакция с CEK: хук `baseMeta.fileKeyResolver` + `compactionAllowed`; `CompactChunk` msg с `{fek, driveFileID, fekVersion}`; `vfs.Compact` (read с CEK → merge → new CEK → `WrapCEK`); `doCompactChunk` += `wrappedCEK` (redis — в запись, sql/tkv — nil legacy); fail-closed skip при ошибке resolve. Файлы: `pkg/meta/base.go`, `cmd/mount.go`, `pkg/vfs/compact.go`, `pkg/meta/redis.go`, `pkg/meta/sql.go`, `pkg/meta/tkv.go`. Проверка: `go test ./pkg/vfs/ -run 'TestCompact_WithCEK'`; `go test ./pkg/meta/ -run 'TestCompaction_SkippedWhenNotAllowed'`; `make test.meta.core`.
- [ ] 5.4 Clone-оркестрация: proxy (`GetFileFEK(src)` → `Clone` → `CreateFileKey(dst)` → `SetFileCrypto` → `RewrapSlices`, ошибка → `Unlink(dst)`) + render path (локально). Файлы: `pkg/meta/grpc_server_fuse.go`, `pkg/meta/render_meta.go`. Проверка: `go test -run 'TestClone_Rewrap_NoS3Rewrite|TestClone_Subset|TestClone_LegacyFile' ./pkg/meta/` — S3 keys идентичны до/после (AC-7).
- [ ] 5.5 CopyFileRange: slice-sharing путь → `RewrapSlices` на диапазоне dst (FEK из handles); data-copy путь — через reader/writer с FEK; разрезание чанка → новый CEK. Файлы: `pkg/vfs/vfs.go`, при необходимости `pkg/meta/redis_fek.go`. Проверка: интеграционный тест CopyFileRange зашифрованных файлов (read обоих файлов после операции).
- [ ] 5.6 Предусловия FR-VER-2/3: хук `baseMeta.sliceDeletable` + точки в GC. Файлы: `pkg/meta/base.go`, `pkg/meta/redis.go`. Проверка: `go test ./pkg/meta/ -run 'TestSliceDeletable_Hook'`.

## 6. Stage 6 — Offline + No Residuality (форк)

Детали: `.qwen/plans/stage-06-offline-no-residuality.md`. Предусловие: этап 3 (этапы 4–5 не блокируют).

- [ ] 6.1 `MemClear`/`MlockPage` + обнуление всех plaintext-ключей: eviction LRU, `grpcMeta.WipeKeys`, `VFS.InvalidateAllKeys` (handles → stale EIO), mlock для FEK/KEK (metric при неудаче). Файлы: `pkg/utils/memclr.go`, `pkg/meta/grpc_client.go`, `pkg/vfs/vfs.go`. Проверка: `go test ./pkg/utils/ -run TestMemClear`; `go test ./pkg/meta/ -run 'TestWipeKeys'`; `go test ./pkg/vfs/ -run 'TestInvalidateAllKeys'`.
- [ ] 6.2 State machine hub (online → offline-connected → disconnected, timeout default 15 мин) + триггеры WipeKeys: logout control file `_JFS_LOGOUT`, OIDC expiry, offline timeout. Файлы: `pkg/meta/grpc_client.go`, `pkg/vfs/internal.go`. Проверка: `go test ./pkg/meta/ -run 'TestHubStateMachine'`; интеграционный `TestLogout_CacheUnreadable` (AC-8).
- [ ] 6.3 Write journal offline-записей (append-only, replay по seq при reconnect, truncate) + проверка идемпотентности replay по семантике `redisMeta.doWrite` (результат зафиксировать в решениях). Файлы: `pkg/vfs/write_journal.go`, `pkg/vfs/writer.go`, `cmd/mount.go`. Проверка: `go test ./pkg/vfs/ -run 'TestWriteJournal'`; интеграционный `TestOfflineWrites_ReplayOnReconnect` (NFR-OFF-3).
- [ ] 6.4 Флаги + wiring (`--offline-timeout`, `SetOnWipe`, journal в cache-dir) + offline-чтение из кэша / fail-closed новые Open. Файлы: `cmd/mount.go`, `pkg/meta/grpc_client.go`. Проверка: `go build ./...`; интеграционные `TestOfflineConnected_ReadFromCache` (AC-9), `TestOfflineTimeout_Disconnected`; `make test.pkg`.

## 7. Stage 7 — Revocation + STS (оба репозитория)

Детали: `.qwen/plans/stage-07-revocation-sts.md`. Предусловие: этап 3 (этап 6 желателен).

- [ ] 7.1 Platform: permission generation — `INCR drivepermgen:{userID}` при SetRole/DeleteRole/ClearRoles + явная `Invalidate` PermissionCache; production TTL кэша ≤ 30s; RPC `GetPermissionGeneration`. Файлы: `src/internal/drive/infrastructure/adapters/auth.go`, `src/config/config.go`, `src/application/authz/proto/key_manager.proto`, `key_manager_service.go`. Проверка: `go test ./internal/drive/... -run 'TestDeleteRole_BumpsGeneration|TestGetPermissionGeneration'` (miniredis).
- [ ] 7.2 Platform: STS-провайдер (порты + AWS `AssumeRole` с prefix-scoped inline policy; YC — проверить возможности IAM, ограничение зафиксировать) + RPC `GetSTSCredentials` (TTL ≤ 60 мин, аудит). Файлы: `src/internal/drive/application/ports/sts.go`, `src/internal/drive/infrastructure/adapters/sts_aws.go`, `sts_yc.go`, `key_manager_service.go`. Проверка: `go test ./internal/drive/... -run 'TestGetSTSCredentials_PolicyPrefix'` (fake provider: prefix + duration).
- [ ] 7.3 Форк: heartbeat generation — `FlushSessionResponse.permission_generation`; proxy читает generation на каждом heartbeat; клиент при изменении → `WipeKeys` + `InvalidateAllKeys`. Файлы: `pkg/meta/pb/meta_lifecycle.proto`, `pkg/meta/grpc_server_lifecycle.go`, `pkg/meta/grpc_client.go`. Проверка: unit-тест «generation change → WipeKeys вызван» (mock).
- [ ] 7.4 Форк: `stsRefresher` (refresh на половине TTL, fail-safe до expiry) + `ReloadableStorage.SetCredentials` (локальный swap blob, без записи в Redis Format); флаг `--sts-enabled`; render mode — прямой cloud STS по IAM ноды. Файлы: `cmd/sts_refresher.go`, `cmd/mount.go`, `cmd/render_mount.go`. Проверка: unit-тест refresh/swap (fake clock); интеграционный `TestSTS_RefreshAndExpiry` (FR-REV-3).
- [ ] 7.5 FEK rotation: RPC proxy `RotateFileKey` (admin-gated: GetFileFEK(old) → `GenerateRotatedFileKey` platform → `SetFileCrypto` → `RewrapSlices`, fail-closed на ошибке) + platform handler с org-admin check и аудитом. Файлы: `pkg/meta/pb/meta.proto`, `pkg/meta/grpc_server_fuse.go`, `src/application/authz/proto/key_manager.proto`, `key_manager_service.go`. Проверка: интеграционный `TestFekRotation` — S3 keys не изменились, старый FEK не разворачивает новые wrapped_cek (FR-ROT-4).
- [ ] 7.6 Offboarding: batch RPC proxy `RotateFileKeysByPaths` (rate-limited, resumable с checkpoint, skipped-paths в ответе) + CLI platform `keymanager rotate-user-keys` (user → пути из PG → proxy RPC). Файлы: `pkg/meta/pb/meta.proto`, `pkg/meta/grpc_server_fuse.go`, `src/cmd/keymanager.go`. Проверка: интеграционный `TestRotation_OffboardingBatch` (10 файлов, версии bumped, данные целы; повторный запуск — 0 операций).
- [ ] 7.7 Интеграция отзыва: grant → read OK → DeleteRole → deny ≤ TTL(30s) + heartbeat(12s) + WipeKeys. Проверка: `TestRevoke_LosesFEK` (FR-TEST-10, AC-10); регрессия: форк `make test.meta.core`, platform `go test ./application/authz/... ./internal/drive/...`.

## 8. Stage 8 — Миграция legacy-данных (форк)

Детали: `.qwen/plans/stage-08-migration.md`. Предусловие: этапы 1–3.

- [ ] 8.1 `baseMeta.ReencryptChunk` — атомарный swap chunk list одним slice (txn: DEL + RPush + refcount −1 старые/+1 новый) с обязательной проверкой неизменности list с момента Read (конфликт → повтор чанка); реализации engine (redis — полностью, sql/tkv/grpcMeta — ENOSYS/делегирование). Файлы: `pkg/meta/base.go`, `pkg/meta/interface.go`, `pkg/meta/redis.go`. Проверка: `go test -run 'TestReencryptChunk_Refcount' ./pkg/meta/`; `make test.meta.core`.
- [ ] 8.2 Команда `juicefs reencrypt` + worker: walk от root/path, FEK по Company KEK (`FetchCompanyKEK` по service identity; `--kek-file` только dev), per-chunk merge в новый slice с новым CEK, идемпотентность из состояния Redis (all-AGCK = done), resume. Файлы: `cmd/reencrypt.go`, `cmd/reencrypt_worker.go`. Проверка: интеграционные `TestReencrypt_RoundTrip`, `TestReencrypt_Idempotent`, `TestReencrypt_Resume` (Redis + MinIO).
- [ ] 8.3 Включение шифрования на томе: флаг format `--encryption-enabled` + админ-команда `juicefs enable-encryption` (`ProvisionCompanyKEK` при отсутствии KEK → `Format.EncryptionEnabled=true, KEKVersion`; идемпотентно). Файлы: `cmd/format.go`, `cmd/enable_encryption.go` (новый). Проверка: `TestLegacyReadable_AfterEnable` (FR-MIG-1, AC-6); `./juicefs enable-encryption --help`.
- [ ] 8.4 CEK rotation как режим `--file <path> --rotate-cek` (полный проход: decrypt → new CEK → swap; документация FR-ROT-7). Файлы: `cmd/reencrypt.go`, `cmd/reencrypt_worker.go`. Проверка: интеграционный тест rotate-cek (старые S3-объекты GC-кандидаты, данные читаются).
- [ ] 8.5 Rate-limiting (concurrency/IOPS/bandwidth) + доступность файла для чтения/записи во время миграции. Файлы: `cmd/reencrypt_worker.go`. Проверка: `TestRateLimit` (measured IOPS ≤ лимит×tolerance), `TestReencrypt_ConcurrentReadWrite` (FR-MIG-6).

## 9. Stage 9 — Тестирование (оба репозитория)

Детали: `.qwen/plans/stage-09-testing.md`. Предусловие: этапы 1–8.

- [ ] 9.1 Cross-repo known-answer векторы форматов AGFK/AGCK/AGDF — идентичный `vectors.json` в обоих репозиториях + `TestKnownAnswerVectors` в каждом. Файлы: `pkg/agio/testcrypto/vectors.json` (форк), `src/application/authz/service/testcrypto/vectors.json` (platform) + тесты. Проверка: `go test ./pkg/agio/... -run TestKnownAnswerVectors` (форк) и `go test ./application/authz/... -run TestKnownAnswerVectors` (platform) — оба зелёные.
- [ ] 9.2 Аудит покрытия unit-тестов FR-TEST-1..7, 19..21: таблица «FR → тест», заполнить пробелы (fail-closed KMS/Redis, plaintext не на диске включая staging/journal, identity FR-TEST-19/20/21). Файлы: тесты в `pkg/chunk/`, `pkg/meta/`, `pkg/vfs/` (форк), `src/application/authz/service/` (platform). Проверка: таблица полная; `make test.meta.core && make test.pkg`; platform unit зелёные.
- [ ] 9.3 Интеграционный suite + Makefile-target: `docker-compose.enc-test.yml` (Redis + MinIO), `make test.enc.integration` (compose up → `go test -tags=encintegration ./pkg/meta/... ./cmd/...` → down); включить существующие тесты этапов 3–8 в suite. Файлы: `docker-compose.enc-test.yml`, `Makefile`. Проверка: `make test.enc.integration` зелёный.
- [ ] 9.4 Нагрузочные тесты FR-TEST-22..25: harness `tests/load/` (N клиентов, hit/miss Open, throughput encrypted vs legacy, render RPS) + отчёт с p50/p99 и pass/fail по целям (≤5 мс / ≤100 мс / ≤10% / 0 доп. round-trips). Файлы: `tests/load/main.go`, `tests/load/report-<date>.md`. Проверка: отчёт закоммичен; недостижение целей — зафиксировано как отклонение (не блокирует).
- [ ] 9.5 Security-сценарии FR-TEST-26..30: Go-тесты + скрипты (инсайдер deny, утечка S3/Redis/Redis+S3, компрометация render-ноды) + отчёт. Файлы: `tests/security/*`, `tests/security/report.md`. Проверка: отчёт pass/fail по всем 5 сценариям; AC-3 подтверждён.
- [ ] 9.6 Полный прогон на stage-окружении (manual): реальный platform + KMS/SpiceDB/PG, owner bypass через SpiceDB, revoke с замером времени, STS prefix-policy, render-нода с IAM. Файлы: `tests/stage-runbook.md`. Проверка: чеклист выполнен, результаты записаны.
- [ ] 9.7 AC-чеклист: `tests/acceptance.md` — таблица AC-1..16 → тест(ы) → команда → статус + трассировка «каждый FR/NFR ДОЛЖЕН → тест» (AC-1). Файл: `tests/acceptance.md`. Проверка: все 16 AC со статусом и ссылкой на тест.

## 10. Stage 10 — Production rollout (оба репозитория + инфраструктура)

Детали: `.qwen/plans/stage-10-production-rollout.md`. Предусловие: этап 9, acceptance зелёный.

- [ ] 10.1 Per-company KMS master keys + auto-rotation (проверить поведение Decrypt старых версий у провайдера ДО включения) + CLI `keymanager provision --company <id>` (идемпотентно) + IAM least privilege matrix. Файлы: `src/cmd/keymanager.go` (platform), `docs/ops/iam-matrix.md`. Проверка: provision idempotent (повторный запуск — 0 операций); матрица IAM задокументирована (NFR-SEC-12).
- [ ] 10.2 Encrypted Redis backups (AOF everysec + RDB, SSE-KMS экспорт в S3, хранение ≥ 35 дней) + обязательный restore-тест. Файлы: `tests/security/redis-restore-test.sh`, инфраструктурные конфиги. Проверка: скрипт пройден (restore → mount → чтение зашифрованных файлов OK); зафиксировано в runbook (FR-REDIS-5/6).
- [ ] 10.3 Аудит: партиционирование/архивация `drive_key_access_log` ≥ 12 мес (append-only), anomaly detection worker (>N файлов за период T → security event, исключения render_node), daily reconciliation Kratos↔platform (R12). Файлы: platform worker + SQL-миграции, `docs/ops/`. Проверка: `go test ./worker/... -run TestAnomaly` (platform); reconciliation-отчёт.
- [ ] 10.4 Метрики + алерты: форк (`jfs_fek_cache_hits/misses_total`, `jfs_fek_unwrap_latency_seconds`, `jfs_sts_refresh_failures_total`, `jfs_offline_events_total`, `jfs_encrypted_blocks_read/written_total`, `jfs_reencrypt_progress`, `jfs_fek_mlock_failures_total`), platform (`keymanager_*`), Grafana-правила (p99 > 100 мс, STS failures, deny-rate spike, offline events). Файлы: код метрик в этапах 3–8 (проверить наличие), k8s values, `docs/ops/`. Проверка: `curl -s localhost:9091/metrics | grep keymanager` (platform) и `/metrics` форка содержат метрики; алерт-правила закоммичены.
- [ ] 10.5 Runbooks (8 документов): rb-kek-rotation, rb-fek-rotation, rb-cek-rotation, rb-revocation, rb-incident-kek-compromise, rb-incident-redis-loss, rb-incident-s3-leak, rb-enable-company. Файлы: `docs/ops/rb-*.md` (оба репозитория + общий README). Проверка: документы закоммичены и ревьюированы; тайминги отзыва (≤30s / ≤12s+TTL / ≤60 мин) отражены.
- [ ] 10.6 Audit package для SOC 2 / MPAA TPN: threat model с принятыми границами (T8/T9 + компенсирующие контроли), иерархия ключей и форматы как реализовано, traceability «SRS → код → тест», процедуры, допущения A1–A6. Файл: `docs/security/audit-package.md`. Проверка: таблица traceability полная (на основе `tests/acceptance.md`).
- [ ] 10.7 Rollout-план + rollback + пилотная компания: порядок (test → stage → prod per company), предусловия (все клиенты на slice-формате v2, STS, бэкапы), rollback = остановка новых зашифрованных файлов; финальный прогон AC на production-конфигурации. Файлы: `docs/ops/rollout.md`, `tests/acceptance.md`. Проверка: пилотная компания работает с шифрованием; алерты не стреляют 72 часа; все 16 AC закрыты.

## Definition of Done

- [ ] D.1 Форк: `go build ./...` и `go vet ./pkg/... cmd/...` — без ошибок.
- [ ] D.2 Platform: `go build ./...` и `go vet ./...` (в `src/`) — без ошибок.
- [ ] D.3 Тесты форка: `make test.meta.core`, `make test.pkg`, `make test.enc.integration` — зелёные.
- [ ] D.4 Тесты platform: `go test ./application/authz/... ./internal/drive/...` — зелёные.
- [ ] D.5 Lint: `golangci-lint run` (по `.golangci.yml`) без новых нарушений; `go fmt -l pkg/ cmd/` — пусто.
- [ ] D.6 `tests/acceptance.md`: все 16 AC закрыты со ссылками на тесты; отклонения (если есть) зафиксированы с причиной.
- [ ] D.7 В коммитах нет локальных `replace`-директив в go.mod; новый `.go` код — с Apache 2.0 header; plaintext ключи не в логах/метриках (NFR-SEC-10).
