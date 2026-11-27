# Tasks: `implement-cfapi-provider`

Формат задачи: файлы + команда проверки. Команды выполняются из корня репозитория.

## 0. Предусловия

- [x] 0.1 Создать ветку `implement-cfapi-provider` от `agio-drive-v2`. Файлы: —. Проверка: `git merge-base --is-ancestor agio-drive-v2 HEAD`.
- [x] 0.2 Собрать бинарь ветки с CGO по рецепту Makefile-таргета `juicefs.exe` (PATH `C:\mingw64\bin`, `CGO_ENABLED=1`, `CGO_CFLAGS='-IC:/WinFsp/inc/fuse'`, `go build -ldflags='-s -w' -o juicefs.exe .`). Toolchain обязан соответствовать `go 1.26.5` из go.mod. Файлы: —. Проверка: существование `juicefs.exe`; `go version` и фактическую команду записать в design.md (D2). Стоп-точка: при неудаче — эскалация, код не пишется.
- [x] 0.3 Получить эталонный `cldapi.h` (пакет `Microsoft.Windows.SDK.CPP` на NuGet либо заголовки mingw-w64). Источник, версия, SHA-256 — в design.md (раздел Decisions & Rationale, D1). Файлы: design.md. Проверка: файл header существует локально, запись в design.md. В репозиторий header не коммитить.
- [x] 0.4 Сверить имена функций биндинга с экспортами. Файлы: design.md (список D1). Проверка: `objdump -p C:\Windows\System32\cldapi.dll | grep -i Cf` содержит все функции списка.
- [x] 0.5 Записать имена build-тегов `pkg/meta` для sqlite и fdb (`pkg/meta/sql_sqlite.go`, `pkg/meta/tkv_fdb.go`) в design.md. Файлы: design.md. Проверка: grep `//go:build` в обоих файлах.

## 1. Биндинг, регистрация, подключение

- [x] 1.1 `pkg/cfapi`: структуры и процедуры из списка D1 design.md, пул страница-выровненных буферов (VirtualAlloc). CfOpenFileWithOplock и CfRevertPlaceholder — деферрал в 4.1/5.x (обёртки operations.go по плану задач). Файлы: `pkg/cfapi/*.go`. Проверка: `go build ./pkg/cfapi/...` — go build/vet/test/race зелёные, CgoFiles пусто (reviewer APPROVED).
- [x] 1.2 `pkg/cfapi`: эталонные размеры и смещения структур и тест `TestLayout`. Значения получены одноразовой C-утилитой (mingw64 gcc, флаги NTDDI обязательны — без них mingw отсекает содержимое по NTDDI); утилита и header вне репо. TestLayout поймал и исправил ошибку транскрипции (pad перед ProviderId). Файлы: `pkg/cfapi/layout_test.go`. Проверка: `go test ./pkg/cfapi -run TestLayout` — PASS.
- [x] 1.3 `pkg/cfapi`: thunk'и и диспетчер с очередями. В thunk'ах нет ввода-вывода. Фактическая поверхность (проба U9 закрыта по header): 7 OS-thunk'ов на 9 OS-точек D5 — RENAME/DELETE маршрутизируют файл/каталог по `*_IS_DIRECTORY`-флагам; отдельного NOTIFY_DIRECTORY_CREATED в CfAPI нет; 10-я точка движка (`OnNotifyDirectoryCreated`) — в интерфейсе диспетчера, наполняется запасным механизмом U9 (design.md D5 обновлён). Файлы: `pkg/cfapi/callbacks.go`, `pkg/cfapi/dispatcher.go`, `pkg/cfapi/dispatcher_test.go`. Проверка: `go test ./pkg/cfapi` — PASS (14 тестов + race).
- [x] 1.4 `cmd/clouddrive.go`: `register`, `unregister`, `status`; корень по умолчанию `%USERPROFILE%\AgioDrive`; предпроверка NTFS; ключ реестра. Файлы: `cmd/clouddrive.go`, регистрация подкоманды в `cmd/main.go`. Проверка: `go build ./cmd/...`; ручной прогон сценариев «Регистрация sync root», «Отмена регистрации», «Корень не на NTFS».
- [x] 1.5 Пробы U1, U9, U10; результаты и решения — в таблицу Open Questions design.md. Файлы: design.md. Проверка: строки U1/U9/U10 заполнены.
- [x] 1.6 `serve`: `CfConnectSyncRoot` с таблицей callback'ов, `CfDisconnectSyncRoot` при завершении, именованный mutex. Файлы: `cmd/clouddrive.go`, `pkg/cfprovider/provider.go`. Проверка: `go build ./...`; сценарий «Второй экземпляр».

DoD этапа: `go test ./pkg/cfapi` зелёный; сценарии спек «Регистрация sync root», «Отмена регистрации», «Корень не на NTFS», «Второй экземпляр» пройдены.

## 2. Плейсхолдеры и готовность

- [ ] 2.1 Обвязка data path по gc-паттерну и mount с STS (блок кода в design.md, Integration Points). Общий хелпер `cmd`, если копирование неизбежно. Файлы: `cmd/clouddrive.go`, опционально `cmd/storage_helper.go`. Проверка: `go build ./cmd/...`.
- [ ] 2.2 `pkg/cfprovider`: `Index` — inode → путь, журнал в каталоге состояния. Файлы: `pkg/cfprovider/index.go`, `pkg/cfprovider/index_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [x] 2.3 Upfront population: полный рекурсивный walk hub при старте (пивот 27.11, design.md D6). Такт 1 — populateTopLevel до маркера (уже реализовано). Такт 2 — фоновый walk после маркера: выделить из populateTopLevel примитив «обработать каталог» (листинг → классификация to-create/existing/conflict → create-примитив 6.4 (реальная запись + convert, поштучно) → индекс/журнал), обойти им все каталоги (BFS, 4 воркера, cancel при остановке serve). FETCH-воркер не удалять (fallback). Файлы: `pkg/cfprovider/placeholders.go`, `pkg/cfprovider/walk.go`, `pkg/cfapi/operations.go` (если нужен хелпер массивного создания вне колбэка), `cmd/clouddrive.go`. Проверка: `go build ./...`; `go vet ./...`; `go test ./pkg/cfapi/... ./pkg/cfprovider/...`; Windows: serve → маркер → `walk complete:` на дереве ≥3 уровней, все записи hub созданы.
- [ ] 2.4 Конфликт одноимённого локального файла: пропуск и запись в лог (в walk — один Warn на каталог). Файлы: `pkg/cfprovider/placeholders.go`, `pkg/cfprovider/placeholders_test.go`, `pkg/cfprovider/walk_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [x] 2.5 Маркер готовности `OK, <имя тома> is ready` — после верхнего уровня, до/независимо от walk (пивот 27.11); полнота namespace = строка `walk complete:`. Файлы: `pkg/cfprovider/provider.go`, `pkg/cfprovider/walk.go`. Проверка: ручной прогон сценариев «Готовность», «Hub недоступен», «Сбой в глубине».
- [x] 2.6 `cmd/clouddrive.go`: `ls`, `hash`. Файлы: `cmd/clouddrive.go`. Проверка: `go build ./cmd/...`; ручной `ls` по сценарию «Дочерний каталог».
- [x] 2.7 Walk-воркер: BFS-очередь, пул из 4 воркеров, старт после маркера, остановка по ctx. Файлы: `pkg/cfprovider/walk.go`, `pkg/cfprovider/walk_test.go`. Проверка: юнит-тест на фейковом backend и создателе — каждый каталог посещён один раз, дети после родителя, cancel останавливает.
- [x] 2.15 Сверить константы population/hydration primary в pkg/cfapi с заголовком (сомнение архитектора: FULL=1/ALWAYS_FULL=2?). Результат 27.11: биндинг = header (population PARTIAL=0/FULL=2/ALWAYS_FULL=3; hydration NO=0/PROGRESSIVE=1/FULL=2/ALWAYS_FULL=3) — XOR-интерпретация верна (p=2=FULL, p=3=ALWAYS_FULL); батчевый CfCreatePlaceholders основным путём не остаётся (INVALID_REQUEST при p=3 — U14). Файлы: `pkg/cfapi/consts.go`, header jfs-tmp. Проверка: grep vs header.
- [x] 2.8 Convert-примитив create (замена порций): интерфейс «создать запись в каталоге» за абстракцией (юнит-тесты на любой ОС); Windows-реализация по design.md D6.4 вариант A (файл: CreateFile CREATE_NEW → SetEndOfFile(N) → SetFileTime(mtime) → CfConvertToPlaceholder(identity, MARK_IN_SYNC|DEHYDRATE); каталог: CreateDirectory → SetFileTime → convert(MARK_IN_SYNC)); при отказе convert(MARK_IN_SYNC|DEHYDRATE) — fallback convert(MARK_IN_SYNC) → CfUpdatePlaceholder(FsMetadata, DEHYDRATE|MARK_IN_SYNC); откат созданного при ошибке (удаление; quarantine при sharing violation); CREATE_NEW-отказ → переклассификация по индексу. Файлы: `pkg/cfapi` (недостающие биндинги CfUpdatePlaceholder/флаги DEHYDRATE), `pkg/cfprovider/placeholders.go`, `pkg/cfprovider/walk.go`, тесты. Проверка: go build/vet/test; Windows живьём: размер=N, mtime=hub, alloc≈0, чтение байта → FETCH_DATA в логе; конверсия каталога; отказ на одной записи (недопустимое имя) → запись удалена, failed++, остальные созданы. В D6.4 фиксируется выбранный вариант.
- [x] 2.9 Идемпотентность walk: existing по индексу — skip без Warn и без записи в журнал; conflict — skip и один Warn на каталог. Файлы: `pkg/cfprovider/walk.go`, `pkg/cfprovider/walk_test.go`. Проверка: юнит-тест «повторный walk — 0 create, 0 строк журнала»; Windows — рестарт без Warn.
- [x] 2.10 Сбои листинга: транзиентные — backoff 1с→30с, перманентные — skip поддерева. Файлы: `pkg/cfprovider/walk.go`, `pkg/cfprovider/walk_test.go`. Проверка: юнит-тест с ошибкой, затем успехом — `walk complete:` только после успеха.
- [x] 2.11 Лог walk: `walk:` не чаще раза в 5с, `walk complete:` один раз, `walk cancelled:` при остановке. Файлы: `pkg/cfprovider/walk.go`, `pkg/cfprovider/walk_test.go`. Проверка: юнит-тест и grep в Windows-прогоне.
- [x] 2.12 Дефолт `--population-policy` = ALWAYS_FULL(3), Warn при 0/2; `--management-policy` default 0x7 (уже реализовано — задача фиксируется в значении флага). Файлы: `cmd/clouddrive.go`. Проверка: `clouddrive register --help`; Windows — create в заселённом каталоге без FETCH в логе.
- [ ] 2.13 M1-замер walk: время до маркера и до `walk complete:` при первичном заселении и рестарте; пропускная способность convert-примитива при 1/4/8 воркерах (против базовой линии create-walk 10ms); RSS и размер JSONL на 100k и 1M; RPS листинга; число FETCH_DATA во время walk (антивирус/индексатор); путь длиннее 260 символов; коллизия регистра; недопустимое имя; аллокация на диске при примитиве. Результат — таблицей в design.md (D11). Файлы: `pkg/cfprovider/walk.go` (бенч), design.md. Проверка: результаты в design.md.
- [x] 2.14 Windows-проверка фронтира (D6.2): create в заселённом каталоге; в существующем не обработанном; по отсутствующему пути (`ERROR_PATH_NOT_FOUND`); перечисление не обработанного каталога. Файлы: — . Проверка: ручной прогон, ожидания по design.md D6.2; результаты в design.md.

DoD этапа: сценарии «Верхний уровень», «Дочерний каталог», «Перезапуск без дублей», «Маркер готовности», «Одноимённый локальный файл», «Сбой одной записи в батче», «Конфликт имён», «Сбой листинга в глубине», «Каталог, не достигнутый walk», «Прерывание walk» пройдены; в дефолтном прогоне нет FETCH_PLACEHOLDERS.

## 3. Гидратация и отмена

- [x] 3.1 Пул гидратации: очередь, буферы, `CfExecute(TRANSFER_DATA)`. Пробы U3, U4, U13. Файлы: `pkg/cfprovider/fetch.go`, `pkg/cfapi/operations.go`, `pkg/cfprovider/fetch_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [x] 3.2 Разрешение inode → путь с подъёмом по родителям. Проба U12. Файлы: `pkg/cfprovider/index.go`, `pkg/cfprovider/index_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [x] 3.3 CANCEL_FETCH_DATA: флаг отмены и ответ со статусом отмены. Файлы: `pkg/cfapi/callbacks.go`, `pkg/cfprovider/fetch.go`, `pkg/cfprovider/fetch_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [x] 3.4 Ошибки backend: ответ кодом ошибки, процесс жив; замер усиления чтения — в design.md. Файлы: `pkg/cfprovider/fetch.go`, design.md. Проверка: ручной прогон сценариев «Содержимое совпадает с backend», «Частичная гидратация»; запись замера в design.md.
- [x] 3.5 Замеры гидратации M2 (design.md D11): streaming MB/s при N=1/8/64 параллельных FETCH_DATA на файле ≥1 GiB; число воркеров и глубина очереди фиксируются. Файлы: `pkg/cfprovider/fetch.go` (бенч-обвязка), design.md. Проверка: результаты M2 в design.md; сравнение с сырым S3-GET той же машины.

DoD этапа: сценарии «Содержимое совпадает с backend», «Частичная гидратация», «Ровно один ответ на задание», «Прерванное чтение» пройдены; `go test ./pkg/cfprovider` зелёный; результаты M2 в design.md.

## 4. Pin и unpin

- [x] 4.1 `pin` и `unpin` с `CfSetPinState`, флаг `--recursive`; обёртки `CfOpenFileWithOplock`/`CfRevertPlaceholder` добавляются в `pkg/cfapi/operations.go` на этапе 4/5 по потребности (деферрал из 1.1, замечание ревью P2-1). Проба U5. Файлы: `cmd/clouddrive.go`, `pkg/cfapi/operations.go`. Проверка: `go build ./cmd/...`; ручной прогон сценариев «Pin файла и чтение без провайдера», «Pin каталога», «Unpin».
- [x] 4.2 Ручная проверка встроенного меню Explorer без MSIX; результат — в design.md. Файлы: design.md. Проверка: запись результата U5 в таблице.

DoD этапа: сценарии «Pin файла и чтение без провайдера», «Pin каталога», «Unpin» пройдены.

## 5. Выгрузка новых и изменённых файлов

- [x] 5.1 Сверить write-API `pkg/fs` (`Create`, `Write`, `Flush`, `Fsync`, каталогные операции) в `pkg/fs/fs.go`. Отсутствующий метод добавляется в `pkg/fs` по образцу существующих; решение — в design.md. Файлы: design.md, при необходимости `pkg/fs/fs.go` (+тест в `pkg/fs/`). Проверка: `go test ./pkg/fs`; запись решения в design.md.
- [x] 5.2 Thunk NOTIFY_FILE_CLOSE_COMPLETION и классификация файла. Проба U6; при необходимости — журнал ReadDirectoryChangesW. Файлы: `pkg/cfapi/callbacks.go`, `pkg/cfprovider/ingest.go`. Проверка: `go test ./pkg/cfapi ./pkg/cfprovider`.
- [x] 5.3 Пул выгрузки: чтение с `FILE_SHARE_READ`, запись блоками, `Flush`, преобразование или обновление плейсхолдера. Пробы U7, U8. Файлы: `pkg/cfprovider/ingest.go`, `pkg/cfprovider/ingest_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [x] 5.4 Объединение заданий по пути с задержкой 2 с. Файлы: `pkg/cfprovider/ingest.go`, `pkg/cfprovider/ingest_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [x] 5.5 Write-back выключен; проба U11 по очистке staging. Файлы: `pkg/cfprovider/ingest.go`, design.md. Проверка: сценарий «Видимость версии»; запись результата U11.
- [x] 5.6 Флаг `--read-only`. Файлы: `cmd/clouddrive.go`. Проверка: ручной сценарий «Файл в режиме без записи».
- [x] 5.7 (уточнение отображения FULL-in-sync placeholder — state=NO_STATES честно; STANDARD-запрос при 0 — follow-up 5.8)`status <путь>`: плейсхолдер и состояние in-sync. Файлы: `cmd/clouddrive.go`. Проверка: ручной прогон сценариев «Новый файл», «Изменённый плейсхолдер».
- [ ] 5.8 (follow-up): `status <путь>` для FULL-in-sync placeholder — STANDARD-запрос при state=NO_STATES (сейчас печатается «not a placeholder», 27.11)

DoD этапа: сценарии «Новый файл», «Изменённый плейсхолдер», «Обрыв выгрузки», «Видимость версии», «Файл в режиме без записи» пройдены.

## 6. Удаление, переименование, каталоги, офлайн

- [x] 6.1 Thunk'и NOTIFY_FILE_RENAMED, NOTIFY_FILE_DELETED, NOTIFY_DIRECTORY_CREATED, NOTIFY_DIRECTORY_RENAMED, NOTIFY_DIRECTORY_DELETED. Файлы: `pkg/cfapi/callbacks.go`, `pkg/cfprovider/ingest.go`. Проверка: `go build ./...`. (thunk'и уже были (этап 1); добавлены маршрутизатор ingestRouter для 5 NOTIFY-очередей + обработчики notifyDeletePath/notifyRenamePaths/notifyDirCreate/notifyDirRename; источник «Создание каталога» — запасной механизм U9: watcher подаёт dir-события в DirectoryCreated/Renamed)
- [x] 6.2 Передача операций только для известных плейсхолдеров; sync root никогда. Файлы: `pkg/cfprovider/ingest.go`, `pkg/cfprovider/ingest_test.go`. Проверка: `go test ./pkg/cfprovider`. (knownPlaceholder = inode в индексе по FileID/пути; sync root → Warn one-time, never; unit: TestNotifyDeleteKnownPlaceholder/UnknownIgnored/SyncRootNever)
- [x] 6.3 `Journal`: сохранение, повторы с нарастающей паузой, загрузка при старте. Файлы: `pkg/cfprovider/journal.go`, `pkg/cfprovider/journal_test.go`. Проверка: `go test ./pkg/cfprovider`. (JSONL ops.journal в state-dir; key=type:lower(path) дедуп; FIFO; compaction tmp+rename; replayer 1с→30с на injectable clock; интеграция: неуспешные upload/delete/rename/mkdir этапов 5/6; replace-after-upload busy → upload op; тесты: AddAckReload, ReplayDeleteOffline/UploadOffline на fake clock)
- [x] 6.4 Сверка при старте: очередь выгрузки и очередь удаления. Файлы: `pkg/cfprovider/provider.go`, `pkg/cfprovider/reconcile_test.go`. Проверка: `go test ./pkg/cfprovider`; ручной сценарий «Восстановление после сбоя». (reconcileDir по-каталожно из populateDirectory — каталог processed = «завершённое наполнение»: (а) вне in-sync + plain-файлы без записи backend → очередь выгрузки (коалисер, до его поднятия — журнал); (б) записи индекса без локального файла → delete op журнала; walk не воскрешает известные удалённые (гард index-known в to-create); readOnly off)
- [x] 6.5 Отмена выгрузки при удалении локального файла; удаление по inode, если версия уже подтверждена. Файлы: `pkg/cfprovider/ingest.go`. Проверка: ручной сценарий «Удаление локального файла до выгрузки». (uploadHandle-реестр; REMOVED watcher → handleLocalRemoved: активная → cancel (до Flush — снятие, после — journal delete по inode); нет активной + pending upload op → ReplaceUploadWithDelete; своё событие финализации — окно супрессии; unit: TestUploadCanceledAfterFlushJournalsDelete/CancelBeforeFlushNoJournal/RemovedReplacesPendingUploadWithDelete)
- [ ] 6.6 Замеры метаданных M1 (design.md D11): наполнение плейсхолдерами + readdir каталога backend с 100k записей, ops/s; латентность ответа на дисковый листинг. Файлы: `pkg/cfprovider/placeholders.go` (бенч-обвязка), design.md. Проверка: результаты M1 в design.md.

DoD этапа: сценарии «Удаление плейсхолдера», «Удаление локального файла до выгрузки», «Переименование», «Новый каталог», «Удаление sync root», «Создание офлайн», «Удаление офлайн», «Восстановление после сбоя» пройдены; результаты M1 в design.md.

## 7. Проход DoD

- [ ] 7.1 `go build ./...`, `go vet ./pkg/cfapi/... ./pkg/cfprovider/... ./cmd/...`, `go test ./pkg/cfapi/... ./pkg/cfprovider/...` зелёные.
- [ ] 7.2 `go list -f "{{.CgoFiles}}" ./pkg/cfapi ./pkg/cfprovider` пуст.
- [ ] 7.3 `golangci-lint run ./pkg/cfapi/... ./pkg/cfprovider/... ./cmd/...` по `.golangci.yml` без новых замечаний (в Makefile таргета нет — конфиг в корне репо; если golangci-lint недоступен локально, зафиксировать замену на `go vet` в отчёте change).
- [ ] 7.4 Интеграционные сценарии оформлены тестами с build-тегом `integration`; без тега пропускаются. Файлы: `pkg/cfprovider/integration_test.go`. Проверка: `go test -tags integration ./pkg/cfprovider` на машине с зарегистрированным sync root.
- [ ] 7.5 Десять циклов «запуск — чтение — запись — удаление — остановка»: число горутин и занятых буферов возвращается к исходному.
- [ ] 7.6 Таблица Open Questions U1–U13 в design.md закрыта: для каждой указаны результат пробы и принятое решение.
- [ ] 7.7 Расхождение имён CfAPI со спекой устранено через правку спеки до архивации.
- [ ] 7.8 Профиль GC/heap M3 (design.md D11) во время прогонов 3.5/6.6 и циклов 7.5: размер heap, частота и латентность GC-пауз (`runtime/metrics`), allocation rate hot path после прогрева пулов. Файлы: design.md. Проверка: профили записаны в design.md.
- [ ] 7.9 Отчёт языкового гейта D11 для владельца: сводка M1/M2/M3 против целевых значений (подтверждённых до шага 3), вердикт «Go остаётся / порт в Rust», правка ADR-005. Файлы: design.md, `specs/decisions/ADR-005-cfapi-provider-windows-transport.md`. Проверка: ADR-005 обновлён, вердикт в таблице D11.
- [ ] 7.10 Файлы-плейсхолдеры дегидратированы: чтение любого файла sync root даёт FETCH_DATA в логе, размер на диске ≈ 0 (проверка по всем DoD-сценариям этапа 2/3).

## Definition of Done

- `go build ./...` — зелёный.
- `go vet ./pkg/cfapi/... ./pkg/cfprovider/... ./cmd/...` — чисто.
- `go test ./pkg/cfapi/... ./pkg/cfprovider/...` — зелёный; `go test ./pkg/fs` зелёный, если задача 5.1 меняла `pkg/fs`.
- `golangci-lint run` по затронутым пакетам — без новых замечаний (fallback `go vet` — см. 7.3).
- `go list -f "{{.CgoFiles}}" ./pkg/cfapi ./pkg/cfprovider` — пусто.
- `git diff agio-drive-v2 -- pkg/meta pkg/chunk pkg/object pkg/compress cmd/sts_refresher.go` — пусто; `cmd/render_mount.go` может отличаться ТОЛЬКО вызовом общего хелпера `startNodeSTS` (`cmd/storage_helper.go`, задача 2.1) — провайдеры кредов и refresher без изменений.
- Все сценарии `specs/domain-cfapi-provider/spec.md` пройдены вручную или тестами; таблица U закрыта.
- Результаты замеров M1/M2/M3 (design.md D11) записаны; отчёт языкового гейта передан владельцу, ADR-005 обновлён (7.8–7.9).

## Follow-up (отдельный change `implement-cfapi-drive-integration`)

В этот change не входит: tray, OIDC-логин через agio-login v0.0.12, supervisor по маркеру готовности, автозапуск, инсталлятор.
