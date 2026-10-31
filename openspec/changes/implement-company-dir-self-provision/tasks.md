# Tasks: implement-company-dir-self-provision

## Предусловие

- [ ] 0.1 agio-platform change `allow-self-provision-company-dir` применён
  (proto с полями `op`/`working_dir` доступна как источник).
  Проверка: в agio-platform `go build ./...` зелёный с новыми полями.

## 1. Proto и прокси

- [ ] 1.1 Регенерация pb-копий agio authz/STS сервисов в `pkg/meta/pb/`
  (+`op` в CheckPermissionRequest, +`working_dir` в StsCredentialsResponse).
  Проверка: `GOTOOLCHAIN=go1.26.5 go build ./...`.
- [ ] 1.2 Прокси: классификация Mkdir → `OP_MKDIR` в authz-вызове; прочие
  модифицирующие RPC → `OP_OTHER`.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test ./pkg/meta/ -run AuthzOp`.
- [ ] 1.3 Тесты: `TestAuthzOpContext_MkdirClassified`,
  `TestAuthzOpContext_OtherOpsDefault`.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./pkg/meta/`.

## 2. Клиентский ensure

- [ ] 2.1 `pkg/meta/grpc_client_sts.go` / контекст маунта: проброс
  `working_dir` из ответа GetSTSCredentials.
  Проверка: `GOTOOLCHAIN=go1.26.5 go build ./...`.
- [ ] 2.2 `cmd/mount.go`: после ready при `--company-id` — mkdir -p
  `working_dir` через meta-клиент (EEXIST = успех, отказ = warn, пустое поле
  = no-op).
  Проверка: `GOTOOLCHAIN=go1.26.5 go test ./cmd/ -run EnsureWorkingDir`.
- [ ] 2.3 Тесты: `TestEnsureWorkingDir_CreatesDeliveredPath`,
  `TestEnsureWorkingDir_ExistingDirIsSuccess`,
  `TestEnsureWorkingDir_DeniedIsWarningOnly`,
  `TestEnsureWorkingDir_NoFieldNoBehavior`.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./cmd/`.

## 3. Приёмка

- [ ] 3.1 Stage (после выката agio-platform): свежий том → issue → mount →
  авто-ensure → запись/чтение под `companies/<code>/…` (md5 roundtrip, chunk
  в S3).
  Проверка: лог маунта (ensure ok), `juicefs info`, объект в бакете.
- [ ] 3.2 Регресс совместимости: новый клиент против старой платформы (без
  полей) — поведение прежнее; старый прокси против новой платформы — mkdir
  в корне по-прежнему отказан.
  Проверка: прогон на mixed-версиях, логи authz.

## Definition of Done

- `GOTOOLCHAIN=go1.26.5 go build ./... && go vet ./cmd/ ./pkg/meta/`
- `GOTOOLCHAIN=go1.26.5 go test -count=1 ./cmd/ ./pkg/meta/`
- Stage 3.1 пройден; implement-juicefs-mount (agio-drive) — отчёт дополнен;
  приёмочный критерий T0.1 «запись файла через v2 маунт» закрыт.
