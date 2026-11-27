# Proposal: `fix-cfapi-followups`

## Why

Прототип `implement-cfapi-provider` заархивирован и работает живьём на facility vector-stage (вердикт владельца 27.11: ручная проверка пройдена, кросс-машинная видимость Mac↔Windows подтверждена), но в U-таблице остались четыре открытых пункта, мешающих продуктивному использованию и следующей волне (приложение V3): U20 (рестарт-walk объявляет конфликтом полностью синхронизированный плейсхолдер), отсутствие restart-сверки свежести (правка с другой машины при выключенном serve подтягивается только реактивно — а её нет), мёртвые тесты `pkg/meta` на Windows (пре-существующий брейк компиляции закрывает TestServerErrno) и ручная непрожимаемая чистка осиротевших корней проб (U15/U16: осиротевшие плейсхолдеры блокируют удаление каталогов).

## What Changes

- **U20 — классификация walk**: точка «наш плейсхолдер» в `classifyOccupied` и FETCH-пути `buildPopulatedDir` перестаёт полагаться только на атрибутную маску (`StateOf`): индекс → `LookupPath` → STANDARD-запрос `PlaceholderStandardInfoByPath` (FileId == inode) → adopt/skip ДО атрибутной эвристики. Записи с почищенными атрибутами (state=0) больше не падают в конфликт; чужой plain-файл остаётся конфликтом (инвариант «локальные файлы не перезаписываются»).
- **Restart-сверка cross-machine freshness**: walk при существующих записях сверяет (size, mtime) локального плейсхолдера с BackendEntry hub: локаль новее → очередь выгрузки (с учётом `--read-only`); backend новее → ревалидация (metadata-update плейсхолдера, дешидратация устаревшего гидратированного контента; каталоги — mtime). Правка с другой машины при выключенном serve подтягивается на рестарте. Реактивного обновления в сессии по-прежнему нет (U17 вне скоупа).
- **pkg/meta тесты на Windows**: платформенная пара flock-алиасов (`flock_other_test.go` / `flock_windows_test.go`) снимает 63 вхождения `syscall.F_*` в `base_test.go`; `grpc_fuse_integration_test.go` получает тег `!windows`; `go vet ./pkg/meta` и `TestServerErrno` зелёные на Windows, Linux-CI не меняется.
- **cleanup-режим**: команда `clouddrive cleanup --root X [--apply] [--delete-files] [--unregister]` — orphan-placeholder детект (STANDARD-запрос без провайдера), plan-then-act, revert через новую обёртку `RevertPlaceholderByPath` (+ конст `RevertFlagDelete`), удаление только подтверждённо-reverted, реестр через общий хелпер unregister. По умолчанию dry-run; `--apply` = ack. Реальное применение к корням AgioDrive/3/4/5/6/8/10 — [OWNER-GATE] (в change: реализация + живой dry-run на AgioDrive8 + runbook владельцу).

## Related Requirements

- **ADR-005** (`specs/decisions/ADR-005-cfapi-provider-windows-transport.md`): Windows-транспорт V3 — CfAPI-провайдер на Go; настоящий change развивает его follow-ups, решение и инварианты не меняет.
- **SRS-001** (Подсистема шифрования): инвариант криптоцепочки сохраняется — change не трогает FEK/CEK/AGDF-пути (walk-классификация и metadata-update не касаются данных).
- **domain-cfapi-provider** (main spec, 21 requirement): MODIFIED — «Плейсхолдеры из метаданных hub» (классификация/adopt) и «Сверка при старте» (freshness-ветка); ADDED — «Cleanup-режим осиротевших плейсхолдеров».
- BRD отсутствует — change закрытия известных follow-ups архивного change (аналогично прототипу; бизнес-контекст — ADR-005 и решения владельца).

## Capabilities

### New Capabilities

Нет (все дельты — к существующему capability).

### Modified Capabilities

- `domain-cfapi-provider`: (1) классификация walk индекс/STANDARD-первая вместо атрибутной эвристики (U20); (2) restart-сверка freshness при старте serve; (3) новый cleanup-режим; (4) TestServerErrno исполняем (Verified-by).

## Non-Goals

- Реактивное обновление дерева в сессии (U17) — только рестарт-сверка.
- Удаление stale-плейсхолдеров walk'ом (U16 walk-часть) — только cleanup-командой с ack.
- Конфликты нескольких писателей, приоритеты recently-used, resume-cursor walk.
- Очистка state-dir, `--json`-отчёт cleanup, параллельный обход.
- pkg/vfs и `vfs_test.go` (пре-существующий брейк cgofuse на Windows), Makefile `test.meta.core` (bash-синтаксис), Windows-CI-джоба.
- Реальное применение cleanup к корням AgioDrive/3/4/5/6/8/10 (владелец; в change — dry-run на AgioDrive8 + runbook).
- Изменения движка ради приложения V3 (очередь B agio-drive не зависит от этого change: pin/unpin/dehydrate уже в движке, клиенты делают сами).

## Impact

- Файлы: `pkg/cfprovider/placeholders.go`, `walk.go`, `reconcile.go` (классификация + freshness), `pkg/cfapi/operations.go` + `consts.go` (RevertPlaceholderByPath, RevertFlagDelete), `pkg/meta/base_test.go` + новые `flock_*_test.go` + `grpc_fuse_integration_test.go`, `cmd/clouddrive.go` (команда cleanup + хелперы NTFS/unregister), новые `pkg/cfprovider/cleanup.go` + тесты.
- Риски: регрессия классификации (чужой файл должен остаться конфликтом — юнит-тесты на фейках), races с watcher (markSelfEvent), живой гейт обязателен для data-path веток (урок 27.11).
- Живая верификация: рестарт-проба на facility vector-stage (AgioDrive9 владельца — только рестарт и чтение; сценарий с критериями PASS/FAIL в design), U-C1..C4 на AgioDrive8.
- Owner-gates: применение cleanup к реальным корням (после change); результаты U-C проб владельцу к сведению.
