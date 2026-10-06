# implement-proxy-skip-posix-checks

## Why

Meta-движок внутри drive-meta-proxy исполняет stored-POSIX-проверки (`baseMeta.Access`, pkg/meta/base.go:1461, вызовы из Mknod/Rename/SetAttr и др., например pkg/meta/redis.go:1476) с uid из КЛИЕНТСКОГО контекста (`MetaProxyServer.metaCtx` ← `pb.MetaContext`, grpc_server.go:136; `wrapContext.CheckPermission()` всегда true, context.go:82). Для agio-клиентов это двойное и некорректное гейтирование: (1) carried-uid — недоверенный ввод самого клиента (враждебный клиент пошлёт uid=0, и `baseMeta.Access` вернёт 0 сразу — проверки через carried-uid не дают безопасности по построению); (2) честный десктоп-клиент с uid, не совпадающим со stored-владельцем каталогов, получает EACCES ДО серверного authz — подтверждено приёмкой `implement-mount-owner-override` 2026-10-06 (запись 1 MiB отклонена, CheckPermission в логах authz-gRPC отсутствует — отказ до AuthzService). Реальный гейт в архитектуре agio Drive — authz-интерцептор прокси (OIDC `sub`, fail-closed), он уже полноценен; stored-POSIX-слой на прокси — только источник ложных отказов.

## What Changes

- При включённом authz-режиме прокси (`--authz-service` задан) `MetaProxyServer.metaCtx` SHALL строить контекст с `CheckPermission() == false` — engine-level stored-POSIX-проверки (`baseMeta.Access` и все её вызовы в движках) не выполняются; доступ определяется исключительно authz-интерцептором (без изменений его контракта).
- Без authz-режима поведение не меняется: `metaCtx` продолжает строить контексты с `CheckPermission() == true` (upstream-семантика локальных POSIX-проверок).
- Связывание автоматическое (authz вкл. → POSIX-проверки выкл.), без нового CLI-флага: деплой stage сводится к новой сборке образа, chart agio-cloud не меняется.
- Клиенты agio Drive после этого используют bare `--owner-override=` без композиции со squash (закрытие остатка приёмки 4.1 change `implement-mount-owner-override`).

## Capabilities

### New Capabilities
<!-- Нет. -->

### Modified Capabilities
- `domain-meta-proxy`: в authz-режиме engine-level POSIX-проверки доступа отключаются (контексты RPC строятся с CheckPermission=false); authz-интерцептор остаётся единственным гейтом доступа.

## Non-goals

- Не менять authz-интерцептор: маппинг RPC→permission, fail-closed, root short-circuit, DirHandler-фильтр, кэш решений — всё остаётся как специфицировано (это единственный гейт).
- Не менять клиентскую сторону (флаг `--owner-override`, `--all-squash`, superuser-bypass `uid==0` в `baseMeta.Access`) — поведение локальных (не-прокси) маунтов не затрагивается.
- Не менять per-engine код (redis.go/sql.go/tkv.go): точка изменения — построение контекста в `MetaProxyServer.metaCtx` и `wrapContext`; правило parity Redis/SQL/KV соблюдается по построению (гейт один, движко-агностичный).
- Не вводить CLI-флаг включения/выключения связки — только автоматическая привязка к `--authz-service`.
- Не трогать FEK-over-TLS (`connectionIsTLS`), квоты (`checkQuota`), OIDC-валидацию — соседние слои без изменений.

## Related Requirements

- ADR-003 (specs/decisions/ADR-003-data-plane-sts.md, accepted): data-plane доступ через platform STS, authz — единственный гейт; настоящий change устраняет второй (POSIX) гейт на прокси, приведя реализацию в соответствие с ADR.
- SRS-001 (specs/srs/SRS-001-agio-drive-encryption.md, схема FR-*/NFR-*): системный контур agio Drive; change не меняет требований SRS-001.
- Связка с change `implement-mount-owner-override` (клиент): его приёмка 4.1 зафиксировала серверный остаток — данный change его закрывает; спека `mount-ownership` сценарий «Клиент с чужим stored-uid пишет» закрывается end-to-end после деплоя.
