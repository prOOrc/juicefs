# Proposal: add-render-selectel-company-keys

## Why

Render-контур заморожен на YC-инфраструктуре и на Selectel мёртв: аутентикация
нод к KeyManager идёт через YC IAM-токен (`FetchCompanyKEK`,
`GetNodeSTSCredentials` — интерцептор `src/api/iam_interceptor.go`),
а storage-креды — через YC STS (`platformNodeSTSProvider`); ни того, ни
другого на Selectel нет (ADR-004 закрыл FR-REV-3 статическими ключами).
При этом сам data-plane рендера от инфраструктуры не зависит: нода монтирует
том напрямую в Redis (без meta-proxy/OIDC/authz), ограничиваясь каталогом
компании (subdir-chroot), проверки сознательно выключены — так и остаётся.
Решение владельца (2026-11-01): рендер-нодам доставляются СТАТИЧЕСКИЕ ключи
компании (те же `company_storage_key`, что и для user-пути по ADR-004) —
никакого STS для рендера не нужно; нужен только канал доставки пары ключей
(+ KEK, как сегодня) с аутентикой ноды, работающей на Selectel.

## What Changes

- Platform (agio-platform, KeyManager):
  - Аутентикация IAM-guarded RPC расширяется режимом «platform node
    credential»: нода предъявляет статический токен, выданный платформой
    (SHA-256 хэш в БД, per-node, отзывный). Режим выбирается конфигом
    (`node_auth: yc-iam | node-token`); YC-развёртывания продолжают
    работать как сегодня.
  - Новый RPC `GetNodeStorageCredentials(company_id)` → активная статическая
    пара компании (расшифровка `company-storage-keys-kek`), volume/bucket из
    facility-конфига; fail-closed при revoked/нет строки; аудит; TLS-only.
  - `FetchCompanyKEK` — без изменений по смыслу (KEK ноде), только режим
    аутентики расширяется.
- Fork (render-клиент):
  - `render-mount`: новый флаг `--node-credential-file` (замена
    `--iam-token-file` на Selectel); bootstrap-вызов получает пару ключей
    (+ KEK через `FetchCompanyKEK`), ключи кладутся в static S3-провайдер —
    STS-провайдеры (`platformNodeSTSProvider`, AWS AssumeRole) остаются для
    YC/AWS-развёртываний, на Selectel не используются.
  - subdir-chroot компании, отключённые проверки, прямой Redis — без
    изменений.

## Capabilities

### Modified Capabilities

- `domain-encrypt` (modified): MODIFIED «Render path» — источник S3-кредов
  рендер-ноды (платформенные статические ключи компании вместо
  YC-STS/instance-profile), аутентика ноды к KeyManager. Капабилити живёт
  дельтой открытого implement-encrypt; слияние бесконфликтно (правка одного
  требования). Platform-сторона (proto/интерцептор/БД) — задачи внутри
  этого change'а (прецедент: stages 1/7 implement-encrypt); при apply
  platform-коммиты идут в agio-platform, и его SoT `drive-keymanager`
  дополняется отдельным change'ом там.

## Related Requirements

- ADR-004 (статические ключи на компанию; B4 — render on hold → этот change
  снимает hold для Selectel).
- SRS-001 (render-контур: KEK в RAM/mlock, chroot компании, чужая компания →
  EIO) — не меняется.
- implement-encrypt delta domain-encrypt «Render path» (база для MODIFIED).

## Impact

- Platform: миграция (`render_node_credential`), proto + regen,
  `key_manager_service` (новый RPC), интерцептор (второй режим),
  config; тесты.
- Fork: `cmd/render_mount.go` (флаг, bootstrap-флоу, static-провайдер),
  `pkg/meta/keymanager_pb` (regen), тесты.
- Совместимость: YC-развёртывания не затронуты (режим по конфигу);
  `--iam-token-file` сохраняется; Slice-формат/шифрование — без изменений.
