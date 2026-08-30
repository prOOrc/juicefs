# Tasks: inventory-encrypt

Инвентаризационный change: задачи — исследование документов и кода, написание spec, верификация утверждений. Продакшен-код не меняется.

## 1. Research

- [x] 1.1 Прочитать SRS-001 v2.2 полностью (`specs/srs/SRS-001-agio-drive-encryption.md`): иерархия ключей и форматы §4, хранение в Redis §5, user path §6, render path §7, операции §8, предусловия версионирования §9, offline/no-residuality §10, revocation §11, ротация §12, NFR §14, gRPC API §15, миграция §16, тестирование §18, AC §22. Проверка: каждый раздел, процитированный в `specs/domain-encrypt/spec.md`, существует в SRS.
- [x] 1.2 Прочитать мастер-план (`.qwen/plans/agio-drive-encrypt-master.md`): обзор этапов §3, межэтапные контракты §4.1–4.8 (бинарные форматы, поля метаданных, proto-расширения, параметры кэшей, решения D1–D12), статусная таблица §8. Проверка: все 10 этапов «не начат» (greenfield подтверждено).
- [x] 1.3 Прочитать stage-планы 01–10 (`.qwen/plans/stage-01-foundation.md` … `stage-10-production-rollout.md`): «Решения этапа», критерии приёмки, риски каждого подплана. Проверка: все отклонения от SRS зафиксированы в design.md → Known Deviations (9 пунктов).
- [x] 1.4 Верифицировать as-is состояние форка (`agio-drive-v2`): per-file шифрования нет — `grep -rn "AGFK\|AGCK\|AGDF\|WrappedFek\|WrappedCEK" pkg/` возвращает только upstream `pkg/object/encrypt.go` (volume-level, sync/load); `sliceBytes = 24` (`pkg/meta/slice.go:91`); `ChunkStore.NewReader/NewWriter` без ключей (`pkg/chunk/chunk.go:41-42`); `Format.EncryptKey/EncryptAlgo/KeyEncrypted` (`pkg/meta/config.go:93-95`); существуют `handle` (`pkg/vfs/handle.go:32`), `fileReader`/`fileWriter`, `NewReloadableStorage` (`cmd/mount.go:462`). Проверка: grep-команды возвращают ожидаемые результаты.
- [x] 1.5 Верифицировать as-is состояние agio-platform: KMS / Secret Manager / STS / KeyManager отсутствуют — `grep -ril "kms\|secretmanager" src/internal/drive src/application/authz` и `grep -rn "DriveKeyManagerService" src` пусты (проверено на текущей ветке репозитория). Проверка: grep возвращает ничего.

## 2. Spec

- [x] 2.1 Написать delta-спеку `specs/domain-encrypt/spec.md`: 16 требований — иерархия ключей и форматы AGFK/AGCK/AGDF; crypto-метаданные в Redis (attr-suffix, slice-хвост, Format); шифрование чанков в data path (ciphertext S3+кэш, legacy passthrough); user path Create (KeyManager + rollback) и Open (authz-gated FEK, LRU, `cached_fek_version`); инвариант идентичности (`sub` ≡ `user.id` UUID, fail-closed); render path (прямой Redis+S3, Company KEK по IAM ноды, cross-company EIO); Clone/CopyFileRange/Compaction (CEK re-wrap, zero-copy); предусловия версионирования (хуки + `fek_version`); offline-connected + no residuality (memclr/mlock/журнал); revocation + STS (generation через heartbeat, TTL ≤30s, STS ≤60 мин prefix-scoped); ротация ключей (KEK/FEK/CEK/KMS); миграция legacy (идемпотентный возобновляемый reencrypt); gRPC API surface (MetaService-расширения + контракт `DriveKeyManagerService`); аудит и anomaly detection; performance targets. Проверка: `openspec validate inventory-encrypt` — PASS.
- [x] 2.2 Строки Verified-by не ставить: шифрование greenfield, тестов ещё нет (правило: «Теста нет — строку не ставь»). Проверка: `grep -c "Verified-by" openspec/changes/inventory-encrypt/specs/domain-encrypt/spec.md` == 0.

## 3. Verification

- [x] 3.1 Cross-check согласованности SRS ↔ мастер-план ↔ stage-планы: форматы AGFK/AGCK/AGDF (SRS §4.3 = мастер-план §4.1 = код этапов 1–2), параметры кэшей (SRS §14.2 = мастер-план §4.6), proto-расширения (SRS §15.2 = мастер-план §4.3), иерархия ключей (SRS §4.1 = мастер-план §4.7). Проверка: расхождений без фиксации в Known Deviations нет.
- [x] 3.2 Spot-check as-is фактов, процитированных в design.md (Component Map, Architectural Context): все file:line-ссылки и имена типов действительны на ветке `agio-drive-v2`. Проверка: `grep -n "sliceBytes" pkg/meta/slice.go`, `grep -n "NewReader(" pkg/chunk/chunk.go`, `grep -n "EncryptKey" pkg/meta/config.go` — совпадают со ссылками.
- [x] 3.3 Регрессия (код не менялся): `go build ./...` — PASS; `git status` — изменены только файлы `openspec/changes/inventory-encrypt/`.

## Definition of Done

- [x] `go build ./...` — PASS.
- [x] `go vet ./pkg/meta/ ./pkg/chunk/ ./pkg/vfs/` — PASS (без изменений, регрессия).
- [x] `openspec validate inventory-encrypt` — PASS.
- [x] Продакшен-код не изменён: `git diff --stat` — только файлы под `openspec/`.
