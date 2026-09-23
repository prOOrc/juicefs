# Security test report (stage 9, task 9.5)

- Date: 2026-09-23 19:22 MSK
- Scenarios 26/29: Go tests against Redis at `127.0.0.1:6379`
- Scenarios 27/28/30: pure crypto harness (no services required)

| FR-TEST | Scenario | Test / harness | Status |
|---|---|---|---|
| FR-TEST-26 | Инсайдер с Read на X пытается получить FEK Y → deny | `TestInsiderCrossFileFEK_Denied` (pkg/meta) | PASS |
| FR-TEST-27 | Утечка S3: чанки нечитаемы без CEK | crypto harness (tests/security) | PASS |
| FR-TEST-28 | Утечка Redis: wrapped_fek нечитаем без Company KEK | crypto harness (tests/security) | PASS |
| FR-TEST-29 | Компрометация render-ноды: доступ только к своей компании | `TestRenderCrossCompany` (pkg/meta) | PASS |
| FR-TEST-30 | Утечка Redis + S3 одновременно: данные нечитаемы без Company KEK | crypto harness (tests/security) | PASS |

Overall: **PASS**

## Details

### FR-TEST-27 — S3 leak: chunks unreadable without CEK

- DecryptBlock with a random CEK → error (GCM tag mismatch)
- raw AES-256-GCM open of the AGDF payload with a zero key (empty and exact AAD guesses) → error
- the AGDF blob contains no plaintext bytes (first 256 B checked)

### FR-TEST-28 — Redis leak: wrapped_fek unreadable without Company KEK

- UnwrapFEK with a random KEK → error (GCM tag mismatch)
- raw AES-256-GCM open of the AGFK payload with a zero key → error
- the AGFK blob contains no plaintext FEK bytes

### FR-TEST-30 — Redis+S3 leak: data unreadable without Company KEK

- step 1 (Redis): UnwrapFEK with any non-Company KEK → error — the key chain is broken at its first link
- step 2 (S3): UnwrapCEK with a random FEK → error (GCM tag mismatch)
- step 3 (S3): DecryptBlock with a random CEK → error (GCM tag mismatch)
- sanity: with the real FEK the AGCK→AGDF chain decrypts — the Company KEK is the only missing piece
