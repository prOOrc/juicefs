## Why

`juicefs gateway` сегодня поднимает minio поверх тома с одной root-парой (MINIO_ROOT_*) и опциональным fork-IAM. Для platform-режима (ADR-005) нужен запуск gateway'а поверх grpc meta-proxy с сервисной identity: service-token к прокси, node-credential gateway к keymanager платформы, адрес authz-сервиса, привязка к facility и явное имя бакета. minio-форк получает эти параметры через env; этот change — CLI/env-поверхность запуска и стартовые проверки.

## What Changes

- Platform-режим запуска `juicefs gateway`: обязательные входы (env, заполняются деплоем agio-cloud): grpc meta-URL (с tls-параметрами в query), service-token, адрес keymanager платформы, node-credential gateway, адрес authz-сервиса (mTLS), facility id, имя бакета.
- Стартовые проверки (fail-fast): задан `--subdir` → ошибка старта; задан `--refresh-iam-interval` → ошибка старта; выставлены `MINIO_ROOT_*` → ошибка старта (root-ключа в режиме нет); недоступен прокси или service-token не принят (GetFormat от имени service-соединения) → ошибка старта, S3-порта не поднимается.
- CompleteMultipartUpload в jfsObjects (pkg/gateway/gateway.go) создаёт финальный объект непосредственно по целевому пути (новый inode → CreateFileKey по реальной компании тома), вместо создания complete-файла в `.sys/uploads/` с последующим rename: KEK/AAD complete-файла под synthetic KEK (`.sys/uploads` — синтетическая компания на платформе, ADR-005 подрешение 3) после rename дали бы KEK/AAD-мисматч. Abort/cleanup staging без изменений.
- Env-значения секретов не логируются; лог запуска печатает только имена конфигурационных ключей.
- Внутренние поверхности minio-форка (подавление IAM, admin/STS отказ, policy engine) — changes add-platform-identity-resolver / add-platform-authz-policy-engine juicefs-minio; здесь только прокидывание env и проверки.

### Non-goals

- Реализация SigV4-резолва и authz в minio-форке — juicefs-minio changes.
- Service-token/node-credential режимы прокси и платформы — add-meta-proxy-service-identity (этот репо) и agio-platform changes.
- STS-маунт-режим (`--sts-enabled`, `--company-id`) и обычный redis-gateway не меняются.
- Мульти-бакетный режим (`--multi-buckets`) в platform-режиме: один бакет, имя задаётся явно (пилот).

## Capabilities

### New Capabilities

- `domain-gateway-platform-mode`: запуск `juicefs gateway` в platform-режиме — обязательные входы, стартовые проверки, гигиена секретов.

### Modified Capabilities

<!-- нет: обычный режим gateway не меняется -->

## Impact

- cmd/gateway.go (platform-ветка: сборка env для minio ServerMainForJFS, проверки флагов), возможно pkg (сервисная проверка GetFormat через grpc-клиент как у mount).
- Не затрагивается: cmd/mount.go, supervisor agio-drive (клиент агio-drive не трогается в цикле), обычный gateway-режим.

## Related Requirements

- ADR-005 (agio-platform) — границы доверия gateway, отказ от root-ключей.
- SRS-001 (форк juicefs) — grpc-путь для шифрованных томов; gateway в platform-режиме обязан ходить grpc (plaintext-мета для шифрованного тома отклоняется существующим механизмом).
