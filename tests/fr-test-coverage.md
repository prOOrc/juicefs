# FR-TEST coverage audit (stage 9, task 9.2)

Таблица «FR → тест» по SRS §21 (FR-TEST-1..30). Статусы проверены по коду на 2026-09-23.
Репозитории: **форк** = `github.com/juicedata/juicefs` (ветка `agio-drive-v2`), **platform** = `agio-platform` (`src/`).

## Unit / integration (FR-TEST-1..7, 19..21)

| FR-TEST | Требование | Тест(ы) | Репозиторий | Статус |
|---|---|---|---|---|
| FR-TEST-1 | CEK/FEK wrap/unwrap, GCM auth tag failure, nonce uniqueness | `TestEncryptBlock_RoundTrip`, `TestDecryptBlock_TamperTag`, `TestDecryptBlock_TamperCiphertext`, `TestDecryptBlock_WrongKey`, `TestDecryptBlock_AADMismatch`, `TestEncryptBlock_NonceUniqueness`, `TestWrapCEK_RoundTrip`, `TestUnwrapCEK_Tamper`, `TestUnwrapCEK_AADMismatch`, `TestUnwrapCEK_WrongFEK` (pkg/chunk); `TestSetFileCrypto`, `TestRewrapSlices_FailClosed` (pkg/meta); `TestWrapUnwrapRoundTrip`, `TestUnwrapTamperedFailsClosed`, `TestUnwrapAADMismatchFailsClosed`, `TestUnwrapWrongKEKFailsClosed` (platform service); `TestKnownAnswerVectors` (оба) | форк + platform | ✓ |
| FR-TEST-2 | Clone re-wrap CEK (без перешифровки данных) | `TestClone_Rewrap_NoS3Rewrite` (pkg/meta) | форк | ✓ |
| FR-TEST-3 | Compaction с CEK | `TestCompact_WithCEK`, `TestCompact_WithCEK_FailClosed` (pkg/vfs); `TestCompaction_SkippedWhenNotAllowed` (pkg/meta) | форк | ✓ |
| FR-TEST-4 | Fail-closed при ошибках KMS/Redis | KMS: `TestUserWithoutPermission_Denied`, `TestCreateRollback`, `TestResolveFileKey_Denied`, **`TestKeyManagerUnavailable_FailClosed`** (новый, транспортная ошибка ≠ deny), `TestRewrapSlices_FailClosed` (pkg/meta). Redis: стандартная propagation ошибок meta-операций (upstream base-тесты `TestRedisClient` и др.) — крипто-материал в ошибке не участвует, ключи не раскрываются | форк | ✓ (заполнен) |
| FR-TEST-5 | Encrypted local cache: plaintext не на диске | `TestEncryptedStore_CiphertextInS3AndCache` (pkg/chunk — S3 и disk cache содержат только AGDF), `TestLegacyStore_Unchanged`. **Граница (принята):** write journal (`pkg/vfs/write_journal.go`) хранит plaintext-данные (не ключи) локально, 0600; NFR-SEC-1 покрывает только chunk cache. Journal тринкуется после успешного replay и при logout (`TestLogout_CacheUnreadable`), записи переживают crash (NFR-OFF-1). См. threat model T10 | форк | ✓ (граница задокументирована) |
| FR-TEST-6 | memclr ключей при logout | `TestWipeKeys` (pkg/meta), `TestLogoutFile_WipesKeys` (pkg/vfs), `TestHeartbeat_GenerationChange_WipesKeys` (pkg/meta) | форк | ✓ |
| FR-TEST-7 | FEK reference counting / `fek_version` | `TestGCRespectsFileCryptoDeletable`, `TestSliceDeletable_Hook` (pkg/meta); `TestSetFileCrypto` (версионирование); `TestReencryptChunk_Refcount` (этап 8) | форк | ✓ |
| FR-TEST-19 | OIDC `sub` (UUID) → `subject_id` напрямую, без маппинга | platform: `TestKeyManager_CreateFileKey_Allowed`, `TestKeyManager_GetFileFEK_Denied` — mock-ожидания фиксируют, что `UserId` из запроса доходит до `CheckPermissionByPath` неизменённым (прямая передача); fork: `TestInterceptor_NonUUIDSub_Unauthenticated` (отрицательный случай) | platform + форк | ✓ |
| FR-TEST-20 | Токен с `sub` не-UUID → fail-closed | `TestInterceptor_NonUUIDSub_Unauthenticated` (pkg/meta, fork); `TestKeyManager_InvalidIdentity` (platform — пустой user_id → InvalidArgument) | форк + platform | ✓ |
| FR-TEST-21 | Валидный токен, пользователь не импортирован в платформу → deny | **`TestKeyManager_UserNotInPlatform_Deny`** (новый, platform: authz error → fail-closed, FEK не выдан, audit result=error) | platform | ✓ (заполнен) |

## Integration (FR-TEST-8..18)

| FR-TEST | Требование | Тест(ы) | Репозиторий | Статус |
|---|---|---|---|---|
| FR-TEST-8 | Полный цикл mount→create→write→close→unmount→mount→open→read | `TestEncryptedFullCycle` (pkg/meta: create → wrapped FEK в attr, open → plaintext FEK, cache hit, remount) | форк | ✓ |
| FR-TEST-9 | Пользователь B без прав → FEK deny | `TestUserWithoutPermission_Denied` (EACCES, FEK не доставлен) | форк | ✓ |
| FR-TEST-10 | Grant → FEK → revoke → потеря FEK после TTL | `TestRevoke_LosesFEK` (pkg/meta, heartbeat generation → WipeKeys) | форк | ✓ |
| FR-TEST-11 | Render-клиент читает зашифрованный файл без OIDC | `TestRenderFullCycle` (pkg/meta: bidirectional interop, AC-4) | форк | ✓ |
| FR-TEST-12 | Cross-company: render A не разворачивает FEK компании B | `TestRenderCrossCompany` (chroot ENOENT + direct-inode EIO) | форк | ✓ |
| FR-TEST-13 | Legacy plaintext читается после включения шифрования | `TestLegacyReadable_AfterEnable` (cmd, этап 8), `TestLegacyVolume_Unchanged`, `TestClone_LegacyFile` | форк | ✓ |
| FR-TEST-14 | Clone: re-wrap CEK, S3 не переписывается | `TestClone_Rewrap_NoS3Rewrite` (zero-copy: slice-ID set + refcount) | форк | ✓ |
| FR-TEST-15 | Clone подмножества: остальные чанки source недоступны через target | **`TestClone_Subset`** (новый: CopyFileRange одного чанка из трёх; target ссылается только на скопированный слайс; AGCK-блобы остальных чанков не разворачиваются под FEK target — AAD-привязка к identity source), `TestCopyFileRange_Encrypted` (подмножество через диапазон) | форк | ✓ (заполнен) |
| FR-TEST-16 | Offline-connected: работа по кэшу без hub | `TestOfflineConnected_ReadFromCache` (pkg/vfs) | форк | ✓ |
| FR-TEST-17 | Logout → кэш нечитаем (no residuality) | `TestLogout_CacheUnreadable` (pkg/meta), `TestWipeKeys`, `TestLogoutFile_WipesKeys` | форк | ✓ |
| FR-TEST-18 | Company Owner получает FEK байпасом | `TestOwnerBypass` (pkg/meta) | форк | ✓ |

## Нагрузочные (FR-TEST-22..25) — см. `tests/load/report-<date>.md`

| FR-TEST | Требование | Цель | Статус |
|---|---|---|---|
| FR-TEST-22 | 1000 одновременных Open с кэш-хитом | p99 ≤ 5 мс | см. отчёт load |
| FR-TEST-23 | 1000 одновременных Open с кэш-промахом | p99 ≤ 100 мс | см. отчёт load |
| FR-TEST-24 | Throughput с шифрованием vs без | деградация ≤ 10% | см. отчёт load |
| FR-TEST-25 | Render: 5000 metadata RPS, 0 доп. round-trips на FEK | Redis выдерживает | см. отчёт load |

## Security (FR-TEST-26..30) — см. `tests/security/report.md`

| FR-TEST | Сценарий | Статус |
|---|---|---|
| FR-TEST-26 | Инсайдер с Read на X пытается получить FEK Y → deny | `TestInsiderCrossFileFEK_Denied` (pkg/meta, DB 14) — см. отчёт security |
| FR-TEST-27 | Утечка S3: чанки нечитаемы без CEK | см. отчёт security |
| FR-TEST-28 | Утечка Redis: `wrapped_fek` нечитаем без Company KEK | см. отчёт security |
| FR-TEST-29 | Компрометация render-ноды: доступ только к своей компании | см. отчёт security |
| FR-TEST-30 | Утечка Redis + S3 одновременно: данные нечитаемы без Company KEK | см. отчёт security |

## Заполненные пробелы (этап 9)

1. **FR-TEST-4**: добавлен `TestKeyManagerUnavailable_FailClosed` (pkg/meta/encrypt_integration_test.go) — транспортная ошибка KeyManager (`codes.Unavailable`) ведёт себя как deny: Open без FEK, Create с rollback; после восстановления — работает.
2. **FR-TEST-15**: добавлен `TestClone_Subset` (pkg/meta/clone_integration_test.go). Интерпретация: Clone в форке — полный (без off/limit), подмножество реализуется через CopyFileRange; криптографическое свойство FR-OP-3 проверено напрямую (AGCK source не разворачивается под FEK target).
3. **FR-TEST-21**: добавлен `TestKeyManager_UserNotInPlatform_Deny` (platform key_manager_service_test.go) — authz error (unknown subject) → fail-closed, audit result=error.
4. **FR-TEST-5**: задокументирована принятая граница — write journal хранит plaintext-данные (не ключи) локально 0600; NFR-SEC-1 ограничивается chunk cache; journal тринкуется при logout и после replay.
