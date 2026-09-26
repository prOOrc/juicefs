# domain-meta-proxy

## Purpose

Дельта change `implement-encrypt` (шифрование agio Drive) расширяет gRPC-поверхность Meta Proxy: новые RPC (`ResolveFileKey`, `RotateFileKey`, `RotateFileKeysByPaths`), крипто-поля на существующих сообщениях (`OpenResponse.fek/fek_version/encrypted`, `CreateRequest.drive_file_id`, `FlushSessionResponse.permission_generation`, `Format.encryption_enabled/kek_version`, `ProtoSlice.wrapped_cek`) и authz-маппинг новых RPC. Модифицируются только перечисленные ниже требования; остальное поведение капабилити (OIDC, кэши, DirHandler) не меняется. Поведенческий контракт шифрования — в дельте `domain-encrypt` этого же change.

## MODIFIED Requirements

### Requirement: gRPC service surface

The server SHALL implement the `MetaService` gRPC service (81 RPCs: the 78 pre-encryption RPCs plus `ResolveFileKey`, `RotateFileKey` and `RotateFileKeysByPaths`) covering lifecycle (Init, Load, NewSession, CloseSession, FlushSession, GetSession, ListSessions, CleanStaleSessions), core FUSE operations, data path (Read, Write, NewSlice, InvalidateChunkCache, CopyFileRange), locks (Flock, Getlk, Setlk), xattrs, POSIX ACL (SetFacl/GetFacl passthrough), format tokens, admin operations (GetFormat, Remove, Clone, Check, Compact, quota, scans, Chroot, trash cleanup), DirHandler (NewDirHandler, DirHandlerList, DirHandlerInsert, DirHandlerDelete, DirHandlerClose) and streaming backup (DumpMeta, LoadMeta, DumpMetaV2, LoadMetaV2 with 64KB chunks). Message extensions carried by this service: `OpenResponse` fields `fek` (bytes, TLS-only), `fek_version`, `encrypted`; `CreateRequest` field `drive_file_id` (client-generated UUID); `FlushSessionResponse` field `permission_generation`; `Format` fields `encryption_enabled`, `kek_version`; `ProtoSlice` field `wrapped_cek`. The `ListLocks` RPC SHALL return `codes.Unimplemented`. `GetDirStat` SHALL return errno `ENOSYS` in every response. `NewSession` SHALL return a non-zero session id only when the wrapped metadata engine is `*redisMeta`; for other engines it SHALL log an error and return sid 0.

#### Scenario: Unimplemented RPC is reported as such

- **WHEN** a client calls `ListLocks`
- **THEN** the server SHALL respond with gRPC status `Unimplemented`

#### Scenario: Encryption fields ride existing messages

- **WHEN** a client performs Create, Open or FlushSession on an encrypted volume
- **THEN** the corresponding requests/responses SHALL carry the crypto fields (`CreateRequest.drive_file_id`; `OpenResponse.fek`/`fek_version`/`encrypted` with the plaintext FEK only in `OpenResponse` over TLS; `FlushSessionResponse.permission_generation`) without introducing additional round-trips

### Requirement: Authorization interceptor

When `--authz-service` is set, the server SHALL install a unary authz interceptor that maps each RPC to a required permission level (None, View, Read, Write, Admin) and enforces it via the agio-platform `AuthzService` gRPC (`CheckPermission`, `CheckBulkPermissions`, `CheckOrganizationAdmin`). Rules: lifecycle and DirHandler RPCs require no check; admin operations require `CheckOrganizationAdmin`; file-level operations require `CheckPermission` on resolved paths with fail-closed semantics (unresolved path, authz error, or empty user id → deny); the user id SHALL be the OIDC token `sub` claim without any mapping. Special mappings: `Open` requires Write when flags include write access else Read; `Access` maps W_OK→Write, R_OK→Read, else View; `Rename` requires Write on both source and destination parents; `Link` requires Read on the source inode and Write on the destination parent; `CopyFileRange` requires Read on the source and Write on the destination; `Resolve` SHALL be denied for all users. Encryption RPCs: `ResolveFileKey` requires Read on the resolved path (fail-closed on unresolved inode); `RotateFileKey` and `RotateFileKeysByPaths` are admin operations requiring `CheckOrganizationAdmin`, with the acting `admin_user_id` taken from the OIDC `sub` claim. An unknown RPC method SHALL be denied with `PermissionDenied`.

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

#### Scenario: Rotation RPCs are org-admin gated

- **WHEN** a user without organization-admin status calls `RotateFileKey` or `RotateFileKeysByPaths` while authz is enabled
- **THEN** the call SHALL be denied via `CheckOrganizationAdmin` (fail-closed), and the audit trail SHALL record the acting `admin_user_id` from the OIDC `sub`

#### Scenario: ResolveFileKey requires Read on the target path

- **WHEN** a user without Read permission on the resolved path calls `ResolveFileKey`
- **THEN** the call SHALL be denied and no plaintext FEK SHALL be returned
