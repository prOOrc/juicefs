---
title: Render Mount
sidebar_position: 10
---

`juicefs render-mount` mounts a volume for a **render node**: direct access to the metadata backend (e.g. Redis) and object storage with local FEK handling under the Company KEK. There is no Meta Proxy, no OIDC, no per-file KeyManager RPCs and no authz on this path — the mount's security rests on the Company KEK and node authentication (SRS §7).

Key properties:

- **One-shot bootstrap.** The Company KEK is fetched once at mount from the platform's `DriveKeyManagerService` over TLS 1.3 and kept in RAM (mlocked); it is the node's only trust anchor. The KEK and all cached FEKs are wiped on every exit path. The KEK is never accepted via CLI flags or environment.
- **Company chroot.** The mount is chrooted to `companies/{--company-code}` as defense in depth on top of the cryptographic isolation: a node of company A cannot even address paths of company B.
- **Encrypted volumes only.** render-mount refuses to start on a volume with encryption disabled.
- **Render-tuned caching.** Kernel attr/entry/dir-entry caches are raised to at least 60s (render workloads re-stat the same files thousands of times).
- The command runs in the foreground; a supervisor (e.g. Kubernetes) should restart it.

## Flags

| Flag | Description |
|---|---|
| `--company-id` | Company ID (UUID) of this node's company; used for `FetchCompanyKEK` and bound into the FEK AAD. Required. |
| `--company-code` | Company code; the mount is chrooted to `companies/{code}`. Required. |
| `--keymanager-service` | gRPC address (`host:port`) of the platform `DriveKeyManagerService`. Required. |
| `--keymanager-tls-ca` | CA certificate (PEM file) for the KeyManager channel; TLS 1.3 is enforced when set. Without it the transport is insecure — development only. |
| `--keymanager-server-name` | TLS server name override for the KeyManager channel. |

## Node authentication

There are two mutually exclusive modes, selected solely by the flags — no autodetect. The modes never mix.

### Selectel: node credential (node-token mode)

| Flag | Description |
|---|---|
| `--node-credential-file` | File with the platform-issued node credential; the file must be owner-readable only (`0600`). Enables the node-token bootstrap (`FetchCompanyKEK` + `GetNodeStorageCredentials`); the token travels as the `x-node-credential` gRPC metadata. |
| `--node-id` | Node identifier reported to the KeyManager for audit; defaults to the hostname. |

The node token is issued by a platform administrator (`keymanager node-token issue --node-id <id>`); it is printed once and only its SHA-256 is stored platform-side. Re-issue rotates the credential (the previous token is revoked), `keymanager node-token revoke --node-id <id>` kills the node's KeyManager access immediately — the next bootstrap fails with `Unauthenticated`. Material already delivered to the node (KEK, storage keys) lives in its RAM until the node is unloaded. See the platform runbook `docs/ops/rb-render-node-tokens.md` (agio-platform repository) for the full lifecycle.

`--node-credential-file` cannot be combined with `--iam-token-file` or `--sts-enabled`.

### YC (legacy): IAM token

| Flag | Description |
|---|---|
| `--iam-token-file` | File containing the YC IAM token; when empty, the token is fetched from the instance metadata service (169.254.169.254). |

In this mode only `FetchCompanyKEK` is called at bootstrap; the platform verifies the node's YC IAM identity.

## Storage credentials

How the node obtains its object-storage credentials depends on the mode:

- **node-token mode (Selectel):** the platform delivers the company's active static key pair plus the volume/bucket via `GetNodeStorageCredentials`; the delivered volume/bucket override the volume format, and the patch is re-applied on every format reload so a refresh from Redis never restores stale credentials. The keys exist only in the node's RAM.
- **`--sts-enabled` (YC/AWS):** short-lived STS credentials replace static keys. On AWS the node assumes an IAM role with its instance profile (`--sts-role-arn` is required); on YC the platform issues volume-scoped STS via `GetNodeSTSCredentials`, authenticated by the node IAM token.
- **Without either (legacy YC behavior):** static keys come from the volume format.

## Example

Selectel render node (node-token mode, static keys delivered by the platform):

```shell
sudo juicefs render-mount redis://localhost /mnt/render \
    --company-id 6f1e... --company-code acme \
    --keymanager-service keymanager.agio.svc:9000 \
    --keymanager-tls-ca /etc/ssl/agio/ca.pem \
    --node-credential-file /etc/agio/node-credential
```

YC render node (IAM identity, STS storage credentials):

```shell
sudo juicefs render-mount redis://localhost /mnt/render \
    --company-id 6f1e... --company-code acme \
    --keymanager-service keymanager.agio.svc:9000 \
    --keymanager-tls-ca /etc/ssl/agio/ca.pem \
    --sts-enabled
```

A missing, unknown or revoked credential aborts the mount at bootstrap with `Unauthenticated` ("invalid or revoked node credential") — nothing is mounted. Revocation takes effect on the next request: a running mount keeps working until it is unloaded, while any remount fails.
