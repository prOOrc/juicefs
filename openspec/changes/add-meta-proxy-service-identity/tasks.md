## 1. Service identity

- [x] 1.1 `pkg/meta/service_identity.go`: kind-ключ ctx; service-check интерцептор (unary+stream): both-headers reject, SHA-256 constant-time против `--service-credential-hash`, mismatch → Unauthenticated без fallback, match → kind в ctx + метод-таблица {GetFormat: allow; admin-набор минус GetFormat (включая стриминговые DumpMeta/LoadMeta/Tokens/RotateFileKey): PermissionDenied; остальное: pass}. Verify: `go test ./pkg/meta/...` — матрица: service-ok/both/wrong/no-header × unary+stream × GetFormat/admin/lifecycle/file-RPC
- [x] 1.2 Ветка kind=service в `pkg/oidc/interceptor.go` (pass-through) и первая строка skip в `pkg/meta/authz_interceptor.go` UnaryInterceptor. Verify: `go test ./pkg/oidc/... ./pkg/meta/...` — oidc-запросы без service-header не изменились (существующие тесты зелёные без правок)

## 2. Роутинг node-credential

- [x] 2.1 Decorator keymanager pb-клиента прокси: CreateFileKey/GetFileFEK/RotateFileFEK при kind=service добавляют metadata `x-node-credential` (значение из конфига); остальные методы и oidc-kind — без метадаты. Verify: `go test ./pkg/meta/...` — fake-сервер платформы: header present/absent по матрице kind×метод
- [x] 2.2 user_id пуст для service-kind через extract (`authz_interceptor.go:102-116` + kind-ветка); call sites не меняются. Verify: `go test` — request user_id пуст при service-kind
- [x] 2.3 Флаги `--service-credential-hash`, `--node-credential` (cmd/meta_proxy.go): секреты не в логах; отсутствие node-credential при включённом service-режиме → service-соединения отклоняются на входе; маскировка логов (по образцу Credential masking требования). Verify: `go test ./cmd/...` + grep-тест логов

## 3. DoD

- [x] 3.1 `go build ./...`, `go vet ./...`, `go test ./pkg/meta/... ./pkg/oidc/... ./cmd/...` зелёные (оговорка: пред-существующие env-зависимые падения — TestLoadDump нужен локальный TiKV, cmd TestIntegration/TestJFS/TestRestore* требуют sudo/MinIO env — падают идентично на базовом коммите)
- [x] 3.2 `openspec validate add-meta-proxy-service-identity --strict` без ошибок
- [x] 3.3 Regression: полный набор существующих тестов pkg/meta, pkg/oidc зелёный без правок (инвариант oidc-пути); правки затронули только интерцепторы/декоратор (см. 3.1 про env-исключения)
- [ ] 3.4 reviewer APPROVED (по конвейеру)
