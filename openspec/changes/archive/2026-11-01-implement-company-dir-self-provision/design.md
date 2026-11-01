# Design: implement-company-dir-self-provision

## Context

Мастер-change: agio-platform `allow-self-provision-company-dir` (вариант D,
утвёрден владельцем): authz разрешает MKDIR на корневом неймспейсе, платформа
доставляет клиенту `working_dir` в `GetSTSCredentials`. Форк обязан только
(1) не потерять «это mkdir» по дороге к authz и (2) обеспечить доставленный
путь. Company-семантика (имя `companies`, чей путь) — целиком на платформе.

## Goals / Non-Goals

**Goals**
- Pass-through op-контекста без company-семантики; клиентский ensure
  доставленного пути (idempotent, warn-only); обратная совместимость в обе
  стороны.

**Non-Goals**
- Правила authz (платформа), формат/имена путей, bootstrap-идентичности,
  изменения fail-safe контракта refresher'а.

## Architectural Decisions

- **D1: классификация в authz-интерцепторе прокси.** Интерцептор уже
  маппит RPC → (user, path); добавляется вывод op: `Mkdir` → `OP_MKDIR`,
  остальное → `OP_OTHER` (rename/delete наследуют OP_OTHER — платформа
  запрещает их на корне независимо).
- **D2: proto — регенерация по платформенной .proto.** Поля optional:
  `op` (enum с дефолтом OP_OTHER) в CheckPermissionRequest, `working_dir`
  (string) в StsCredentialsResponse. Порядок полей/номера фиксирует
  agio-platform proto как источник; в форке обновляется pb-копия.
- **D3: ensure — на пути маунта, не refresher'а.** Выполняется один раз после
  успешного `mount` c `--company-id` (refresher при первом swap тоже знает
  креды, но ensure не привязан к кредам — это разовая операция тома).
  Директории создаёт через тот же meta-клиент (обычный Mkdir — authz применит
  новое правило).
- **D4: deny/ошибка ensure → warn + продолжение.** Пользователь получит
  осмысленный отказ на первой записи; не превращаем ensure в точку отказа
  маунта (D4 мастер-design).

## Component Map

| Компонент | Файл | Изменение |
|---|---|---|
| Authz interceptor | `pkg/meta/grpc_server_fuse.go` / authz interceptor | op-классификация Mkdir → CheckPermissionRequest.Op |
| pb-копии | `pkg/meta/pb/` (agio authz/sts) | регенерация по платформенной proto (+op, +working_dir) |
| STS-клиент | `pkg/meta/grpc_client_sts.go` | проброс `working_dir` из ответа (в структуру sts-контекста маунта) |
| Mount ensure | `cmd/mount.go` | после ready при `--company-id`: mkdir -p working_dir (EEXIST=ok, отказ=warn) |
| Тесты | `cmd/*_test.go`, `pkg/meta/*_test.go` | классификация, ensure, no-field-регресс |

## Execution Flow

1. Regenerate pb по agio-platform proto (platform change уже льёт proto).
2. Прокси: op-классификация; тест: Mkdir → OP_MKDIR, SetAttr → OP_OTHER.
3. Клиент: parse working_dir → ensure после ready; тесты на 4 сценария спеки.
4. Совместимость: старая платформа (нет полей) — ensure не выполняется
   (пустой working_dir), op-поле игнорируется сервисом; новый прокси против
   старой платформы — OP_OTHER дефолт, поведение прежнее.

## Invariants

- Ни одной строки company-семантики в форке (имена путей — только из ответа
  платформы).
- Поведение при отсутствии новых полей — байт-в-байт текущее.

## Integration Points

- agio-platform `allow-self-provision-company-dir` (мастер; порядок выката:
  platform → fork); configure-sts-refresh-ttl (капабилити drive-sts-client);
  implement-juicefs-mount (agio-drive) — приёмка T0.1.

## Open Questions

- Классифицировать ли Rename как пару op (source/dest) — сейчас OP_OTHER
  (платформа запрещает rename на корне независимо); revisit при появлении
  требований.
