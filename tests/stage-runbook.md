# Stage-прогон: чеклист (stage 9, task 9.6)

Ручной прогон полного цикла на **stage-окружении** с реальным platform
(authz + KeyManager), YC KMS, SpiceDB и PostgreSQL. Локальные unit/integration
тесты покрывают логику; этот чеклист проверяет интеграцию с реальными
сервисами (TLS, IAM, SpiceDB-схема, KMS-ключи, STS).

**Статус:** чеклист подготовлен 2026-09-23, **выполнен на stage 2026-09-27** —
результаты в таблице «Результаты» (проверка задачи 9.6: «чеклист выполнен,
результаты записаны»). Инвентарь фактического окружения и применённые для
прогона подготовительные шаги — в разделе «Инвентарь P1–P6 (прогон 2026-09-27)».
**Перепрогон S4 2026-09-27** (закрытие FR-REV-3, tasks 7.8–7.12, ADR-003) —
**pass**, см. строку S4 и раздел «Инвентарь STS-перепрогона (2026-09-27)»;
открыый вопрос «принимает ли YC STS условие `s3:prefix`» закрыт фактом
прогона (принимает, fallback не нужен).

## Предусловия

| # | Ресурс | Что проверить |
|---|---|---|
| P1 | platform на stage | `DriveKeyManagerService` и authz поднят, TLS-сертификаты валидны, CA доступен клиентам (`--keymanager-tls-ca`) |
| P2 | YC KMS | Company KEK provisioned для двух компаний (A, B); роли: `kms.keys.encrypterDecrypter` у platform, у render-ноды — только через KeyManager (прямой доступ к KEK не нужен) |
| P3 | SpiceDB | Схема drive задеплоена; компания A: owner + 2 пользователя (user-a1 с Read/Edit на `/projects/`, user-a2 без прав); компания B: owner-b |
| P4 | PostgreSQL | `users` импортированы (OIDC sub = UUID = user.id); user-a1, user-a2, owner-a, owner-b |
| P5 | Redis + S3 (stage) | Том JuiceFS с `EncryptionEnabled=true`; S3-бакет с prefix-политиками по компаниям |
| P6 | Клиенты | Бинарь форка (свежий HEAD `agio-drive-v2`, содержит фиксы, найденные при прогоне); OIDC-клиент (браузер/`oidc-login`) для user-a1, user-a2, owner-a; render-нода в YC с IAM-профилем компании A |

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

Прогон 2026-09-27, автоматизированная сессия (I. Obukhov, ZCode); форк
`agio-drive-v2` HEAD `cb881d42`, platform `feature/drive-v2` HEAD `a8fa325c`.
Тест-компании `tst-a`/`tst-b` (новые, существующие данные stage не тронуты);
том `tst-enc-a` (stage Redis DB 25, S3 `juicefs-data-b-stage`,
`EncryptionEnabled=true`, KEK v1).

| Сценарий | Дата | Исполнитель | Результат (pass/fail) | Замечания / замеры |
|---|---|---|---|---|
| S1 Полный цикл | 2026-09-27 | I. Obukhov (ZCode) | **pass** | user-a1: mount через meta-proxy (OIDC Bearer, authz, KeyManager) → create `projects/s1.exr`, запись 1 MiB, sync, чтение — md5 совпадает (`908e4e56…`). В attr инода: `wrapped_fek` 69 байт (AGFK) + `drive_file_id` (UUID) + `AES-256-GCM`, fek_version 1. Audit platform: `create_file_key`/`get_file_fek` allow для user-a1. user-a2 (без прав): каталоги компании скрыты, чтение файла → EIO, 0 записей выдачи FEK в audit — deny |
| S2 Owner bypass | 2026-09-27 | I. Obukhov (ZCode) | **pass** | owner-a (company `own` в SpiceDB, без path-прав) открыл и прочитал `s1.exr` — md5 совпал; `CheckOrganizationAdmin` не вызывался (0 обращений в логах за окно прогона) |
| S3 Revocation (время до потери доступа: **новый Open — сразу (deny); кэш FEK — ~12–15 c**) | 2026-09-27 | I. Obukhov (ZCode) | **pass** | Отзыв: soft-delete `drive_object_permission` + удаление SpiceDB-роли + `INCR drivepermgen:<user-a1>`. Новый Open чужого файла → deny немедленно (authz). Уже закэшированный FEK: чтение терялось через **~14.5 c** после отзыва (heartbeat 12 c → generation bump → WipeKeys → re-Open denied) — в пределах целевого TTL heartbeat. STS-креденшелы после отзыва не выдаются (fail-closed, см. S4) |
| S4 STS prefix-policy | 2026-09-27 (перепрогон, закрытие FR-REV-3) | I. Obukhov (ZCode) | **pass** | Полный сценарий на реальном YC STS (platform `feature-drive-v2-10656` = ed2ee79b, форк HEAD `113c10f2`). **S4.1 user-path:** mount user-a1 через meta-proxy с `--sts-enabled --company-id <tst-a>` — выдача `GetSTSCredentials` allow (audit: actor user-a1, refresh-выдачи каждые ttl/2), запись 1 MiB + чтение — md5 совпадает (`c6bed149…`), чанки реально залиты в S3 временными кредами (объекты `tst-enc-a/chunks/…` появились в момент записи); authz-проверки пути allow (`/companies/tst-a/projects/s4-sts-test.bin` VIEW/EDIT). **S4.2 отзыв:** после удаления SpiceDB-роли editor + INCR permgen новый запрос кред → fail-closed `PermissionDenied: no read permission on company` (mount не стартует), 3× deny в audit; TTL-истечение активных кред — 60 мин по дизайну (не ждали). **S4.3 node-path:** render-mount с IAM-токеном ноды (SA `drive-runbook-sa`, node `MAC3244`) — `FetchCompanyKEK` + `GetNodeSTSCredentials` allow (audit actor `render_node`), чтение зашифрованного файла S4.1 через node-маунт — md5 совпал; регрессия S5 (KEK-путь на свежем бинаре) — pass. **s3:prefix:** YC STS принял `StringLike` на `s3:prefix` (in-prefix LIST OK, out-prefix LIST denied — проверено живым probe'ом и прогоном) — fallback `sts_listbucket_unscoped` НЕ нужен (флаг остаётся false). Policy scope `<volume>/*` подтверждён: PUT/GET в `tst-enc-a/*` разрешены, `other-prefix/*` denied. |
| S5 Render-нода с IAM | 2026-09-27 | I. Obukhov (ZCode) | **pass** | render-mount от компании A (локальная эмуляция ноды с YC IAM-токеном): `FetchCompanyKEK` по IAM → чтение `projects/s1.exr` — md5 совпал, **без OIDC, без прокси, без PG**. S5.3: render-mount компании B — chroot `companies/tst-b` пуст, `projects/s1.exr` через B-chroot → ENOENT; разворот FEK компании A под KEK компании B невозможен (AAD-привязка, fail-closed EIO — подтверждено `TestRenderCrossCompany` и диагностикой: чужой KEK → GCM tag failure). S5.4: audit `fetch_company_kek` allow (actor_type `render_node`, node-id из IAM-верификации) для обеих компаний |

## Инвентарь P1–P6 (прогон 2026-09-27)

Фактическое состояние stage на момент прогона и шаги, потребовавшиеся для
закрытия предусловий (все — аддитивные, новые ресурсы; существующие не менялись).

| # | Ресурс | Состояние при разведке | Как закрыто |
|---|---|---|---|
| P1 | platform (authz + KeyManager) | Деплой `platform-api-authz` работал с образом `master-*` и args `["api"]` (только HTTP; gRPC-сервис отсутствует; Image собран без drive-v2) | Создан отдельный деплой `platform-api-authz-grpc` + svc :9090 (args `["grpc","--port","9090"]`, образ `feature-drive-v2-10654` = a8fa325c; configmap/secret общие). Доступ клиентов — `kubectl port-forward`; gRPC plaintext (клиенты `insecure`), TLS — prod-этап |
| P2 | YC KMS | Ключ `drive-kek-stage` не создан; `KMS_KEY_ID`/`LOCKBOX_FOLDER` в конфигах отсутствуют; у пода нет YC-креденшелов | KMS-ключ создан terraform apply -target (`drive-kek-stage`, роль `kms.keys.encrypterDecrypter` на `sa-k8s-resources`); поду выдан authorized key SA (secret `drive-grpc-creds`, env `YC_SA_KEY_FILE` — правка platform `sdk.go`, коммит b3028528). KEK provisioned для tst-a и tst-b (`agio-platform keymanager provision`) |
| P3 | SpiceDB | Схема drive задеплоена; writable `spicedb:50051` + readonly | Тест-компании tst-a/tst-b: company rows + SpiceDB-отношения founder/owner (+editor для user-a1); path-права — PG `drive_object_permission` (user-a1: edit на `projects/`) |
| P4 | PostgreSQL | Миграции 203–204 (drive_company_crypto_key, drive_key_access_log) не применены (max=202) | Применены через `migrate` на rw-хост; пользователи tst-owner-a/tst-a1/tst-a2/tst-owner-b созданы в Kratos (admin API) + PG `user` (OIDC sub = UUID = user.id) |
| P5 | Redis + S3 | Stage Redis доступен извне; тестового шифрованного тома нет | Том `tst-enc-a` на свободной DB 25 (S3 `juicefs-data-b-stage`, ключи неймспейсятся UUID тома); `enable-encryption` → `EncryptionEnabled=true`, KEK v1; маппинг volume→facility (`facility_juicefs_config`, companies-prefix) для authz-путей |
| P6 | Клиенты | Локальный бинарь форка; OIDC issuer публичен (`platform-stage.agio.services/.ory/hydra/public`); hydra-клиента для PKCE нет; render-ноды с IAM нет | Бинарь собран из свежего HEAD `cb881d42` (см. ниже — фикс NewSlice); зарегистрирован публичный PKCE-клиент `juicefs-drive-stage` в hydra; токены тест-юзеров получены authorization_code+PKCE и положены в кэш oidc-login; render-нода эмулирована локально: FetchCompanyKEK работает с любым валидным YC IAM-токеном (доступ определяется IAM-политиками на KMS-ключ и Lockbox), отдельная YC VM не требовалась |

### Дефекты, выявленные и исправленные при прогоне

1. **`NewSlice` всегда denied** (форк, `cb881d42`): authz-interceptor резолвил
   NewSlice через inode-кэш, но запрос не несёт inode → fail-closed deny →
   запись данных через authz-прокси была невозможна. Фикс: NewSlice
   (аллокация id без привязки к файлу) разрешён аутентифицированному
   пользователю; привязка слайса к файлу по-прежнему проверяется на `Write`
   (`Verified-by: TestInterceptor_NewSlice_Allowed`).
2. **`FetchCompanyKEK` возвращал KEK из нулей** (platform, `a8fa325c`):
   `memClear(kek)` выполнялся до копирования в ответ — render-нода не могла
   развернуть ни один FEK. Фикс + регресс-тест (red→green).
3. **IAM-верификатор ходил на легаси-эндпоинт** (platform, `99e4bbc3`):
   `resource-manager.apiary.io` не согласует ALPN h2 → все FetchCompanyKEK
   fail-closed; заменён на `resource-manager.api.cloud.yandex.net:443`
   (промежуточные `fc859338`/`61e49a51` — итерации диагностики эндпоинта).
4. **YC STS требует `Principal` в каждом statement'е session policy**
   (platform, `ed2ee79b`, перепрогон S4 2026-09-27): AssumeRole без Principal
   проходил, но любой S3-запрос под выданными кредами падал с
   `400 MalformedPolicy: required field missed: principal should be
   specified` — запись через `--sts-enabled` зависала в ретраях upload'а.
   Фикс: `buildSessionPolicy(..., includePrincipal)` ставит `"Principal":"*"`
   обоим statement'ам только для YC-ветки (AWS session policy Principal
   запрещает; AWS-политика осталась байт-в-байт прежней). Тем же прогоном
   подтверждено: YC STS принимает условие `StringLike` на `s3:prefix`
   (in-prefix LIST OK, out-prefix denied) — fallback
   `sts_listbucket_unscoped` не требуется.
5. **`platformKeyManager.Close()` был no-op** (форк, `113c10f2`, polish из
   ревью P3): type-assertion на сгенерённом клиенте не срабатывал — gRPC
   соединение не закрывалось на error-путях `--sts-enabled` render-mount.
   Фикс: клиент хранит `*grpc.ClientConn` и закрывает его напрямую; заодно
   соединение KeyManager закрывается на error-путях STS-блока и покрыт
   Bearer-metadata тестом (bufconn, замена удалённому
   `TestYCEphemeralKeyProvider`).
6. **Флейк CI `TestConsolidationE2E_JuiceFSFullFlow`** (platform, pipeline
   10656): E2E-тест консолидации drive периодически не дожидается условия за
   30s на свежем runner'е; с STS-изменениями не связан, ретрай зелёный.
   Кандидат на отдельный фикс (вне скоупа гейта).

## Инвентарь STS-перепрогона (2026-09-27, закрытие FR-REV-3)

Аддитивные ресурсы и шаги поверх инвентаря 9.6b (все в namespace `stage`,
существующие не удалялись; роли user-a1, отозванные в S3 9.6b, восстановлены
SpiceDB-отношением editor + un-delete `drive_object_permission` и после S4.2
возвращены).

| # | Ресурс | Как закрыто |
|---|---|---|
| R1 | SA для STS-выдачи | Terraform `drive_sts.tf` (agio-terraform-yc): SA `sa-drive-sts` + `yandex_storage_bucket_iam_binding` `storage.editor` на бакет `juicefs-data-b-stage` (уровень бакета, не folder); статический access key создан вручную `yc iam access-key create` (secret в tf state не попадает) |
| R2 | Секреты деплоя | K8s secret `drive-grpc-creds` (namespace stage): `STS_YC_ACCESS_KEY_ID`/`STS_YC_ACCESS_KEY_SECRET`/`STS_ROLE_ARN=agio-drive-sts-stage`; env в деплой — через `envFrom` |
| R3 | Деплой `platform-api-authz-grpc` | Обновлён образ до `feature-drive-v2-10656` (ed2ee79b, фикс Principal), env `STS_PROVIDER=yc` + `STS_ROLE_ARN`; `/tmp/drive-authz-grpc.yaml`; svc :9090, доступ клиентов через `kubectl port-forward` |
| R4 | PG-конфиг резолвера | `facility_juicefs_config` (facility immers): добавлен jsonb-ключ `Bucket=juicefs-data-b-stage` (jsonb_set); `company.default_facility_id` (Tst A) → immers — навигация резолвера `JuicefsConfigResolver`, пустые значения fail-closed |
| R5 | Node-path | render-нода эмулируется локально: IAM-токен `yc iam create-token --profile drive-runbook-sa` → `--iam-token-file`; SA ноды без ролей на S3-бакет (folder-bindings пусты, bucket-биндинг только у `sa-drive-sts`) — данные ходят под кредами, выданными platform'ом от своего SA |
| R6 | Клиенты | Форк-бинарь `113c10f2` (`make juicefs`); meta-proxy на `127.0.0.1:9561` (`--authz-service/--keymanager-service 127.0.0.1:9090`, `--authz-volume-name tst-enc-a`); mount user-path: `grpc://…/tst-enc-a?oidc-…&oidc-scopes=openid,offline_access,profile,email&oidc-cache-dir=/tmp/jfs-oidc-a1 --sts-enabled --company-id <tst-a>` (набор scopes должен совпадать с 9.6b — имя кэш-файла oidc-login хэшируется от конфига провайдера); негатив: `--sts-enabled` против `redis://` meta → fail-fast «--sts-enabled requires a meta proxy (grpc) endpoint» |
