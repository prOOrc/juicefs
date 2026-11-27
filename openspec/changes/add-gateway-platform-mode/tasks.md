## 1. Platform-режим запуска

- [ ] 1.1 cmd/gateway.go: флаг `--platform`; fail-fast проверки (`--subdir` → ошибка, `--refresh-iam-interval` → ошибка, MINIO_ROOT_* в env → ошибка, отсутствие обязательных env AGIO_* → ошибка с именованием ключа); обход обязательности ROOT-кредов (:132-148) в platform-режиме; env-значения не логируются (grep-тест). Verify: `go build ./...`, `go test ./cmd/...` — матрица стартов (7 кейсов спеки)
- [ ] 1.2 Startup GetFormat от service-identity (grpc-клиент с service-token из env) до запуска minio-поверхности; отказ → exit. Verify: `go test ./cmd/...` — fake-прокси accept/deny
- [ ] 1.3 Сборка env для minio ServerMainForJFS (адреса keymanager/authz, node-credential, facility, bucket — по контракту Component Map). Verify: `go test ./cmd/...` — env передан, секреты не в логах

## 2. Complete-at-destination

- [ ] 2.1 pkg/gateway/gateway.go CompleteMultipartUpload: создание целевого файла по финальному пути, CopyFileRange частей в него, удаление staging-каталога; при ошибке копирования — удаление частичного файла + лог. Verify: `go test ./pkg/gateway/...` — happy path (ключи объектов), fail-path (частичный удалён), unencrypted-эквивалентность
- [ ] 2.2 Abort/cleanup без изменений: существующие тесты зелёные. Verify: `go test ./pkg/gateway/...`

## 3. DoD

- [ ] 3.1 `go build ./...`, `go vet ./...`, `go test ./cmd/... ./pkg/gateway/...` зелёные
- [ ] 3.2 `openspec validate add-gateway-platform-mode --strict` без ошибок
- [ ] 3.3 E2E-контур пилота (после minio/agio-platform changes): multipart rclone на шифрованном томе — FEK читается по реальному пути (приёмка п.9-11)
- [ ] 3.4 reviewer APPROVED (по конвейеру)
