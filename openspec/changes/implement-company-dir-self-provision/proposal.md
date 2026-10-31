# Proposal: implement-company-dir-self-provision

## Why

Self-provisioning директории компании (agio-platform change
`allow-self-provision-company-dir`, решение владельца 2026-10-31 — вариант D)
требует от инфраструктурного слоя двух generic-способностей: передавать
authz-сервису контекст операции (это mkdir?) и уметь обеспечить существование
доставленного платформой рабочего пути при маунте. Без этого свежий v2-том
неотbootstrappable: корень read-only для всех операций кроме mkdir, который
сейчас неотличим для authz от любой другой модификации. Company-семантика
остаётся в agio-platform: форк передаёт «это создание директории» и
обеспечивает «путь, который платформа назвала» — без единого упоминания
`companies` в коде форка.

## What Changes

- Прокси: authz-вызов для Mkdir RPC передаёт op-контекст `OP_MKDIR` (прочие
  модифицирующие RPC — `OP_OTHER`); новая optional-поля протокола agio-platform
  (`CheckPermissionRequest.op`) прокидываются через существующий authz-клиент
  прокси.
- Клиент: после успешного маунта с `--company-id` — идемпотентный mkdir -p
  `working_dir` из ответа `GetSTSCredentials` (новое optional-поле; EEXIST —
  успех; отказ authz — warn в лог, маунт не срывается; поле отсутствует —
  поведение как сейчас).
- Протокол: обновление pb-копий agio-platform authz/STS сервисов в форке
  (regenerate по новой платформенной proto; обратная совместимость — старая
  платформа/новый клиент и наоборот).

## Capabilities

### New Capabilities

- `drive-sts-client`: расширение контракта клиента — ensure доставленного
  рабочего пути при маунте с company-id (дополняет капабилити, введённую
  change'ом configure-sts-refresh-ttl).
- `domain-meta-proxy` (modified → ADDED-требование): pass-through op-контекста
  в authz-вызовы (требование «Authz operation context pass-through»).

### Modified Capabilities

## Impact

- `pkg/meta/pb/` (regenerate agio proto-копий), authz-клиент прокси
  (`pkg/meta/`), `cmd/mount.go` + `pkg/meta/grpc_client_sts.go` (ensure),
  тесты.
- Порядок выката: agio-platform (proto+правило) → форк. Обратная совместимость
  в обе стороны (optional-поля, дефолт OP_OTHER).
- Связанное: agio-platform change allow-self-provision-company-dir (мастер-
  change направления), configure-sts-refresh-ttl (та новая капабилити
  drive-sts-client), ADR-004, приёмка T0.1 «запись файла через v2 маунт».
