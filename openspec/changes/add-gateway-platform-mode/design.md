## Architectural Context

`juicefs gateway` (cmd/gateway.go): флаги 41-119 (`multi-buckets`, `bucket-name`, `refresh-iam-interval`, `mountpoint`, …), args `META-URL ADDRESS`; креды обязательны из env — ROOT_USER ≥3 / ROOT_PASSWORD ≥8, fallback MINIO_ACCESS_KEY/SECRET_KEY, иначе Fatal (:132-148); `MINIO_DOMAIN`/`MINIO_REFRESH_IAM_INTERVAL` пробрасываются os.Setenv (:151-158); строится `jfsgateway.NewJFSGateway` (:166-186) → синтетический minio-CLI `{"server","--address",…,"--anonymous"}` → `gateway2` → minio `ServerMainForJFS` (:207-235; minio-сторона `cmd/server-main.go:530`, `globalIsJFSGateway=true`). Object layer — jfsObjects (`pkg/gateway/gateway.go`), служебный префикс `.sys` (metaBucket :57): multipart staging `.sys/uploads/<up[:2]>/<upID>/` (parts, fs.json), complete-файл создаётся там же (:1425-1427), части копируются `fs.CopyFileRange` (:1439-1442), затем `fs.Rename(complete → path(bucket,object))` (:1490) и `Rmr` upload-каталога (:1508); Abort — `Rmr` (:1523-1528); фоновая очистка тиками 24ч `.sys/tmp/`, `.sys/uploads/`, trash >7д (:1531-1550,1571). На шифрованном томе каждое создание файла = CreateFileKey по пути создания (KEK/AAD всегда из пути — agio-platform key_manager_service.go:216/:273/:689).

## Component Map

- `cmd/gateway.go` — platform-ветка: флаг `--platform`; проверки fail-fast (subdir/refresh-iam-interval/MINIO_ROOT_*/обязательные env); обход обязательности ROOT-кредов (:132-148) в platform-режиме; startup GetFormat через grpc-клиент от service-identity; сборка env для minio.
- Env-контракт (заполняет деплой agio-cloud, change 8): `AGIO_S3_SERVICE_TOKEN` (секрет), `AGIO_KEYMANAGER_ADDR`, `AGIO_KEYMANAGER_NODE_CREDENTIAL` (секрет), `AGIO_AUTHZ_ADDR` (+ mTLS-пути), `AGIO_FACILITY_ID`, `AGIO_S3_BUCKET` (имя бакета). Секреты только env из k8s Secret — не в URL/флагах/логах.
- `pkg/gateway/gateway.go` — CompleteMultipartUpload: создание целевого файла по финальному пути, CopyFileRange частей, удаление staging; abort/cleanup без изменений.
- minio-форк получает всё через env (changes add-platform-identity-resolver / add-platform-authz-policy-engine).

## Execution Flow

1. Старт: `--platform` → fail-fast проверки → GetFormat к meta-proxy с service-token (service-identity прокси, change 6 разрешает) → при успехе запуск minio-server поверхности без ROOT-кредов.
2. Запросы: SigV4/policy — в minio-форке; метаданные — через grpcMeta с service-token (исходящая авторизация клиента, как oidc today).
3. Multipart: PutObjectPart → `.sys/uploads/…` (CreateFileKey под synthetic KEK — узловая ветка платформы); Complete → целевой файл по реальному пути (CreateFileKey по реальной компании) → CopyFileRange частей → удаление staging.

## Invariants

- Обычный (redis/minio-IAM) режим gateway — байт-в-байт прежний (без `--platform`).
- Секреты (service-token, node-credential) — только env; в логах имена.
- Complete не оставляет частичных объектов по целевому пути при ошибке (удаляются).
- `.sys/uploads`/`.sys/tmp` — единственные новые служебные префиксы записей gateway (без config-инфраструктуры: config ops в gateway и так выключены).

## Decisions & Rationale

1. **`--platform` флаг + env-секреты**: флаг виден в ps (не секрет); секреты через env из k8s Secret — не в meta URL (логи) и не в argv.
2. **GetFormat как startup-проба**: дешёвый RPC, разрешён service-identity на прокси (change 6); подтверждает и соединение, и токен, и том — до открытия S3-порты.
3. **Complete-at-destination безусловно в jfsObjects** (не только `--platform`): нешифрованные тома получают семантически идентичное поведение (новый файл вместо rename tmp — S3-контракт тот же), а grpc-шифрованный gateway без platform-флага всё равно сломан бы был; гейт усложнил бы код без выигрыша. Атомарность rename теряется: частичный объект при ошибке копирования виден в окне до удаления — принимается (лог + удаление; minio 2021-винтаж и так не даёт транзакционности Complete).
4. **Креды ROOT в platform-режиме — ошибка старта** (а не игнор): minio-сторона тоже отклоняет подписи root (change 4) — несовпадение ожиданий исключено на обоих слоях.

## Integration Points

- meta-proxy service-режим (change add-meta-proxy-service-identity): GetFormat разрешён, per-file RPC без authz.
- agio-platform узловая ветка + synthetic KEK (changes add-s3-gateway-service-identity; provision ops — change 8): `.sys/uploads/` → synthetic company; `companies/...` → реальная.
- minio-форк (changes add-platform-identity-resolver/add-platform-authz-policy-engine): env-контракт из Component Map.
- agio-cloud (change 8): деплой, секреты, runbook provision.

## Open Questions

- Маппинг `--bucket-name` в path(bucket,·) при одном бакете: подтверждается при кодировании (ожидание: бакет = корень тома, ключи с `companies/`-префиксом — как согласовано в ADR-005); если бакет маппится в подкаталог — правка only-design, спека не меняется (path-контракт уже полный от корня).
- Latency: startup-проба GetFormat добавляет один RPC к времени старта — не блокирует.
