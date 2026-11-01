# Delta: domain-meta-proxy

## MODIFIED Requirements

### Requirement: Authorization interceptor

The system SHALL install a unary authz interceptor that maps each RPC to a required permission level (None, View, Read, Write, Admin) and enforces it via the agio-platform `AuthzService` gRPC (`CheckPermission`, `CheckBulkPermissions`, `CheckOrganizationAdmin`). Rules: lifecycle and DirHandler RPCs require no check; admin operations require `CheckOrganizationAdmin`; file-level operations require `CheckPermission` on resolved paths with fail-closed semantics (path not resolved even after inode path recovery, authz error, or empty user id → deny); the user id SHALL be the OIDC token `sub` claim without any mapping. Special mappings: `Open` requires Write when flags include write access else Read; `Access` maps W_OK→Write, R_OK→Read, else View; `Rename` requires Write on both source and destination parents; `Link` requires Read on the source inode and Write on the destination parent; `CopyFileRange` requires Read on the source and Write on the destination; `Resolve` SHALL be denied for all users; `NewSlice` (slice-id allocation, which carries no inode and writes no data) SHALL be allowed for any authenticated user without an authz call — a slice becomes user-visible data only when committed via `Write`, which is path-checked. An unknown RPC method SHALL be denied with `PermissionDenied`. Inode path recovery (see the Inode path recovery requirement) SHALL run before the fail-closed deny for inode-only operations — the interceptor's default inode branch (GetAttr, SetAttr, CheckSetAttr, Truncate, Fallocate, ReadLink, Read, Write, extended-attribute and lock operations) and its Open, Access and NewDirHandler branches — and for the parent inode of `Lookup`. Parent-bearing mutating RPCs (Create, Mkdir, Mknod, Symlink, Unlink, Rmdir, Rename, Link, CopyFileRange) SHALL keep the plain cache check and deny when a parent path is missing: the kernel re-issues Lookups (entry/attr timeouts of ~1s) that repopulate parent paths, so recovery is not invoked for them and the miss window is transient and fail-closed. When the engine reports the inode missing, the RPC SHALL fail with ENOENT; any other recovery failure SHALL preserve the deny.

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

### Requirement: Inode-to-path cache

The server SHALL maintain a bidirectional inode↔path cache (FIFO eviction, max size from `--authz-path-cache-size`, default 100000; 0 = unlimited) used to resolve authz paths for handle-based RPCs. The root inode SHALL map to `/` and be pinned against eviction. Hard links SHALL keep the first-seen path (later `Set` calls for the same inode are rejected). Directory paths SHALL be stored with a trailing slash. Mutating RPCs SHALL maintain the cache: Lookup/Mknod/Mkdir/Create/Symlink add entries, Unlink removes by path, Rmdir removes the subtree, Rename moves and renames subtrees (with a guard against renaming a directory into itself or its descendant). The cache SHALL be a derivative structure: the metadata engine (`attr.Parent` chain) is the source of truth, and on a miss the Inode path recovery SHALL repopulate entries with first-seen-wins semantics. Rename/Unlink/Rmdir SHALL also invalidate the affected recovered paths through the recovery resolver.

Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_RootPinned
Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_HardlinkSafety
Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_FIFO
Verified-by: pkg/meta/inode_path_cache_test.go::TestInodePathCache_RenameSubtree_SelfGuard
Verified-by: pkg/meta/inode_path_recovery_test.go::TestRecovery_PopulatesAncestorsFirstSeenWins

#### Scenario: Rename updates all descendant paths

- **WHEN** a directory with cached descendants is renamed
- **THEN** all cached descendant paths SHALL be rewritten under the new parent path

#### Scenario: Cold cache repopulates from the engine

- **WHEN** a proxy restarts (cache reduced to the pinned root) and a client continues to issue handle-based requests for existing inodes
- **THEN** the first request per inode repopulates the chain of ancestor paths from the engine and subsequent requests hit the cache

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

## ADDED Requirements

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
