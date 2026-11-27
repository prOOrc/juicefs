# Tasks: `fix-cfapi-followups`

## 0. Precondition

- [ ] 0.1 Структурный фикс main spec: `openspec/specs/domain-cfapi-provider/spec.md` — добавлен заголовок секции `## Requirements` (требования созданы при архивации прототипа вне секции; validate/archive их не видели). Проверка: `openspec validate fix-cfapi-followups` без INFO о невидимых требованиях.

## 1. A1 — Identity-first классификация (U20)

- [ ] 1.0 Pre-checks (чтение, без кода): тип `CFPlaceholderStandardInfo` (pkg/cfapi/structs.go) — поля FileId/FileIdentity, семантика идентичности (U-F0); интерфейс store провайдера и все фейки (`grep StateOf`); `standardOnce` (reconcile.go:58-64); декодер FileIdentity в fetch.go; текст status-логики (cmd/clouddrive.go:494-527). Проверка: выводы в диффе/комментарии к задачам.
- [ ] 1.1 pkg/cfapi: аксессор FileIdentity STANDARD-записи (`FileIdentity[:FileIdentityLength]`) + именованный предикат «не плейсхолдер» (HRESULT), рядом cfapi.go:423-444 и consts.go:329-341; тесты. Проверка: `go test ./pkg/cfapi/... -count=1`.
- [ ] 1.2 store: метод `StandardInfoOf(path) (info, isPlaceholder, err)` (не плейсхолдер → (nil,false,nil); BACKUP_SEMANTICS — работает для каталогов), реализация osStore + все фейки. Проверка: `go build ./... && go vet ./...`.
- [ ] 1.3 `pkg/cfprovider/identity.go`: `classifyIdentity(path, inode) → identityOwn|identityForeign|identityPlain|identityUnknown`; тесты таблицей (own/plain/foreign/unknown/FileIdentityLength≠8). Проверка: `go test ./pkg/cfprovider/ -run TestClassifyIdentity -count=1`.
- [ ] 1.4 Ветка classifyOccupied :713 (`!isPlaceholder` → `kind != identityOwn`) и own-ветка :732-751 (idx.Put + skipped + adopted; ensureDehydrated только при положительном атрибутном флаге); тесты U20-регрессии (чистые атрибуты + своя идентичность → adopt; plain → конфликт без мутаций; foreign → конфликт; ошибка STANDARD → конфликт без мутаций; known → ноль STANDARD-запросов; absent+allowCreate → createEntry). Проверка: `go test ./pkg/cfprovider/ -run TestClassifyOccupied -count=1`.
- [ ] 1.5 FETCH-путь buildPopulatedDir :514-516 — тот же предикат; тест adopt. Проверка: `go test ./pkg/cfprovider/ -run TestBuildPopulatedDir -count=1`.
- [ ] 1.6 walk.go: счётчик adopted (учёт в skipped), строки `walk adopt: entries=%d` (Info, при >0) и `walk classify: <path>: <reason>` (Debug); формат `walk:` байт-в-байт прежний. Проверка: `go test ./pkg/cfprovider/ -run TestWalk -count=1`; grep-гейт формата строки `walk:`.
- [ ] 1.7 cmd/clouddrive.go: `status` переиспользует общий хелпер/аксессор pkg/cfapi, вывод не меняется. Проверка: `go build ./...` + ручной `status` на тест-корне.
- [ ] 1.8 Stage 0 живого гейта (рестарт facility vector-stage, root AgioDrive9): PASS = нет `walk conflict:` по file.txt.txt; `status` прежний; файл не изменился. (Выполняет оркестратор после reviewer.)

## 2. A2 — Restart freshness (поверх 1.x)

- [ ] 2.0 Pre-checks: сигнатура queueUpload + гард read-only + дедупликация (reconcile.go:1-140); markSelfEvent (ingest.go:540-712, мьютекс); счётчики walk.go; сигнатуры UpdatePlaceholderByPath/openPlaceholderHandle.
- [ ] 2.1 `pkg/cfprovider/freshness.go`: `freshMtimeSlack = 2s`, `compareVersion` (4 вердикта), `checkFreshness`; store-метод `StatOf(path) (size, mtime, isDir, err)`; отчётные счётчики в known- и own-ветках БЕЗ действий; каталоги исключены. Тесты: TestCompareVersion (таблица, граница допуска), TestCheckFreshness* (equal-без-мутаций, stat-error, каталог skipped, path-mismatch skipped). Проверка: `go test ./pkg/cfprovider/ -run 'TestCompareVersion|TestCheckFreshness' -count=1`.
- [ ] 2.2 Stage 1 живого гейта (только отчёт): 20 файлов (10 Windows + 10 Mac) под companies\home-gri-llo\cf-followups\, рестарт без изменений → PASS: differs=0, drift ≤2s. Решение об ужесточении допуска. (Оркестратор.)
- [ ] 2.3 Действия: localNewer → queueUpload (read-only → Warn; out-of-sync → отдача reconcileDir); backendNewer → markSelfEvent + Update(VerifyInSync|Dehydrate); pinned/out-of-sync → skip+Warn; ambiguous → skip+Warn; строка `walk fresh: revalidated=... queued-upload=... readonly=... pinned=... ambiguous=... out-of-sync=...` (Info, при ненулевой сумме); идемпотентность (equal — ноль мутаций/строк). При необходимости Update-обёртка с метаданными (cfapi.go:406-417). Тесты: TestCheckFreshnessLocalNewer*/BackendNewer*/Ambiguous*/Adopt*. Проверка: `go test ./pkg/cfprovider/ -run TestCheckFreshness -count=1`.
- [ ] 2.4 Пробы U-F1/U-F3/U-F5/U-F6 на своём каталоге cf-followups (pinned-dehydrate; STANDARD-поля после офлайн-правки; DEHYDRATE на out-of-sync/открытом handle; watcher-событие после Update). (Оркестратор; результаты в design.md.)
- [ ] 2.5 Stage 2 живого гейта: remote-edit с Mac (2 файла) → рестарт → revalidated=2 → чтение SHA совпадает; local-edit → рестарт → queued-upload=1; два рестарта без изменений → ноль строк. (Оркестратор.)
- [ ] 2.6 DoD пакета: `go build ./... && go vet ./... && go test ./...` зелёные.

## 3. A3 — pkg/meta тесты на Windows

- [ ] 3.1 Инвентаризация: `go test -c -o NUL -gcflags=-e ./pkg/meta` — полный список unix-only мест сверх известных. Проверка: список зафиксирован.
- [ ] 3.2 Пара алиасов: `pkg/meta/flock_other_test.go` (`//go:build !windows`, алиасы syscall.F_*) + `pkg/meta/flock_windows_test.go` (`//go:build windows`, 0/1/2), заголовки двухстрочные по прецеденту errno_server_*.go. Проверка: `go vet ./pkg/meta` и `GOOS=linux go vet ./pkg/meta` компилируются.
- [ ] 3.3 Замена 63 вхождений в base_test.go (testListLocks :1334+) на алиасы. Проверка: `grep -nE 'syscall\.F_(RD|WR|UN)LCK' pkg/meta/*_test.go | grep -v flock_other_test.go` — пусто.
- [ ] 3.4 Сверка ABI: grep по не-test файлам pkg/meta — движки ожидают lock-типы 0/1/2; если собственные константы иные — СТОП, эскалация. Проверка: вывод зафиксирован.
- [ ] 3.5 Тег `!windows` на grpc_fuse_integration_test.go (Stat_t). Проверка: `go vet ./pkg/meta` продвигается дальше.
- [ ] 3.6 Итерация до зелёного: все места из 3.1 по правилам (константы — алиас, unix-типы — тег на файл). Проверка: `go vet ./pkg/meta` RC=0 на Windows.
- [ ] 3.7 DoD Windows: `go build ./pkg/meta` RC=0; `go test ./pkg/meta -run TestServerErrno -count=1 -v` RC=0; `go test ./pkg/meta -run '^$' -count=1` RC=0; `go test ./pkg/meta -list '.*'` — зафиксировать доступные на Windows тесты.
- [ ] 3.8 Parity Linux: `go build ./...`, `go vet ./...`, тесты без изменений.

## 4. A4 — Cleanup-режим

- [ ] 4.0 Pre-checks: вход санитизации mutex (provider.go:634-643 — путь или volume); status-детект регистрации; NTFS-проверка register :252-258 (инлайн/хелпер); сигнатуры PlaceholderStandardInfoByPath/openPlaceholderHandle/тип revertFlags; sentinel «не плейсхолдер».
- [ ] 4.1 pkg/cfapi: `RevertPlaceholderByPath(path, flags)` (Lstat → BACKUP_SEMANTICS для каталогов; доступ GENERIC_READ|GENERIC_WRITE, +DELETE при RevertFlagDelete; share-all); расширение openPlaceholderHandle параметром доступа (старая сигнатура делегирует); конст `RevertFlagDelete = 0x1` (consts.go:344). Негативный windows-тест на plain-файле. Проверка: `go build ./pkg/cfapi && go test ./pkg/cfapi/... -count=1`.
- [ ] 4.2 Пробы U-C1..U-C4 на AgioDrive8 (запрещены AgioDrive9/7): STANDARD/revert на незарегистрированном корне; revert каталога; RevertFlagDelete; прямой os.Remove плейсхолдера. Watchdog 120с, одна запись за шаг, результаты в design.md. (Оркестратор.)
- [ ] 4.3 Движок cleanup: `pkg/cfprovider/cleanup.go` (классификация L/P/N/U, plan-then-act, правила R1-R9, отчёт) + `cleanup_test.go` (классификация; dry-run ноль мутаций; apply удаляет только reverted; каталоги снизу вверх, kept; отказы: mutex/NTFS/нет корня/вложенный живой корень/зарегистрирован без --unregister; --unregister до revert; идемпотентность). Общий построитель имени mutex переиспользуется из provider.go. Проверка: `go test ./pkg/cfprovider/ -run TestCleanup -count=1`.
- [ ] 4.4 Команда `cleanup` в cmd/clouddrive.go (список :187-234); хелперы NTFS (:252-258) и unregister (:362-379) извлечены; чтение ключа :1047-1049. Проверка: `go build ./... && go vet ./...` + dry-run smoke на тест-корне.
- [ ] 4.5 Живой dry-run на AgioDrive8: `clouddrive cleanup --root C:\Users\iobukhov\AgioDrive8`; отчёт сохранён; атрибуты до/после — изменений нет. (Оркестратор.)
- [ ] 4.6 Runbook владельцу (в change): для AgioDrive3/4/5/6/8/10 — dry-run → разбор → `--apply --delete-files --unregister` → ручное удаление пустого каталога; ЗАПРЕТ AgioDrive7 (плейграунд) и AgioDrive9 (живой корень). [OWNER-GATE]
- [ ] 4.7 DoD: `go build ./... && go vet ./... && go test ./...` зелёные; Linux-джобы не затронуты.

## Definition of Done

- `go build ./...`, `go vet ./...`, `go test ./...` — зелёные на Windows (рецепт CGO mingw для полной сборки).
- `go vet ./pkg/meta` RC=0; `go test ./pkg/meta -run TestServerErrno -count=1` RC=0 (оживлённый тест).
- Спеки delta актуальны; каждый новый тест имеет Verified-by строку в delta-спеке (файл::тест).
- Живой гейт facility vector-stage: Stage 0 + Stage 1 + Stage 2 PASS (design.md, критерии PASS/FAIL).
- U-C1..C4 выполнены на AgioDrive8; результаты в design.md; реальное применение cleanup к корням AgioDrive/3/4/5/6/8/10 — [OWNER-GATE] вне этого change (runbook готов).
- Форматы `walk:`/`walk conflict:`/`OK, ... is ready` байт-в-байт не изменены (grep-гейт).
