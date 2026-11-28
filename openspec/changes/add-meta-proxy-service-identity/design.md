## Architectural Context

Порядок интерцепторов прокси сегодня: TLS (опц.) → OIDC strict на unary+stream (`pkg/oidc/interceptor.go:171-186`, кладёт `*oidc.IDToken` в ctx) → authz только на unary (`cmd/meta_proxy.go:250-302`: OIDC 267-268, authz 295-299; цепочки 327-332). Стриминговые RPC получают только OIDC (authz-дыра известна: DumpMeta* зарегистрированы, но «disabled» логом — `meta_proxy.go:301`). Admin-методы (`pkg/meta/grpc_server_admin.go`: GetFormat, Remove, BatchUnlink, GetSummary, GetTreeSummary, Clone, GetPaths, Check, Compact*, ListSlices, CleanupSlices, ScanSlices, HandleQuota, ScanUserGroupUsage, Chroot, CleanupTrash/DetachedNodes, ScanDeletedObject, ScanChangelog + стриминговые DumpMeta/LoadMeta/Tokens/RotateFileKey) охраняются authz-классификацией `requiredPermission` → Admin → CheckOrganizationAdmin (`authz_interceptor.go:152-161,207-309`); lifecycle-исключения Init/Load/NewSession/CloseSession/FlushSession/GetSession(s)/CleanStaleSessions/Chroot/DirHandler* (:215-219). Keymanager-вызовы прокси — ВСЕ внутри стеков RPC-обработчиков (фон-вызовов на прокси нет; клиентский `writer.refreshFEK` с `meta.Background()` — на стороне FUSE-клиента, grpc-клиент сам навешивает авторизацию на исходящие вызовы). user_id в keymanager request'ах заполняется из `extractUserIDFromOIDC` (`authz_interceptor.go:102-116`) на call sites: grpc_server_fuse.go:272-279,318-331,369+,1035-1047,1032-1037,489-506; grpc_server_admin.go:128-131; grpc_server_lifecycle.go:101-112; grpc_server_sts.go:43.

## Component Map

- `cmd/meta_proxy.go` — флаги `--service-credential-hash` (hex SHA-256), `--node-credential` (файл/env-путь с сырым токеном; значение не в логах), wiring интерцепторов.
- `pkg/meta/service_identity.go` (новое) — service-check интерцептор (unary+stream), kind-ключ ctx, таблица методов для service {GetFormat: allow; admin-набор минус GetFormat: deny; остальное: pass}.
- `pkg/oidc/interceptor.go` — ветка: kind=service → pass-through (короткое замыкание цепочки).
- `pkg/meta/authz_interceptor.go` — первая строка UnaryInterceptor: kind=service → skip (без CheckPermission/CheckBulk, без authz-кэша).
- Обёртка keymanager pb-клиента прокси — decorator: для CreateFileKey/GetFileFEK/RotateFileFEK читает kind из входящего ctx; service → добавляет metadata `x-node-credential`; oidc/без identity → без метадаты.

## Execution Flow

1. Запрос с `x-service-credential`: service-check первым (до OIDC) → both-headers → Unauthenticated; hash mismatch → Unauthenticated (без fallback); match → kind=service в ctx; далее метод-таблица: GetFormat → pass; admin-набор (включая стриминговые DumpMeta/LoadMeta — закрывает authz-дыру для service) → PermissionDenied; остальное → handler.
2. Запрос без service-header при включённом режиме → service-check pass-through → OIDC strict как сегодня.
3. Handler (kind=service) вызывает keymanager: decorator видит kind → `x-node-credential` метадата; user_id не заполняется (extract возвращает «не найден» для service-kind — call sites пишут пустой user_id).
4. Отсутствие `--node-credential` при включённом service-режиме → service-соединения отклоняются на входе (fail-closed).

## Invariants

- Поведение запросов без `x-service-credential` — байт-в-байт прежнее (OIDC strict, authz, кэши, аудит).
- Service-решения не попадают в authz-кэш (skip целиком).
- Service-соединение теперь тоже достигает pre-existing дефекта Chroot (Chroot = AuthzPermissionNone → pass, меняет общий s.root authz-free): известноe следствие модели доверия ADR-005 (gateway ≈ прокси); gateway Chroot не вызывает (--subdir запрещён) — отдельный issue на сам дефект, вне этого change.
- Service-соединениям недоступен admin-набор, кроме GetFormat; стриминговые admin — недоступны (дыра закрыта для service).
- node-credential прикрепляется только к трём FEK-методам и только для service-kind; платформа не получит node-ветку ни для какого oidc-вызова.
- Значения `--service-credential-hash`/`--node-credential` не логируются; в конфигах — имена/файлы.

## Decisions & Rationale

1. **Service-check до OIDC + kind-ветка в OIDC** (короткое замыкание): альтернатива — две параллельные цепочки — сложнее в wiring; ветка в OIDC на 3 строки и сохраняет единый порядок.
2. **Decorator pb-клиента вместо grpc DialOption-интерцептора**: pb-клиент прокси вызывается с ctx handler'а — decorator над интерфейсом надёжнее (не зависит от порядка dial-опций) и тестируется юнит-тестом без сети.
3. **Метод-таблица service-доступа в service_identity**: одна точка правды для unary+stream; стриминговые методы явно в deny-наборе (сегодня они мимо unary-authz).
4. **user_id через существующий extract**: для service-kind возвращается пусто — call sites не меняются (пишут пустой user_id; платформа в node-ветке его игнорирует — change add-s3-gateway-service-identity).
5. **Пилот: ревокация service-token/node-credential = рестарт общего stage-прокси** (hot-reload — follow-up ADR-005).

## Integration Points

- agio-platform: presence-интерцептор + узловая ветка (changes add-s3-gateway-credential-rpc / add-s3-gateway-service-identity) — контракт `x-node-credential` на трёх FEK-методах.
- gateway (change add-gateway-platform-mode): подключается service-token'ом; startup-проверка GetFormat разрешена именно этой таблицей.
- platform-authz-grpc: не вызывается для service (пропуск authz) — authz-квоты/нагрузка прокси не меняются.

## Open Questions

- Hot-reload node-credential без рестарта — follow-up (не блокирует пилот).
- `writer.refreshFEK` (клиентский фон) в service-конфигурации gateway: подтверждается на E2E; если деградирует — клиентский вопрос agio-drive, не прокси.
