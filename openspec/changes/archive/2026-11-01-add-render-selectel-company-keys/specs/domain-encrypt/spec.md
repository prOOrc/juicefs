# Delta: domain-encrypt

## MODIFIED Requirements

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
