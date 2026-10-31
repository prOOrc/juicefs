# Tasks: implement-company-dir-self-provision

## Предусловие

- [x] 0.1 agio-platform change `allow-self-provision-company-dir` применён
  (proto с полями `op`/`working_dir` доступна как источник).
  Проверка: в agio-platform `go build ./...` зелёный с новыми полями.

## 1. Proto и прокси

- [x] 1.1 Регенерация pb-копий agio authz/STS сервисов в `pkg/meta/pb/`
  (+`op` в CheckPermissionRequest, +`working_dir` в StsCredentialsResponse).
  Проверка: `GOTOOLCHAIN=go1.26.5 go build ./...`.
- [x] 1.2 Прокси: классификация Mkdir → `OP_MKDIR` в authz-вызове; прочие
  модифицирующие RPC → `OP_OTHER`.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test ./pkg/meta/ -run AuthzOp`.
- [x] 1.3 Тесты: `TestAuthzOpContext_MkdirClassified`,
  `TestAuthzOpContext_OtherOpsDefault`.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./pkg/meta/`.

## 2. Клиентский ensure

- [x] 2.1 `pkg/meta/grpc_client_sts.go` / контекст маунта: проброс
  `working_dir` из ответа GetSTSCredentials.
  Проверка: `GOTOOLCHAIN=go1.26.5 go build ./...`.
- [x] 2.2 `cmd/mount.go`: после ready при `--company-id` — mkdir -p
  `working_dir` через meta-клиент (EEXIST = успех, отказ = warn, пустое поле
  = no-op).
  Проверка: `GOTOOLCHAIN=go1.26.5 go test ./cmd/ -run EnsureWorkingDir`.
- [x] 2.3 Тесты: `TestEnsureWorkingDir_CreatesDeliveredPath`,
  `TestEnsureWorkingDir_ExistingDirIsSuccess`,
  `TestEnsureWorkingDir_DeniedIsWarningOnly`,
  `TestEnsureWorkingDir_NoFieldNoBehavior`.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./cmd/`.

## 3. Приёмка

- [x] 3.1 Stage: свежая компания tst-v3 → issue → mount → авто-ensure
  ПРОЙДЕН (`Working directory "companies/tst-v3" is ready` — клиент создал
  поддерево сам через MKDIR-правило). Запись файла ЗАБЛОКИРОВАНА вне скоупа
  change'а: CreateFileKey → Internal `no active KEK … (not provisioned)` —
  AGDF KEK не провиженится (`keymanager provision` падает на YandexKMS — YC
  мёртв; Selectel KMS-бэкенд для ports.KMS отсутствует, портфель миграции).
- [x] 3.2 Регресс на stage: mkdir произвольного корневого имени → denied;
  delete корневого companies → denied; чужая компания (companies/ilm) →
  denied. ПЕРВЫЙ прогон нашёл дыру: mkdir проверял путь РОДИТЕЛЯ ("edit on /")
  → произвольные корневые директории создавались; fix 74f49189a — проверять
  путь ребёнка (companies → "" → MKDIR-правило; <code> → company-scope;
  прочее → deny). Совместимость: proto-дефолт OP_OTHER покрывает старый
  прокси/новую платформу.

## Definition of Done

- `GOTOOLCHAIN=go1.26.5 go build ./... && go vet ./cmd/ ./pkg/meta/`
- `GOTOOLCHAIN=go1.26.5 go test -count=1 ./cmd/ ./pkg/meta/`
- Stage 3.1 пройден; implement-juicefs-mount (agio-drive) — отчёт дополнен;
  приёмочный критерий T0.1 «запись файла через v2 маунт» закрыт.
