# Tasks: fix-meta-proxy-per-session-chroot (v2)

> Пути — относительно корня worktree форка (база `agio-drive-v2` @`2d672403c`).
> Файлы сервера прокси лежат рядом с `grpc_server.go`; полные каталоги — по v1.

## 0. Verification (Stage 0 — закрыт)

- [x] 0.1 Разбор движка (A3): `CloseSession` удаляет запись сессии из redis, отменяет
      sessCtx, ждёт sessWG; сетевые ресурсы закрывает только `Shutdown()`; teardown =
      `CloseSession` + `Shutdown`. Факты — design, A3.
- [x] 0.2 Двойное path-пространство под chroot (A5): подтверждено; фиксы — Stage 4.
- [x] 0.3 База adv2 и клиент: `x-session-id` игнорируется, `NewSession` один раз,
      `reChroot` до replay; номера строк зоны на adv2 не сдвинулись; service identity
      ранний выход `authz_interceptor.go:132`.

## 1. Stage 1 — пул движков и сессии

- [x] 1.1 Создать `grpc_server_session.go`: `EngineFactory`, `poolEntry`, `enginePool`
      (`newEnginePool`, `acquire`, `settle`, `create`, `discard`, `release`,
      `teardown`, `shutdown`), константы `engineCreateTimeout`, `engineCloseTimeout`,
      хелперы `normalizeSubdir`, `displaySubdir`, `lookupSubdir`, `chrootRejectReason`.
      Критерий: `go build ./...`.
- [x] 1.2 Там же: `proxySession`, `sessionRegistry`, `sessionStatsHandler`
      (`TagConn` → `openSession`; `HandleConn(*stats.ConnEnd)` → `endSession`),
      `sessionUnaryInterceptor`, `sessionStreamInterceptor`, `sessionFor`,
      `sessionFromContext`, `bind`, `endSession`, `rootPath`.
- [x] 1.3 В `grpc_server.go`: поля `pool`, `sessions`, `maxSessions`, `authzEnabled`;
      методы `engineFor`, `sessionEntry`, `sessionEntryFor`, `CloseSessions`. Поле
      `meta` пока остаётся (снимается в 2.2), чтобы сборка была зелёной.
- [x] 1.4 Собрать цепочку: session interceptor первым, `grpc.StatsHandler` —
      `sessionStatsHandler`. Критерий: `go build ./... && go vet ./...`.
- [x] 1.5 Тесты на fake-движке (реальный TCP-листенер `127.0.0.1:0`, не bufconn):
      `TestMetaProxySessionLifecycleOnConnection`, `TestMetaProxyNoSessionFailsClosed`,
      `TestMetaProxySessionCleanupOnConnEnd`, `TestMetaProxyMaxSessions`,
      `TestMetaProxyEngineRefcountTeardown`, `TestMetaProxyEngineSharedPerSubdir`,
      `TestMetaProxyEngineReconnectKeepsEngine`,
      `TestMetaProxyEngineReacquireAfterTeardown`, `TestMetaProxyEngineBoundedBySessions`,
      `TestMetaProxyEnginePoolGracefulShutdown`.
      Критерий: `go test -race ./pkg/meta/ -run 'TestMetaProxyEngine|TestMetaProxySession|TestMetaProxyMaxSessions|TestMetaProxyNoSession'`.

## 2. Stage 2 — маршрутизация, cmd, тест-харнесс

- [x] 2.1 Заменить во всех обработчиках обращения к `s.meta` на `s.engineFor(ctx)`:
      `grpc_server.go`, `grpc_server_admin.go`, `grpc_server_fuse.go`,
      `grpc_server_dir_handler.go`. `pathResolver` остаётся на базовом движке.
- [x] 2.2 Удалить поле `meta` из `MetaProxyServer`; `NewMetaProxyServer` получает
      фабрику третьим аргументом. В `cmd/meta_proxy.go`: фабрика с `Load(true)`, флаг
      `--max-sessions` (default 256, `0` — без лимита), `CloseSessions(ctx)` в
      graceful-пути (`cmd/meta_proxy.go:396-411`) до `Shutdown` базового движка.
- [x] 2.3 Тест-харнесс: фабрика с общим счётчиком созданий и fake-движком
      (записывает вызовы `Chroot`/`NewSession`/`CloseSession`/`Shutdown`/`Lookup`;
      встраивает `Engine`, остальные методы — nil). Отдельный харнесс с реальными
      движками для изоляции (нужен redis, как прочим `TestMeta*`).
      Критерий: `! grep -nE '\bs\.meta\.' pkg/meta/grpc_server*.go` (0 совпадений;
      обращение к базовому движку — только в `pathResolver`).

## 3. Stage 3 — Chroot

- [x] 3.1 Обработчик `Chroot` в `grpc_server_admin.go` по design (Key Code):
      нормализация, `chrootMu`, идемпотентность, pre-check, `acquire(key, noCreate)`,
      `finishChroot`, `chroot applied`/`chroot rejected`.
- [x] 3.2 `checkChrootTarget` на движке `""`; `chrootLogUser` (`sub` | `service` |
      `-`); `oidc.IsServiceIdentity` — импорт как в `authz_interceptor.go`.
- [x] 3.3 `TestMetaProxyChrootIdempotency`, `TestMetaProxyChrootAuthz`,
      `TestMetaProxyChrootServiceIdentity`, `TestRequiredPermissionChroot`,
      `TestMetaProxyPerSessionChrootIsolation` (реальные движки).
      Критерий: `go test -race ./pkg/meta/ -run 'TestMetaProxyChroot|TestRequiredPermission|TestMetaProxyPerSession'`.

## 4. Stage 4 — пути authz

- [x] 4.1 `inode_path_cache.go`: `rootPathFromContext`; ctx первым параметром в
      `InodePathCache.Get` и `BuildChildPath`; кеш хранит только полные пути тома.
- [x] 4.2 Обновить 23 call-site: `authz_interceptor.go`, `grpc_server_fuse.go`,
      `grpc_server_dir_handler.go`, `grpc_server_admin.go` (включая `Clone`).
- [x] 4.3 `TestMetaProxyAuthzPathsUnderChroot` + unit-тест кеша на корень сессии.
      Критерий: `go build ./... && go test -race ./pkg/meta/ -run 'TestMetaProxyAuthzPaths|TestInodePath'`.

## 5. Stage 5 — dir-хендлы и lifecycle

- [x] 5.1 `grpc_server_dir_handler.go`: `dirHandlerEntry` хранит ссылку на сессию;
      чужая сессия → `EBADF`; `endSession` закрывает хендлы сессии (убирает утечку
      `s.handlers`, `:150-159`).
- [x] 5.2 `newEngineSession`, `fixedSessionArgs` (по маппингу текущего обработчика
      `NewSession`); `poolEntry` хранит `newSessResp`; RPC `NewSession` возвращает
      клон сохранённого ответа; RPC `CloseSession` завершает только сессию
      отправителя.
- [x] 5.3 `TestMetaProxyDirHandleSessionBinding`,
      `TestMetaProxyNewSessionIdempotentPerEngine`,
      `TestMetaProxyCloseSessionRPCReleasesOnlyCaller`.

## 6. Stage 6 — логи

- [x] 6.1 События пула: `meta-proxy engine created|create failed|closed subdir=...`.
- [x] 6.2 `authz_interceptor.go`: три `Debugf` (`:172`, `:180`, `:195`) → `Warnf`
      `authz denied user= method= path= perm=`; успехи остаются `Tracef`;
      `RemovePassword` не трогать.
- [x] 6.3 `TestMetaProxySessionAuditLogs`, `TestAuthzDenyLoggedAtWarn`.
      Критерий: `grep -n 'authz denied' pkg/meta/authz_interceptor.go` — три
      совпадения, все `Warnf`.

## 7. Stage 7 — финал

- [x] 7.1 grep-контракты:
      `grep -n 'max-sessions' cmd/meta_proxy.go`;
      `grep -n 'meta-proxy engine \(created\|closed\|create failed\)' pkg/meta/grpc_server_session.go`;
      `grep -n 'meta-proxy chroot \(applied\|rejected\)' pkg/meta/grpc_server_admin.go`;
      `grep -n 'Chroot(' pkg/meta/grpc_server*.go` — единственный вызов движка в
      `enginePool.create`.
- [x] 7.2 `SKIP_NON_CORE=true go test -race -count=1 ./pkg/meta/ -run
      'TestMetaProxy|TestAuthzDeny|TestRequiredPermission|TestInodePath'` — зелёные
      (52/52, DATA RACE в нашем коде нет). Полный race-прогон пакета отдельным гейтом
      на этой линии не достигается вне скоупа: pre-existing DATA RACE ×8 в dump-пути
      (`load_dump_test.go`/`tkv.go`/`dump.go`, роняет `TestLoadDump_MemKV`),
      rapid-тест `TestFSOps` не укладывается в таймаут под race, 3 интеграционных
      FAIL от proto-паники minio-replace @2d672403c — всё эскалировано владельцу
      отдельно.
- [x] 7.3 `openspec validate fix-meta-proxy-per-session-chroot --strict`.
- [x] 7.4 Адаптировать существующие тесты метапрокси, сломанные fail-closed по I1
      (dir_handler_test `newTestHandle`, fuse/fek transport, authz_op, posix и др.):
      через сессионный харнесс (`grpc_server_session_test.go`) или прямой ctx с
      сессией. Критерий: полный `SKIP_NON_CORE=true go test -count=1 ./pkg/meta/`
      зелёный (локальный redis).

## Definition of Done

- `go build ./...`, `go vet ./...` — зелёные.
- `make test.meta.core` (= `SKIP_NON_CORE=true go test ./pkg/meta/...`) — зелёный
  (нужен локальный redis на 6379).
- `golangci-lint run` — зелёный (Makefile-таргета lint в форке нет; CI —
  golangci-lint-action v7/v2.6, конфиг `.golangci.yml`).
- `go test -race ./pkg/meta/... -run 'TestMetaProxy|TestAuthzDeny|TestRequiredPermission|TestInodePath'` — зелёный.
- `openspec validate fix-meta-proxy-per-session-chroot --strict` — без замечаний.
