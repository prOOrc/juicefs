# domain-encrypt Delta

## ADDED Requirements

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
