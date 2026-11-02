# Delta: domain-encrypt

## ADDED Requirements

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
