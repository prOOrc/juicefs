# RB: инцидент — утечка S3-bucket (rb-incident-s3-leak)

**Триггеры:** публичный доступ к bucket, компрометация STS-кред, выгрузка объектов наружу, алерт доступа (Cloud Audit Logs).

**Модель угрозы:** утечка bucket даёт ТОЛЬКО ciphertext (AGDF-чанки) + имена объектов (slice-id). FEK нет в S3 по построению (data path не хранит ключи) → данные нечитаемы без Redis (wrapped FEK/CEK) и KEK (platform). FR-TEST-27: raw AES-GCM brute-force — fail-closed.

## Порядок

1. **Контейнировать**: отозвать STS-кред — `yc iam access-key delete`/rotate SA `drive-sts` (KMS_KEY_ID/STS_ROLE_ARN в `drive-grpc-creds`); проверить политику bucket (запрет публичного доступа); при компрометации prefix-политики — перекат inline-policy (`drive_sts.tf`).
2. **Оценить объём**: листинг объектов/логи за окно; сопоставить slice-id с `drive_key_access_log` (какие файлы затронуты — по chunk-индексам через Redis).
3. **Ротация слоя ключей** (по rb-fek-rotation/rb-cek-rotation) — НЕ требуется для защиты от уже утёкшего ciphertext (ключей в S3 нет), но выполняется при подозрении на **комбинированную** утечку (S3+Redis, rb-incident-kek-compromise).
4. **Оповещение**: по процедуре инцидентов (SOC 2 / TPN) — disclosure границы см. audit-package.

## Проверки fail-closed (воспроизводимые)

- `tests/security/run-all.sh` → S27 (S3 leak: decrypt без KEK), S29 (Redis+S3 leak).
- Скачать утёкший объект → попытка чтения как JuiceFS-блока без FEK → EIO/мусор, без раскрытия plaintext.

## Профилактика

- STS: prefix-scoped ephemeral creds (ADR-003), TTL 3600, `s3:prefix` StringLike (S4 pass).
- Никаких статических `Format.SecretKey` в зашифрованных volumes (FR-REV-3 закрыт 2026-09-27).
