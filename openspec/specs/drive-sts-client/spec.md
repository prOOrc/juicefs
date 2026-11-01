# drive-sts-client Specification

## Purpose
STS-клиент side JuiceFS-маунта в AGIO Drive: получение credentials компании через agio-platform (`GetSTSCredentials` с OIDC-токеном), автообновление до истечения TTL и самообеспечение рабочего каталога компании из ответа платформы.

## Requirements

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

### Requirement: Refresh cadence follows the issued credential TTL

The STS credential refresher SHALL derive its poll interval from the actual
credential set delivered by the platform: the interval SHALL be
`(Expiry − issue time) × margin`, where `margin` defaults to 0.5 and MAY be
overridden by the client (`sts-refresh-margin` parameter, bounded to
`0 < margin ≤ 1`). After every successful credential swap the interval SHALL
be recomputed from the newly delivered set (the platform MAY vary the TTL
between deliveries). The interval SHALL be clamped to the range
`[5s, Expiry − 5s]`: never faster than 5 seconds, and never later than 5
seconds before the delivered credentials expire. The built-in default
(30 minutes, poll at half) SHALL apply only until the first platform
response arrives.

Verified-by: cmd/sts_refresher_test.go::TestSTSRefresher_IntervalFollowsDeliveredTTL
Verified-by: cmd/sts_refresher_test.go::TestSTSRefresher_IntervalRecomputedAfterSwap
Verified-by: cmd/sts_refresher_test.go::TestSTSRefresher_IntervalClampedBeforeExpiry

#### Scenario: Short platform TTL is polled within its lifetime

- **WHEN** the platform issues credentials with a synthetic TTL shorter than the built-in default (for example 6 minutes with the 30-minute default)
- **THEN** the refresher polls at `TTL × margin` (3 minutes by default) so a fresh set is always installed before the current one expires, and no expired-credential alarm is raised while refreshes succeed

#### Scenario: TTL change between deliveries is honored

- **WHEN** two consecutive deliveries carry different synthetic TTLs
- **THEN** the poll interval after the second delivery is derived from the second TTL, not the first

### Requirement: Fail-safe semantics are preserved

Deriving the interval from the delivered TTL SHALL NOT change the fail-safe
contract: on refresh failure the previous credentials stay in place until
they expire, the situation is reported exactly once (metric `sts_refresh_failures`
plus the expired alarm) once the current set is past its `Expiry`, and a
successful refresh clears the alarm. A failure to obtain the initial
credential set SHALL still abort the mount (fail-fast, no silent fallback).

Verified-by: cmd/sts_refresher_test.go::TestSTSRefresher_FailSafeUntilExpiry
Verified-by: cmd/sts_refresher_test.go::TestSTSRefresher_FailFastOnInitialError

#### Scenario: Refresh failure inside a short-TTL window keeps the pair until expiry

- **WHEN** a refresh fails while the delivered TTL is shorter than the built-in default
- **THEN** the previous credentials stay in place, the poll continues on the last computed interval, and the expired alarm fires exactly once once the set is past its `Expiry`
