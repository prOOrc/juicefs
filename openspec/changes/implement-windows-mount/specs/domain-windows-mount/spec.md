# domain-windows-mount

## Purpose

Контракт монтирования agio Drive волюмов на Windows: Windows-сборка CLI (`juicefs mount`, WinFsp) с meta URL `grpc://` — паритет STS-флагов с Unix, интерактивный OIDC browser flow как сценарий первого входа, сохранение полной `grpc://` конфигурации (включая query-параметры) в WinFsp service mode, и задокументированные платформенные ограничения (mlock no-op, symlink ENOSYS, одноволюмовый service mode).

## ADDED Requirements

### Requirement: Windows mount STS flag parity

The Windows build of `juicefs mount` SHALL accept the `--sts-enabled` and `--company-id` flags with the same semantics as the Unix build: STS mode SHALL require an encrypted volume (`encryption_enabled=true`) and a `grpc://` meta URL, mount SHALL fail fast when STS credential issuance fails at startup, and there SHALL be no fallback to static bucket credentials. When `--sts-enabled` is used without meeting the prerequisites (non-encrypted volume or non-`grpc://` meta URL), the mount SHALL refuse to start with an error naming the unmet prerequisite.

#### Scenario: STS mount on Windows with encrypted grpc volume

- **WHEN** a user mounts `grpc://proxy:9561/vol?...` with `--sts-enabled --company-id <id>` on Windows on an encrypted volume
- **THEN** the mount SHALL obtain STS credentials through the Meta Proxy pass-through before serving the filesystem and SHALL fail fast with a clear error if issuance fails

#### Scenario: STS flag rejected on unencrypted volume

- **WHEN** `juicefs mount` on Windows is invoked with `--sts-enabled` against a volume with `encryption_enabled=false`
- **THEN** the mount SHALL refuse to start with an error stating that STS requires an encrypted volume

### Requirement: Interactive OIDC first login on Windows

The first authentication on Windows SHALL use the interactive browser flow: the client SHALL open the system browser (via localhost OAuth callback from `oidc-redirect-url`) in an interactive user session, with a manual code-entry fallback. This requirement covers interactive sessions only; mounts started outside an interactive session (WinFsp service mode, `-d`) SHALL rely on the existing token cache (`$HOME/.oidc-login`) and SHALL NOT hang indefinitely when the cache is absent — the pending authentication attempt SHALL terminate with a clear `Unauthenticated`-derived error after the OIDC library's timeout.

#### Scenario: First mount in interactive session opens browser

- **WHEN** a user runs `juicefs mount "grpc://...?oidc-issuer=..." Z:` in an interactive Windows session with an empty token cache
- **THEN** the system browser SHALL open for authentication and the mount SHALL complete after successful login

#### Scenario: Service mode without cached token fails with clear error

- **WHEN** a Windows service-mode mount starts with no valid cached token and no interactive session
- **THEN** the authentication attempt SHALL terminate with a clear authentication error (browser flow is not attempted against a non-interactive session) and the mount SHALL NOT hang

### Requirement: Service mode preserves grpc configuration

WinFsp service mode (`juicefs mount -d` on Windows) SHALL reconstruct the original command line from the service registry entry losslessly with respect to the `grpc://` configuration: the meta URL including all query parameters (`oidc-*`, `tls*`, cache parameters), `--sts-enabled` and `--company-id` SHALL be preserved so that a service-mode mount connects with the same OIDC, transport and STS configuration as the foreground mount that registered it.

#### Scenario: Service registry preserves query parameters

- **WHEN** a foreground mount with `grpc://host:port/vol?oidc-issuer=...&tls=1&tls-ca=...` and `--sts-enabled --company-id <id>` is converted to a Windows service
- **THEN** the reconstructed command line in the service registry SHALL contain the identical meta URL and both flags, and a service restart SHALL connect with the same configuration

### Requirement: Documented Windows platform limitations

The Windows mount of an agio Drive volume SHALL operate within the following documented limitations, which SHALL be recorded in this capability as the contract of what is NOT supported: FEK memory locking is a no-op (keys are not protected from swap), symlink creation returns `ENOSYS`, `Chown` is a no-op, POSIX ACL mount options are not passed to WinFsp, and WinFsp service mode supports a single volume per service. These limitations SHALL NOT fail the mount; they SHALL be observable only as reduced platform guarantees.

#### Scenario: Encrypted mount succeeds without mlock

- **WHEN** an encrypted volume is mounted on Windows
- **THEN** the mount SHALL succeed and encrypted reads/writes SHALL work, with FEK pages not locked in memory (no mlock failure metric expected)

#### Scenario: Symlink creation is not supported

- **WHEN** a client attempts to create a symlink on a Windows mount
- **THEN** the operation SHALL return `ENOSYS` and the volume SHALL remain consistent
