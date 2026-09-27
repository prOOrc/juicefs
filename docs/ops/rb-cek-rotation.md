# RB: ротация CEK (rb-cek-rotation)

**Сценарий:** перезавёртывание Content Encryption Keys слайсов файла под свежим FEK (компрометация FEK/CEK-слоя, плановый re-wrap после смены KEK-иерархии).

**Механика:** CEK живёт внутри slice-record (wrapped CEK tail, D2). Ротация = перезапись wrapped-CEK без перезаписи ciphertext чанков в S3:
- `RewrapSlices` / `RewrapSlicesRange` (stage 5) — AGCK re-wrap по списку чанков, fail-closed (EIO при ошибке);
- компакция с CEK (`CompactChunk` + wrapped-CEK writeback);
- `juicefs reencrypt --rotate-cek <path>` (stage 8): все зашифрованные чанки файла получают свежий CEK; all-AGCK чанки НЕ пропускаются (в отличие от миграции legacy).

## Порядок

1. Определить файл(ы): `drive_file_id` из аудита (`drive_key_access_log`) или путь в volume.
2. В окно низкой нагрузки запустить `juicefs reencrypt <meta-url> <path> --rotate-cek` (IOPS-limiter, bandwidth-cap; идемпотентен, возобновляем из Redis-состояния).
3. Мониторинг: `juicefs_reencrypt_files_total` / `juicefs_reencrypt_files_remaining`; ошибки — fail-closed, воркер прерывает файл и логирует.

## Проверки

- Чтение файла пользователем и render-нодой — OK (interop, AC-4).
- `drive_key_access_log`: нет `result=error` по файлу после ротации.

## Когда НЕ нужно

- Плановая KMS-ротация KEK (rb-kek-rotation) — переписывание CEK не требуется (Decrypt старых версий валиден).
