# domain-meta-proxy

## Purpose

Дельта change `implement-grpc-tls` добавляет опциональный TLS на канал клиент→Meta Proxy: флаги прослушивания сервера (`--tls-cert`/`--tls-key`), query-параметры `grpcMeta` (`tls`, `tls-ca`, `tls-server-name`) и fail-closed выдачу plaintext FEK только по TLS-соединению. Модифицируются только перечисленные ниже требования; OIDC, authz, кэши и DirHandler не меняются.

## MODIFIED Requirements

### Requirement: Meta Proxy server and CLI

The system SHALL provide a `juicefs meta-proxy` command that wraps a metadata engine (Redis or PostgreSQL URL via `--meta-backend`, default `redis://localhost:6379/0`) with a single gRPC service `MetaService` listening on `--addr` (default `:9561`). Flags SHALL include: `--grpc-max-send-msg-size` / `--grpc-max-recv-msg-size` (256 MB), `--grpc-keepalive-time` (1h), `--grpc-keepalive-timeout` (20s), keepalive enforcement MinTime 5s, `--oidc-issuer`, `--oidc-client-id`, `--authz-service`, `--authz-volume-name`, `--authz-path-cache-size` (100000), `--authz-cache-ttl` (30s), and `--tls-cert` / `--tls-key`. When both `--tls-cert` and `--tls-key` are set, the server SHALL serve the `MetaService` over TLS (server-side certificate, minimum version TLS 1.2); otherwise the listener SHALL remain plaintext. When only one of the two flags is set, the server SHALL refuse to start with a clear error. On SIGINT/SIGTERM the server SHALL perform a graceful stop with a 30-second timeout before forced stop and metadata shutdown.

#### Scenario: TLS listener starts with certificate pair

- **WHEN** `juicefs meta-proxy` is started with valid `--tls-cert` and `--tls-key`
- **THEN** the gRPC listener SHALL accept TLS handshakes with minimum protocol version 1.2

#### Scenario: Incomplete TLS configuration refuses to start

- **WHEN** exactly one of `--tls-cert` / `--tls-key` is provided
- **THEN** the server SHALL exit at startup with an error naming the missing flag

#### Scenario: Authn and authz are opt-in by flags

- **WHEN** `--oidc-issuer` is empty
- **THEN** no OIDC interceptor SHALL be installed and requests pass without a token; the same SHALL hold for `--authz-service` and the authz interceptor

### Requirement: gRPC metadata client

The system SHALL provide a `grpcMeta` metadata driver (registered as `grpc`, pure proxy without baseMeta embedding) connecting to `grpc://host:port[/volume?query]`. Query parameters with defaults: `attr-cache-size` (100000), `dir-cache-size` (5000), `attr-cache-ttl` (1s), `dir-cache-ttl` (1s), `heartbeat-interval` (12s); OIDC parameters (`oidc-issuer`, `oidc-client-id` — required when issuer is set, `oidc-client-secret`, `oidc-redirect-url`, `oidc-scopes`, `oidc-cache-dir`); TLS parameters (`tls`, `tls-ca`, `tls-server-name`). Without `tls=1` the connection SHALL be plaintext (current behavior). With `tls=1` the client SHALL dial with TLS: with `tls-ca` the root CA pool SHALL be loaded from the given file, otherwise the system pool SHALL be used; `tls-server-name` SHALL override the server name used for certificate verification. A missing or unreadable `tls-ca` file SHALL fail client creation. Outgoing requests SHALL carry `authorization: Bearer <token>` (token acquisition coalesced via singleflight) and `x-session-id` when a session exists. After `NewSession` the client SHALL send a heartbeat every `heartbeat-interval` implemented as a `FlushSession` RPC with a 5-second timeout; heartbeat errors SHALL be logged at debug level only. gRPC status mapping: PermissionDenied/Unauthenticated → EACCES, NotFound → ENOENT, AlreadyExists → EEXIST, InvalidArgument → EINVAL, other → EIO.

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

Verified-by: pkg/meta/grpc_client_test.go::TestGRPCMetaCreation
Verified-by: pkg/meta/grpc_client_auth_test.go::TestWithAuthSingleflightCoalescing
Verified-by: pkg/meta/grpc_client_auth_test.go::TestWithAuthWithoutOIDC

## ADDED Requirements

### Requirement: Plaintext FEK delivery requires TLS

On an encrypted volume, the server SHALL return the plaintext FEK in `OpenResponse.fek` (and `GetFileFEK`-derived fields) only when the RPC arrived over a TLS connection. When an encrypted-file `Open` arrives over a plaintext connection, the server SHALL NOT deliver the plaintext FEK and SHALL respond with `codes.Unauthenticated`; the client SHALL map it to `EACCES` per the existing status mapping. On volumes with `encryption_enabled=false` the check SHALL NOT apply. The check SHALL be enforced regardless of the server's own listener mode (a plaintext listener on an encrypted volume simply never qualifies).

#### Scenario: Open on encrypted volume over plaintext connection

- **WHEN** a client connected without TLS opens an encrypted file on a volume with `encryption_enabled=true`
- **THEN** the server SHALL respond with `Unauthenticated` and the response SHALL NOT contain the plaintext FEK

#### Scenario: Open on encrypted volume over TLS connection

- **WHEN** a client connected with TLS opens an encrypted file on a volume with `encryption_enabled=true`
- **THEN** the server SHALL return the plaintext FEK in `OpenResponse.fek` subject to the existing authz checks

#### Scenario: Unencrypted volumes unaffected

- **WHEN** a client connected without TLS opens a file on a volume with `encryption_enabled=false`
- **THEN** the server SHALL behave exactly as before this change (no TLS requirement)
