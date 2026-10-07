# implement-proxy-skip-posix-checks — Tasks

Проверки по умолчанию: `go build ./...`; тесты — `go test ./pkg/meta/... ./cmd/...`; статика — `go vet ./pkg/meta/... ./cmd/...` + `golangci-lint run pkg/meta/... cmd/...`.

## 1. Контекст и состояние сервера

- [x] 1.1 `pkg/meta/context.go`: добавить конструктор контекста с отключённой проверкой прав (например, `WrapWithCancelSkipPermCheck(ctx, pid, uid, gids)` или тип-обёртка над `wrapContext`, переопределяющий `CheckPermission() → false`); существующие конструкторы не менять. Проверка: `go build ./pkg/meta/...`.
- [x] 1.2 `pkg/meta/grpc_server.go`: поле `authzMode bool` в `MetaProxyServer`; сеттер (рядом с `SetAuthzInterceptor` :89, например `SetAuthzMode(bool)` или параметр `NewMetaProxyServer`); `metaCtx` :136 — при `s.authzMode` строить контекст из 1.1. Проверка: `go build ./pkg/meta/...`; `go vet ./pkg/meta/...`.
- [x] 1.3 `cmd/meta_proxy.go`: выставлять режим после успешного создания authz-клиента (блок :272-285), лог «authz mode: engine-level POSIX access checks disabled». Проверка: `go build ./cmd/...`; ручной smoke `meta_proxy --help`.

## 2. Тесты

- [x] 2.1 `pkg/meta`: `TestMetaCtxCheckPermissionAuthzMode` — сервер с `authzMode=true`: `metaCtx(req.Ctx).CheckPermission()==false` (все поля MetaContext, включая нулевой → Background-ветка остаётся консистентной); `authzMode=false` → `true`. Проверка: `go test ./pkg/meta/ -run TestMetaCtxCheckPermissionAuthzMode`.
- [x] 2.2 `pkg/meta`: `TestAccessCheckPermissionGate` (base-уровень, движко-агностично) — `baseMeta.Access` при `CheckPermission()==false` возвращает 0 без обращения к движку (stub/harness base-тестов); `uid==0` bypass и ACL-ветка без изменений. Проверка: `go test ./pkg/meta/ -run TestAccessCheckPermissionGate`.
- [x] 2.3 Интеграционный тест прокси (харнесс `grpc_server_fuse_test.go`): Mknod-путь в authz-режиме с carried uid ≠ stored-владельца `0755`-каталога — создаёт файл (stored uid = carried); deny-путь authz (fail-closed) не изменён; без authz-режима — EACCES как раньше (сценарии дельты). Проверка: `go test ./pkg/meta/ -run TestProxyMknodAuthzModeSkipsPosix` — PASS (реальный redisMeta, db 15: EACCES без authz-режима; create + stored carried-uid с authz-режимом).
- [x] 2.4 Регресс base-тестов движков (parity): `make test.meta.core` (или эквивалентный общий прогон base_test.go по Redis/SQL/KV) — зелёный без правок движков. Проверка: `SKIP_NON_CORE=true go test ./pkg/meta/...` — 199 PASS; FAIL только env-зависимые `TestMySQLClient`/`TestTiKVClient`/`TestLoadDump` (локальных MySQL 3306 / TiKV 2379 нет — не регресс, фиксируется и до change).

## 3. Приёмка и выкат

- [x] 3.1 End-to-end на itx.lan (повтор сценария 4.1 `implement-mount-owner-override`): собрать прокси-образ из этого change, выкатить в stage (`drive-meta-proxy-tst-enc-a`, agio-cloud CI — образ только, чарт без правок), клиент Linux с bare `--owner-override=` uid ≠ stored-uid — запись в stored-`0755` каталог успешна, md5 кросс-читается с Mac; client-side meta-слои и презентация без изменений. Проверка: create+md5 roundtrip; в логах authz-gRPC CheckPermission по пути присутствует (отказов нет).
  **Статус (2026-10-06 ночь)**: выкат выполнен — образ `cr.yandex/crpj6qcvss1hbegtfb01/juicedata/juicefs:agio-v1.4.1_2026-10-06a_b15eb902` (собран с `--platform linux/amd64 --provenance=false --sbom=false`, пуш digest `sha256:26cebd13…`), Dockerfile-пин agio-cloud → `b15eb902`, values stage tst-enc-a обновлены; `kubectl set image` (контейнер `meta-proxy`) → rollout OK, в логах пода `authz mode: engine-level POSIX access checks disabled`. **Mac-часть проверки PASS**: регресс записи matching-uid (502) OK; сценарий «stored-владелец ≠ caller» подготовлен, но chown-existing на Mac невозможен (не-root) — положительная нога с РЕАЛЬНЫМ чужим uid ждёт itx.lan (хост ушёл из сети в момент прогона, автоповтор запущен).
  - **ФИНАЛ (2026-10-07, itx.lan вернулся; бинарь fc66b75b, прокси b15eb902)**: маунт с bare `--owner-override=` («Map presented ownership to 1000/1000»), запись 1 MiB в stored-`0755` каталог владельца 502 — **УСПЕШНА без композиции** (сценарий, падавший до change с EACCES); md5 `9ed6f0db…` совпал при чтении с Mac (полный FEK-цикл, второй клиент); в логах authz-gRPC CheckPermission по созданному файлу (EDIT/VIEW/READ, allowed) — authz-интерцептор подтверждён как единственный гейт. Сценарий спеки `mount-ownership` «Клиент с чужим stored-uid пишет» закрыт end-to-end; задача 4.1 change `implement-mount-owner-override` закрыта.
- [x] 3.2 Отрицательный контроль на stage: пользователь без прав (ревокация founder, сценарий 8.2 agio-drive) — запись по-прежнему PermissionDenied (authz гейтит), при carried uid=0 — тоже. Проверка: EACCES/PermissionDenied в выводе клиента, allow=false в логах authz.
  **Статус (2026-10-06 ночь): PASS (с Mac, uid 502, против нового прокси)** — ревокация founder (zed delete) → запись EIO/отказ; возврат founder → запись OK; регресс matching-uid между фазами OK. uid=0-ветка покрыта юнит-гейтом (base.go:1462) и не зависит от change.
- [ ] 3.3 Документация: обновить статус 4.1 в tasks `implement-mount-owner-override` (закрыт end-to-end), OQ «UID-mismatch» в agio-drive design, заметку в tasks agio-drive 4.4; agio-cloud values-комментарий по решению OQ design. Проверка: `openspec validate` обоих change.

## Definition of Done

- `go build ./...` — зелёный.
- `go vet ./pkg/meta/... ./cmd/...` — чисто.
- `go test ./pkg/meta/... ./cmd/...` — зелёный (новые 2.1-2.3 + регресс base/движков 2.4; предсуществующий env-тест `cmd/integration_test.go` вне скоупа — как в `implement-mount-owner-override`).
- `golangci-lint run pkg/meta/... cmd/...` — 0 новых замечаний.
- `openspec validate implement-proxy-skip-posix-checks` — валиден.
- Приёмки 3.1/3.2 зафиксированы с артефактами в tasks agio-drive `implement-juicefs-mount`.
