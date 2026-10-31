# Delta: drive-sts-client

## ADDED Requirements

### Requirement: Working directory ensure on company mounts

When a mount is started with `--company-id` and the platform's
`GetSTSCredentials` response carries a `working_dir`, the client SHALL
idempotently ensure the delivered directory exists inside the mounted volume
(`mkdir -p` semantics: an existing directory is success). A denied or failed
ensure SHALL be reported as a warning and SHALL NOT abort the mount; absence
of the field SHALL preserve the current behavior. The client SHALL NOT derive
or hardcode any path naming convention of its own: the only source of the
path is the platform response.

Verified-by: cmd/mount_test.go::TestEnsureWorkingDir_CreatesDeliveredPath
Verified-by: cmd/mount_test.go::TestEnsureWorkingDir_ExistingDirIsSuccess
Verified-by: cmd/mount_test.go::TestEnsureWorkingDir_DeniedIsWarningOnly
Verified-by: cmd/mount_test.go::TestEnsureWorkingDir_NoFieldNoBehavior

#### Scenario: Fresh volume self-provisions on first mount

- **WHEN** the first company mount starts on a fresh volume and the platform delivers `working_dir`
- **THEN** the client creates the delivered directory tree (both levels) and subsequent user writes under it succeed through the normal authz path

#### Scenario: Ensure failure never blocks the mount

- **WHEN** the ensure mkdir is denied or fails for any reason
- **THEN** the mount completes with a warning in the log and the storage remains usable for operations the user is authorized for
