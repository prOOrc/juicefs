# domain-meta-proxy

## Purpose

gRPC-прокси JuiceFS metadata для внешних клиентов AGIO Drive: единая точка контроля доступа (OIDC authn + authz через agio-platform) над metadata-движком, stateful readdir-хендлеры и gRPC-клиент `grpcMeta` с клиентскими кэшами.

## ADDED Requirements

### Requirement: Meta Proxy server and CLI

The system SHALL provide a `juicefs meta-proxy` command that wraps a metadata engine (Redis or PostgreSQL URL via `--meta-backend`, default `redis://localhost:6379/0`) with a single gRPC service `MetaService` listening on `--addr` (default `:9561`). Flags SHALL include: `--grpc-max-send-msg-size` / `--grpc-max-recv-msg-size` (256 MB), `--grpc-keepalive-time` (1h), `--grpc-keepalive-timeout` (20s), keepalive enforcement MinTime 5s, `--oidc-issuer`, `--oidc-client-id`, `--authz-service`, `--authz-volume-name`, `--authz-path-cache-size` (100000), `--authz-cache-ttl` (30s). On SIGINT/SIGTERM the server SHALL perform a graceful stop with a 30-second timeout before forced stop and metadata shutdown.

#### Scenario: Authn and authz are opt-in by flags

- **WHEN** `--oidc-issuer` is empty
- **THEN** no OIDC interceptor SHALL be installed and requests pass without a token; the same SHALL hold for `--authz-service` and the authz interceptor

### Requirement: gRPC service surface

The server SHALL implement the `MetaService` gRPC service (78 RPCs) covering lifecycle (Init, Load, NewSession, CloseSession, FlushSession, GetSession, ListSessions, CleanStaleSessions), core FUSE operations, data path (Read, Write, NewSlice, InvalidateChunkCache, CopyFileRange), locks (Flock, Getlk, Setlk), xattrs, POSIX ACL (SetFacl/GetFacl passthrough), format tokens, admin operations (GetFormat, Remove, Clone, Check, Compact, quota, scans, Chroot, trash cleanup), DirHandler (NewDirHandler, DirHandlerList, DirHandlerInsert, DirHandlerDelete, DirHandlerClose) and streaming backup (DumpMeta, LoadMeta, DumpMetaV2, LoadMetaV2 with 64KB chunks). The `ListLocks` RPC SHALL return `codes.Unimplemented`. `GetDirStat` SHALL return errno `ENOSYS` in every response. `NewSession` SHALL return a non-zero session id only when the wrapped metadata engine is `*redisMeta`; for other engines it SHALL log an error and return sid 0.

#### Scenario: Unimplemented RPC is reported as such

- **WHEN** a client calls `ListLocks`
- **THEN** the server SHALL respond with gRPC status `Unimplemented`

### Requirement: OIDC authentication

When `--oidc-issuer` is set, the server SHALL install strict OIDC interceptors on all unary and streaming RPCs. Tokens SHALL be validated against the issuer's JWKS (signature, issuer, expiry) using the bearer token from the `authorization` metadata; the `client_id` claim SHALL be checked only when `--oidc-client-id` is set. A missing token SHALL yield `codes.Unauthenticated` ("authentication required"); an invalid or expired token SHALL yield `codes.Unauthenticated` ("invalid or expired token"). Validated ID token claims SHALL be stored in the request context. There SHALL be no audience validation and no RPC whitelist — every RPC, including lifecycle RPCs, requires a valid token.

Verified-by: pkg/oidc/interceptor_test.go::TestStrictUnaryInterceptorNoToken
Verified-by: pkg/oidc/interceptor_test.go::TestStrictUnaryInterceptorInvalidToken
Verified-by: pkg/oidc/interceptor_test.go::TestUnaryInterceptorValidToken

#### Scenario: Missing token is rejected in strict mode

- **WHEN** a request arrives without `authorization` metadata while `--oidc-issuer` is configured
- **THEN** the RPC SHALL fail with `Unauthenticated` before reaching the handler

### Requirement: Authorization interceptor

When `--authz-service` is set, the server SHALL install a unary authz interceptor that maps each RPC to a required permission level (None, View, Read, Write, Admin) and enforces it via the agio-platform `AuthzService` gRPC (`CheckPermission`, `CheckBulkPermissions`, `CheckOrganizationAdmin`). Rules: lifecycle and DirHandler RPCs require no check; admin operations require `CheckOrganizationAdmin`; file-level operations require `CheckPermission` on resolved paths with fail-closed semantics (unresolved path, authz error, or empty user id → deny); the user id SHALL be the OIDC token `sub` claim without any mapping. Special mappings: `Open` requires Write when flags include write access else Read; `Access` maps W_OK→Write, R_OK→Read, else View; `Rename` requires Write on both source and destination parents; `Link` requires Read on the source inode and Write on the destination parent; `CopyFileRange` requires Read on the source and Write on the destination; `Resolve` SHALL be denied for all users. An unknown RPC method SHALL be denied with `PermissionDenied`.

Verified-by: pkg/meta/authz_interceptor_test.go::TestRequiredPermission_View
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_DeniesUnauthorizedAccess
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_RenameRequiresBothParents
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_DeniesNonAdminOnAdminOps
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_DenyOnAuthzError

#### Scenario: Authz service error fails closed

- **WHEN** `CheckPermission` returns an error for a file-level operation
- **THEN** the RPC SHALL fail with `PermissionDenied` ("access denied")

#### Scenario: Resolve is always denied

- **WHEN** any authenticated user calls `Resolve` while authz is enabled
- **THEN** the call SHALL be denied with `PermissionDenied`

### Requirement: Root short-circuit and directory listing filter

The path `/` with permission View SHALL be allowed without an authz call (root short-circuit); all other paths and permissions SHALL go through the authz service. In authz mode, `DirHandlerList` SHALL return a stable filtered snapshot: on the first call the server performs a full `Readdir`, removes `.`/`..`, filters entries with `CheckBulkPermissions` (View) for the caller, caches the allowed entries in the dir-handler entry, and serves subsequent offsets from that filtered list. Filtering SHALL be fail-closed: empty user id, parent path not in cache, bulk-check error, or result-length mismatch SHALL yield an empty listing. Only allowed entries SHALL be added to the inode→path cache.

Verified-by: pkg/meta/authz_interceptor_test.go::TestAuthzInterceptor_RootPathViewAlwaysAllowed
Verified-by: pkg/meta/grpc_server_dir_handler_test.go::TestDirHandlerList_Authz_NoDuplicateEntries
Verified-by: pkg/meta/grpc_server_dir_handler_test.go::TestDirHandlerList_Authz_AllDenied

#### Scenario: Denied entries are invisible in listings

- **WHEN** a user lists a directory containing entries they cannot view
- **THEN** the listing SHALL contain only the allowed entries and repeated calls with increasing offsets SHALL not produce duplicates

### Requirement: Authz decision cache

Authz decisions SHALL be cached per (user id, path, permission) key with a configurable TTL (`--authz-cache-ttl`, default 30s; TTL ≤ 0 disables caching) and a bounded size (default 10000 entries). Only successful decisions SHALL be cached; authz errors SHALL always trigger a live call. Bulk checks SHALL use partial caching (only cache misses are sent to the service in one batch). Organization-admin status SHALL be cached under a dedicated key with the same TTL. When the cache is full, expired entries SHALL be evicted first; if it remains full, new decisions SHALL not be cached.

Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_CachesResult
Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_TTLExpiry
Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_ErrorNotCached
Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_MaxSizeEviction

#### Scenario: Expired decision triggers a live call

- **WHEN** a cached decision older than the TTL is requested
- **THEN** the authz service SHALL be called again and the cache entry refreshed on success

### Requirement: Inode-to-path cache

The server SHALL maintain a bidirectional inode↔path cache (FIFO eviction, max size from `--authz-path-cache-size`, default 100000; 0 = unlimited) used to resolve authz paths for handle-based RPCs. The root inode SHALL map to `/` and be pinned against eviction. Hard links SHALL keep the first-seen path (later `Set` calls for the same inode are rejected). Directory paths SHALL be stored with a trailing slash. Mutating RPCs SHALL maintain the cache: Lookup/Mknod/Mkdir/Create/Symlink add entries, Unlink removes by path, Rmdir removes the subtree, Rename moves and renames subtrees (with a guard against renaming a directory into itself or its descendant).

Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_RootPinned
Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_HardlinkSafety
Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_FIFO
Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_RenameSubtree_SelfGuard

#### Scenario: Rename updates all descendant paths

- **WHEN** a directory with cached descendants is renamed
- **THEN** all cached descendant paths SHALL be rewritten under the new parent path

### Requirement: Stateful directory handler

The server SHALL support stateful readdir via `NewDirHandler` (returns a uint64 handle), `DirHandlerList(handle, offset)`, `DirHandlerInsert`, `DirHandlerDelete` and `DirHandlerClose`. Handles SHALL be allocated incrementally; operations on an unknown handle SHALL return errno `EBADF`. Without authz, `DirHandlerList` SHALL delegate to the underlying handler's offset-based listing.

Verified-by: pkg/meta/grpc_server_dir_handler_test.go::TestDirHandlerList_NoAuthz_Unchanged

#### Scenario: Unknown handle is rejected

- **WHEN** a client calls `DirHandlerList` with a handle that was never created or already closed
- **THEN** the response SHALL carry errno `EBADF`

### Requirement: gRPC metadata client

The system SHALL provide a `grpcMeta` metadata driver (registered as `grpc`, pure proxy without baseMeta embedding) connecting to `grpc://host:port[/volume?query]`. Query parameters with defaults: `attr-cache-size` (100000), `dir-cache-size` (5000), `attr-cache-ttl` (1s), `dir-cache-ttl` (1s), `heartbeat-interval` (12s); OIDC parameters (`oidc-issuer`, `oidc-client-id` — required when issuer is set, `oidc-client-secret`, `oidc-redirect-url`, `oidc-scopes`, `oidc-cache-dir`). The connection SHALL be insecure (no TLS). Outgoing requests SHALL carry `authorization: Bearer <token>` (token acquisition coalesced via singleflight) and `x-session-id` when a session exists. After `NewSession` the client SHALL send a heartbeat every `heartbeat-interval` implemented as a `FlushSession` RPC with a 5-second timeout; heartbeat errors SHALL be logged at debug level only. gRPC status mapping: PermissionDenied/Unauthenticated → EACCES, NotFound → ENOENT, AlreadyExists → EEXIST, InvalidArgument → EINVAL, other → EIO.

Verified-by: pkg/meta/grpc_client_test.go::TestGRPCMetaCreation
Verified-by: pkg/meta/grpc_client_auth_test.go::TestWithAuthSingleflightCoalescing
Verified-by: pkg/meta/grpc_client_auth_test.go::TestWithAuthWithoutOIDC

#### Scenario: Concurrent token requests are coalesced

- **WHEN** multiple goroutines need an OIDC token simultaneously while none is cached
- **THEN** exactly one authentication flow SHALL execute and all callers SHALL receive its result

### Requirement: Client-side attribute and directory caching

The client SHALL cache attributes (inode → Attr) and directory listings (inode → entries) in expirable LRU caches with the configured sizes and TTLs. `GetAttr` SHALL be cache-first; `Readdir` SHALL use the cache only when attribute results are requested. Mutating operations (SetAttr, Mkdir, Create, Unlink, Rmdir, Rename, Link, Symlink, Truncate, Fallocate, Write) SHALL invalidate the affected inode entries in both caches.

#### Scenario: Mutation invalidates cached attributes

- **WHEN** a client writes to or renames an object it previously read
- **THEN** subsequent `GetAttr`/`Readdir` for the affected inodes SHALL fetch fresh data from the server
