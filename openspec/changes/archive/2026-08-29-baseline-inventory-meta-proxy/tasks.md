# Tasks: baseline-inventory-meta-proxy

Инвентаризационный change: задачи — исследование кода, написание spec, верификация утверждений. Продакшен-код не меняется.

## 1. Research

- [x] Прочитать CLI и запуск: `cmd/meta_proxy.go` (флаги, дефолты, цепочка интерцепторов, graceful shutdown).
- [x] Прочитать gRPC-сервер: `pkg/meta/grpc_server.go` + `grpc_server_*.go` (78 RPC по группам, DirHandler, admin, streaming).
- [x] Прочитать authn: `pkg/oidc/{validator,interceptor,token_manager,config}.go`.
- [x] Прочитать authz: `pkg/meta/authz_interceptor.go`, `authz_client.go`, `authz_cache.go`, `inode_path_cache.go`, `authz_pb/authz.proto`.
- [x] Прочитать клиент: `pkg/meta/grpc_client*.go` (grpcMeta, кэши, heartbeat, singleflight).
- Проверка: `grep -c "rpc " pkg/meta/pb/meta.proto` — 78 RPC.

## 2. Spec

- [x] Написать delta-спеку `specs/domain-meta-proxy/spec.md`: 10 требований (server/CLI, service surface, OIDC authn, authz interceptor, root short-circuit + listing filter, authz cache, InodePathCache, DirHandler, gRPC client, client caching).
- [x] Verified-by строки только для требований с существующими тестами.

## 3. Verification

- [x] Прогнать unit-тесты meta proxy: `go test ./pkg/meta/ ./pkg/oidc/ -short -count=1 -run 'TestAuthz|TestInterceptor|TestIsAlwaysAllowed|TestCachingAuthzClient|TestInodePathCache|TestDirHandlerList|TestGRPCMeta|TestWithAuth|TestUnaryInterceptor|TestStrict'` — PASS.
- [x] Spot-check grep-ами: `:9561`, keepalive MinTime 5s, graceful stop 30s; `--oidc-audience` объявлен но не читается; `ListLocks` отсутствует в сервере; `GetDirStat` → ENOSYS; `Resolve` → deny (PoC); root short-circuit только `/`+View; `defaultAuthzCacheSize = 10_000`; клиентские дефолты 100_000/5_000/1s/12s; `grpc.WithInsecure()`; heartbeat = FlushSession timeout 5s; `NewSession` sid только для `*redisMeta`.
- [x] Подтвердить Known Deviations по коду (8 пунктов в design.md).

## Definition of Done

- [x] `go build ./...` — PASS.
- [x] `go vet ./pkg/meta/ ./pkg/oidc/` — PASS.
- [x] Unit-тесты meta proxy (команда выше) — PASS.
- [x] `openspec validate baseline-inventory-meta-proxy` — PASS.
