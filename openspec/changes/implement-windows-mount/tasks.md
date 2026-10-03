# Tasks: implement-windows-mount

Контракт — delta-спека `specs/domain-windows-mount/spec.md` этого change; решения D1–D4 — design.md. Порядок: 1 → 2 → 3 → 4 → 5. Коммиты: префикс `windows-mount:`; правки `cmd/mount_windows.go` — минимальные (только декларации флагов). Сборка Windows-бинарника: `make juicefs.exe` (нужен mingw-w64 + WinFsp-заголовки, `Makefile:77-84`).

## 1. STS flag parity

- [ ] 1.1 Добавить флаги `--sts-enabled` и `--company-id` в Windows-flagset с той же семантикой дефолтов, что в `cmd/mount_unix.go:503-510` (`sts-enabled` bool, `company-id` string). Общую обработку в `cmd/mount.go:726-755` не трогать. Файл: `cmd/mount_windows.go`. Проверка: `GOOS=windows go vet ./cmd/` (CGO_ENABLED=0, vet не требует линковки); `make juicefs.exe`.
- [ ] 1.2 Smoke-проверка отказных сценариев на Windows-машине: `juicefs mount redis://... X: --sts-enabled` → ошибка «STS requires grpc:// meta URL» (или фактический текст общего кода); `--sts-enabled` на незашифрованном волюме → ошибка про encrypted volume. Проверка: ручной прогон, зафиксировать фактические сообщения об ошибках в комментариях задачи.

## 2. Interactive OIDC first login (прогон на Windows)

- [ ] 2.1 Прогон первого входа: на Windows-машине с пустым `%USERPROFILE%\.oidc-login` выполнить `juicefs mount "grpc://<stage-proxy>?oidc-issuer=...&oidc-client-id=...&oidc-redirect-url=...&oidc-scopes=..." Z:` — браузер открывается, после логина маунт завершается, `Z:` читается. Проверка: ручной прогон на stage-топологии; зафиксировать результат и время блокировки.
- [ ] 2.2 Прогон повторного маунта из кеша: umount, повторный mount — без браузера, токен из `%USERPROFILE%\.oidc-login`. Проверка: ручной прогон; отсутствие сетевого запроса к OIDC-issuer на авторизацию (по логам).
- [ ] 2.3 Прогон сервисного контекста без кеша: зарегистрировать `-d`-маунт при пустом кеше — убедиться, что попытка аутентификации завершается ошибкой (не висит бесконечно); зафиксировать фактический таймаут oauth2cli. Если маунт висит дольше ~5 минут — завести фикс-таск в этом change (в пределах фазы). Проверка: ручной прогон; результат зафиксирован.

## 3. Service mode configuration preservation

- [ ] 3.1 Unit-тест реконструкции командной строки в `RunAsSystemService` (`pkg/winfsp/winfs.go:1346-1486`, реконструкция `:1365-1424`): URL со всеми query-параметрами (`oidc-*`, `tls=1&tls-ca=...`, cache-параметры) и флаги `--sts-enabled --company-id <id>` сохраняются в registry-строке CommandLine без потерь экранирования `&`/`=`. Файл: `pkg/winfsp/winfs_test.go` (новый, `//go:build windows`, Apache 2.0 header). Проверка: `GOOS=windows go test ./pkg/winfsp/...` (на Windows-раннере / wintest.yml).
- [ ] 3.2 Если тест 3.1 выявил потерю query-параметров или экранирования — починить реконструкцию. Файл: `pkg/winfsp/winfs.go`. Проверка: тест 3.1 зелёный.
- [ ] 3.3 Прогон service mode на Windows: foreground-маунт с полным `grpc://`+OIDC+STS конфигом → `-d`-конвертация → перезапуск сервиса (`launchctl-x64 start` / `net use`) — коннект с той же конфигурацией, `Z:` (или network path) работает. Проверка: ручной прогон на stage-топологии.

## 4. E2E: зашифрованный волюм + STS на Windows

- [ ] 4.1 E2E-прогон: `juicefs mount "grpc://..." Z: --sts-enabled --company-id <id>` зашифрованного stage-волюма: запись файла, чтение (расшифрование корректно, md5 совпадает), wipe по logout (`_JFS_LOGOUT`), повторный вход. Проверка: ручной прогон на stage-топологии; зафиксировать логи/результаты (по образцу гейтов 10.7 change `implement-encrypt`).
- [ ] 4.2 Зафиксировать вердикт прогона: пройден/не пройден; при не пройденном — дефекты списком с задачами-фиксами в этом change или отдельными issues (решение по критичности). Проверка: запись в этом чекбоксе со ссылкой на логи.

## 5. CI (решение)

- [ ] 5.1 Оценить автоматизацию grpc://-сценария в `.github/workflows/wintest.yml` (нужен OIDC-провайдер + зашифрованный волюм): если поднимается без stage-зависимостей — добавить джобу; иначе — зафиксировать «ручной прогон как верификацию фазы» с обоснованием. Файл: `.github/workflows/wintest.yml` (при автоматизации). Проверка: джоба зелёная в PR, либо записанное решение с обоснованием.

## Definition of Done

- [ ] `make juicefs.exe` — собирается; `juicefs mount --help` на Windows показывает `--sts-enabled` и `--company-id`.
- [ ] `go build ./...`, `go vet ./...` — чисто (Linux-сборка не тронута).
- [ ] `go test ./pkg/winfsp/...` на Windows-раннере — зелёный (тест 3.1).
- [ ] E2E-прогон задачи 4.1 выполнен, вердикт зафиксирован (4.2).
- [ ] Решение по CI зафиксировано (5.1).
- [ ] `golangci-lint run` по затронутым пакетам — чисто.
- [ ] `openspec validate implement-windows-mount` — валидно.
