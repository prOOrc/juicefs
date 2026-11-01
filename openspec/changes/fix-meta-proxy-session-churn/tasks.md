# Tasks: fix-meta-proxy-session-churn

## 1. Рекавери-модуль

- [ ] 1.1 `pkg/meta/inode_path_recovery.go` (новый): тип `pathResolver` с
  `Resolve(ctx, ino) (path string, resolved bool, gone bool)` + `Invalidate(ino)`.
  Реализация: fast path через кэш; singleflight
  (`golang.org/x/sync/singleflight`, key=ino); подъём по `attr.Parent`
  прямыми чтениями engine (образец — `baseMeta.GetPaths`,
  pkg/meta/base.go:2498-2547), visited-set против циклов, depth cap
  `maxRecoverDepth = 64`; прогрев предков через PutIfAbsent (first-seen-wins);
  ENOENT листа/середины → gone; ошибка engine → unrecoverable. Инъекция в
  `MetaProxyServer` по образцу `HandleResolver`
  (pkg/meta/authz_interceptor.go:66-69).
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./pkg/meta/ -run Recovery`.
- [ ] 1.2 Юнит-тесты `pkg/meta/inode_path_recovery_test.go`: линейная цепочка,
  дерево с общими предками, цикл parent-ссылок, превышение depth, ENOENT,
  ошибка engine, конкурентные miss'ы одного инода (счётчик вызовов
  mock-engine == 1), Invalidate, прогрев предков first-seen-wins.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./pkg/meta/ -run Recovery`.

## 2. Встраивание

- [ ] 2.1 Подтвердить отсутствие authz-рекурсии: интерсептор висит на
  gRPC-пути MetaService; рекавери вызывает engine-методы напрямую (мимо
  интерсептора). Зафиксировать комментарием в recovery-модуле.
- [ ] 2.2 `pkg/meta/authz_interceptor.go`: на cache miss во всех inode-only
  call sites (GetAttr/SetAttr/Truncate/Fallocate/Open/Access/Read/Write/
  xattrs/locks — ветка default resolveChecks :441-450) и для path-bearing
  Lookup (parent miss :304-309) — вызов recovery до fail-closed deny;
  resolved → обычный authz-чек; gone → ENOENT; failure → deny как сейчас.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./pkg/meta/ -run Interceptor`.
- [ ] 2.3 `pkg/meta/grpc_server_fuse.go`: Open-FEK/ResolveFileKey — resolve
  до `path == ""` ветки (:258-261, :310-313).
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./pkg/meta/ -run FEK`.
- [ ] 2.4 Rename/Unlink/Rmdir — пост-колл `resolver.Invalidate(ino)` для
  затронутых инод. Проверка: юнит-тест rename → GetAttr по новому пути.

## 3. Readdir-семантика и клиент

- [ ] 3.1 `pkg/meta/grpc_server_fuse.go` (`filterEntriesByAuthz`, :698-701,
  :723-724) и `pkg/meta/grpc_server_dir_handler.go` (`authzListing`):
  parentPath нерезолвим даже после рекавери → EACCES (errno), не пустой
  листинг; известный путь + 0 авторизованных детей → пустой успех (без
  изменений). Проверка: `go test -run DirHandlerList ./pkg/meta/`.
- [ ] 3.2 Errno-маппинг клиента: `pkg/meta/grpc_client_fuse.go` (:88-90
  Lookup, :145-148 GetAttr) и `pkg/meta/grpc_client_data.go` (:762-764
  NewDirHandler) — через `grpcStatusToErrno`; обновить существующие ассерты
  EIO в тестах. Проверка: `go test -run 'Lookup|GetAttr|DirHandler' ./pkg/meta/`.
- [ ] 3.3 `pkg/vfs` / `pkg/meta/grpc_client_dir_handler.go`: EBADF от
  DirHandler-операции → дроп локального хендла (пересоздание на off==0 уже
  есть в vfs.go:480-504 — убедиться, что EBADF до него доходит).
  Проверка: `go test ./pkg/vfs/ -run DirHandler`.
- [ ] 3.4 Re-Chroot при реконнекте: `pkg/meta/grpc_client.go`
  (`markHubOnline`, :443-452) — при сконфигурированном subdir, до onReconnect
  (replay write journal). Гейт: один subdir на прокси-деплой (design D4).
  Проверка: юнит-тест с mock: offline→online → Chroot отправлен до replay.

## 4. Регрессия и приёмка

- [ ] 4.1 Restart-churn регрессия (расширить существующий набор в
  `pkg/meta/*_test.go`): свежий MetaProxyServer (холодный кэш) поверх живого
  engine + сессия без ремонта → GetAttr/Open/Lookup/Readdir по глубоким
  путям успешны; неавторизованное поддерево → EACCES; vanished inode →
  ENOENT. Проверка: `make test.meta.core`.
- [ ] 4.2 Первый-write-шифрование не регрессирует (existing encrypt-тесты
  зелёные); `go build ./... && go vet ./cmd/ ./pkg/meta/`.

## Definition of Done

- `GOTOOLCHAIN=go1.26.5 go build ./... && go vet ./cmd/ ./pkg/meta/`
- `make test.meta.core` зелёный; `make test.pkg` зелёный (vfs-правки)
- Stage-проверка: рестарт drive-meta-proxy при живом маунте tst-v3 —
  readdir и open работают без перемонтирования.
