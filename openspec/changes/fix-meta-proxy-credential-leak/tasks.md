# Tasks: fix-meta-proxy-credential-leak

## 1. Маскирование URL в стартовом логе

- [ ] 1.1 `cmd/meta_proxy.go`: заменить `loggerProxy.Infof("Metadata backend URL: %s", metaBackendUrl)` на печать через `utils.RemovePassword(metaBackendUrl)` (импорт `github.com/juicedata/juicefs/pkg/utils` уже доступен в пакете `cmd`). Проверка: `grep -n "Metadata backend URL" cmd/meta_proxy.go` показывает вызов с `utils.RemovePassword(metaBackendUrl)`; `grep -n "Infof(\"Metadata backend URL: %s\", metaBackendUrl)" cmd/meta_proxy.go` пуст.

- [ ] 1.2 `cmd/meta_proxy.go`: grep-аудит — в файле не осталось лог-вызовов, выводящих meta URL без маскировки. Проверка: `grep -n "loggerProxy\." cmd/meta_proxy.go | grep -E "metaBackendUrl|MetaURL|metaurl" | grep -v RemovePassword` пуст.

## 2. Проверки

- [ ] 2.1 Ручная проверка маски: собрать бинарь и запустить `juicefs meta-proxy --meta-backend "redis://:s3cret@localhost:6379/0" --addr 127.0.0.1:19561` на 2 секунды; в stdout строка с backend URL содержит `redis://:****@localhost:6379/0` и не содержит `s3cret`. Проверка: `go build -o /tmp/jfs . && timeout 2 /tmp/jfs meta-proxy --meta-backend "redis://:s3cret@localhost:6379/0" --addr 127.0.0.1:19561 2>&1 | tee /tmp/jfs-log.txt; ! grep -q s3cret /tmp/jfs-log.txt && grep -q ':****@' /tmp/jfs-log.txt`.

## Definition of Done

- [ ] DoD.1 `go build ./...` — успешно.
- [ ] DoD.2 `go vet ./cmd/...` — без замечаний.
- [ ] DoD.3 `go test ./pkg/utils/... ./cmd/...` — зелёные (пакет `cmd` содержит тесты, затрагивающие `meta_proxy`; `pkg/utils` — покрытие `RemovePassword` не меняется).
- [ ] DoD.4 `golangci-lint run cmd/...` — без новых замечаний.
