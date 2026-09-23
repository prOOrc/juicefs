# Stage-прогон: чеклист (stage 9, task 9.6)

Ручной прогон полного цикла на **stage-окружении** с реальным platform
(authz + KeyManager), YC KMS, SpiceDB и PostgreSQL. Локальные unit/integration
тесты покрывают логику; этот чеклист проверяет интеграцию с реальными
сервисами (TLS, IAM, SpiceDB-схема, KMS-ключи, STS).

**Статус:** чеклист подготовлен 2026-09-23. Выполняется на stage вручную —
результаты записываются в таблицу «Результаты» (проверка задачи 9.6:
«чеклист выполнен, результаты записаны»).

## Предусловия

| # | Ресурс | Что проверить |
|---|---|---|
| P1 | platform на stage | `DriveKeyManagerService` и authz поднят, TLS-сертификаты валидны, CA доступен клиентам (`--keymanager-tls-ca`) |
| P2 | YC KMS | Company KEK provisioned для двух компаний (A, B); роли: `kms.keys.encrypterDecrypter` у platform, у render-ноды — только через KeyManager (прямой доступ к KEK не нужен) |
| P3 | SpiceDB | Схема drive задеплоена; компания A: owner + 2 пользователя (user-a1 с Read/Edit на `/projects/`, user-a2 без прав); компания B: owner-b |
| P4 | PostgreSQL | `users` импортированы (OIDC sub = UUID = user.id); user-a1, user-a2, owner-a, owner-b |
| P5 | Redis + S3 (stage) | Том JuiceFS с `EncryptionEnabled=true`; S3-бакет с prefix-политиками по компаниям |
| P6 | Клиенты | Бинарь форка с этапами 1–8; OIDC-клиент (браузер/`oidc-login`) для user-a1, user-a2, owner-a; render-нода в YC с IAM-профилем компании A |

## Сценарии

### S1. Полный пользовательский цикл (реальный platform + KMS + SpiceDB + PG)

1. Mount тома user-a1 через Meta Proxy с OIDC:
   ```sh
   juicefs mount <meta-url> /mnt/a1 \
       --keymanager-service <km-stage-host:port> --keymanager-tls-ca /etc/ssl/agio/ca.pem \
       --sts-enabled --company-id <uuid-A> ...
   ```
2. `touch /projects/s1.exr`, запись 1 MiB, `sync`, чтение обратно — данные совпадают.
3. В Redis: у inode файла `wrapped_fek` (AGFK, 69 байт) + `drive_file_id`.
4. В audit log platform: записи `CreateFileKey` и `GetFileFEK` для user-a1 (AC-12).
5. user-a2 (без прав) открывает тот же файл → EACCES, FEK не выдан; audit: `deny`.

**Ожидание:** шаги 1–4 работают; шаг 5 — deny. **Покрывает:** AC-1, AC-2, AC-12, AC-16.

### S2. Owner bypass через SpiceDB (AC-15)

1. owner-a (без явных прав на `/projects/s1.exr`, только `own` на компанию A)
   открывает файл → FEK выдан, чтение работает.
2. В коде platform: `CheckOrganizationAdmin` **не** вызывается в пользовательской
   модели KeyManager (проверить по логам/коду — байпас идёт через `PermissionOwn`).

**Ожидание:** owner получает доступ; админ-проверка организации не используется.

### S3. Revocation с замером времени (AC-10)

1. user-a1 смонтирован, файл открыт, FEK в кэше клиента.
2. В SpiceDB отозвать право user-a1 на `/projects/` (revoke).
3. Замерить время до потери доступа:
   - новый `Open` другого файла → deny сразу (authz);
   - уже закэшированный FEK: клиент теряет доступ после heartbeat generation
     change + TTL (замерить фактическое время, сравнить с целевым TTL).
4. STS-креденшелы user-a1 истекают по expiry — после этого даже metadata-операции
   через S3 невозможны.

**Ожидание:** доступ потерян в пределах TTL heartbeat + STS expiry; время записано ниже.

### S4. STS prefix-policy (NFR-SEC)

1. Выдать STS-креденшелы user-a1 (`GetSTSCredentials`).
2. С этими креденшелами: GET объекта в prefix компании A → OK; GET объекта в
   prefix компании B → AccessDenied.

**Ожидание:** политика ограничивает доступ своим prefix'ом.

### S5. Render-нода с IAM (AC-4, AC-5)

1. На render-ноде (YC VM, IAM-профиль компании A):
   ```sh
   juicefs render-mount <meta-url> /mnt/render \
       --company-id <uuid-A> --company-code <code-A> \
       --keymanager-service <km-stage-host:port> --keymanager-tls-ca /etc/ssl/agio/ca.pem
   ```
2. Чтение зашифрованного файла компании A → работает **без OIDC, без прокси, без PG**
   (KEK получен один раз через `FetchCompanyKEK` по IAM).
3. Попытка прочитать inode компании B напрямую (из метаданных) → EIO/ENOENT
   (chroot + AAD-привязка FEK к company_id).
4. В audit log: запись `FetchCompanyKEK` с node-id ноды.

**Ожидание:** шаги 1–2 работают, шаг 3 — deny, шаг 4 — запись есть.

## Результаты

| Сценарий | Дата | Исполнитель | Результат (pass/fail) | Замечания / замеры |
|---|---|---|---|---|
| S1 Полный цикл | | | | |
| S2 Owner bypass | | | | |
| S3 Revocation (время до потери доступа: ___) | | | | |
| S4 STS prefix-policy | | | | |
| S5 Render-нода с IAM | | | | |
