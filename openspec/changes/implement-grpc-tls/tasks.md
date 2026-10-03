# Tasks: implement-grpc-tls

Контракт — delta-спека `specs/domain-meta-proxy/spec.md` этого change; решения D1–D5 и план миграции — design.md. Порядок: 1 → 2 → 3. Коммиты: префикс `grpc-tls:` по логическим шагам; новый `.go` файл — с Apache 2.0 header; `go fmt` перед коммитом.

## 1. Transport TLS (клиент + сервер)

- [ ] 1.1 Клиент: парсинг query-параметров `tls`, `tls-ca`, `tls-server-name` в `newGRPCMeta` (рядом с блоком `oidc-*`) и условный dial — при `tls=1` строится `*tls.Config` (RootCAs из `tls-ca` или системный пул, `ServerName` из `tls-server-name`, MinVersion TLS 1.2) и `grpc.WithTransportCredentials(credentials.NewTLS(cfg))`; без `tls=1` — `grpc.WithInsecure()` как сейчас. Ошибка чтения `tls-ca` — fail-fast при создании клиента. Файлы: `pkg/meta/grpc_client.go`. Проверка: `go test ./pkg/meta/... -run 'TestGRPCMeta'`.
- [ ] 1.2 Клиент: unit-тесты парсинга и выбора creds — plaintext по умолчанию; `tls=1` без `tls-ca` (системный пул); `tls=1&tls-ca=<missing>` — ошибка с именем файла; `tls-server-name` попадает в `tls.Config.ServerName`. Файл: `pkg/meta/grpc_client_test.go`. Проверка: `go test ./pkg/meta/... -run 'TestGRPCMeta'`.
- [ ] 1.3 Сервер: флаги `--tls-cert` / `--tls-key` в `cmd/meta_proxy.go`; при обоих — `grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromFile(...)))` с MinVersion TLS 1.2; при одном из двух — fail-fast с ошибкой, называющей недостающий флаг; без флагов — поведение не меняется. Файл: `cmd/meta_proxy.go`. Проверка: `go build ./cmd/...`; unit-тест валидации пары флагов (`go test ./cmd/... -run TestMetaProxyTLS`, при отсутствии тестовой инфраструктуры флагов в cmd — проверить интеграционно на локальном сервере, зафиксировать вывод).

## 2. Fail-closed FEK

- [ ] 2.1 Сервер: в `Open` (`pkg/meta/grpc_server_fuse.go`, ветка выдачи plaintext FEK) — детекция TLS-канала через `grpc.AuthInfoFromContext(ctx)` + type assert `credentials.TLSInfo`; на волюме с `encryption_enabled=true` при отсутствии TLS — ответ `codes.Unauthenticated` без FEK; при `encryption_enabled=false` проверка не выполняется. Файл: `pkg/meta/grpc_server_fuse.go` (+ вспомогательный хелпер рядом, при необходимости). Проверка: `go test ./pkg/meta/... -run 'TestOpen'`.
- [ ] 2.2 TLS-bufconn harness: self-signed сертификат, генерируемый в `TestMain`/helper (`crypto/tls`), bufconn с `credentials.NewServerTLSFromFile`/`NewTLS` для клиентской стороны. Файл: `pkg/meta/grpc_server_test_helpers_test.go` (новый, с Apache 2.0 header; имя по фактическим конвенциям тестовых хелперов пакета). Проверка: `go test ./pkg/meta/... -run 'TestTLS'`.
- [ ] 2.3 Тесты сценариев спеки: plaintext `Open` зашифрованного файла → `Unauthenticated` и пустой `fek`; TLS `Open` зашифрованного файла → FEK выдан (при прошедшем authz); plaintext `Open` на незашифрованном волюме → поведение без изменений. Существующие тесты FEK-пути через insecure bufconn перевести на TLS-harness. Файлы: `pkg/meta/grpc_server_fuse_test.go` (или фактические файлы тестов `Open`/FEK). Проверка: `make test.meta.core`.

## 3. Сверка и стабилизация

- [ ] 3.1 Прогон полного набора затронутых пакетов и линтера; при наличии расхождений спека ↔ код — правка кода (спека зафиксирована). Проверка: `make test.meta.core`, `go build ./...`, `go vet ./...`, `golangci-lint run pkg/meta/... cmd/...`.

## Definition of Done

- [ ] `go build ./...` — без ошибок.
- [ ] `go vet ./...` — чисто.
- [ ] `go test ./pkg/meta/...` (make test.meta.core) — зелёный, включая новые тесты сценариев спеки (plaintext/TLS/unencrypted).
- [ ] `go build ./cmd/...` и ручной smoke: сервер с `--tls-cert/--tls-key` принимает TLS-клиента с `tls=1&tls-ca=...`; сервер без флагов работает как раньше.
- [ ] `golangci-lint run` по затронутым пакетам — чисто.
- [ ] `openspec validate implement-grpc-tls` — валидно.
