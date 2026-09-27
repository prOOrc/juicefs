# Load test report — gate 10.7 re-measure on Linux (FR-TEST-24 concurrent + FR-TEST-25 concurrent)

- Date: 2026-09-27 21:10 MSK
- Fork HEAD: `1ff46710` (agio-drive-v2) — includes go-redis fork fix `prOOrc/go-redis/v9 v9.18.1` (commit `908857f`, branch `release-9.18`) and the concurrent harness (`0c1e1a3d`, `1ff46710`).
- Purpose: quality gate 10.7 of `openspec/changes/implement-encrypt` — (a) FR-TEST-24 re-measure on production-class hardware (linux, concurrent load), go/no-go vs the SRS "degradation ≤ 10%" target; (b) concurrent FR-TEST-25 after the go-redis fix.

## Hardware & environment

| Item | Value |
|---|---|
| Host | itx.lan (bare metal desktop) |
| CPU | AMD Ryzen 7 9800X3D, 8C/16T, up to 5.27 GHz, L3 96 MiB |
| RAM | 60 GiB |
| Kernel / OS | 7.0.0-34-generic, KDE neon (Ubuntu-based) |
| Go | go1.27.1 linux/amd64 (native build) |
| Redis | 7.4.11 (docker redis:7-alpine, localhost:16379, DB 15) |
| Load | desktop host, load average ~2–3 from unrelated processes; CPU freq scaling active (~68%) |
| GOMAXPROCS | 16 |

Methodology: `tests/load` harness — local Redis + in-process Meta Proxy + counting fake KeyManager; FR-TEST-24 measures write+read throughput through `chunk.CachedStore` with mem object storage, encrypted (per-1MiB-block AES-256-GCM via AGDF, CEK) vs legacy, N concurrent goroutines each streaming its own blob (slices ≤ 64 MiB); FR-TEST-25 measures RenderMeta metadata ops (GetAttr/Open mix) with N goroutines and asserts 0 KeyManager round-trips. Full auto-reports: `/tmp/report-*.md` on itx.lan (regenerable).

## FR-TEST-24 — Throughput encrypted vs legacy (concurrent)

### 64 MiB total per run (default harness size; short runs, higher noise)

| Run | clients | legacy MB/s | encrypted MB/s | degradation | vs target ≤10% |
|---|---|---|---|---|---|
| r1 | 8 | 1972.1 | 2050.3 | −4.0% | PASS |
| r2 | 8 | 2353.4 | 2155.2 | 8.4% | PASS |
| r3 | 8 | 2595.1 | 2168.6 | 16.4% | miss |
| r1 | 16 | 2347.5 | 2010.3 | 14.4% | miss |
| r2 | 16 | 2536.0 | 2025.1 | 20.1% | miss |
| r3 | 16 | 2324.2 | 1586.7 | 31.7% | miss |

### 1 GiB total per run (longer interval, stabler)

| Run | clients | legacy MB/s | encrypted MB/s | degradation | vs target ≤10% |
|---|---|---|---|---|---|
| r1 | 8 | 2588.6 | 2417.5 | 6.6% | PASS |
| r2 | 8 | 2921.2 | 2399.2 | 17.9% | miss |
| r3 | 8 | 2770.4 | 2209.1 | 20.3% | miss |
| r1 | 16 | 2548.7 | 1867.6 | 26.7% | miss |
| r2 | 16 | 2545.3 | 2216.3 | 12.9% | miss |
| r3 | 16 | 2557.8 | 1783.5 | 30.3% | miss |

### 1 GiB, pinned to physical cores (taskset; c=8 → cores 0–7, c=16 → 0–15)

| Run | clients | legacy MB/s | encrypted MB/s | degradation | vs target ≤10% |
|---|---|---|---|---|---|
| r1 | 8 | 2546.1 | 2374.3 | 6.7% | PASS |
| r2 | 8 | 2895.1 | 2330.5 | 19.5% | miss |
| r3 | 8 | 2823.2 | 2261.0 | 19.9% | miss |
| r1 | 16 | 2654.4 | 2080.1 | 21.6% | miss |
| r2 | 16 | 2581.9 | 2100.7 | 18.6% | miss |
| r3 | 16 | 2550.3 | 2182.0 | 14.4% | miss |

### Summary FR-TEST-24

- Median degradation: **~18–20% at 8 clients, ~19–27% at 16 clients** (best cases ~7%, worst ~30%). Pinning did not change the picture.
- Legacy throughput is stable (~2.5–2.9 GB/s); the encrypted path is the noisy side (1.6–2.4 GB/s).
- Comparison with the darwin/arm64 baseline (separate platform, sequential, 64 MiB: legacy 3865.7 vs encrypted 2355.1 MB/s = 39.1%, `report-2026-09-27.md`): linux concurrent numbers are better but still above the ≤10% target.
- **GO/NO-GO: NO-GO against the ≤10% target** — the gate criterion is not met; per gate 10.7 the target is NOT weakened and AC-11 stays "DEVIATION pending decision". The decision is escalated to the product owner (see "Decision request" below).

### Cause analysis (measured)

Micro-benchmark on the same host (`crypto/aes` + `cipher.NewGCM`, 1 MiB buffers): 5.0–6.6 GB/s per core, 13.6 GB/s aggregate at 8 goroutines. Raw crypto capability therefore exceeds the measured encrypted throughput (~2.2–2.4 GB/s) by ~6×, i.e. AES-GCM compute is NOT the bottleneck. The overhead comes from the extra data movement and allocation in the encrypted block path (`pkg/chunk/cek_encrypt.go`): `gcm.Seal(nil, …)` allocates a fresh ciphertext buffer and copies the plaintext into it, then a second copy into the AGDF header+body buffer (and symmetrically on decrypt) — ~2–3 extra full-buffer copies per 1 MiB block on a workload whose legacy path is pure memory bandwidth (mem object store). This matches the measured 15–20% gap and predicts the gap shrinks on a production topology where the data path is network/disk-bound (S3 upload at ~1 GB/s ≪ the in-memory 2.4 GB/s case).

## FR-TEST-25 — Render metadata RPS (concurrent) — PASS

| Run set | goroutines | ops/s | GetFileFEK calls | p50 / p99 (Open hit) | Status |
|---|---|---|---|---|---|
| 64 MiB runs | 8 | 261561–262038 | 0 | 100µs / 300–600µs | PASS |
| 64 MiB runs | 16 | 278028–278601 | 0 | 100µs / 300–500µs | PASS |
| 1 GiB runs | 8 / 16 | 261449–278625 | 0 | — | PASS |

- The "0 extra KeyManager round-trips" assertion holds under concurrency: every Open resolves the FEK locally (LRU or UnwrapFEK under the Company KEK).
- Throughput scales with goroutines (261k → 278k ops/s) and is bounded by the single local Redis, not the client.
- FR-TEST-22 (p99 ≤ 5 ms) and FR-TEST-23 (p99 ≤ 100 ms) pass with two orders of margin.
- **Gate 10.7(b) — concurrent FR-TEST-25 after the go-redis fix: CLOSED (PASS).** The v9.18.0 reply-desync hang is gone with `prOOrc/go-redis/v9 v9.18.1`; no hangs or errors in any run (≈45 min of total concurrent load incl. development runs).

## Decision request (gate 10.7a — FR-TEST-24)

The SRS target "degradation ≤ 10%" (concurrent, linux) is not met by the in-memory harness (median ~18–27%). Options:

1. **Accept DEVIATION** (AC-11 stays "DEVIATION, pending decision"): ship with documented ~20% worst-case metadata/crypto overhead on the in-memory micro-benchmark; production data path is S3/network-bound where the relative cost is expected to be materially lower (single-GiB/s networks vs 2.4 GB/s in-memory). Cheapest; consistent with the harness being a worst case.
2. **Optimize the encrypted block path** in the fork (single-allocation Seal into a preallocated AGDF buffer, avoid the second copy, pooled buffers) — plausibly halves the added copies; re-run this gate afterwards. Engineering effort: days, fork-only change + vectors re-run.
3. **Re-measure on the production topology** (FUSE mount + S3 bucket on stage infra, concurrent readers/writers through the real path) and use that measurement for go/no-go instead of the in-memory micro-benchmark — measures what production actually experiences, but needs a stage runbook window.
4. **Weaken the SRS target** — explicitly NOT recommended; the SRS was frozen with ≤10%.

Decision needed from the product owner; per gate wording the pilot rollout stays blocked until 10.7a has a recorded go decision (either via option 1/3 acceptance or option 2 re-measure).
