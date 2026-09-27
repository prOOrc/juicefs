# agio Drive — encryption ops runbooks (task 10.5)

| Runbook | Сценарий |
|---|---|
| [rb-kek-rotation](rb-kek-rotation.md) | плановая/досрочная ротация Company KEK (KMS-версии) |
| [rb-fek-rotation](rb-fek-rotation.md) | ротация FEK файла: offboarding, утечка файла |
| [rb-cek-rotation](rb-cek-rotation.md) | re-wrap CEK слайсов (`reencrypt --rotate-cek`) |
| [rb-revocation](rb-revocation.md) | отзыв доступа: тайминги ≤30 c (факт 12–15 c) / ≤12 c+TTL / ≤60 мин |
| [rb-incident-kek-compromise](rb-incident-kek-compromise.md) | компрометация KEK компании |
| [rb-incident-redis-loss](rb-incident-redis-loss.md) | потеря Redis-метаданных, restore |
| [rb-incident-s3-leak](rb-incident-s3-leak.md) | утечка S3-bucket (ciphertext-only) |
| [rb-enable-company](rb-enable-company.md) | включение шифрования для компании (пилот/prod) |

**Связанные материалы:** IAM-матрица [iam-matrix.md](iam-matrix.md), алерты [grafana-alerts-drive.yaml](grafana-alerts-drive.yaml), threat model и traceability [../security/audit-package.md](../security/audit-package.md), порядок rollout [rollout.md](rollout.md), restore-тест [../../tests/security/redis-restore-test.sh](../../tests/security/redis-restore-test.sh).

**Мастер-runbook stage-инфры** (k8s/OIDC/STS): `tests/stage-runbook.md`; platform-репозиторий (agio-platform) ссылается на этот набор как на канонический (`docs/ops/README.md` там — кросс-реф).

Runbook'и в репозитории agio-platform — канонические копии отсутствуют намеренно: единый источник здесь; при изменении скопируйте README-кроссреф.
