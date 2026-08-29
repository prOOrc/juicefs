# Design: baseline-inventory-meta-proxy

## Architectural Context

Meta Proxy — AGIO-расширение форка JuiceFS (ветка `agio-drive-v2`): gRPC-сервер, оборачивающий metadata-движок (`redisMeta`/`dbMeta`) и являющийся единственной точкой контроля доступа для внешних клиентов. Authn — OIDC (Ory Hydra/Kratos) через JWKS-валидацию; authz — gRPC-вызовы в agio-platform `AuthzService` (`agio.platform.authz.v1`). Инвариант идентичности: OIDC `sub` ≡ `user.id` (UUID) — маппинг отсутствует, `sub` используется напрямую как `user_id` в authz-запросах (подтверждает SRS-001 §1.5 S12).

```text
AGIO Client (grpcMeta)
  │  gRPC (insecure), Bearer OIDC token + x-session-id
  ▼
juicefs meta-proxy (:9561)
  ├─ StrictOIDC interceptors (unary+stream) → claims в context
  ├─ AuthzInterceptor (unary-only) → InodePathCache → platformAuthzClient (cachingAuthzClient, TTL 30s)
  │     └─ agio-platform AuthzService: CheckPermission / CheckBulkPermissions / CheckOrganizationAdmin
  ├─ MetaProxyServer → Meta (redisMeta/dbMeta)
  └─ DirHandler entries (stateful readdir, authz-фильтрованный снапшот)
```

## Component Map

| Компонент | Файл | Роль |
|---|---|---|
| `cmdMetaProxy`, `outboxConsumer`-аналог | `cmd/meta_proxy.go` | CLI: флаги, gRPC opts, цепочка интерцепторов, graceful shutdown 30s |
| `MetaProxyServer`, `NewMetaProxyServer`, `metaCtx`, `ResolveHandle` | `pkg/meta/grpc_server.go` | Сервер: обёртка Meta, контекст (uid/gid/pid), handle→inode для authz |
| RPC-хендлеры | `pkg/meta/grpc_server_{lifecycle,fuse,directory,dir_handler,admin,streaming,xattrs,locks,token,acl}.go` | 78 RPC `MetaService`; maintenance InodePathCache в мутающих RPC |
| `filterEntriesByAuthz`, `authzListing` | `pkg/meta/grpc_server_fuse.go`, `grpc_server_dir_handler.go` | Fail-closed фильтрация листингов; стабильный снапшот для offset-протокола FUSE |
| OIDC: `Validator`, interceptors, `TokenManager`, `Config` | `pkg/oidc/{validator,interceptor,token_manager,config}.go` | JWKS-валидация (go-oidc v3.20.0), strict/non-strict interceptors, клиентский token manager (prOOrc/oidc-login) |
| `AuthzInterceptor`, `requiredPermission`, `isAlwaysAllowed` | `pkg/meta/authz_interceptor.go` | Маппинг RPC→право, fail-closed, root short-circuit |
| `platformAuthzClient` | `pkg/meta/authz_client.go` | gRPC-клиент agio-platform (mTLS опционально), proto `authz_pb/authz.proto` |
| `cachingAuthzClient` | `pkg/meta/authz_cache.go` | Кэш решений: TTL 30s, size 10000, bulk partial cache, admin key `__admin__` |
| `InodePathCache` | `pkg/meta/inode_path_cache.go` | Двусторонний inode↔path FIFO-кэш (root pinned, hardlink-safe, dir trailing slash) |
| `grpcMeta`, `withAuth`, `startHeartbeat`, `doHeartbeat` | `pkg/meta/grpc_client*.go` | Клиент: чистый прокси (Variant B), LRU-кэши, singleflight, heartbeat FlushSession 5s |
| Proto | `pkg/meta/pb/*.proto`, `pkg/meta/authz_pb/authz.proto` | `MetaService` (78 RPC), `MetaContext{uid,gid,gids,pid,check_permission,session_id}`, authz-контракт |

## Execution Flow

1. **Вход:** gRPC-запрос → StrictOIDC interceptor (JWKS-валидация, claims в context) → AuthzInterceptor (unary): `requiredPermission(method)` → None: пропуск; Admin: `CheckOrganizationAdmin`; file-level: `resolveChecks` через InodePathCache → `CheckPermission`/`CheckBulkPermissions` (через кэш) → deny: `PermissionDenied`.
2. **Хендлер:** `metaCtx()` восстанавливает uid/gid/pid из `MetaContext` (нулевой → Background); RPC делегируется в `Meta`; мутации обновляют InodePathCache.
3. **Листинг (authz):** `DirHandlerList` → первый вызов: полный `Readdir` → срез `.`/`..` → `CheckBulkPermissions(View)` → кэш `entry.authzList` → offset по отфильтрованному потоку.
4. **Клиент:** `grpcMeta` — cache-first GetAttr/Readdir; мутации инвалидируют кэши; `withAuth` — singleflight-токен + `x-session-id`; heartbeat = `FlushSession` каждые 12s (timeout 5s, ошибки debug).

## Invariants

- Fail-closed: любая неопределённость authz (ошибка сервиса, нерезолвлённый путь, пустой userID, mismatch длины bulk-результата) → deny/пустой листинг.
- Root `/` + View — единственный bypass без вызова authz; company owner bypass отсутствует (as-is).
- Кэш authz хранит только успешные решения; ошибки всегда уходят в live-вызов.
- InodePathCache: root pinned; hardlink — first-seen wins; rename в себя/потомка запрещён guard'ом.
- Authz покрывает только unary RPC; streaming (DumpMeta/LoadMeta) защищены лишь OIDC.
- Клиентский `grpcMeta` не содержит baseMeta — вся семантика на сервере.

## Decisions & Rationale

- **Strict OIDC на всех RPC (включая lifecycle):** единая точка входа, whitelist не нужен; backward-compatible non-strict interceptors оставлены в `pkg/oidc`, но cmd использует только strict.
- **Unary-only authz:** streaming RPC (backup) — админские операции, покрываются OIDC + (отсутствующим) admin-гейтом; при включении authz логируется warning.
- **Снапшот листинга вместо post-filter по cursor'у:** post-filter рассинхронизирует FUSE offset-протокол и даёт дубликаты (регрессия покрыта `TestDirHandlerList_Authz_NoDuplicateEntries`).
- **InodePathCache на сервере:** handle-based RPC не несут путей; кэш строится из мутающих/lookup RPC, root pinned для стабильности.
- **Клиент: чистый прокси (Variant B) + LRU-кэши 1s:** минимальная задержка при приемлемой stale-окне; инвалидация на мутациях.

## Integration Points

- **agio-platform:** gRPC `AuthzService` (`CheckPermission`, `CheckBulkPermissions` — до 1000 путей в батче, `CheckOrganizationAdmin`); mTLS опционально (все три `--authz-tls-*`), иначе insecure; `volume_name` — proto-поле запросов.
- **Ory Hydra/Kratos:** OIDC discovery + JWKS по `--oidc-issuer`; клиентский token manager кэширует токен в `~/.juicefs/oidc_cache/token.json`.
- **Клиенты:** AGIO Client (FUSE/File Provider) через `grpcMeta`; web UI — через backend API → Meta Proxy.

## Known Deviations

As-is отклонения, зафиксированные как кандидаты на fix-change:

1. **`--oidc-audience` — мёртвый флаг:** объявлен в CLI, но нигде не читается; audience-валидации нет. Кандидат: реализовать валидацию или удалить флаг.
2. **`ListLocks` не реализован на сервере** (в proto есть) → `codes.Unimplemented`.
3. **`GetDirStat` всегда возвращает `ENOSYS`** — в ответе нет поля для stat; RPC фактически мёртвый.
4. **`Resolve` всегда denied** при включённом authz («deny for PoC — multi-component paths not fully verified»).
5. **Streaming RPC не покрыты authz** (unary-only interceptor) — DumpMeta/LoadMeta доступны любому с валидным OIDC-токеном.
6. **gRPC-клиент только insecure** (`grpc.WithInsecure()`) — TLS на клиенте отсутствует.
7. **`NewSession` sid только для `*redisMeta`** — другие бэкенды: sid=0 + error log (клиент продолжает работать без session id).
8. **Нет интеграционных тестов meta-proxy с включённым authz** — authz покрыт только юнит-тестами с mockAuthzClient.

## Open Questions

- [ ] Audience: реализовать валидацию `--oidc-audience` или удалить флаг?
- [ ] TLS на клиенте `grpcMeta` — когда (зависит от сетевой модели AGIO Client)?
- [ ] `Resolve`: реализовать валидацию multi-component путей или оставить denied?
- [ ] Authz для streaming RPC: отдельный stream-interceptor или явный запрет без admin-токена?
