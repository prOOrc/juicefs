# AC-чеклист (stage 9, task 9.7)

Таблица AC-1..16 (SRS §22) → тест(ы) → команда → статус. Статусы проверены по
коду на 2026-09-23. **форк** = `github.com/juicedata/juicefs` (`agio-drive-v2`),
**platform** = `agio-platform` (`src/`). «stage: ожидает» — часть критерия
проверяется только ручным прогоном на stage (см. `tests/stage-runbook.md`).

| AC | Критерий | Тест(ы) | Команда | Статус |
|---|---|---|---|---|
| AC-1 | Все FR/NFR ДОЛЖЕН покрыты тестами | трассировка ниже + `tests/fr-test-coverage.md` (FR-TEST-1..30) | `make test.meta.core && make test.pkg` (форк); `cd platform/src && go test ./application/authz/...` | ✓ (зазоров нет, см. ниже) |
| AC-2 | Read на X ≠ расшифровка Y | `TestInsiderCrossFileFEK_Denied` (форк pkg/meta); AAD-привязка: `TestDecryptBlock_AADMismatch`, `TestUnwrapCEK_AADMismatch` (pkg/chunk), `TestUnwrapAADMismatchFailsClosed` (platform) | `go test ./pkg/meta/ -run TestInsiderCrossFileFEK_Denied`; `go test ./pkg/chunk/ -run 'AAD'` | ✓ |
| AC-3 | Утечка S3 + Redis не раскрывает данные | security-сценарии FR-TEST-27/28/30 (`tests/security/main.go`) + `TestEncryptedStore_CiphertextInS3AndCache` (pkg/chunk) | `bash tests/security/run-all.sh` → `tests/security/report.md` | ✓ PASS 5/5 (report.md) |
| AC-4 | Render без OIDC, прокси, PG | `TestRenderFullCycle` (форк pkg/meta: прямой Redis + локальный KEK); load FR-TEST-25 (0 round-trips на KeyManager) | `go test ./pkg/meta/ -run TestRenderFullCycle`; `go run ./tests/load` | ✓ |
| AC-5 | Cross-company невозможен | `TestRenderCrossCompany` (chroot ENOENT + direct-inode EIO); stage S5.3 | `go test ./pkg/meta/ -run TestRenderCrossCompany` | ✓ (stage 2026-09-27: chroot-изоляция B→A подтверждена, см. `tests/stage-runbook.md`) |
| AC-6 | Legacy файлы читаются | `TestLegacyReadable_AfterEnable` (cmd), `TestLegacyVolume_Unchanged`, `TestResolveFileKey_Legacy`, `TestClone_LegacyFile` (pkg/meta); byte-compat: `TestAttrMarshalLegacyByteIdentical`, `TestMarshalSliceCEK_LegacyByteIdentical`, `TestSliceRoundTripMixedLegacyAndEncrypted` | `go test ./cmd/ -run TestLegacyReadable_AfterEnable`; `go test ./pkg/meta/ -run 'Legacy'` | ✓ |
| AC-7 | Clone zero-copy через CEK re-wrap | `TestClone_Rewrap_NoS3Rewrite`, `TestClone_Subset`, `TestCopyFileRange_Encrypted` (pkg/meta) | `go test ./pkg/meta/ -run 'TestClone|TestCopyFileRange'` | ✓ |
| AC-8 | Logout → кэш нечитаем | `TestLogout_CacheUnreadable`, `TestWipeKeys` (pkg/meta), `TestLogoutFile_WipesKeys` (pkg/vfs) | `go test ./pkg/meta/ -run 'TestLogout_CacheUnreadable|TestWipeKeys'`; `go test ./pkg/vfs/ -run TestLogoutFile_WipesKeys` | ✓ |
| AC-9 | Offline-connected работает | `TestOfflineConnected_ReadFromCache`, `TestOfflineWrites_ReplayOnReconnect` (pkg/vfs) | `go test ./pkg/vfs/ -run 'TestOffline'` | ✓ |
| AC-10 | Revocation: отзыв + TTL + STS expiry → потеря доступа | `TestRevoke_LosesFEK`, `TestHeartbeat_GenerationChange_WipesKeys` (pkg/meta); замер времени — stage S3 | `go test ./pkg/meta/ -run 'TestRevoke_LosesFEK|TestHeartbeat_GenerationChange'` | ✓ (stage 2026-09-27: доступ потерян за ~12–15 c после отзыва, см. `tests/stage-runbook.md`) |
| AC-11 | Нагрузочные тесты пройдены | Гейт 10.7: конкурентный перезамер на linux — `tests/load/report-2026-09-27-linux.md` (FR-TEST-25 concurrent PASS, 0 round-trips, 0 зависаний после фикса prOOrc/go-redis v9.18.1; FR-TEST-24 NO-GO: медиана 18–27% > ≤10%, решение — DEVIATION pending decision у владельца); история: `report-2026-09-23.md`, `report-2026-09-27.md` (darwin 39.1%) | `go run ./tests/load -redis 127.0.0.1:16379 -db 15 -clients 16 -bytes 1073741824` | ⚠ DEVIATION pending decision (гейт 10.7a; базовые статусы task 9.4 остаются ✓ с отклонениями) |
| AC-12 | Audit log на каждую выдачу FEK | platform: `kmMockAudit`-ассерты в `key_manager_service_test.go` (allow/deny/error на CreateFileKey/GetFileFEK); запись `FetchCompanyKEK` — stage S5.4 | `cd platform/src && go test ./application/authz/... -run TestKeyManager` | ✓ (stage 2026-09-27: audit `create_file_key`/`get_file_fek`/`fetch_company_kek` в `drive_key_access_log`, см. `tests/stage-runbook.md`) |
| AC-13 | Threat model с границами T8, T9 | задокументирована в SRS §«Threat model» (T8/T9 = принятые границы); граница write journal — FR-TEST-5 в `tests/fr-test-coverage.md` | — (документация) | ✓ задокументировано |
| AC-14 | Версионирование: `fek_version`, reference counting | `TestFekRotation`, `TestRevoke_LosesFEK` (pkg/meta); `TestSetFileCrypto`, `TestGCRespectsFileCryptoDeletable`, `TestSliceDeletable_Hook` (pkg/meta); `TestReencryptChunk_Refcount` (pkg/meta, этап 8) | `go test ./pkg/meta/ -run 'TestFekRotation|TestSetFileCrypto|TestGCRespectsFileCryptoDeletable|TestReencryptChunk_Refcount'` | ✓ |
| AC-15 | Owner bypass через `PermissionOwn`; `CheckOrganizationAdmin` не в пользовательской модели | `TestOwnerBypass` (форк pkg/meta); platform: байпас — `own`-permission в SpiceDB-схеме, `CheckOrganizationAdmin` в KeyManager не вызывается (проверка по коду + stage S2) | `go test ./pkg/meta/ -run TestOwnerBypass` | ✓ (stage 2026-09-27: owner-a прочитал файл байпасом, 0 вызовов `CheckOrganizationAdmin`, см. `tests/stage-runbook.md`) |
| AC-16 | OIDC `sub` (UUID) напрямую = user.id; fail-closed при неверном формате | `TestInterceptor_NonUUIDSub_Unauthenticated` (форк pkg/meta); platform: `TestKeyManager_InvalidIdentity`, `TestKeyManager_UserNotInPlatform_Deny`; прямая передача sub — FR-TEST-19 в `tests/fr-test-coverage.md` | `go test ./pkg/meta/ -run TestInterceptor_NonUUIDSub_Unauthenticated`; `cd platform/src && go test ./application/authz/... -run 'TestKeyManager_InvalidIdentity|TestKeyManager_UserNotInPlatform'` | ✓ |

## AC-1: трассировка «FR/NFR ДОЛЖЕН → тест»

Полная таблица FR-TEST-1..30 (все тестовые требования SRS §21) — в
`tests/fr-test-coverage.md`. Покрытие по группам требований:

| Группа | Требования | Покрытие |
|---|---|---|
| Ключи и криптография | FR-KEY-1..5, NFR-SEC-1..3 | FR-TEST-1 (wrap/unwrap/tamper/nonce), known-answer векторы AGFK/AGCK/AGDF (оба репозитория), `TestEncryptedStore_CiphertextInS3AndCache` |
| Пользовательская модель | FR-USR-1..6, FR-ID-1..4 | FR-TEST-8..10, 19..21 (create/get FEK, deny, revoke, identity fail-closed) |
| Render | FR-RND-1..3 | FR-TEST-11..12, 25 (`TestRenderFullCycle`, `TestRenderCrossCompany`, load 0 round-trips) |
| Операции | FR-OP-1..4 (clone/compact/reencrypt) | FR-TEST-2..3, 14..15 (`TestClone_*`, `TestCompact_WithCEK`, `TestReencryptChunk_Refcount`) |
| Ротация | FR-ROT-1..4 | `TestFekRotation`, `TestRotateFileKey`/`TestRotateCompanyKEK` (platform), `TestRevoke_LosesFEK` |
| Offline / no-residuality | NFR-OFF-1, FR-TEST-5..6, 16..17 | `TestOffline*` (pkg/vfs), `TestLogout*`, `TestWipeKeys`, `TestHeartbeat_GenerationChange_WipesKeys` |
| Версионирование | FR-VER-1..4 | `TestSetFileCrypto`, `TestGCRespectsFileCryptoDeletable`, `TestSliceDeletable_Hook`, `TestFekRotation` |
| Администрирование | FR-ADMIN (owner bypass) | `TestOwnerBypass`; stage S2 |
| Нагрузка | NFR-PERF (FR-TEST-22..25) | `tests/load/report-2026-09-23.md` (с зафиксированными отклонениями) |
| Безопасность | FR-TEST-26..30, AC-3 | `tests/security/report.md` (PASS 5/5) |

Зазоров «ДОЛЖЕН без теста» нет. Принятые границы (не дефекты): write journal
хранит plaintext-данные локально 0600 (NFR-SEC-1 = chunk cache, threat model
T10); T8/T9 — криптографически незащищённые сценарии по решению SRS.
