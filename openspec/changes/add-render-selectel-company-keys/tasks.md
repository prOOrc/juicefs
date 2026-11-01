# Tasks: add-render-selectel-company-keys

## 1. Platform (agio-platform, ветка feature/add-render-selectel-company-keys платформенного репо)

- [x] 1.1 Миграция `render_node_credential` (id, render_node_id FK, token_hash
  UNIQUE, status, created_at/rotated_at/revoked_at) + admin-мутация/CLI
  выпуска и отзыва токена (CSPRNG 32B, хранится только SHA-256).
  Проверка: `go test ./internal/... -run NodeCredential` (platform).
- [x] 1.2 Proto: `GetNodeStorageCredentials(CompanyID)` в
  `DriveKeyManagerService` + regen; сервис: активная пара из
  CompanyStorageKeyService (расшифровка), volume/bucket из
  facility-конфига; fail-closed (нет строки/revoked/нет facility); аудит
  `get_node_storage_credentials` (actor `render_node:<id>`); TLS-only.
  Проверка: `go test ./application/authz/... -run GetNodeStorageCredentials`.
- [x] 1.3 Интерцептор: режим `node_auth` (`yc-iam` | `node-token`) —
  `node-token` проверяет `x-node-credential` по хэшу БД, кладёт
  `render_node:<id>` в контекст; guard'ы `FetchCompanyKEK` +
  `GetNodeStorageCredentials`; режим без fallback (fail-closed при
  неверной конфигурации).
  Проверка: `go test ./api/... -run IAMInterceptor`.
- [x] 1.4 Отзыв: инвалидация строки → оба RPC отвечают Unauthenticated;
  тест отзыва.
  Проверка: `go test -run NodeCredentialRevoked ./...`.

## 2. Fork (этот репозиторий, worktree)

- [x] 2.1 `pkg/meta/keymanager_pb`: regen под новый RPC; `keymanager_client.go`
  — `GetNodeStorageCredentials` (+ юнит-тест на моке).
  Проверка: `GOTOOLCHAIN=go1.26.5 go build ./... && go test ./pkg/meta/ -run KeyManager`.
- [x] 2.2 `cmd/render_mount.go`: флаг `--node-credential-file`; bootstrap =
  `ctxWithNodeCredential` → `FetchCompanyKEK` → `GetNodeStorageCredentials`;
  полученная пара → static storage-провайдер, volume/bucket из ответа
  (приоритет над Format); `--iam-token-file`/STS-провайдеры сохраняются для
  yc/aws-режимов (выбор по флагам, без автодетекта).
  Проверка: `go test ./cmd/ -run RenderMount`.
- [x] 2.3 Интеграция `TestRenderSelectelBootstrap` (fake KeyManager: KEK +
  пара → render-mount пишет и читает файл компании, chroot-граница держится).
  Проверка: `GOTOOLCHAIN=go1.26.5 go test ./pkg/meta/ -run TestRenderSelectelBootstrap` (локальный Redis).

## 3. Приёмка

- [ ] 3.1 Stage: деплой platform в режиме `node-token`, выпуск токена
  тест-ноде, render-mount на tst-v2 (компания tst-v3) — запись/чтение
  зашифрованного файла, чужая компания → EIO, отзыв токена → bootstrap
  Unauthenticated. Свидетельство в tasks.
- [ ] 3.2 Доки: platform `docs/ops/` (выпуск/ротация/отзыв токенов нод),
  fork render-mount usage.

## Definition of Done

- Platform: `go build ./... && go vet ./...` зелёные; юнит/интеграционные
  тесты пунктов 1.x зелёные.
- Fork: `GOTOOLCHAIN=go1.26.5 go build ./... && go vet ./cmd/ ./pkg/meta/`;
  `make test.meta.core` зелёный (внешне-сервисный набор — не в счёт).
- Stage-приёмка 3.1 выполнена; YC-режим (регресс) не задет.
