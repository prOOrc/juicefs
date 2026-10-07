# implement-proxy-skip-posix-checks — Design

## Architectural Context

Серверный контур agio Drive: клиенты (форк-клиент с `--owner-override`) → drive-meta-proxy (`MetaProxyServer`, gRPC) → meta-движок в поде (`s.meta`, Redis/SQL/KV). Два слоя контроля доступа на пути операции:

1. **authz-интерцептор** (`AuthzInterceptor`, `SetAuthzInterceptor` grpc_server.go:89, ставится из cmd/meta_proxy.go:272 при `--authz-service`): OIDC `sub` → `AuthzService.CheckPermission/CheckBulkPermissions`, fail-closed, маппинг RPC→permission — **доверенный гейт** (идентичность из валидированного токена).
2. **engine-level POSIX** (`baseMeta.Access` base.go:1461, вызовы в движках — напр. redis.go:1476 в Mknod): stored-режимы против uid из **клиентского** `pb.MetaContext` (`metaCtx` grpc_server.go:136 → `WrapWithCancel` → `wrapContext.CheckPermission()`==true, context.go:82) — **недоверенный и ложный** слой: carried-uid контролируется клиентом (uid=0 вообще коротко замыкает проверку в base.go:1462).

Change устраняет слой 2 в authz-режиме, оставляя слой 1 единственным гейтом — в соответствии с ADR-003.

## Component Map

| Компонент | Роль | Изменение |
|---|---|---|
| `pkg/meta/grpc_server.go` `MetaProxyServer.metaCtx` :136 | построение engine-контекста из `pb.MetaContext` | при `s.authzMode` возвращать контекст с `CheckPermission()==false` |
| `pkg/meta/grpc_server.go` `MetaProxyServer` / `NewMetaProxyServer` :79, `SetAuthzInterceptor` :89 | состояние сервера | флаг `authzMode` (выставляется вместе с интерцептором в cmd/meta_proxy.go) |
| `pkg/meta/context.go` `wrapContext.CheckPermission` :82, `WrapWithCancel` :94 | конструирование Context | добавить вариант с переопределённым `CheckPermission()` (новый малый тип или параметризованный конструктор); существующие конструкторы не меняются |
| `pkg/meta/base.go` `baseMeta.Access` :1461 | единый гейт всех POSIX-проверок движков | НЕ меняется — уже шлюзован `ctx.CheckPermission()`; relief достигается контекстом |
| `cmd/meta_proxy.go` :272-285 | сборка сервера, authz-бутстрап | передать authz-режим в сервер (после успешного `NewPlatformAuthzClient`) |
| `pkg/meta/authz_interceptor.go`, `authz_client.go`, `authz_cache.go` | доверенный гейт | НЕ меняются |
| engine-код redis.go/sql.go/tkv.go (call-sites `m.Access`, напр. redis.go:1476) | проверки на операциях | НЕ меняются — покрыты гейтом по построению (parity Redis/SQL/KV автоматически) |

Типы verified grep-ом: `MetaProxyServer`, `metaCtx`, `NewMetaProxyServer`, `SetAuthzInterceptor`, `AuthzInterceptor`, `NewPlatformAuthzClient`, `wrapContext`, `WrapWithCancel`, `baseMeta.Access`, `checkQuota`.

## Execution Flow

Mknod через прокси в authz-режиме (после change):

```
клиент Mknod(parent, name) [ctx: uid=1000]
  → gRPC (OIDC interceptor → authz interceptor: CheckPermission(path) — fail-closed)
  → MetaProxyServer.Mknod → metaCtx(req.Ctx)           // authzMode ⇒ CheckPermission()==false
    → s.meta.Mknod → redis.go:1476 m.Access(parent, W|X)
      → baseMeta.Access: ctx.CheckPermission()==false ⇒ return 0   // POSIX-слой пропущен
    → doMknod: inode создан, stored uid = 1000 (carried, как и раньше)
  → OK
```

Deny-путь не меняется: authz-интерцептор возвращает `PermissionDenied` до вызова хендлера.

## Invariants

1. authz-интерцептор — единственный гейт в authz-режиме; его решения, fail-closed-семантика и root short-circuit не изменяются.
2. Без `--authz-service` байт-в-байт прежнее поведение (`metaCtx` → `CheckPermission()==true`).
3. Локальные маунты (минуя прокси) не затронуты: FUSE-слой по-прежнему выставляет `checkPermission` из `NonDefaultPermission` (pkg/fuse/context.go:65).
4. Stored-метаданные, stored uid новых inode (= carried uid клиента), квоты, FEK-over-TLS, OIDC — без изменений.
5. Изменение движко-агностично: гейт один (`baseMeta.Access`), parity Redis/SQL/KV соблюдается по построению; покрывается общими тестами base-уровня.
6. `uid==0`-bypass в `baseMeta.Access` (base.go:1462) сохраняется — на поведение authz он не влияет (интерцептор не смотрит POSIX-uid).

## Decisions & Rationale

- **D1 — автоматическая связка с `--authz-service`, без нового флага**: proxy в agio-контуре всегда деплоится с authz (stage values `authz.service`); POSIX-слой через carried-uid не даёт безопасности (uid контролируется клиентом, uid=0 — bypass), т.е. связка не ослабляет защиту, а убирает ложный гейт. Отдельный флаг дал бы опасную комбинацию «authz выкл., POSIX выкл.» — не даём её существовать. Следствие: деплой stage = новая сборка образа, chart agio-cloud не меняется.
- **D2 — точка изменения: `metaCtx`, не `baseMeta.Access`**: гейт в base уже существует (`ctx.CheckPermission()`), правка контекста — минимальный дифф, не требующий параллельных правок трёх движков и не создающий риска для локальных маунтов (FUSE-контексты строятся другим путём).
- **D3 — хранить режим в `MetaProxyServer`, а не в глобале**: состояние сервера, тест-дружелюбно (два сервера в одном процессе в тестах), соответствует существующему стилю (`SetAuthzInterceptor`).
- **D4 — клиентский uid остаётся в stored-метаданных новых файлов**: смешанные stored-uid по клиентам допустимы — презентацию выравнивает клиентский `--owner-override` (change `implement-mount-owner-override`); унификация stored-uid через `--all-squash` остаётся опциональной операционной практикой.
- **D5 — отклонено: фильтровать только Mknod/нужные call-sites**: точечная фильтрация оставила бы EACCES на rename/setattr/link и расхождение между движками; гейт в base покрывает все call-sites одинаково.

## Integration Points

- **agio-cloud**: без изменений чарта; выкат = новый тег образа `drive-meta-proxy` / `drive-meta-proxy-tst-enc-a` (JUICEFS_PIN → коммит этого change).
- **agio-drive `implement-juicefs-mount`**: после деплоя клиентский `buildMetaURL` передаёт bare `--owner-override=` (без композиции со squash); приёмочный сценарий 4.1 клиентского change закрывается end-to-end (повтор на itx.lan).
- **spec `mount-ownership`**: сценарий «Клиент с чужим stored-uid пишет» — закрывается после деплоя; при архивации обоих change обновить `specs/index.md`.

## Risks / Trade-offs

- **Локальные (не-agio) инсталляции прокси без authz** — поведение не меняется (инвариант 2); прокси с authz всегда был agio-режимом, других пользователей прокси в форке нет.
- **Диагностика**: отказы авторизации теперь только из интерцептора (единый источник в логах с path/user/permission — существующий `authzFieldsInterceptor`); ложных EACCES станет меньше, отладка проще.
- **Глубина эшелонов**: снимаем второй гейт — осознанно: он не обеспечивал эшелон (обходился подделкой uid), а только ломал честных клиентов. Реальное эшелонирование дают OIDC-валидация + authz + FEK-over-TLS.

## Open Questions

- Нужно ли дублировать skip для `pb.AccessRequest`-хендлера (`MetaProxyServer.Access` → `s.meta.Access` с тем же mctx): покрыто тем же `metaCtx` автоматически — подтвердить тестом, что Access-RPC с CheckPermission=false возвращает 0 без движка.
- Обновлять ли документацию чарта agio-cloud (values-комментарий «authz mode implies no POSIX checks») — решить при выкате образа; в fork-репозитории правок не требуется.
