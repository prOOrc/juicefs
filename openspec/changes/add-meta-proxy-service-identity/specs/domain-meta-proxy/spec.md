## ADDED Requirements

### Requirement: Service credential authentication

When the proxy is configured with a service credential (pilot: SHA-256 hash via proxy configuration), requests carrying metadata `x-service-credential` SHALL be authenticated as service identity if and only if the SHA-256 of the presented credential matches the configured hash (constant-time comparison). A request carrying BOTH `authorization` (bearer) and `x-service-credential` SHALL be rejected with `codes.Unauthenticated`. A request carrying `x-service-credential` that does not match SHALL be rejected with `codes.Unauthenticated` without falling back to the OIDC path. When the proxy has no service credential configured, the `x-service-credential` metadata SHALL be ignored (OIDC-only behavior unchanged). The service credential value SHALL NOT be logged.

Verified-by: pkg/meta/service_identity_test.go::TestServiceIdentityUnaryMatrix
Verified-by: pkg/meta/service_identity_test.go::TestServiceIdentityStreamMatrix
Verified-by: pkg/meta/service_identity_test.go::TestServiceIdentityDisabledHeaderIgnored
Verified-by: pkg/meta/service_identity_test.go::TestServiceIdentityNoNodeCredentialFailClosed

#### Scenario: Valid service credential authenticates as service

- **WHEN** запрос с корректным `x-service-credential` при настроенном service-режиме
- **THEN** запрос проходит authn как service-identity (без OIDC-токена)

#### Scenario: Both credentials rejected

- **WHEN** запрос несёт одновременно валидный bearer и валидный `x-service-credential`
- **THEN** ответ `Unauthenticated` (одновременная двойная аутентификация запрещена)

#### Scenario: Wrong service credential does not fall back to OIDC

- **WHEN** запрос несёт невалидный `x-service-credential` и валидный bearer
- **THEN** ответ `Unauthenticated` (fallback на OIDC после неудачного service-проверки запрещён)

#### Scenario: No service config — header ignored

- **WHEN** service-режим не настроен и запрос несёт `x-service-credential`
- **THEN** поведение идентично текущему OIDC-only (header игнорируется)

### Requirement: Service connections skip authz and admin surface

Для соединений/запросов с identity kind = service authz-интерцептор SHALL пропускать проверку (authz уже выполнен на gateway), при этом: admin-RPC (включая DumpMeta, LoadMeta и все прочие, кроме GetFormat) SHALL возвращать `PermissionDenied`; GetFormat SHALL быть разрешён; lifecycle-участие service-соединений SHALL соответствовать обычным правилам интерцепторов без per-user authz. Для identity kind = oidc-user поведение всех интерцепторов SHALL остаться без изменений.

Verified-by: pkg/meta/service_identity_test.go::TestServiceMethodDecision
Verified-by: pkg/meta/service_identity_test.go::TestAuthzInterceptorSkipsService
Verified-by: pkg/meta/service_identity_e2e_test.go::TestServiceIdentityAdminOverWire

#### Scenario: Service request skips per-user authz

- **WHEN** service-запрос Lookup/GetAttr/Open по пути вне грантов любого пользователя
- **THEN** запрос проходит без authz-вызова (решение принимает gateway)

#### Scenario: Admin surface closed for service

- **WHEN** service-соединение вызывает DumpMeta или LoadMeta
- **THEN** ответ `PermissionDenied`

#### Scenario: GetFormat allowed for service

- **WHEN** service-соединение вызывает GetFormat
- **THEN** ответ успешен (нужно для startup-проверки gateway)

### Requirement: Node credential routing to keymanager

Исходящий keymanager-клиент прокси SHALL добавлять метадату `x-node-credential` (значение из конфига прокси) к вызовам CreateFileKey, GetFileFEK и RotateFileFEK тогда и только тогда, когда вызов порождён запросом с identity kind = service. Вызовы, порождённые oidc-user соединениями, SHALL идти без этой метадаты и с прежним содержимым (user_id в request). Keymanager-вызов вне контекста с identity (фоновая операция) SHALL идти без метадаты (fail-closed на платформе: платформа отклонит или обработает по per-user пути — но не получит node-ветку). Если service-режим включён, а node-credential в конфиге отсутствует, proxy SHALL отказывать в приёме service-соединений.

Verified-by: pkg/meta/keymanager_node_credential_test.go::TestNodeCredentialRoutingMatrix
Verified-by: pkg/meta/service_identity_e2e_test.go::TestServiceIdentityEndToEnd

#### Scenario: Node credential attached only for service-originated calls

- **WHEN** service-соединение вызывает путь, приводящий к GetFileFEK, и oidc-соединение — аналогичный путь
- **THEN** платформа получает `x-node-credential` только от первого вызова; второй идёт с user_id в request и без метадаты

#### Scenario: Missing node credential rejects service connections

- **WHEN** прокси запущен с service-режимом без настроенного node-credential
- **THEN** service-соединение отклоняется на входе (fail-closed), oidc-соединения работают как раньше
