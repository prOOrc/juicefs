# RB: инцидент — компрометация Company KEK (rb-incident-kek-compromise)

**Триггеры:** утечка KEK с render-ноды (дамп памяти), компрометация KMS-ключа/SA, алерт JuiceFSMlockFailures (ключ ушёл в swap), аномалия доступа (drive_security_events: mass_file_access).

**Blast radius:** все файлы компании, KEK которой скомпрометирован. Границы: S3-ciphertext без KEK нечитаем (FR-TEST-27); Redis wrapped-FEK без KEK нечитаем (FR-TEST-28/29).

## Экстренный порядок (время = риск)

1. **Остановить выдачу KEK**: приостановить `platform-api-authz-grpc` rollout получения KEK — `kubectl scale deploy platform-api-authz-grpc --replicas=0` (render/user теряют только выдачу новых KEK; проверка через KeyManagerUnavailable алерт — ожидаем).
2. **Отозвать сессии render-нод**: rotate node IAM / revoke IAM-токенов (`yc compute instance stop` подозрительных нод / смена SA).
3. **KMS**: создать новую версию ключа и сделать её primary БЕЗ выдачи material старой версии через API — `yc kms symmetric-key rotate --id <key-id>`; при полной компрометации ключа (утечён material) — создать НОВЫЙ ключ `drive-kek-<company>-v2`, обновить `KMS_KEY_ID` в secret `drive-grpc-creds`.
4. **Данные**: полный re-wrap иерархии компании: `ProvisionCompanyKEK` (новая версия) → для каждого файла `RotateFileKey` (rb-fek-rotation) → `reencrypt --rotate-cek` (rb-cek-rotation). Старые wrapped-слои инвалидируются.
5. **Аудит**: выгрузить `drive_key_access_log` за период компрометации (операции `fetch_company_kek`, actor_type render_node/unknown), сверить с anomaly-событиями.
6. Вернуть replicas=0 → 1; проверить чтение/запись пользователем и render.

## После инцидента

- Пост-мортем: путь утечки, объём прочитанного (по аудиту — нижняя граница), окно экспозиции.
- Ротация STS SA static key (`drive-grpc-creds`).
- Обновить threat model (audit-package §T9) при изменении границы.
