# domain-meta-proxy — Delta

## ADDED Requirements

### Requirement: Authz mode disables engine-level POSIX access checks

When `--authz-service` is configured, the server SHALL construct meta contexts for all RPCs with `CheckPermission() == false`, so engine-level stored-POSIX access enforcement (`meta.baseMeta.Access` and every engine call site, e.g. the Mknod parent check) SHALL be skipped, and the authz interceptor SHALL remain the only access gate. When `--authz-service` is not configured, meta contexts SHALL keep `CheckPermission() == true` (unchanged behavior). Stored metadata, quotas, FEK-over-TLS enforcement, OIDC validation, and the authz interceptor contract SHALL remain unchanged.

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

## Verified-by

- `pkg/meta/grpc_server_test.go::TestMetaCtxCheckPermissionAuthzMode` (планируется задачами change: authz вкл. → `CheckPermission()==false`, authz выкл. → `true`)
- `pkg/meta/base_test.go::TestAccessCheckPermissionGate` (планируется задачами change: гейт `baseMeta.Access` при `CheckPermission()==false` возвращает 0 без обращения к движку; `uid==0` bypass без изменений)
