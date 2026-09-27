# Load test report — gate 10.7a re-measure on the production topology (FUSE + S3, stage)

- Date: 2026-09-27 22:30 MSK (session 2026-09-28)
- Fork HEAD: `918b42a` (agio-drive-v2) — binary built in-cluster: `juicefs 1.4.1+2026-09-28.918b42a` (lite build)
- Decision context: owner selected option 3 of `report-2026-09-27-linux.md` §Decision request — re-measure FR-TEST-24 on the production data path instead of the in-memory micro-benchmark.

## Topology (production-shaped)

| Item | Value |
|---|---|
| Where | privileged pod on a stage k8s node (`cl1jdf7kc0hc98uqog0i-ujas`, YC standard-v3, 2 vCPU / 8 GiB, kernel via node) |
| Data path | FUSE mount (fusermount3 3.17.2) → JuiceFS client → YC Managed Redis (meta, `c-c9qk6b7skr9u09v58guh.rw.mdb.yandexcloud.net`, internal network) + YC S3 bucket `juicefs-data-b-stage` (internal network) |
| A/B volumes | `bench-enc` (Redis DB 27, `EncryptionEnabled: true`) vs `bench-legacy` (Redis DB 28, no encryption) — both freshly formatted, same bucket/prefix scheme |
| Workload | 8 parallel writers × 256 MiB (dd bs=1M) = 2 GiB per run; then per-file `sync -f` (fsync forces upload to S3); then 8 parallel readers; block cache 1 MiB ⇒ reads are S3-dominated (cold-read path) |
| Runs | 3 per leg, fresh files per run; sizes verified (268 435 456 B) |
| Encryption path | per-file FEK (AES-256-GCM wrap under Company KEK), per-1MiB-block AGDF (AES-256-GCM, random nonce, AAD binding) — the same code path as production |

## Results

Throughput (aggregate MB/s), 2 GiB per run:

| Run | legacy write (async) | enc write (async) | legacy write+fsync (→S3) | enc write+fsync (→S3) | legacy read (S3) | enc read (S3) |
|---|---|---|---|---|---|---|
| r1 | 296.6 | 287.6 | 283.6 | 272.8 | 171.3 | 160.2 |
| r2 | 276.8 | 268.8 | 265.6 | 255.3 | 168.9 | 162.0 |
| r3 | 222.3 | 249.3 | 214.3 | 238.6 | 167.1 | 164.5 |

Degradation (encrypted vs legacy):

| Run | write (async) | write+fsync (→S3) | read (S3) |
|---|---|---|---|
| r1 | +3.0% | +3.8% | +6.5% |
| r2 | +2.9% | +3.9% | +4.1% |
| r3 | −12.2% | −11.3% | +1.5% |
| **median** | **+2.9%** | **+3.9%** | **+4.1%** |

(r3 is noise-dominated on the legacy side — legacy write dipped to 222 MB/s, encrypted was faster; medians are the robust statistic.)

## GO/NO-GO

**Median degradation on the production topology: ≤ 3.9% (write), ≤ 4.1% (read) — within the SRS target "degradation ≤ 10%".**

### **GATE 10.7(a): GO** — 2026-09-27 (owner-selected re-measure option 3)

Both legs ran on identical stage infrastructure over the real data path (FUSE client → Redis meta + S3 blocks); the encrypted path pays ~3–4% median overhead, reads ~4%. Combined with gate 10.7(b) (concurrent FR-TEST-25 PASS, `report-2026-09-27-linux.md`), both quality gates are closed. Remaining for task 10.7 closure: pilot company + 72 h observation (`docs/ops/rollout.md`).

## Methodology notes

- The two legs used two separate freshly formatted volumes (identical format except `--encryption-enabled`), same Redis cluster, same S3 bucket — storage-level conditions identical.
- `sync -f <file>` (fsync) after async writes makes the write number include the S3 upload completion — the honest "data durably written" figure. The near-zero fsync times show JuiceFS already overlapped uploads with the async write phase.
- Reads ran with `--cache-size 1` (1 MiB block cache) so blocks are fetched from S3, matching the cold-read production path rather than a cache-replay.
- Node CPU (2 vCPU) is the shared stage cluster class; absolute numbers (~170–290 MB/s aggregate) are bounded by node CPU + S3 bandwidth, not by the encryption layer: on the same node, the in-memory harness measured 13.6 GB/s aggregate AES-GCM capability (see `report-2026-09-27-linux.md` §Cause analysis).
- Cleanup: both bench volumes destroyed (`juicefs destroy`, UUID-verified), bench pod and config deleted; no YC-managed resources created.
- Raw log: job `drive-bench-107a` (stage ns); RESULT lines reproduced in this report verbatim via the ns-timings above.
