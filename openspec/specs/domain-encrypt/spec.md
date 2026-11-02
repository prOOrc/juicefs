# domain-encrypt Specification

## Purpose

Подсистема шифрования agio Drive (Per-File FEK + Per-Chunk CEK, двухуровневый envelope): защита содержимого файлов в S3, изоляция per-file через authz-gated выдачу FEK, zero-copy Clone/Compaction через CEK re-wrap, render-доступ без OIDC через Company KEK, no-residuality и отзыв доступа.

Капабилити greenfield: в SoT её нет до применения этого change; целевой контракт зафиксирован здесь (перенесён из отозванной ветки `inventory-encrypt`, которая не соответствовала семантике SoT для нереализованной капабилити).

## Requirements

### Requirement: Key hierarchy and binary formats

The system SHALL use a four-level key hierarchy: KMS Master Key → Company KEK (one per company, stored in YC Lockbox wrapped by the KMS Master Key) → FEK (one random 32-byte AES-256-GCM key per file/inode) → CEK (one random 32-byte AES-256-GCM key per slice). All keys SHALL be generated with a CSPRNG. Three binary blob formats SHALL be used, each starting with a 4-byte magic and a 1-byte version (currently 1): `AGFK` (wrapped FEK: kek_version uint32, nonce 12 bytes, ciphertext 32 bytes, GCM tag 16 bytes; fixed size 69 bytes), `AGCK` (wrapped CEK: nonce 12 bytes, ciphertext 32 bytes, tag 16 bytes; fixed size 65 bytes), `AGDF` (encrypted block in S3: nonce 12 bytes, ciphertext N bytes, tag 16 bytes; overhead over plaintext is 33 bytes). All multi-byte integers SHALL be big-endian. AAD composition SHALL be: for `AGFK` — volume_uuid, company_id, drive_file_id, inode, fek_version; for `AGCK` — drive_file_id, slice_id, fek_version; for `AGDF` — slice_id, block_index. The exact byte encoding is: string fields are length-prefixed (u32 big-endian length followed by UTF-8 bytes) and concatenated in the order listed, followed by the integer fields — i.e. `AGFK` AAD = len(volume_uuid):4 ‖ volume_uuid ‖ len(company_id):4 ‖ company_id ‖ len(drive_file_id):4 ‖ drive_file_id ‖ inode:8 ‖ fek_version:4; `AGCK` AAD = len(drive_file_id):4 ‖ drive_file_id ‖ slice_id:8 ‖ fek_version:4; `AGDF` AAD = slice_id:8 ‖ block_index:4 (the encoding is additionally pinned by cross-repo known-answer test vectors). The GCM nonce SHALL be unique per write operation with the same key. Plaintext CEK/FEK/Company KEK SHALL NOT be written to any durable store (Redis, PostgreSQL, S3, files, logs); plaintext is allowed only in RAM for the duration of an operation.

#### Scenario: Wrap and unwrap round-trip

- **WHEN** a FEK is wrapped with a Company KEK into `AGFK` and later unwrapped with the same KEK and identical AAD
- **THEN** the original 32-byte FEK SHALL be recovered and the embedded kek_version SHALL be returned

#### Scenario: Tampered blob fails closed

- **WHEN** any byte of an `AGFK`, `AGCK` or `AGDF` blob (ciphertext or tag) is modified after encryption
- **THEN** decryption/unwrap SHALL return an error and no plaintext SHALL be produced

#### Scenario: AAD mismatch fails closed

- **WHEN** an `AGFK` or `AGCK` blob is unwrapped with a different drive_file_id, inode or fek_version than at wrap time
- **THEN** unwrap SHALL return an error

### Requirement: Crypto metadata in Redis

The inode attribute (`Attr`) SHALL carry persisted crypto fields — `encrypted` (bool), `drive_file_id` (UUID), `fek_version` (uint32), `crypto_alg` (string, `"AES-256-GCM"`), `wrapped_fek` (bytes, `AGFK` format) — as a variable-length suffix so that `wrapped_fek` is read in the same Redis round-trip as `GetAttr` (zero additional requests). A transient plaintext FEK field on `Attr` SHALL NEVER be marshaled into Redis. The slice record SHALL keep its fixed 24-byte base layout and carry an optional length-prefixed `AGCK` blob (`wrapped_cek`); records without the blob SHALL remain valid legacy plaintext records. The volume `Format` SHALL carry `encryption_enabled` (bool) and `kek_version` (int32). No file path SHALL be part of any AAD, so rename does not invalidate wrapped keys. An encrypted volume SHALL require the Redis metadata engine: the administrative enable-encryption operation SHALL refuse to run on non-Redis engines, and the crypto metadata operations (`SetFileCrypto`, `RewrapSlices`) are Redis-only. Metadata dump and load (JSON and binary V2, including V2 streamed through Meta Proxy) SHALL carry the crypto metadata losslessly: `wrapped_fek`, `drive_file_id`, `fek_version` and `crypto_alg` in the inode attribute and the per-slice `wrapped_cek` survive byte-for-byte, and a volume restored from either format reads its files back with decryption. Both formats SHALL stay backward compatible — dumps written before these fields existed load with all crypto fields at zero values, and the new optional fields are ignored by older binaries. The binary V2 format stores slice records of variable length (the 24-byte base plus the optional `AGCK` tail), which take precedence over the fixed-step record array.

#### Scenario: Enabling encryption on a non-Redis engine is refused

- **WHEN** the enable-encryption administrative command is run against a volume whose metadata engine is not Redis
- **THEN** the command SHALL fail with an explicit error and SHALL NOT set `encryption_enabled` on the volume

#### Scenario: Legacy attribute and slice records are unchanged

- **WHEN** an inode or chunk list was written before encryption existed
- **THEN** its `Attr` and 24-byte slice records SHALL parse with all crypto fields at zero values and read as plaintext

#### Scenario: Transient FEK is never persisted

- **WHEN** an `Attr` carrying a transient plaintext FEK is marshaled for storage
- **THEN** the serialized bytes SHALL be identical to the serialization of the same attribute without the FEK

### Requirement: Chunk encryption in the data path

Encrypted blocks stored in S3 SHALL be `AGDF` ciphertext; the local disk cache SHALL store only ciphertext (encryption before caching). A block without the `AGDF` magic SHALL be read as plaintext passthrough (legacy). Range-GET SHALL NOT be used for slices that carry a CEK (full-block load instead). Ciphertext blocks SHALL NOT be compressed. Data already starting with the `AGDF` magic SHALL NOT be encrypted again (double-encryption guard on upload and writeback staging).

#### Scenario: Encrypted write stores ciphertext in S3 and cache

- **WHEN** a client writes a block through an encrypted writer
- **THEN** the object in S3 and the local cache file SHALL both start with the `AGDF` magic and SHALL NOT contain the plaintext bytes

#### Scenario: Legacy block reads unchanged

- **WHEN** a reader without a CEK (or a legacy block) is read
- **THEN** the bytes SHALL be returned as stored, without decryption

### Requirement: User path — file creation

When `encryption_enabled` is set on the volume, Meta Proxy SHALL call KeyManager `CreateFileKey` during `Create` of a regular file and write the returned `wrapped_fek`, `drive_file_id`, `fek_version` and `crypto_alg` into the inode attribute; if the KeyManager call or the attribute write fails after successful inode creation, the proxy SHALL remove the created inode (rollback) and return an error. The `drive_file_id` (UUID) SHALL be generated by the client and passed in `CreateRequest`. The proxy SHALL return the plaintext FEK in `CreateResponse` (TLS-only fields `fek`, `fek_version`) so that the creating client encrypts its very first write session; over a plaintext connection the fields SHALL stay empty (the TLS gate follows the `Open` contract). When `Create` resolves to an existing file that already carries a crypto binding (`wrapped_fek` present), the proxy SHALL NOT re-issue the binding (no new `drive_file_id`): idempotently serves the existing binding's FEK via `GetFileFEK`; a legacy unencrypted regular file on an encrypted volume SHALL receive its first binding. `Mknod` of a regular file (S_IFREG) on an encrypted volume SHALL provision the binding the same way (device/fifo/socket nodes SHALL NOT be affected).

Verified-by: pkg/meta/grpc_server_fuse_test.go::TestCreateReturnsFekOverTLS
Verified-by: pkg/meta/grpc_server_fuse_test.go::TestCreatePlaintextConnNoFek
Verified-by: pkg/meta/grpc_server_fuse_test.go::TestCreateOverwriteKeepsExistingBinding
Verified-by: pkg/meta/grpc_server_fuse_test.go::TestCreateLegacyFileGetsBinding
Verified-by: pkg/meta/grpc_server_fuse_test.go::TestMknodRegularFileProvisionsBinding
Verified-by: pkg/meta/grpc_client_fuse_test.go::TestCreateUsesResponseFek
Verified-by: pkg/meta/grpc_client_fuse_test.go::TestCreateFallbackResolveFileKey

#### Scenario: Create failure rolls back the inode

- **WHEN** KeyManager denies or errors during `Create` on an encrypted volume
- **THEN** the new inode SHALL NOT remain in the directory listing and the RPC SHALL return an error

#### Scenario: First write session is encrypted

- **WHEN** a client creates an encrypted file and writes within the same session (no close/re-open)
- **THEN** committed slices carry a wrapped CEK (`WrappedCEK` non-empty, `FekVersion` set) and the stored blocks are ciphertext

#### Scenario: Overwrite does not rebind the crypto identity

- **WHEN** `Create` opens an existing encrypted file (O_CREAT without O_EXCL) and writes to it
- **THEN** `drive_file_id` and the FEK stay unchanged, previously written slices remain readable, and new slices are encrypted under the same FEK

#### Scenario: Mknod regular file gets a binding

- **WHEN** a regular file is created via `Mknod` (S_IFREG) on an encrypted volume
- **THEN** the inode carries the crypto binding and a later `Open` delivers the FEK; fifo/device nodes are unaffected

### Requirement: Write fail-closed without FEK

On an encrypted volume, a write to a file whose attribute marks it encrypted (`Encrypted=true`) SHALL fail with EIO when the client does not hold the plaintext FEK; committing a slice with an empty `WrappedCEK` for such a file SHALL be rejected. This replaces the legacy silent plaintext commit (per-slice passthrough) for encrypted-flagged files with an explicit error; legacy slices recorded before this change remain readable as before.

Verified-by: pkg/vfs/writer_test.go::TestEncryptedWriteWithoutFekFails
Verified-by: pkg/vfs/writer_test.go::TestEncryptedCommitWithoutCEKRejected

#### Scenario: Missing FEK stops the write instead of leaking plaintext

- **WHEN** a client writes to an encrypted-flagged file without a usable FEK (create response carried none and fallback resolve failed)
- **THEN** the write SHALL fail with EIO and no plaintext block SHALL be stored or committed


### Requirement: User path — open and FEK issuance

On `Open` of an encrypted file, Meta Proxy SHALL return the plaintext FEK only in `OpenResponse` (TLS-only field); plaintext FEK delivery SHALL additionally exist on the create path (`CreateResponse`, TLS-only — see the file creation requirement); `GetAttr`, `Readdir` and `Lookup` responses SHALL carry only `wrapped_fek` (ciphertext). FEK SHALL be issued only when the caller holds Read permission (read flags) or Edit permission (write flags); a company owner (`PermissionOwn`) SHALL receive the FEK through the same path via the existing authz bypass. When the client's `OpenRequest.cached_fek_version` equals the current `fek_version`, the proxy SHALL NOT call KeyManager and the client SHALL take the FEK from its local LRU cache (100k entries, TTL 15 minutes); the authz interceptor check on every `Open` SHALL still apply. Unwrapped CEKs SHALL be cached in RAM for the lifetime of the open file (one unwrap per slice per open). KeyManager SHALL coalesce concurrent requests for the same FEK via singleflight and SHALL log every issuance to the audit log; any KeyManager error SHALL fail closed (no FEK returned).

#### Scenario: Denied user gets no FEK

- **WHEN** a user without Read permission opens an encrypted file
- **THEN** the `Open` SHALL fail with EACCES and no plaintext FEK SHALL be returned or logged as plaintext

#### Scenario: Cached FEK skips KeyManager

- **WHEN** a client opens the same encrypted file twice and its LRU still holds the FEK at the current version
- **THEN** the second `Open` SHALL carry `cached_fek_version` equal to the attribute version and KeyManager SHALL NOT be called for it

### Requirement: Identity invariant

The OIDC `sub` claim SHALL be used directly as the platform `user.id` (UUID) in all components; no mapping or transformation SHALL be performed. A `sub` that is not a valid UUID SHALL be rejected fail-closed: Meta Proxy SHALL answer `Unauthenticated`, KeyManager SHALL answer `InvalidArgument`. A user whose `sub` is a valid UUID but who has no records on the platform (not imported or removed) SHALL be denied by the authz layer (no permission records → deny), not by identity validation.

#### Scenario: Non-UUID sub is rejected at the proxy

- **WHEN** a request arrives with a valid OIDC token whose `sub` is not a UUID
- **THEN** the RPC SHALL fail with `Unauthenticated` before reaching the handler

#### Scenario: Unknown platform user is denied by authz

- **WHEN** a valid-UUID user without any permission records requests a FEK
- **THEN** the request SHALL be denied with PermissionDenied by the authorization check

### Requirement: Render path

The render client SHALL mount the volume directly against Redis and S3 without Meta Proxy, OIDC, authz checks or PostgreSQL. The Company KEK SHALL be fetched at mount time via KeyManager `FetchCompanyKEK`; passing the KEK through CLI arguments SHALL NOT be supported. The KEK SHALL reside only in RAM (mlock, no swap) and SHALL be zeroed on unmount. FEKs SHALL be unwrapped locally with the Company KEK and cached in an LRU (100k entries, TTL 1 hour). On `Create`, the render client SHALL generate the FEK locally and write `wrapped_fek` to Redis without KeyManager. A `wrapped_fek` wrapped by another company's KEK SHALL fail to unwrap (EIO, fail-closed); access SHALL additionally be confined to the company prefix via chroot. The render mode SHALL use aggressive metadata caching (attr/entry timeouts ≥ 60s) and pipelined readdir.

Storage credentials for the render node SHALL be the company's active static key pair delivered by the platform: `GetNodeStorageCredentials` SHALL return the decrypted active pair (plus the volume name and bucket derived from the facility config), and the render client SHALL install them as its static S3 credentials. STS providers (YC node STS, AWS AssumeRole) SHALL remain only for deployments configured with those providers. The node SHALL authenticate the bootstrap RPCs (`FetchCompanyKEK`, `GetNodeStorageCredentials`) with the platform node credential per the deployment's `node_auth` mode (`yc-iam` — legacy YC IAM token; `node-token` — platform-issued per-node static token); a revoked or unknown credential SHALL fail closed with no KEK and no key material.

Verified-by: cmd/render_mount_test.go::TestRenderStaticKeysFromPlatform
Verified-by: pkg/meta/render_integration_test.go::TestRenderSelectelBootstrap

#### Scenario: Cross-company file is unreadable

- **WHEN** a render node of company B opens a file whose `wrapped_fek` was wrapped by company A's KEK
- **THEN** unwrap SHALL fail and the open SHALL return EIO

#### Scenario: Render node bootstraps from the platform on Selectel

- **WHEN** a render node mounts with a valid platform node credential on a Selectel deployment (`node_auth: node-token`)
- **THEN** `FetchCompanyKEK` returns the Company KEK and `GetNodeStorageCredentials` returns the company's active static pair with volume/bucket; the mount serves the company subtree directly against Redis and S3 with no STS involved

#### Scenario: Revoked node credential fails closed

- **WHEN** the node credential was revoked (or never issued) and the node calls either bootstrap RPC
- **THEN** the RPC fails with Unauthenticated, no KEK and no S3 key material are returned, and the audit log records the denial


### Requirement: Clone, CopyFileRange and Compaction with CEK re-wrap

`Clone` of an encrypted file SHALL give the target a new FEK and re-wrap every shared slice's CEK under the target FEK (`wrapped_cek` rewritten in metadata) without rewriting any S3 object (zero-copy); the source file SHALL remain readable with its own FEK, and slices not included in the clone SHALL NOT be accessible through the target. `CopyFileRange` that shares slices SHALL re-wrap CEKs under the destination FEK; a split chunk SHALL get a new CEK. Compaction of an encrypted file SHALL read (decrypt) source slices, merge them into a new slice with a new CEK, encrypt and wrap the new CEK under the file's FEK; if the file key cannot be resolved, compaction SHALL be skipped (fail-closed) without corrupting data. `Truncate`, `SetAttr` and `Rename` SHALL NOT change the FEK.

#### Scenario: Clone does not rewrite S3 objects

- **WHEN** an encrypted file is cloned
- **THEN** the set of S3 object keys SHALL be identical before and after, the target SHALL read correctly with its new FEK, and the old CEKs wrapped under the source FEK SHALL NOT unwrap under the target FEK

#### Scenario: Compaction without resolvable key is skipped

- **WHEN** compaction is triggered for an encrypted file whose FEK cannot be resolved
- **THEN** compaction SHALL be skipped with a log entry and the file data SHALL remain intact

### Requirement: Versioning preconditions

The metadata layer SHALL expose extension hooks so that future versioning/snapshots do not require reworking the crypto layer: a file-crypto-deletable hook (FEK lifecycle — FEK must not be deletable while referenced by an active version, a trash entry or a snapshot), a slice-deletable hook (version-aware GC), and a compaction-allowed hook (version-aware compaction); all hooks SHALL default to the current behavior (deletable/allowed) when unset. The `fek_version` field SHALL be supported end-to-end (attribute, `AGFK`/`AGCK` AAD, `Format.kek_version`) so that a file can hold multiple FEK versions without a format change.

#### Scenario: Hook blocks crypto cleanup

- **WHEN** the file-crypto-deletable hook returns false for an inode
- **THEN** garbage collection SHALL NOT remove its crypto metadata

### Requirement: Offline-connected mode and no residuality

Plaintext keys (FEK, CEK) SHALL reside only in RAM of the client process. On explicit logout, OIDC session expiry, revoke signal, or hub-unavailability timeout, the client SHALL zero all plaintext keys with an explicit memclear (not relying on GC) and transition to disconnected. Key memory pages (FEK cache entries, render KEK) SHALL be mlock'ed (no swap). The client SHALL maintain a state machine online → offline-connected → disconnected: while offline-connected, reads of already-open/cached data SHALL continue from the local cache, new `Open` of encrypted files SHALL fail closed, and writes SHALL be appended to a local write journal that is replayed on reconnect; the unavailability timeout SHALL be configurable (default 15 minutes). After disconnected, the local cache SHALL be unreadable.

#### Scenario: Logout makes the cache unreadable

- **WHEN** a logged-out client attempts to read previously cached encrypted data
- **THEN** the read SHALL fail because all plaintext keys have been zeroed

#### Scenario: Offline writes survive reconnect

- **WHEN** the hub is unavailable and the client writes data to files that are still open when the connection is restored
- **THEN** the journaled writes of those files SHALL be replayed (under the current FEK resolved via a re-Open of each journaled file) and the data SHALL be readable and consistent; journaled records of files with no open writer SHALL NOT be silently dropped — they SHALL be retained in the journal and reported as a conflict

#### Scenario: Write journal properties are pinned

- **WHEN** offline-connected writes are journaled on the local disk
- **THEN** journal records SHALL be appended only while the client is in the offline-connected state, the journal file SHALL have 0600 permissions and SHALL be truncated on explicit logout and after a successful replay; the payload of pending records is plaintext as an accepted boundary (see SRS-001 §3.2 T10, v2.3) and no plaintext key material SHALL ever be journaled

### Requirement: Revocation and STS

On permission revocation (role delete/clear), the platform SHALL bump a per-user permission generation counter; the proxy SHALL deliver the current generation in every `FlushSession` heartbeat response, and the client SHALL wipe all plaintext keys when the generation changes. The authz decision cache TTL in production SHALL be ≤ 30 seconds and role changes SHALL additionally trigger explicit cache invalidation. The data plane SHALL use short-lived STS S3 credentials (TTL ≤ 60 minutes) scoped to the company prefix; STS credentials SHALL NOT be written into the Redis `Format`. FEK rotation (re-wrap of CEKs under a new FEK) SHALL be supported as an operation that does not re-encrypt S3 data; client caches SHALL be invalidated by `fek_version` mismatch on the next `Open`.

#### Scenario: Revoked user loses access within bounded time

- **WHEN** a user's permission is revoked
- **THEN** new FEK requests SHALL be denied immediately, the client SHALL wipe keys upon receiving the bumped generation (within one heartbeat), and S3 access SHALL expire with the STS token (≤ 60 minutes)

#### Scenario: STS credentials are prefix-scoped

- **WHEN** a user's STS credentials are issued
- **THEN** the attached policy SHALL allow S3 operations only under that company's prefix and the credentials SHALL NOT appear in Redis `Format`

### Requirement: Key rotation

Company KEK rotation SHALL re-wrap all `wrapped_fek` under the new KEK without re-encrypting S3 data; the old KEK SHALL stay in `retiring` status until re-wrap completes, and `kek_version` in each `wrapped_fek` SHALL identify the wrapping KEK version. FEK rotation SHALL generate a new FEK and re-wrap all CEKs under it without re-encrypting S3 data, invalidating stored old FEKs but not stored CEKs. CEK rotation SHALL re-encrypt the data in S3 (via the re-encryption tool) and invalidate old CEKs for the S3 copy; it SHALL NOT protect an attacker's offline copy. The KMS Master Key SHALL support automatic rotation.

#### Scenario: FEK rotation leaves S3 objects untouched

- **WHEN** a file's FEK is rotated
- **THEN** the S3 object keys SHALL be unchanged, CEKs SHALL unwrap under the new FEK and NOT under the old one, and reads SHALL succeed

### Requirement: Legacy data migration

Existing unencrypted chunks SHALL remain readable transparently (`encrypted=0`) both before and after encryption is enabled on the volume. After enabling, all newly created files SHALL be encrypted by default. A background re-encryption tool SHALL convert legacy files (read plaintext → generate FEK/CEK → encrypt → update S3 → update Redis metadata) and SHALL be idempotent and resumable without an external checkpoint (progress derived from Redis state: a chunk is done when all its slice records carry `AGCK`), rate-limited (concurrency, IOPS, bandwidth). A file SHALL remain readable (old version) and writable (new encrypted version) during its re-encryption. Enabling encryption on an existing volume SHALL be an idempotent administrative operation.

#### Scenario: Re-encryption is idempotent

- **WHEN** the re-encryption tool runs a second time over already-migrated files
- **THEN** it SHALL perform zero data operations

#### Scenario: Legacy file readable after enabling encryption

- **WHEN** encryption is enabled on a volume containing legacy files and a legacy file is read before migration
- **THEN** the read SHALL succeed with plaintext passthrough

### Requirement: gRPC API surface

The `MetaService` proto SHALL be extended with: `OpenResponse` fields `fek` (bytes, TLS-only), `fek_version`, `encrypted`; `CreateRequest` field `drive_file_id`; `Format` fields `encryption_enabled`, `kek_version`; `FlushSessionResponse` field `permission_generation`; and RPCs `ResolveFileKey` (returns plaintext FEK for an inode, authz Read), `RotateFileKey` (admin-gated FEK rotation) and `RotateFileKeysByPaths` (admin-gated batch FEK rotation by file paths; behavioral contract in the requirement Offboarding batch FEK rotation). The fork SHALL consume the agio-platform service `agio.platform.drive.crypto.v1.DriveKeyManagerService` with RPCs: `CreateFileKey`, `GetFileFEK`, `GetBulkFileFEK` (batch up to 1000 keys), `FetchCompanyKEK` (node-identity authenticated), `ProvisionCompanyKEK`, `GetPermissionGeneration`, `GetSTSCredentials`, `RotateFileFEK` (org-admin gated; the response carries the new wrapped FEK and the new plaintext FEK needed by the proxy for CEK re-wrap) and `RotateCompanyKEK`.

#### Scenario: Plaintext FEK travels only in OpenResponse

- **WHEN** a client fetches attributes or listings of encrypted files
- **THEN** the responses SHALL contain only `wrapped_fek` ciphertext, never the plaintext FEK

### Requirement: Audit and anomaly detection

Every FEK issuance (create/get/rotate/deny) SHALL be logged with timestamp, actor type and id, company id, drive_file_id, inode, operation, required permission, result (allow/deny/error), reason, request id and client IP. Audit records SHALL be append-only with retention ≥ 12 months. A detection rule SHALL raise a security event when an actor requests FEKs for more than N files within period T (thresholds configurable).

#### Scenario: Denied issuance is audited

- **WHEN** a FEK request is denied
- **THEN** an audit record with result `deny` and the reason SHALL be written

### Requirement: Performance targets

`GetFileFEK` latency SHALL be ≤ 5 ms p99 on client cache hit (no KeyManager RPC) and ≤ 100 ms p99 on cache miss. Encryption overhead on sequential throughput SHALL be ≤ 10% versus an unencrypted volume. Reading `wrapped_fek` SHALL add zero Redis round-trips beyond the attribute read. Batch FEK retrieval SHALL support up to 1000 keys per call. Each FEK/CEK SHALL be unwrapped at most once per file per session.

#### Scenario: Cache-hit open does not call KeyManager

- **WHEN** 1000 concurrent opens hit the client FEK cache
- **THEN** p99 latency SHALL be ≤ 5 ms and no KeyManager RPC SHALL be issued for those opens

### Requirement: Offboarding batch FEK rotation

The Meta Proxy SHALL expose an admin-gated batch RPC `RotateFileKeysByPaths` that rotates the FEK of a list of file paths in bounded batches: for each resolvable encrypted file it SHALL generate a new FEK, write the new `wrapped_fek` with an incremented `fek_version`, and re-wrap all CEKs under the new FEK without re-encrypting S3 objects. The operation SHALL be rate-limited and resumable: an interrupted run restarted with the same path list SHALL skip files already rotated (checkpoint of processed paths) and produce the same final state as an uninterrupted run. Paths that cannot be resolved to an encrypted inode SHALL be reported in the response as skipped with a reason and SHALL NOT abort the batch.

#### Scenario: Batch rotation rotates each resolvable file

- **WHEN** an administrator calls `RotateFileKeysByPaths` with a list of paths of encrypted files
- **THEN** each resolvable file SHALL have its `fek_version` incremented, its CEKs re-wrapped under the new FEK, and its S3 object keys unchanged

#### Scenario: Unresolvable path does not abort the batch

- **WHEN** the path list contains a path that cannot be resolved to an encrypted inode
- **THEN** that path SHALL be reported as skipped with a reason and the remaining paths SHALL still be processed

#### Scenario: Restarted run skips already-rotated files

- **WHEN** a batch run is interrupted after some files were rotated and is restarted with the same path list
- **THEN** already-rotated files SHALL NOT receive a second rotation and the final state SHALL be identical to an uninterrupted run

### Requirement: Data-plane short-lived S3 credentials (FR-REV-3, ADR-003)

The system SHALL provide short-lived S3 credentials (TTL ≤ 60 minutes) for data-plane access on encrypted volumes, issued only by the platform KeyManager with a session policy scoped to the mounted volume's object prefix (`<volume>/*` in the shared bucket). Issuance SHALL be authenticated and audited: user mounts obtain credentials through the Meta Proxy pass-through RPC, which fills `user_id` from the authenticated OIDC session (a client-supplied identity SHALL be ignored); render-node mounts obtain credentials via the `GetNodeSTSCredentials` RPC authenticated by the node's YC IAM token. The volume and bucket SHALL be derived server-side from the company's facility configuration — clients SHALL NOT be able to name the volume or bucket. A mount with `--sts-enabled` SHALL fail fast when credential issuance fails and SHALL NOT fall back to static bucket credentials.

#### Scenario: User mount receives volume-scoped credentials

- **WHEN** a user with Read permission on the company root mounts with `--sts-enabled` through the Meta Proxy
- **THEN** the returned credentials SHALL be valid only for object operations under `arn:aws:s3:::<bucket>/<volume>/*` and SHALL expire within 60 minutes

#### Scenario: User without company access is denied

- **WHEN** a request for STS credentials names a company on which the authenticated user has no Read permission
- **THEN** the request SHALL be denied and no credentials SHALL be returned

#### Scenario: Render node authenticates by IAM token

- **WHEN** a render node calls `GetNodeSTSCredentials` with a valid YC IAM token for its company
- **THEN** the issuance SHALL be audited with actor type `render_node` and the node identity taken from the verified token

#### Scenario: Mount fails fast without credentials

- **WHEN** STS credential issuance fails at mount time with `--sts-enabled`
- **THEN** the mount SHALL NOT start and SHALL NOT fall back to the volume's static bucket credentials

### Requirement: Create-time binding allocated exactly once

When two `Create` or `Mknod` operations race to provision the crypto binding of the same regular file on an encrypted volume, exactly one provisioning SHALL win: the engine SHALL write the binding (`SetFileCrypto`) only when the inode carries no binding yet, inside the same transaction that reads the attribute; a loser SHALL observe the standing binding (answer `EEXIST` from the guard) without writing anything, and the proxy SHALL then serve the standing binding's identity and FEK (the Stage-11 idempotent overwrite path) so the POSIX create still succeeds. Deliberate re-binding paths — FEK rotation, clone/copy destination provisioning, render-side create — SHALL keep the unconditional `SetFileCrypto`. The race window SHALL therefore contain no state where a committed slice's AAD references a `drive_file_id` that the inode no longer carries.

Verified-by: pkg/meta/redis_fek_test.go::TestSetFileCryptoIfUnbound_FirstWriteWins
Verified-by: pkg/meta/redis_fek_test.go::TestSetFileCryptoIfUnbound_ExistingBindingRefused
Verified-by: pkg/meta/redis_fek_test.go::TestSetFileCryptoIfUnbound_RotationUnaffected
Verified-by: pkg/meta/grpc_server_fuse_test.go::TestCreateRaceLoserServesStandingBinding
Verified-by: pkg/meta/encrypt_integration_test.go::TestConcurrentCreateSingleBinding

#### Scenario: Concurrent creates leave one binding

- **WHEN** two clients concurrently `Create` the same path on an encrypted volume and both reach provisioning
- **THEN** the inode ends with exactly one `drive_file_id`, both responses succeed, and slices written by the first writer remain readable after the second create completes

#### Scenario: Loser serves the standing binding

- **WHEN** the loser of the provisioning race receives the already-bound answer
- **THEN** it does not re-issue `CreateFileKey`, does not call `SetFileCrypto`, and returns the standing binding's FEK path (or the client falls back to `ResolveFileKey`) — no error surfaces to the caller

#### Scenario: Rotation is not blocked by the guard

- **WHEN** `RotateFileFEK` writes a new wrapped FEK with an incremented version over an existing binding
- **THEN** the unconditional write path is used and the guard does not refuse it
