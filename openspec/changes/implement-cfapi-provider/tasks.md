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
- [ ] 2.3 Наполнение верхнего уровня и отложенное наполнение каталогов. Проба U2; при отрицательном результате — запасной механизм из design.md (D6). Файлы: `pkg/cfprovider/placeholders.go`. Проверка: ручной `dir` по сценарию «Верхний уровень».
- [ ] 2.4 Конфликт одноимённого локального файла: пропуск и запись в лог. Файлы: `pkg/cfprovider/placeholders.go`, `pkg/cfprovider/placeholders_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [ ] 2.5 Маркер готовности `OK, <имя тома> is ready`. Файлы: `pkg/cfprovider/provider.go`. Проверка: ручной прогон сценариев «Готовность», «Hub недоступен».
- [ ] 2.6 `cmd/clouddrive.go`: `ls`, `hash`. Файлы: `cmd/clouddrive.go`. Проверка: `go build ./cmd/...`; ручной `ls` по сценарию «Дочерний каталог».

DoD этапа: сценарии «Верхний уровень», «Дочерний каталог», «Перезапуск без дублей», «Маркер готовности», «Одноимённый локальный файл» пройдены.

## 3. Гидратация и отмена

- [ ] 3.1 Пул гидратации: очередь, буферы, `CfExecute(TRANSFER_DATA)`. Пробы U3, U4, U13. Файлы: `pkg/cfprovider/fetch.go`, `pkg/cfapi/operations.go`, `pkg/cfprovider/fetch_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [ ] 3.2 Разрешение inode → путь с подъёмом по родителям. Проба U12. Файлы: `pkg/cfprovider/index.go`, `pkg/cfprovider/index_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [ ] 3.3 CANCEL_FETCH_DATA: флаг отмены и ответ со статусом отмены. Файлы: `pkg/cfapi/callbacks.go`, `pkg/cfprovider/fetch.go`, `pkg/cfprovider/fetch_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [ ] 3.4 Ошибки backend: ответ кодом ошибки, процесс жив; замер усиления чтения — в design.md. Файлы: `pkg/cfprovider/fetch.go`, design.md. Проверка: ручной прогон сценариев «Содержимое совпадает с backend», «Частичная гидратация»; запись замера в design.md.
- [ ] 3.5 Замеры гидратации M2 (design.md D11): streaming MB/s при N=1/8/64 параллельных FETCH_DATA на файле ≥1 GiB; число воркеров и глубина очереди фиксируются. Файлы: `pkg/cfprovider/fetch.go` (бенч-обвязка), design.md. Проверка: результаты M2 в design.md; сравнение с сырым S3-GET той же машины.

DoD этапа: сценарии «Содержимое совпадает с backend», «Частичная гидратация», «Ровно один ответ на задание», «Прерванное чтение» пройдены; `go test ./pkg/cfprovider` зелёный; результаты M2 в design.md.

## 4. Pin и unpin

- [ ] 4.1 `pin` и `unpin` с `CfSetPinState`, флаг `--recursive`; обёртки `CfOpenFileWithOplock`/`CfRevertPlaceholder` добавляются в `pkg/cfapi/operations.go` на этапе 4/5 по потребности (деферрал из 1.1, замечание ревью P2-1). Проба U5. Файлы: `cmd/clouddrive.go`, `pkg/cfapi/operations.go`. Проверка: `go build ./cmd/...`; ручной прогон сценариев «Pin файла и чтение без провайдера», «Pin каталога», «Unpin».
- [ ] 4.2 Ручная проверка встроенного меню Explorer без MSIX; результат — в design.md. Файлы: design.md. Проверка: запись результата U5 в таблице.

DoD этапа: сценарии «Pin файла и чтение без провайдера», «Pin каталога», «Unpin» пройдены.

## 5. Выгрузка новых и изменённых файлов

- [ ] 5.1 Сверить write-API `pkg/fs` (`Create`, `Write`, `Flush`, `Fsync`, каталогные операции) в `pkg/fs/fs.go`. Отсутствующий метод добавляется в `pkg/fs` по образцу существующих; решение — в design.md. Файлы: design.md, при необходимости `pkg/fs/fs.go` (+тест в `pkg/fs/`). Проверка: `go test ./pkg/fs`; запись решения в design.md.
- [ ] 5.2 Thunk NOTIFY_FILE_CLOSE_COMPLETION и классификация файла. Проба U6; при необходимости — журнал ReadDirectoryChangesW. Файлы: `pkg/cfapi/callbacks.go`, `pkg/cfprovider/ingest.go`. Проверка: `go test ./pkg/cfapi ./pkg/cfprovider`.
- [ ] 5.3 Пул выгрузки: чтение с `FILE_SHARE_READ`, запись блоками, `Flush`, преобразование или обновление плейсхолдера. Пробы U7, U8. Файлы: `pkg/cfprovider/ingest.go`, `pkg/cfprovider/ingest_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [ ] 5.4 Объединение заданий по пути с задержкой 2 с. Файлы: `pkg/cfprovider/ingest.go`, `pkg/cfprovider/ingest_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [ ] 5.5 Write-back выключен; проба U11 по очистке staging. Файлы: `pkg/cfprovider/ingest.go`, design.md. Проверка: сценарий «Видимость версии»; запись результата U11.
- [ ] 5.6 Флаг `--read-only`. Файлы: `cmd/clouddrive.go`. Проверка: ручной сценарий «Файл в режиме без записи».
- [ ] 5.7 `status <путь>`: плейсхолдер и состояние in-sync. Файлы: `cmd/clouddrive.go`. Проверка: ручной прогон сценариев «Новый файл», «Изменённый плейсхолдер».

DoD этапа: сценарии «Новый файл», «Изменённый плейсхолдер», «Обрыв выгрузки», «Видимость версии», «Файл в режиме без записи» пройдены.

## 6. Удаление, переименование, каталоги, офлайн

- [ ] 6.1 Thunk'и NOTIFY_FILE_RENAMED, NOTIFY_FILE_DELETED, NOTIFY_DIRECTORY_CREATED, NOTIFY_DIRECTORY_RENAMED, NOTIFY_DIRECTORY_DELETED. Файлы: `pkg/cfapi/callbacks.go`, `pkg/cfprovider/ingest.go`. Проверка: `go build ./...`.
- [ ] 6.2 Передача операций только для известных плейсхолдеров; sync root никогда. Файлы: `pkg/cfprovider/ingest.go`, `pkg/cfprovider/ingest_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [ ] 6.3 `Journal`: сохранение, повторы с нарастающей паузой, загрузка при старте. Файлы: `pkg/cfprovider/journal.go`, `pkg/cfprovider/journal_test.go`. Проверка: `go test ./pkg/cfprovider`.
- [ ] 6.4 Сверка при старте: очередь выгрузки и очередь удаления. Файлы: `pkg/cfprovider/provider.go`, `pkg/cfprovider/reconcile_test.go`. Проверка: `go test ./pkg/cfprovider`; ручной сценарий «Восстановление после сбоя».
- [ ] 6.5 Отмена выгрузки при удалении локального файла; удаление по inode, если версия уже подтверждена. Файлы: `pkg/cfprovider/ingest.go`. Проверка: ручной сценарий «Удаление локального файла до выгрузки».
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

## Definition of Done

- `go build ./...` — зелёный.
- `go vet ./pkg/cfapi/... ./pkg/cfprovider/... ./cmd/...` — чисто.
- `go test ./pkg/cfapi/... ./pkg/cfprovider/...` — зелёный; `go test ./pkg/fs` зелёный, если задача 5.1 меняла `pkg/fs`.
- `golangci-lint run` по затронутым пакетам — без новых замечаний (fallback `go vet` — см. 7.3).
- `go list -f "{{.CgoFiles}}" ./pkg/cfapi ./pkg/cfprovider` — пусто.
- `git diff agio-drive-v2 -- pkg/meta pkg/chunk pkg/object pkg/compress cmd/sts_refresher.go cmd/render_mount.go` — пусто.
- Все сценарии `specs/domain-cfapi-provider/spec.md` пройдены вручную или тестами; таблица U закрыта.
- Результаты замеров M1/M2/M3 (design.md D11) записаны; отчёт языкового гейта передан владельцу, ADR-005 обновлён (7.8–7.9).

## Follow-up (отдельный change `implement-cfapi-drive-integration`)

В этот change не входит: tray, OIDC-логин через agio-login v0.0.12, supervisor по маркеру готовности, автозапуск, инсталлятор.
