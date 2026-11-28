# Proposal: add-meta-proxy-outbox

## Why

Том hector-stage (и любые будущие platform-томы) обслуживается grpc meta-proxy — все клиенты
(drive-маунты, s3-gateway platform-режима) ходят через него. Нативная эмиссия событий
(outbox) реализована только в redis-движке (`pkg/meta/redis.go`) и включается флагами
`--outbox-enabled`/`--outbox-stream` у команд mount/gateway. Прокси держит полноценный
redis-движок (`cmd/meta_proxy.go:220` `meta.NewClient(metaBackendUrl, meta.DefaultConf())`),
но: (а) команда `meta-proxy` этих флагов не регистрирует, (б) `DefaultConf()` даёт
`Outbox.Enabled=false`. В результате на proxy-пути события не возникают ни на одной стороне:
grpc-клиент эмиссии не имеет вовсе, серверная часть — не настроена. Платформенная
консолидация файлового индекса (event-bridge → juicefs.events → ConsolidateDriveObjects)
для таких томов мертва без ручного `consolidateDriveObjects`.

## What Changes

1. `cmd/meta_proxy.go`: регистрация флагов `--outbox-enabled` (bool, default false) и
   `--outbox-stream` (string, default `juicefs:outbox`) — имена и семантика идентичны
   mount/gateway (`cmd/flags.go:347-355`).
2. Парсинг в conf движка по образцу `cmd/mount.go:378-381`:
   `metaConf := meta.DefaultConf(); metaConf.Outbox.Enabled = c.Bool("outbox-enabled")`;
   `--outbox-stream` применяется только при явном `IsSet`; `meta.NewClient(metaBackendUrl, metaConf)`
   вместо `DefaultConf()`.
3. Поведение при выключенном флаге — байт-в-байт прежнее (`DefaultConf()` эквивалентен
   новой ветке при `Enabled=false`).
4. Сама эмиссия — существующий движковый контракт `domain-outbox` (doMknod/doWrite/doRename/
   doUnlink/doRmdir/doTruncate в том же redis-пайплайне, XADD+XTRIM в stream) — НЕ меняется.
   Публикацию в очередь-бакет выполняет существующий отдельный процесс
   `juicefs outbox --sink s3` (change 2026-11-19-add-outbox-s3-sink) — деплой его sidecar'ом
   в agio-cloud вне этого change.

## Impact

- **Specs**: `domain-meta-proxy` — ADDED Requirement «Outbox emission configuration»
  (флаги, виринг в движок, default-off, stream override).
- **Specs не тронуты**: `domain-outbox` (движковый контракт), service-identity, STS.
- **Риски**: нет — флаг default-off; единственный потребитель флага сегодня — деплой
  drive-meta-proxy-hector-stage (agio-cloud change add-hector-outbox-events).
- **Деплой-линия**: agio-drive-v2; новый образ нужен ТОЛЬКО релизу meta-proxy
  (gateway-бинарь не меняется).
