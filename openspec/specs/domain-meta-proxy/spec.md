# domain-meta-proxy Specification

## Purpose
gRPC-прокси JuiceFS metadata для внешних клиентов AGIO Drive: единая точка контроля доступа (OIDC authn + authz через agio-platform) над metadata-движком, stateful readdir-хендлеры и gRPC-клиент `grpcMeta` с клиентскими кэшами.

## Requirements

### Requirement: Meta Proxy server and CLI

The system SHALL provide a `juicefs meta-proxy` command that wraps a metadata engine (Redis or PostgreSQL URL via `--meta-backend`, default `redis://localhost:6379/0`) with a single gRPC service `MetaService` listening on `--addr` (default `:9561`). Flags SHALL include: `--grpc-max-send-msg-size` / `--grpc-max-recv-msg-size` (256 MB), `--grpc-keepalive-time` (1h), `--grpc-keepalive-timeout` (20s), keepalive enforcement MinTime 5s, `--oidc-issuer`, `--oidc-client-id`, `--authz-service`, `--authz-volume-name`, `--authz-path-cache-size` (100000), `--authz-cache-ttl` (30s), and `--tls-cert` / `--tls-key`. When both `--tls-cert` and `--tls-key` are set, the server SHALL serve the `MetaService` over TLS (server-side certificate, minimum version TLS 1.2); otherwise the listener SHALL remain plaintext. When only one of the two flags is set, the server SHALL refuse to start with a clear error. On SIGINT/SIGTERM the server SHALL perform a graceful stop with a 30-second timeout before forced stop and metadata shutdown.

Verified-by: cmd/meta_proxy_test.go::TestMetaProxyTLS

#### Scenario: TLS listener starts with certificate pair

- **WHEN** `juicefs meta-proxy` is started with valid `--tls-cert` and `--tls-key`
- **THEN** the gRPC listener SHALL accept TLS handshakes with minimum protocol version 1.2

#### Scenario: Incomplete TLS configuration refuses to start

- **WHEN** exactly one of `--tls-cert` / `--tls-key` is provided
- **THEN** the server SHALL exit at startup with an error naming the missing flag

#### Scenario: Authn and authz are opt-in by flags

- **WHEN** `--oidc-issuer` is empty
- **THEN** no OIDC interceptor SHALL be installed and requests pass without a token; the same SHALL hold for `--authz-service` and the authz interceptor

### Requirement: Credential masking in command logs

Команда `juicefs meta-proxy` SHALL NOT выводить в логи пароль metadata-движка: любое лог-сообщение, содержащее `--meta-backend` URL, SHALL печатать URL с замаскированным паролем (формат `redis://:****@host:port/db`) — тем же способом, что и лог metadata-адреса в `meta.NewClient`. Маскировка SHALL применяться безусловно, на всех уровнях логирования.

#### Scenario: Startup log masks the backend password

- **WHEN** прокси запускается с `--meta-backend` URL, содержащим пароль
- **THEN** строка лога с metadata backend URL содержит `:****@` и не содержит подстроку пароля

#### Scenario: No unmasked backend URL log statements remain

- **WHEN** выполняется поиск лог-вызовов с `metaBackendUrl` (или иным meta URL) без маскировки в `cmd/meta_proxy.go`
- **THEN** такие вызовы отсутствуют — каждый вывод URL проходит через `utils.RemovePassword`

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

When `--authz-service` is set, the server SHALL install a unary authz interceptor that maps each RPC to a required permission level (None, View, Read, Write, Admin) and enforces it via the agio-platform `AuthzService` gRPC (`CheckPermission`, `CheckBulkPermissions`, `CheckOrganizationAdmin`). Rules: lifecycle and DirHandler RPCs require no check; admin operations require `CheckOrganizationAdmin`; file-level operations require `CheckPermission` on resolved paths with fail-closed semantics (path not resolved even after inode path recovery, authz error, or empty user id → deny); the user id SHALL be the OIDC token `sub` claim without any mapping. Special mappings: `Open` requires Write when flags include write access else Read; `Access` maps W_OK→Write, R_OK→Read, else View; `Rename` requires Write on both source and destination parents; `Link` requires Read on the source inode and Write on the destination parent; `CopyFileRange` requires Read on the source and Write on the destination; `Resolve` SHALL be denied for all users; `Chroot` — although listed among admin operations in «gRPC service surface» — SHALL be an exception: it requires `View` via `CheckPermission` on the absolute volume path `"/" + <normalized subdir>` (instead of `CheckOrganizationAdmin`), and a deny SHALL fail the RPC with `PermissionDenied` leaving the caller's session root unchanged; `NewSlice` (slice-id allocation, which carries no inode and writes no data) SHALL be allowed for any authenticated user without an authz call — a slice becomes user-visible data only when committed via `Write`, which is path-checked. An unknown RPC method SHALL be denied with `PermissionDenied`. Inode path recovery (see the Inode path recovery requirement) SHALL run before the fail-closed deny for inode-only operations — the interceptor's default inode branch (GetAttr, SetAttr, CheckSetAttr, Truncate, Fallocate, ReadLink, Read, Write, extended-attribute and lock operations) and its Open, Access and NewDirHandler branches — and for the parent inode of `Lookup`. Parent-bearing mutating RPCs (Create, Mkdir, Mknod, Symlink, Unlink, Rmdir, Rename, Link, CopyFileRange) SHALL keep the plain cache check and deny when a parent path is missing: the kernel re-issues Lookups (entry/attr timeouts of ~1s) that repopulate parent paths, so recovery is not invoked for them and the miss window is transient and fail-closed. When the engine reports the inode missing, the RPC SHALL fail with ENOENT; any other recovery failure SHALL preserve the deny.

Verified-by: pkg/meta/authz_interceptor_test.go::TestRequiredPermission_View
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_DeniesUnauthorizedAccess
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_RenameRequiresBothParents
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_DeniesNonAdminOnAdminOps
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_DenyOnAuthzError
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_NewSlice_Allowed
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_UnresolvedPathRecoveredByEngine
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_RecoveryGoneReturnsENOENT
Verified-by: pkg/meta/authz_interceptor_test.go::TestInterceptor_RecoveryFailureStillDenies

#### Scenario: Authz service error fails closed

- **WHEN** `CheckPermission` returns an error for a file-level operation
- **THEN** the RPC SHALL fail with `PermissionDenied` ("access denied")

#### Scenario: Resolve is always denied

- **WHEN** any authenticated user calls `Resolve` while authz is enabled
- **THEN** the call SHALL be denied with `PermissionDenied`

#### Scenario: Cache miss recovers the path from the engine before the decision

- **WHEN** a request references an inode that is not in the inode→path cache (fresh proxy after a restart) and the inode exists in the metadata engine
- **THEN** the proxy reconstructs the path via the engine's parent chain, and the authz decision is made against the recovered path exactly as if it had been cached

#### Scenario: Recovery still respects authorization

- **WHEN** a user references an inode they are not authorized for, whose path is unknown to the proxy cache
- **THEN** recovery finds the true path but the authz service denies the operation and the RPC fails with `PermissionDenied`

#### Scenario: Chroot is path-checked instead of admin-checked

- **WHEN** an authenticated user without organization-admin rights sends `Chroot` with subdir `companies/A` while having `View` on `/companies/A`
- **THEN** the decision is made via `CheckPermission` (View on `/companies/A`) and, on allow, the RPC proceeds to the handler

### Requirement: Root short-circuit and directory listing filter

The path `/` with permission View SHALL be allowed without an authz call (root short-circuit); all other paths and permissions SHALL go through the authz service. In authz mode, `DirHandlerList` SHALL return a stable filtered snapshot: on the first call the server performs a full `Readdir`, removes `.`/`..`, filters entries with `CheckBulkPermissions` (View) for the caller, caches the allowed entries in the dir-handler entry, and serves subsequent offsets from that filtered list. When the parent directory path cannot be resolved even after inode path recovery, the listing SHALL fail with `EACCES`; an empty listing SHALL mean only that the path is known and no entries passed the filter. Filtering SHALL be fail-closed: empty user id, bulk-check error, or result-length mismatch SHALL yield an empty listing. Only allowed entries SHALL be added to the inode→path cache.

Verified-by: pkg/meta/authz_interceptor_test.go::TestAuthzInterceptor_RootPathViewAlwaysAllowed
Verified-by: pkg/meta/grpc_server_dir_handler_test.go::TestDirHandlerList_Authz_NoDuplicateEntries
Verified-by: pkg/meta/grpc_server_dir_handler_test.go::TestDirHandlerList_Authz_AllDenied
Verified-by: pkg/meta/grpc_server_dir_handler_test.go::TestDirHandlerList_Authz_UnresolvableParentIsEACCES

#### Scenario: Denied entries are invisible in listings

- **WHEN** a user lists a directory containing entries they cannot view
- **THEN** the listing SHALL contain only the allowed entries and repeated calls with increasing offsets SHALL not produce duplicates

#### Scenario: Unresolvable directory is an error, not an empty listing

- **WHEN** a client lists a directory whose path cannot be resolved even after recovery
- **THEN** the listing fails with `EACCES`; a legitimately empty result is produced only when the directory path is known and zero entries pass the filter

### Requirement: Authz decision cache

Authz decisions SHALL be cached per (user id, path, permission, operation context) key with a configurable TTL (`--authz-cache-ttl`, default 30s; TTL ≤ 0 disables caching) and a bounded size (default 10000 entries). Only successful decisions SHALL be cached; authz errors SHALL always trigger a live call. Bulk checks SHALL use partial caching (only cache misses are sent to the service in one batch). Organization-admin status SHALL be cached under a dedicated key with the same TTL. When the cache is full, expired entries SHALL be evicted first; if it remains full, new decisions SHALL not be cached.

The operation context (see "Authz operation context pass-through") SHALL be part of the cache key, so a decision granted for a directory creation on one path can never be served for a different operation on the same path.

Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_CachesResult
Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_TTLExpiry
Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_ErrorNotCached
Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_DifferentOps
Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_MaxSizeEviction

#### Scenario: A mkdir grant is not served for another operation

- **WHEN** a directory creation on the companies namespace root was allowed and cached, and the same user then attempts a different operation on the same path within the cache TTL
- **THEN** the cached mkdir decision is not reused — the check goes live and the other operation is denied per its own rule

#### Scenario: Expired decision triggers a live call

- **WHEN** a cached decision older than the TTL is requested
- **THEN** the authz service SHALL be called again and the cache entry refreshed on success

### Requirement: Authz operation context pass-through

The meta proxy SHALL classify modifying metadata RPCs and forward an
operation context to the authorization service on every authz check:
directory-creation operations (Mkdir) SHALL be reported as `MKDIR`, all other
operations as the default. The classification SHALL NOT embed any storage- or
company-specific naming: it is a generic operation kind. Proxies built from
sources without the field (older clients against a newer authz service) and
newer proxies against an older authz service SHALL behave exactly as before
(the field is optional with a non-mkdir default).

Verified-by: pkg/meta/meta_proxy_authz_op_test.go::TestAuthzOpContext_MkdirClassified
Verified-by: pkg/meta/meta_proxy_authz_op_test.go::TestAuthzOpContext_OtherOpsDefault

#### Scenario: Mkdir reaches the authz service flagged as a directory creation

- **WHEN** a client issues Mkdir through the proxy and the proxy performs its authz check
- **THEN** the check request carries the mkdir operation context, and the service's decision (allow for the companies namespace root, deny otherwise) applies

### Requirement: Inode-to-path cache

The server SHALL maintain a bidirectional inode↔path cache (FIFO eviction, max size from `--authz-path-cache-size`, default 100000; 0 = unlimited) used to resolve authz paths for handle-based RPCs. The root inode SHALL be pinned against eviction, and its path SHALL be resolved per the requesting session: `/` for a session without `Chroot`, `"/" + <normalized subdir>` for a chrooted session; stored entries SHALL remain full volume paths. Hard links SHALL keep the first-seen path (later `Set` calls for the same inode are rejected). Directory paths SHALL be stored with a trailing slash. Mutating RPCs SHALL maintain the cache: Lookup/Mknod/Mkdir/Create/Symlink add entries, Unlink removes by path, Rmdir removes the subtree, Rename moves and renames subtrees (with a guard against renaming a directory into itself or its descendant). The cache SHALL be a derivative structure: the metadata engine (`attr.Parent` chain) is the source of truth, and on a miss the Inode path recovery SHALL repopulate entries with first-seen-wins semantics. Rename/Unlink/Rmdir SHALL also invalidate the affected recovered paths through the recovery resolver.

Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_RootPinned
Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_HardlinkSafety
Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_FIFO
Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_RenameSubtree_SelfGuard
Verified-by: pkg/meta/inode_path_recovery_test.go::TestRecovery_PopulatesAncestorsFirstSeenWins
Verified-by: pkg/meta/grpc_server_session_test.go::TestMetaProxyAuthzPathsUnderChroot

#### Scenario: Rename updates all descendant paths

- **WHEN** a directory with cached descendants is renamed
- **THEN** all cached descendant paths SHALL be rewritten under the new parent path

#### Scenario: Cold cache repopulates from the engine

- **WHEN** a proxy restarts (cache reduced to the pinned root) and a client continues to issue handle-based requests for existing inodes
- **THEN** the first request per inode repopulates the chain of ancestor paths from the engine and subsequent requests hit the cache

#### Scenario: Root path follows the requesting session

- **WHEN** a chrooted session with subdir `companies/A` and a session without `Chroot` resolve the root inode
- **THEN** the resolved paths are `/companies/A` and `/` respectively, and a child entry `foo` builds as `/companies/A/foo` and `/foo`

### Requirement: Inode path recovery

On an inode→path cache miss, the proxy SHALL reconstruct the missing path server-side by walking the metadata engine's parent chain (`attr.Parent` up to the engine root) and repopulate the cache with every ancestor entry (first-seen-wins). The reconstruction SHALL use only internal engine reads — it SHALL NOT pass through the authz interceptor and SHALL NOT require any caller authorization: it re-establishes a server-side fact, and the subsequent authz decision remains unchanged. The reconstruction SHALL be bounded (maximum depth 64 with a visited-set cycle guard) and SHALL deduplicate concurrent reconstructions of the same inode (singleflight: at most one engine walk per inode at a time). When the engine reports the inode missing, the RPC SHALL fail with ENOENT; cycle detection, depth overrun, and engine errors SHALL fail closed with the same deny as an unresolved path. Recovery SHALL be lazy (on miss only, no warm-up at proxy start). Rename/Unlink/Rmdir SHALL invalidate the affected entries through the recovery resolver.

Verified-by: pkg/meta/inode_path_recovery_test.go::TestRecovery_LinearChain
Verified-by: pkg/meta/inode_path_recovery_test.go::TestRecovery_TreeSharedAncestors
Verified-by: pkg/meta/inode_path_recovery_test.go::TestRecovery_CycleFailsClosed
Verified-by: pkg/meta/inode_path_recovery_test.go::TestRecovery_DepthCapFailsClosed
Verified-by: pkg/meta/inode_path_recovery_test.go::TestRecovery_MissingInodeIsGone
Verified-by: pkg/meta/inode_path_recovery_test.go::TestRecovery_EngineErrorFailsClosed
Verified-by: pkg/meta/inode_path_recovery_test.go::TestRecovery_ConcurrentMissesSingleWalk
Verified-by: pkg/meta/inode_path_recovery_test.go::TestRecovery_Invalidate

#### Scenario: Deep path is reconstructed after a proxy restart

- **WHEN** a client holds inodes below the root and the proxy cache is cold, and the client issues GetAttr/Open/Lookup for those inodes
- **THEN** the proxy reconstructs the full parent chain from the engine in bounded time and the operations proceed through normal authorization

#### Scenario: Reconstruction of a vanished inode returns ENOENT

- **WHEN** the referenced inode no longer exists in the metadata engine
- **THEN** the RPC fails with ENOENT instead of a permission denial

#### Scenario: Concurrent misses of the same inode trigger one walk

- **WHEN** several requests miss the same inode simultaneously
- **THEN** the engine is walked exactly once and all callers receive the recovered path

### Requirement: Stateful directory handler

The server SHALL support stateful readdir via `NewDirHandler` (returns a uint64 handle), `DirHandlerList(handle, offset)`, `DirHandlerInsert`, `DirHandlerDelete` and `DirHandlerClose`. Handles SHALL be allocated incrementally; operations on an unknown handle SHALL return errno `EBADF`. Without authz, `DirHandlerList` SHALL delegate to the underlying handler's offset-based listing.

Verified-by: pkg/meta/grpc_server_dir_handler_test.go::TestDirHandlerList_NoAuthz_Unchanged

#### Scenario: Unknown handle is rejected

- **WHEN** a client calls `DirHandlerList` with a handle that was never created or already closed
- **THEN** the response SHALL carry errno `EBADF`

### Requirement: gRPC metadata client

The system SHALL provide a `grpcMeta` metadata driver (registered as `grpc`, pure proxy without baseMeta embedding) connecting to `grpc://host:port[/volume?query]`. Query parameters with defaults: `attr-cache-size` (100000), `dir-cache-size` (5000), `attr-cache-ttl` (1s), `dir-cache-ttl` (1s), `heartbeat-interval` (12s); OIDC parameters (`oidc-issuer`, `oidc-client-id` — required when issuer is set, `oidc-client-secret`, `oidc-redirect-url`, `oidc-scopes`, `oidc-cache-dir`); TLS parameters (`tls`, `tls-ca`, `tls-server-name`). Without `tls=1` the connection SHALL be plaintext (current behavior). With `tls=1` the client SHALL dial with TLS: with `tls-ca` the root CA pool SHALL be loaded from the given file, otherwise the system pool SHALL be used; `tls-server-name` SHALL override the server name used for certificate verification. A missing or unreadable `tls-ca` file SHALL fail client creation. Outgoing requests SHALL carry `authorization: Bearer <token>` (token acquisition coalesced via singleflight) and `x-session-id` when a session exists. After `NewSession` the client SHALL send a heartbeat every `heartbeat-interval` implemented as a `FlushSession` RPC with a 5-second timeout; heartbeat errors SHALL be logged at debug level only. gRPC status mapping: PermissionDenied/Unauthenticated → EACCES, NotFound → ENOENT, AlreadyExists → EEXIST, InvalidArgument → EINVAL, other → EIO. This mapping SHALL apply uniformly to Lookup, GetAttr and NewDirHandler (denied requests surface as `EACCES`, not a generic EIO). When the connection is restored after a drop and a subdir is configured, the client SHALL re-issue Chroot before replaying the write journal. A DirHandler operation answered with `EBADF` SHALL drop the local handle; the next listing from offset 0 SHALL recreate it.

Verified-by: pkg/meta/grpc_client_test.go::TestGRPCMetaCreation
Verified-by: pkg/meta/grpc_client_test.go::TestBuildClientTLSConfig
Verified-by: pkg/meta/grpc_client_test.go::TestGRPCMetaTLSQueryParsing
Verified-by: pkg/meta/grpc_client_auth_test.go::TestWithAuthSingleflightCoalescing
Verified-by: pkg/meta/grpc_client_auth_test.go::TestWithAuthWithoutOIDC
Verified-by: pkg/meta/grpc_client_test.go::TestDeniedLookupMapsToEACCES
Verified-by: pkg/meta/grpc_client_test.go::TestReconnectReissuesChroot

#### Scenario: Default connection stays plaintext

- **WHEN** a client mounts `grpc://host:port` without the `tls` query parameter
- **THEN** the client SHALL dial the server without transport credentials (backward compatible with existing deployments)

#### Scenario: TLS connection with custom CA

- **WHEN** a client mounts `grpc://host:port?tls=1&tls-ca=/path/ca.pem&tls-server-name=proxy.example.com`
- **THEN** the client SHALL verify the server certificate against the CA file using the overridden server name

#### Scenario: Unreadable CA file fails client creation

- **WHEN** a client mounts with `tls=1&tls-ca=/nonexistent/ca.pem`
- **THEN** client creation SHALL fail with an error mentioning the CA file path

#### Scenario: Concurrent token requests are coalesced

- **WHEN** multiple goroutines need an OIDC token simultaneously while none is cached
- **THEN** exactly one authentication flow SHALL execute and all callers SHALL receive its result

#### Scenario: Denied Lookup surfaces as EACCES

- **WHEN** the authz interceptor denies a Lookup and the client translates the status
- **THEN** the client returns `EACCES` (not a generic EIO)

### Requirement: Client-side attribute and directory caching

The client SHALL cache attributes (inode → Attr) and directory listings (inode → entries) in expirable LRU caches with the configured sizes and TTLs. `GetAttr` SHALL be cache-first; `Readdir` SHALL use the cache only when attribute results are requested. Mutating operations (SetAttr, Mkdir, Create, Unlink, Rmdir, Rename, Link, Symlink, Truncate, Fallocate, Write) SHALL invalidate the affected inode entries in both caches.

#### Scenario: Mutation invalidates cached attributes

- **WHEN** a client writes to or renames an object it previously read
- **THEN** subsequent `GetAttr`/`Readdir` for the affected inodes SHALL fetch fresh data from the server

### Requirement: Plaintext FEK delivery requires TLS

On an encrypted volume, the server SHALL return the plaintext FEK in `OpenResponse.fek` (and `GetFileFEK`-derived fields) only when the RPC arrived over a TLS connection. When an encrypted-file `Open` arrives over a plaintext connection, the server SHALL NOT deliver the plaintext FEK and SHALL respond with `codes.Unauthenticated`; the client SHALL map it to `EACCES` per the existing status mapping. On volumes with `encryption_enabled=false` the check SHALL NOT apply. The check SHALL be enforced regardless of the server's own listener mode (a plaintext listener on an encrypted volume simply never qualifies).

Verified-by: pkg/meta/grpc_server_fuse_test.go::TestOpenFEKPlaintextRefused
Verified-by: pkg/meta/grpc_server_fuse_test.go::TestOpenFEKOverTLS
Verified-by: pkg/meta/grpc_server_fuse_test.go::TestResolveFileKeyOverTLS
Verified-by: pkg/meta/grpc_server_fuse_test.go::TestOpenUnencryptedVolumeUnaffected
Verified-by: pkg/meta/encrypt_integration_test.go::TestEncryptedOpenPlaintextRefused

#### Scenario: Open on encrypted volume over plaintext connection

- **WHEN** a client connected without TLS opens an encrypted file on a volume with `encryption_enabled=true`
- **THEN** the server SHALL respond with `Unauthenticated` and the response SHALL NOT contain the plaintext FEK

#### Scenario: Open on encrypted volume over TLS connection

- **WHEN** a client connected with TLS opens an encrypted file on a volume with `encryption_enabled=true`
- **THEN** the server SHALL return the plaintext FEK in `OpenResponse.fek` subject to the existing authz checks

#### Scenario: Unencrypted volumes unaffected

- **WHEN** a client connected without TLS opens a file on a volume with `encryption_enabled=false`
- **THEN** the server SHALL behave exactly as before this change (no TLS requirement)

### Requirement: Authz mode disables engine-level POSIX access checks

When `--authz-service` is configured, the server SHALL construct meta contexts for all RPCs with `CheckPermission() == false`, so engine-level stored-POSIX access enforcement (`meta.baseMeta.Access` and every engine call site, e.g. the Mknod parent check) SHALL be skipped, and the authz interceptor SHALL remain the only access gate. When `--authz-service` is not configured, meta contexts SHALL keep `CheckPermission() == true` (unchanged behavior). Stored metadata, quotas, FEK-over-TLS enforcement, OIDC validation, and the authz interceptor contract SHALL remain unchanged.

Verified-by: pkg/meta/grpc_server_test.go::TestMetaCtxCheckPermissionAuthzMode
Verified-by: pkg/meta/base_test.go::TestAccessCheckPermissionGate

#### Scenario: Foreign-uid client writes into stored-owned directory with authz allowed

- **WHEN** the proxy runs with `--authz-service`, an authenticated client carries POSIX uid that differs from the stored owner of a `0755` directory, and the authz interceptor allows the operation
- **THEN** the create SHALL succeed (no `EACCES` from the engine-level POSIX check) and the new inode SHALL be stored with the client-carried uid

#### Scenario: Authz denial is not affected

- **WHEN** the proxy runs with `--authz-service` and the authz interceptor denies an operation
- **THEN** the RPC SHALL fail with `PermissionDenied` (fail-closed) regardless of the carried POSIX uid, including uid 0

#### Scenario: Without authz the behavior is unchanged

- **WHEN** the proxy runs without `--authz-service` and a client carries a uid that the engine-level POSIX check denies
- **THEN** the operation SHALL fail with `EACCES` exactly as before this change

#### Scenario: Local (non-proxy) mounts are unaffected

- **WHEN** a JuiceFS client mounts an engine directly (no meta proxy) with the same engine version
- **THEN** engine-level POSIX access checks SHALL behave exactly as before this change (gated by the local FUSE context `CheckPermission()`)

### Requirement: Per-session proxy state

MetaProxyServer SHALL привязывать каждое клиентское gRPC-соединение ровно к одной сессии прокси. Сессия SHALL создаваться при открытии соединения, SHALL иметь уникальный числовой id и SHALL завершаться при закрытии соединения. Meta-RPC и STS RPC на соединении без привязанной сессии SHALL отклоняться и SHALL NOT вызывать ни один движок.

Verified-by: pkg/meta/grpc_server_session_test.go::TestMetaProxySessionLifecycleOnConnection; pkg/meta/grpc_server_session_test.go::TestMetaProxyNoSessionFailsClosed; pkg/meta/grpc_server_session_test.go::TestMetaProxySessionCleanupOnConnEnd

#### Scenario: Connection opens a session
- **WHEN** прокси принимает клиентское gRPC-соединение
- **THEN** создаётся сессия с уникальным числовым id `<N>` и в лог пишется `meta-proxy session started id=<N>` уровня Info

#### Scenario: Meta RPC without session fails closed
- **WHEN** Meta-RPC приходит на соединении без привязанной сессии
- **THEN** прокси отвечает `codes.Internal` и ни один метод ни одного движка не вызывается

#### Scenario: STS RPC without session is rejected
- **WHEN** STS RPC приходит на соединении без привязанной сессии
- **THEN** прокси отклоняет его с `codes.Internal` и учётные данные не выдаются

#### Scenario: Streaming RPC is bound to the connection session
- **WHEN** клиент вызывает стриминговый RPC (`ScanSlices`, `ScanDeletedObject`, `ScanChangelog`, `DumpMeta`, `LoadMeta`, `DumpMetaV2`, `LoadMetaV2`)
- **THEN** RPC выполняется на движке сессии своего соединения, а без сессии отклоняется с `codes.Internal` и движок не вызывается

#### Scenario: Connection closes its session
- **WHEN** соединение закрывается
- **THEN** сессия завершается ровно один раз: закрываются её dir-хендлы, освобождается её ссылка на движок пула, пишется `meta-proxy session closed id=<N> chroot=<root path> uptime=<duration>` уровня Info

#### Scenario: Session end is idempotent
- **WHEN** соединение закрывается после того, как сессия уже завершилась по RPC `CloseSession`
- **THEN** ссылка на движок не освобождается повторно и `session closed` пишется один раз

### Requirement: Chroot-scoped engine pool

MetaProxyServer SHALL обслуживать Meta-RPC из реестра движков с ключом «нормализованный subdir chroot» (`path.Clean("/"+subdir)` без ведущего `/`; ключ `""` — корень тома) вместо одного общего движка. Сессии без Chroot SHALL использовать движок с ключом `""`; сессии, чрутнутые в один и тот же subdir, SHALL использовать один движок этого subdir.

Движок SHALL создаваться фабрикой при первом приобретении своего ключа и SHALL попадать в реестр в состоянии «создан» только после того, как на свежем движке выполнены `Load(true)`, `Chroot(key)` и `NewSession`. Корень движка пула SHALL записываться ровно один раз — при создании. `Chroot` SHALL NOT вызываться на движке с ключом `""`.

Базовый движок прокси SHALL оставаться отдельным, SHALL использоваться только `pathResolver` и SHALL NOT быть доступен обработчикам RPC. `NewMetaProxyServer` SHALL принимать фабрику движков третьим аргументом.

Каждая запись реестра SHALL вести счётчик привязанных сессий. При обнулении счётчика запись SHALL сниматься с реестра, а движок SHALL разбираться в фоновой горутине: `CloseSession` не более одного раза (бюджет 5 секунд), затем `Shutdown` ровно один раз.

Verified-by: pkg/meta/grpc_server_session_test.go::TestMetaProxyEngineSharedPerSubdir; pkg/meta/grpc_server_session_test.go::TestMetaProxyEngineRefcountTeardown; pkg/meta/grpc_server_session_test.go::TestMetaProxyNewSessionIdempotentPerEngine; pkg/meta/grpc_server_session_test.go::TestMetaProxyEngineReconnectKeepsEngine; pkg/meta/grpc_server_session_test.go::TestMetaProxyEngineReacquireAfterTeardown; pkg/meta/grpc_server_session_test.go::TestMetaProxyEngineBoundedBySessions

#### Scenario: Two sessions with the same subdir share one engine
- **WHEN** две сессии выполняют Chroot в один и тот же нормализованный subdir
- **THEN** фабрика вызывается один раз и обе сессии обслуживаются одним экземпляром движка

#### Scenario: Different subdirs get different engines
- **WHEN** сессия A чрутится в `companies/acme`, а сессия B — в `companies/globex`
- **THEN** каждая обслуживается собственным движком, и содержимое корня одной сессии недоступно и не видно другой

#### Scenario: Sessions without Chroot share the root engine
- **WHEN** две сессии не вызывают Chroot
- **THEN** обе обслуживаются движком с ключом `""`, корень которого — `RootInode`

#### Scenario: Concurrent first acquire creates a single engine
- **WHEN** несколько сессий одновременно чрутятся в subdir, движок которого ещё создаётся
- **THEN** создаётся ровно один движок и все сессии привязываются к нему

#### Scenario: Last session releases the engine
- **WHEN** завершается последняя сессия ключа
- **THEN** запись снимается с реестра, движок разбирается в фоне (`CloseSession` — не более одного раза, `Shutdown` — ровно один раз) и пишется `meta-proxy engine closed subdir=<path> refs=0 uptime=<duration>`

#### Scenario: Re-acquire after teardown
- **WHEN** новая сессия чрутится в subdir, движок которого уже разобран
- **THEN** фабрика вызывается снова и создаётся новый движок с новой сессией движка

#### Scenario: Reconnect while the group is alive
- **WHEN** сессия ключа завершается, а другая сессия того же ключа ещё жива, и затем подключается новая сессия того же ключа
- **THEN** новый движок не создаётся и сессия движка не переоткрывается

#### Scenario: Chroot RPC never re-chroots a published engine
- **WHEN** Chroot RPC обслуживается движком, уже находящимся в реестре
- **THEN** `Chroot` на этом движке не вызывается и его корень не меняется

#### Scenario: Base engine is unreachable from handlers
- **WHEN** обработчики Meta-RPC нуждаются в движке
- **THEN** они получают его только через пул по сессии; поле `meta` в `MetaProxyServer` отсутствует

#### Scenario: Engine count never exceeds session count
- **WHEN** каждая живая сессия чрутится в отдельный subdir, а затем сессии завершаются
- **THEN** число движков в реестре не превышает числа живых сессий и падает до нуля после их завершения

#### Scenario: Service connections use the pool
- **WHEN** соединение с service identity выполняет Meta-RPC или Chroot
- **THEN** оно привязывается к сессии и обслуживается из пула по тем же правилам, что и пользовательские

### Requirement: Session limit

Прокси SHALL принимать флаг `--max-sessions` (по умолчанию `256`; `0` — без лимита), ограничивающий число живых сессий, то есть клиентских соединений. Соединение, открытое при достигнутом лимите, SHALL NOT привязываться к сессии; его Meta-RPC SHALL завершаться с `codes.ResourceExhausted`.

Verified-by: pkg/meta/grpc_server_session_test.go::TestMetaProxyMaxSessions; pkg/meta/grpc_server_session_test.go::TestMetaProxyEngineBoundedBySessions

#### Scenario: Limit reached
- **WHEN** живых сессий уже `--max-sessions` и открывается новое соединение
- **THEN** соединение не получает сессию, его Meta-RPC завершаются с `codes.ResourceExhausted` и пишется Warn `meta-proxy session rejected id=<N> reason=max_sessions limit=<L>`

#### Scenario: Below the limit
- **WHEN** число живых сессий меньше лимита
- **THEN** новое соединение получает сессию и обслуживается обычным образом

#### Scenario: Limit disabled
- **WHEN** `--max-sessions 0`
- **THEN** лимит не применяется

#### Scenario: Engines are bounded by subdirs
- **WHEN** каждая живая сессия чрутится в отдельный subdir
- **THEN** число движков пула равно числу таких subdir и не превышает лимита сессий

### Requirement: Per-session chroot isolation

RPC `Chroot` SHALL менять корень только сессии, которая его отправила: сессия переключается на движок пула своего subdir, а корни всех остальных сессий SHALL оставаться неизменными. Chroot SHALL быть одноразовым для сессии: исход первого вызова финален. Ответ `Chroot` SHALL содержать только errno.

Verified-by: pkg/meta/grpc_server_session_test.go::TestMetaProxyPerSessionChrootIsolation; pkg/meta/grpc_server_session_test.go::TestMetaProxyChrootIdempotency

#### Scenario: Chroot applies only to the calling session
- **WHEN** сессия A выполняет Chroot в `companies/acme`, а сессия B продолжает работу
- **THEN** корень B не меняется независимо от того, чрутнута ли B, куда и в каком порядке

#### Scenario: Chrooted session is confined to its subtree
- **WHEN** сессия, чрутнутая в `companies/<code>`, читает свой корень и создаёт в нём файл
- **THEN** возвращаются только записи этого поддерева, а файл создаётся внутри него

#### Scenario: Same subdir is replayed without touching engines
- **WHEN** сессия, уже выполнявшая Chroot в subdir K, повторяет Chroot с K
- **THEN** возвращается errno первого вызова и ни один движок не вызывается

#### Scenario: Another subdir is rejected
- **WHEN** сессия, уже выполнявшая Chroot, вызывает его с другим subdir
- **THEN** прокси отвечает `codes.FailedPrecondition`, движок не вызывается, пишется Warn `meta-proxy chroot rejected id=<N> user=<user> subdir=<path> reason=session_already_chrooted`

#### Scenario: Successful Chroot is logged
- **WHEN** Chroot сессии завершается успешно
- **THEN** пишется Info `meta-proxy chroot applied id=<N> user=<user> subdir=<path>`

### Requirement: Chroot target authorization

В authz-режиме Chroot пользовательского identity SHALL требовать `AuthzPermissionView` на `"/"+<нормализованный subdir>` через `CheckPermission` (проверка выполняется в interceptor авторизации). Отказ SHALL давать `codes.PermissionDenied` без смены корня; путь, который не вычислим, SHALL давать `codes.PermissionDenied`.

В authz-режиме Chroot пользовательского identity SHALL возвращать `syscall.ENOENT`, если цель не существует, причём SHALL NOT создавать каталоги, и `syscall.ENOTDIR`, если цель — не каталог. Проверка выполняется lookup-проходом от `RootInode` на движке `""` до приобретения движка цели.

В не-authz режиме Chroot SHALL работать как сегодня, включая создание отсутствующих каталогов, и логироваться. Запрос service identity не проходит interceptor авторизации и SHALL обрабатываться с семантикой цели «как сегодня».

Verified-by: pkg/meta/meta_proxy_authz_op_test.go::TestMetaProxyChrootAuthz; pkg/meta/authz_interceptor_test.go::TestRequiredPermissionChroot; pkg/meta/grpc_server_session_test.go::TestMetaProxyChrootServiceIdentity

#### Scenario: View on the target is granted
- **WHEN** пользовательский identity с `AuthzPermissionView` на `/companies/acme` вызывает Chroot `companies/acme`
- **THEN** запрос доходит до обработчика и применяется

#### Scenario: View on the target is denied
- **WHEN** у identity нет `AuthzPermissionView` на `/companies/acme`
- **THEN** прокси отвечает `codes.PermissionDenied`, движок цели не приобретается, корень не меняется

#### Scenario: Target is missing in authz mode
- **WHEN** в authz-режиме пользовательский identity чрутится в несуществующий subdir
- **THEN** ответ несёт `syscall.ENOENT`, каталоги в хранилище не создаются, пишется Warn `meta-proxy chroot rejected id=<N> user=<user> subdir=<path> reason=target_missing`

#### Scenario: Target is not a directory in authz mode
- **WHEN** в authz-режиме цель или её компонент — не каталог
- **THEN** ответ несёт `syscall.ENOTDIR` и пишется Warn с `reason=target_not_dir`

#### Scenario: Non-authz mode keeps current behavior
- **WHEN** authz выключен и цель не существует
- **THEN** движок создаёт каталоги как сегодня и пишется `meta-proxy chroot applied id=<N> user=<user> subdir=<path>`

#### Scenario: Service identity Chroot
- **WHEN** Chroot вызывает соединение с service identity
- **THEN** проверка `AuthzPermissionView` не применяется, запрос обрабатывается в общем пуле и логируется с `user=service`

#### Scenario: Engine is unavailable
- **WHEN** создание движка цели или lookup цели завершается ошибкой, отличной от `ENOENT`/`ENOTDIR`
- **THEN** прокси отвечает `codes.Unavailable`, состояние сессии не меняется, пишется Warn `meta-proxy chroot rejected id=<N> user=<user> subdir=<path> reason=engine_error`

### Requirement: Session-scoped authz paths under chroot

`InodePathCache` SHALL вычислять путь inode 1 как корень сессии из контекста запроса: `"/"+<нормализованный subdir>` для чрутнутой сессии и `"/"` для нечрутнутой. `InodePathCache.Get` и `InodePathCache.BuildChildPath` SHALL принимать контекст запроса. Кеш SHALL хранить и возвращать только полные пути тома. Проверки авторизации для сессии под chroot SHALL вычисляться на полных путях тома.

Verified-by: pkg/meta/grpc_server_session_test.go::TestMetaProxyAuthzPathsUnderChroot

#### Scenario: Root listing under chroot
- **WHEN** сессия, чрутнутая в `companies/acme`, читает свой корень
- **THEN** каждая запись авторизуется по пути `/companies/acme/<name>`, а не по `/<name>`

#### Scenario: Create in the session root
- **WHEN** сессия, чрутнутая в `companies/acme`, создаёт `<name>` в своём корне
- **THEN** Write проверяется на `/companies/acme/<name>`, а не на `/`, и пользователь с правом Write в поддереве получает успех вместо `EACCES`

#### Scenario: Un-chrooted session
- **WHEN** нечрутнутая сессия обращается к inode 1
- **THEN** путь равен `/`

#### Scenario: Cached paths are valid across sessions
- **WHEN** путь inode закеширован одной сессией и запрошен другой
- **THEN** возвращается полный путь тома

### Requirement: Directory handle session binding

Dir-хендлы прокси SHALL принадлежать сессии, открывшей их. Обращение к хендлу из другой сессии SHALL возвращать `EBADF`, в том числе когда обе сессии обслуживаются одним движком пула. При завершении сессии её dir-хендлы SHALL закрываться.

Verified-by: pkg/meta/grpc_server_session_test.go::TestMetaProxyDirHandleSessionBinding

#### Scenario: Handle from another session is rejected
- **WHEN** сессия B использует dir-хендл, открытый сессией A
- **THEN** прокси возвращает `EBADF`

#### Scenario: Handle shared inside one engine group is rejected
- **WHEN** сессии A и B чрутнуты в один subdir и обслуживаются одним движком
- **THEN** хендл A для B всё равно возвращает `EBADF`

#### Scenario: Session end closes its handles
- **WHEN** соединение сессии закрывается
- **THEN** все её dir-хендлы закрываются и не остаются в `s.handlers`

### Requirement: Session lifecycle RPC idempotency

`NewSession` движка SHALL выполняться ровно один раз за жизнь движка пула — при его создании. RPC `NewSession` SHALL NOT вызывать `NewSession` движка и SHALL возвращать ответ, сохранённый при создании движка; все сессии группы SHALL получать одинаковый ответ. `CloseSession` движка SHALL выполняться не более одного раза за жизнь движка — при его teardown. RPC `CloseSession` SHALL завершать только сессию отправителя.

Verified-by: pkg/meta/grpc_server_session_test.go::TestMetaProxyNewSessionIdempotentPerEngine; pkg/meta/grpc_server_session_test.go::TestMetaProxyCloseSessionRPCReleasesOnlyCaller; pkg/meta/grpc_server_session_test.go::TestMetaProxyEngineRefcountTeardown

#### Scenario: NewSession RPC does not touch the engine
- **WHEN** сессии группы вызывают RPC `NewSession` однократно или повторно
- **THEN** `NewSession` движка не вызывается и каждый вызов возвращает сохранённый ответ, включая одинаковый sid

#### Scenario: Reconnect without NewSession RPC
- **WHEN** клиент переподключается и не повторяет RPC `NewSession`
- **THEN** его новая сессия обслуживается движком с уже открытой сессией движка

#### Scenario: CloseSession RPC releases only the caller
- **WHEN** одну из сессий группы вызывает RPC `CloseSession`, а другие сессии группы живы
- **THEN** завершается только сессия отправителя, сессия движка остаётся открытой и остальные сессии продолжают работу

#### Scenario: RPCs after CloseSession RPC fail closed
- **WHEN** на том же соединении после RPC `CloseSession` приходит Meta-RPC
- **THEN** он отклоняется с `codes.Internal` и движок не вызывается

### Requirement: Chroot and session event logging

Прокси SHALL писать события через логгер `pkg/meta` в формате key=value. Поле `user` SHALL принимать `sub` из OIDC, значение `service` для service identity и `-`, когда identity нет.

Verified-by: pkg/meta/grpc_server_session_test.go::TestMetaProxySessionAuditLogs

#### Scenario: Session events
- **WHEN** соединение открывается, отклоняется по лимиту или закрывается
- **THEN** пишутся соответственно Info `meta-proxy session started id=<N>`, Warn `meta-proxy session rejected id=<N> reason=max_sessions limit=<L>`, Info `meta-proxy session closed id=<N> chroot=<root path> uptime=<duration>`

#### Scenario: Engine pool events
- **WHEN** движок пула создаётся, не создаётся или разбирается
- **THEN** пишутся соответственно Info `meta-proxy engine created subdir=<path>`, Warn `meta-proxy engine create failed subdir=<path> err=<error>`, Info `meta-proxy engine closed subdir=<path> refs=0 uptime=<duration>`; если `CloseSession` не уложился в бюджет, уровень Warn и добавляется `close_timeout=true`

#### Scenario: Chroot events
- **WHEN** Chroot сессии применяется или отклоняется
- **THEN** пишется Info `meta-proxy chroot applied id=<N> user=<user> subdir=<path>` либо Warn `meta-proxy chroot rejected id=<N> user=<user> subdir=<path> reason=<session_already_chrooted|target_missing|target_not_dir|engine_error>`

#### Scenario: Engine unavailable outside Chroot
- **WHEN** сессии не удаётся приобрести движок вне Chroot
- **THEN** пишется Warn `meta-proxy session engine failed id=<N> err=<error>`

#### Scenario: Path rendering
- **WHEN** событие относится к движку или сессии ключа `""`
- **THEN** путь печатается как `/`

### Requirement: Authz decision logging

Отказы авторизации SHALL логироваться на уровне Warn в формате `authz denied user=<user> method=<method> path=<path> perm=<perm>`; успешные решения SHALL оставаться на уровне Trace. `user` SHALL равняться `sub` из OIDC. `RemovePassword` SHALL NOT меняться.

Verified-by: pkg/meta/authz_interceptor_test.go::TestAuthzDenyLoggedAtWarn

#### Scenario: Deny is logged at Warn
- **WHEN** interceptor авторизации отклоняет запрос
- **THEN** пишется Warn `authz denied user=<user> method=<method> path=<path> perm=<perm>` и ни один отказ не остаётся на уровне Debug

#### Scenario: Allow stays at Trace
- **WHEN** interceptor авторизации разрешает запрос
- **THEN** решение логируется на уровне Trace, как раньше
