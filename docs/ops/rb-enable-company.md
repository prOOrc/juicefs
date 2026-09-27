# RB: включение шифрования для компании (rb-enable-company)

**Сценарий:** подключение компании (пилот или prod-rollout per company, rollout.md §Порядок).

## Предусловия

1. Клиенты компании на slice-формате v2 (fork HEAD с шифрованием, stages 1–9).
2. Terraform: ключ `drive-kek-<company>` создан (change `drive-crypto-infra`, `drive_crypto_companies` + apply).
3. IAM: `sa-k8s-resources` — `kms.keys.encrypterDecrypter` на ключе (drive_crypto.tf).
4. Бэкапы Redis активны (10.2); restore-тест пройден на stage хотя бы раз.
5. STS: `agio-drive-sts` SA + bucket-binding существуют (`drive_sts.tf`).

## Порядок

1. **Provision KEK** (идемпотентно, повтор — 0 операций):
   `keymanager provision --company <company_id>` (platform CLI; ранний возврат при активной версии + unique-violation recheck — `TestKeyManager_ProvisionCompanyKEK_Idempotent`).
2. **Включить шифрование volume** (идемпотентно):
   `juicefs enable-encryption <meta-url>` → `Format.EncryptionEnabled=true, KEKVersion=v` (новые записи шифруются немедленно; legacy-файлы читаются passthrough).
3. **Миграция legacy-файлов** (если есть): `juicefs reencrypt <meta-url> --volume` с IOPS/bandwidth-лимитами; мониторинг `juicefs_reencrypt_files_*`.
4. **Метрики/алерты**: убедиться, что scrape gRPC-деплоя (:9091) активен и `keymanager_*` видны; правила `docs/ops/grafana-alerts-drive.yaml` подключены.
5. **Пилотное окно**: 72 ч наблюдения (алерты молчат), после — включение следующим компаниям.

## Проверки

- Запись нового файла → attr содержит crypto suffix (AGFK), S3-объект = AGDF (magic `AGDF`), slice-record с wrapped-CEK.
- Чтение пользователем и render-нодой (interop AC-4); кросс-компания deny (S29).
- `keymanager_provision` в аудите: `result=allow`, повторный запуск — без операций.

## Откат (до истечения пилотного окна)

- Флаг `--encryption-enabled=false` НЕ изменяет уже зашифрованные файлы (они читаются по-прежнему); откат = остановка НОВЫХ зашифрованных записей (rollout.md §Rollback).
