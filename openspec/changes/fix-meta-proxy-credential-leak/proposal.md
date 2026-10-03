# Proposal: fix-meta-proxy-credential-leak

## Why

`juicefs meta-proxy` при старте логирует полный `--meta-backend` URL вместе с паролем на INFO-уровне (`cmd/meta_proxy.go`, строка `loggerProxy.Infof("Metadata backend URL: %s", metaBackendUrl)`). Пароль Managed Redis попал в pod-логи и систему агрегации логов (Loki) при деплое `drive-meta-proxy` в agio-cloud stage (обнаружено 2026-10-03). При этом `meta.NewClient` уже логирует тот же адрес замаскированным (`pkg/meta/interface.go:744`, `utils.RemovePassword`) — прокси должен вести себя так же.

## What Changes

- Стартовый лог `cmd/meta_proxy.go` печатает metadata backend URL через `utils.RemovePassword` (формат `redis://:****@host:port/db`), а не сырое значение с паролем.
- Регрессионная проверка: в `cmd/meta_proxy.go` не остаётся лог-вызовов, передающих `metaBackendUrl` (или иной meta URL) без маскировки; поведение `utils.RemovePassword` покрывается существующей функцией без изменений.
- Никаких изменений протокола, флагов или интерфейсов — **BREAKING** нет.

## Capabilities

### New Capabilities

(нет)

### Modified Capabilities

- `domain-meta-proxy`: новое требование «Credential masking in command logs» — логи команды `juicefs meta-proxy` не содержат пароля metadata-движка; URL в логах маскируется так же, как в логе `meta.NewClient`.

## Impact

- **Код**: `cmd/meta_proxy.go` — одна строка стартового лога (`utils.RemovePassword(metaBackendUrl)`); файл `pkg/utils/utils.go` не меняется (`RemovePassword` уже используется в `pkg/meta/interface.go:744`).
- **Эксплуатация**: уже утёкший в Loki stage-пароль Redis этим change не отзывается — ротацию пароля нужно выполнить отдельно (Yandex Managed Redis, agio-terraform-yc/agio-cloud).
- **Клиенты**: не затрагиваются (лог-сообщение не часть контракта).

## Related Requirements

- **SRS-001#NFR-SEC-10** — «Plaintext ключи НЕ попадают в логи, метрики, трейсы, core dumps»: требование сформулировано для ключей шифрования; настоящий change применяет тот же принцип по аналогии к кредам metadata-движка (пароль в DSN). Прямого NFR про креды meta-backend в SRS-001 нет — гэп фиксится настоящим change на уровне capability-спеки.

## Non-goals

- Не маскировать URL в других командах (`mount`, `gateway`, `webdav` и т.д.) — там пароль уже не логируется сырым (проверено grep-ом по `cmd/`); change ограничен `meta-proxy`.
- Не менять `utils.RemovePassword` и его покрытие тестами.
- Не логировать URL на DEBUG-уровне полностью (даже под флагом) — маска безусловная.
- Ротация пароля Redis и чистка ретенции Loki — вне репозитория.
