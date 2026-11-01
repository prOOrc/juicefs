# Delta: drive-sts-client

## ADDED Requirements

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
