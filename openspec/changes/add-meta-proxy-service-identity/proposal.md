## Why

Meta-grpc-proxy сегодня аутентифицирует только интерактивных OIDC-пользователей. S3 gateway — headless-процесс: ему нужна сервисная identity на прокси (вариант B из ADR-005 agio-platform — второй auth-режим «service-token»), при этом per-request authz для gateway-соединений выполняется на самом gateway (authz-интерцептор прокси пропускается), а FEK-вызовы keymanager для gateway-соединений должны уходить в платформу с node-token gateway (узловая ветка — agio-platform change add-s3-gateway-service-identity). Без этого режима прокси не может обслуживать headless-клиента, не открывая data-путь всем держателям валидного ID-токена.

## What Changes

- Второй auth-режим прокси «service-token»: заголовок `x-service-credential`, проверка SHA-256 constant-time; включается флагом (hash в конфиге прокси — пилот; lookup в платформе — прод, вне этого change). Запрос с BOTH `authorization` и `x-service-credential` → Unauthenticated.
- Per-connection identity kind в прокси: `oidc-user` (как сегодня) или `service`; при kind=service authz-интерцептор пропускается (authz выполнен на gateway), admin-поверхность для service закрыта кроме GetFormat (DumpMeta/LoadMeta и прочие admin-RPC → PermissionDenied).
- Роутинг node-credential: исходящий интерцептор keymanager-клиента прокси добавляет `x-node-credential` (из конфига прокси) ТОЛЬКО к CreateFileKey/GetFileFEK/RotateFileFEK, порождённым service-соединениями; oidc-user вызовы идут байт-в-байт как сегодня (без header, с user_id в request); вызов без identity — без header (fail-closed на платформе).
- user_id в keymanager request'ах для service-соединений не заполняется.
- Секреты (service-credential, node-credential) в конфиге — только по именам; значения не логируются.
- Отсутствие node-credential в конфиге при включённом service-режиме → сервис-соединения не принимаются.

### Non-goals

- Lookup service-token в платформе (прод-стадия) — follow-up ADR-005, отдельный change.
- Узловая ветка на стороне agio-platform (GetFileFEK/CreateFileKey/RotateFileFEK, синтетический namespace `.minio.sys/multipart`) — agio-platform change add-s3-gateway-service-identity.
- Gateway CLI/env-поверхность — change add-gateway-platform-mode.
- Per-user authz/FEK-поведение для oidc-user соединений не меняется (инвариант спеки).
- Chroot/Chroot-админ RPC authz-free дефект — отдельный issue, не в этом change.

## Capabilities

### New Capabilities

<!-- нет новых -->

### Modified Capabilities

- `domain-meta-proxy`: добавляется второй auth-режим service-token, пропуск authz-интерцептора и админ-поверхности для service-соединений, роутинг node-credential в keymanager-клиент (только для service-соединений).

## Impact

- cmd/meta_proxy.go (флаги service-режима, wiring интерцепторов), pkg/oidc или новый pkg (service-credential валидатор), pkg/meta (auth-интерцептор: kind в контекст; authz-интерцептор: bypass для service; grpc_server_admin.go: закрытие admin для service; keymanager-клиент обёртка: исходящий интерцептор с x-node-credential).
- Не затрагивается: grpcMeta клиент (pkg/meta/grpc_client*.go — клиентская сторона), FUSE-путь drive-клиентов, authz-кэш и inode-кэши.

## Related Requirements

- ADR-005 (agio-platform, specs/decisions/ADR-005-s3-gateway-trusted-component.md) — границы доверия, вариант B.
- SRS-001#§6 (форк juicefs, specs/srs/SRS-001-agio-drive-encryption.md) — FEK только через keymanager; узловая ветка не ослабляет per-user путь.
