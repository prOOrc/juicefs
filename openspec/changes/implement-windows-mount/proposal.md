# Proposal: implement-windows-mount

## Why

Windows-клиенты (рабочие станции художников — «полу-доверенная зона» SRS-001) — целевой сценарий agio Drive, но вся форк-спека и верификация живут в Linux: в `specs/` и `openspec/` слово Windows не встречается ни разу, CI-интеграшка `wintest.yml` гоняет только redis. Код при этом почти готов: `grpcMeta`-клиент (`pkg/meta/grpc_client.go`) не содержит ни одного GOOS-ветвления, WinFsp-маунт работает (`pkg/winfsp`), OIDC и шифрование кроссплатформенны (pure Go/stdlib). Найден один настоящий блокер: флаги `--sts-enabled` / `--company-id` объявлены только в `cmd/mount_unix.go:503-510` — Windows-сборка CLI падает с «flag provided but not defined», а без них недоступен data-plane STS (FR-REV-3, ADR-003) на зашифрованных волюмах.

## What Changes

- **Windows CLI parity**: в `cmd/mount_windows.go` добавляются флаги `--sts-enabled` и `--company-id` с той же семантикой, что в `cmd/mount_unix.go` (требуют зашифрованный волюм + `grpc://`; fail-fast при ошибке выдачи STS-кред, без фолбэка на статические креды).
- **Спека Windows-маунта**: новая capability `domain-windows-mount` фиксирует контракт маунта `grpc://` на Windows — flag parity, интерактивный OIDC browser flow как сценарий первого входа, сохранение полной конфигурации (meta URL с query-параметрами) в WinFsp service mode.
- **Верификация**: e2e-прогон `juicefs mount "grpc://..."` на Windows (WinFsp, буква диска) с OIDC-логином, зашифрованным волюмом и STS; проверка service mode (`-d`) с реконструкцией командной строки из реестра; unit-тест реконструкции URL с query-параметрами.
- **CI (решается в apply)**: добавить grpc://-сценарий в `wintest.yml` — решение по итогу оценки стоимости, зафиксировать вердикт в change.
- **BREAKING**: нет.

## Capabilities

### New Capabilities

- `domain-windows-mount`: контракт монтирования agio Drive volюмов на Windows — STS flag parity в Windows CLI, интерактивный OIDC login, service mode с сохранением `grpc://` конфигурации, известные платформенные ограничения (mlock no-op, symlink ENOSYS).

### Modified Capabilities

(нет — `grpcMeta`-клиент и mount-флоу уже платформенно-нейтральны; change не меняет поведение существующих капабилити на Linux)

## Impact

- **Код**: `cmd/mount_windows.go` (два флага); остальное — верификация без правок кода, кроме дефектов, которые прогон выявит (фиксировать как задачи по факту).
- **Тесты**: unit-тест реконструкции командной строки в service mode (`pkg/winfsp`), прогон `go test ./pkg/winfsp/...` на Windows-раннере.
- **CI**: `.github/workflows/wintest.yml` (опционально).
- **Эксплуатация**: инструкция первого входа на Windows (интерактивная сессия, браузер на localhost-callback); сервисный режим — только с прогретым токен-кешем (out of scope, см. Non-goals).

## Related Requirements

- **ADR-003** «Data-plane креденшелы: platform-issued YC STS» — Windows-клиент получает STS через тот же `GetSTSCredentials` pass-through Meta Proxy; flag parity реализует модель на Windows.
- **SRS-001#FR-REV-3** — data-plane на короткоживущих STS-кредах: без Windows-флагов требование не выполняется на Windows-клиентах.
- **SRS-001#§Таблица активов** («Полу-доверенный: Клиент художника (после OIDC)») — Windows-рабочая станция и есть этот актив.
- **domain-encrypt** (дельта change `implement-encrypt`, требование «Data-plane STS credentials») — «A mount with `--sts-enabled` SHALL fail fast … SHALL NOT fall back to static bucket credentials»: семантика переносится на Windows без изменений.
- **ADR-001** — гибридный workflow.
- **BRD отсутствует** (реестр `specs/index.md`, «Замечания»): бизнес-контекст в архитектурном плане v13; отсутствие BRD объясняется явно.

## Non-goals

- **Device-code flow / pre-populated кеш / keyring для сервисного OIDC** — сервисный режим на Windows работает только с прогретым токен-кешем интерактивным входом; отдельная следующая фаза (решение пользователя, 2026-10-03).
- **TLS клиентского канала** — отдельный change `implement-grpc-tls` (коду не зависим: Windows-верификация гоняется и на plaintext; TLS на Windows верифицируется после вливания TLS-change).
- **render-mount на Windows** — render-ноды остаются Linux-only (существующие заглушки `cmd/render_mount_windows.go` не трогаем).
- **Платформенные ограничения WinFsp** (symlink ENOSYS, chown no-op, POSIX ACL, одноволюмовый service mode) — не устраняем, фиксируем в спеке как задокументированные ограничения.
- **Чистка мёртвого кода `pkg/oidc/config.go`** (`CacheFile()` с неразвёрнутой тильдой) — вне скоупа; реальный путь токен-кеша задаёт библиотека oidc-login (`$HOME/.oidc-login`).
