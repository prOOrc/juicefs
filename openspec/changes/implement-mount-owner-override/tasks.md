# implement-mount-owner-override — Tasks

Проверки по умолчанию: `go build ./...`; тесты затронутых пакетов — `go test ./pkg/vfs/... ./pkg/fuse/... ./cmd/...`; статика — `go vet ./pkg/vfs/... ./pkg/fuse/... ./cmd/...` + `golangci-lint run pkg/vfs/... pkg/fuse/... cmd/...`.

## 1. Конфиг и CLI-флаг

- [x] 1.1 `pkg/vfs/vfs.go`: добавить в `vfs.Config` поле `OwnerOverride *AnonymousAccount` (рядом с `AllSquash` :144, `json:",omitempty"`). Проверка: `go build ./pkg/vfs/...`.
- [x] 1.2 `cmd/mount_unix.go`: регистрация флага `--owner-override` (string, пустое значение = uid:gid процесса) в flagset рядом с `--all-squash`; парсинг в блоке :1161-1172 через существующий `parseUIDGID` с фолбэком `os.Getuid()/os.Getgid()`, лог `Map presented ownership to %d/%d`. Проверка: `go build ./cmd/...`; `./juicefs mount --help | grep owner-override`. Форма пустого значения — `--owner-override=` (urfave/cli StringFlag не допускает голой формы).
- [x] 1.3 Windows-flagset (`implement-windows-mount`): тот же флаг; `buildServiceCmdLine` переносит его в сервисную cmdline. Проверка: `go build ./cmd/...` (GOOS=windows, CGO по правилам windows-фазы); существующий тест `buildServiceCmdLine` расширен кейсом флага — `go test ./cmd/... -run TestBuildServiceCmdLine` (на darwin/linux — кросс-компиляционный юнит). Реализовано в worktree `implement-windows-mount` (флаг в `cmd/mount_windows.go`, тест `TestBuildServiceCmdLineOwnerOverride`, полная mingw-сборка windows OK); на agio-drive-v2 флаг добавлен в `cmd/mount_windows.go` (принимается и игнорируется).

## 2. Override в путях атрибутов

- [x] 2.1 `pkg/vfs/vfs.go`: хелпер `func (v *VFS) applyOwnerOverride(attr *meta.Attr)` (nil-чек `v.Conf.OwnerOverride`); вызов в `VFS.GetAttr` :213 после успешного `v.Meta.GetAttr` и в `VFS.Lookup` :183 после успешного `v.Meta.Lookup` — до возврата `*meta.Entry`. Спец-узлы (`IsSpecialNode`/`getInternalNode`) переопределяются наравне (D6). Проверка: `go build ./pkg/vfs/...`; `go vet ./pkg/vfs/...`. Отклонение от буквы задачи, найденное при ревью: хелпер возвращает КОПИЮ (attr спец-узлов — общие указатели internalNodes, мутация на месте запрещена); результат присваивается в точках вызова.
- [x] 2.2 `pkg/fuse/fuse.go` readdir-путь :404 (`AddDirLookupEntry`): пинуть, несут ли dirents `Attr`; если да — применить тот же override к атрибутам записей (переиспользуя хелпер через экспортируемую обёртку VFS), если attr-less — зафиксировать в коде комментарием, что readdir покрыт `VFS.GetAttr`. Проверка: `go build ./pkg/fuse/...`. Итог: dirents несут `Attr` (`Attr.Full` → `replyEntry`), override применён в источнике `VFS.Readdir`; в `replyAttr` refresh-ветке (`ModifiedSince`) override ре-апплаится (регресс-тест на поток «записал → ls -l», red→green).

## 3. Тесты

- [x] 3.1 `pkg/vfs`: тест `TestGetAttrOwnerOverride` — при заданном `Conf.OwnerOverride` `VFS.GetAttr`/`VFS.Lookup` возвращают override-uid/gid для файлов, каталогов и спец-узла `.stats`; stored-атрибуты в meta-заглушке неизменны; без флага — поведение идентично (инвариант 4). Использовать существующий тестовый meta-движок пакета (in-memory/meta-тестовый харнесс форка). Проверка: `go test ./pkg/vfs/ -run TestGetAttrOwnerOverride`. Файл: `pkg/vfs/owner_override_test.go`.
- [x] 3.2 `pkg/fuse`: тест `TestOwnerOverrideLookupConsistency` — согласованность uid/gid между Lookup и GetAttr одного inode и атрибутами readdir-листинга при включённом флаге (с включёнными `attr-cache`/`dir-entry-cache`). Проверка: `go test ./pkg/fuse/ -run TestOwnerOverrideLookupConsistency` (linux/darwin, FUSE-харнесс пакета). Файл: `pkg/fuse/owner_override_test.go`; хендлеры бриджа вызываются напрямую с синтетическими запросами (без kernel-FUSE — pkg/fuse/fuse_test.go имеет `//go:build linux`), плюс регресс refresh-ветки ModifiedSince.
- [x] 3.3 Регресс squash-композиции: тест-кейс «all-squash 1000:1000 + owner-override 2000:2000» — операции от 1000, атрибуты 2000; изменение override не влияет на идентичность операций (инвариант 6). Проверка: `go test ./pkg/vfs/ ./pkg/fuse/`. Тест `TestOwnerOverrideSquashComposition` в `pkg/vfs/owner_override_test.go`.

## 4. Приёмка и документация

- [ ] 4.1 Приёмка headless-Linux (itx.lan, сценарий 4.4 agio-drive): маунт encrypted-тома с `--owner-override` клиентом uid ≠ stored-uid БЕЗ chmod каталога — создание файла, md5-чтение с второго клиента; хранить артефакты прогона в tasks agio-drive `implement-juicefs-mount` (4.4/6.x-хвост). Проверка: запись в каталог `0755` со stored-uid владельцем завершается успешно.
- [ ] 4.2 Регрессия macOS-стенда agio-drive: маунт с флагом при совпадающем uid — поведение/листинг идентичны, супервизор, offline-сценарии не затронуты. Проверка: `curl /stats` онлайн, файловые циклы.
- [ ] 4.3 Обновить запись задачи 4.4 в agio-drive `implement-juicefs-mount` (design Open Questions «UID-mismatch») ссылкой на закрытие; синхронизировать `specs/index.md` новой capability `mount-ownership` при архивации change. Проверка: `openspec validate implement-mount-owner-override`.

## Definition of Done

- `go build ./...` — зелёный.
- `go vet ./pkg/vfs/... ./pkg/fuse/... ./cmd/...` — чисто.
- `go test ./pkg/vfs/... ./pkg/fuse/... ./cmd/...` — зелёный (включая новые 3.1-3.3 и регресс `buildServiceCmdLine`).
- `golangci-lint run pkg/vfs/... pkg/fuse/... cmd/...` — 0 замечаний в затронутых пакетах.
- `openspec validate implement-mount-owner-override` — валиден.
- Приёмки 4.1/4.2 зафиксированы с артефактами (md5, выводы листингов) в tasks agio-drive.
