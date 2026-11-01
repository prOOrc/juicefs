# Design: add-render-selectel-company-keys

## Context

Render-контур жив (форк: `render_meta.go`, `cmd/render_mount.go`; platform:
IAM-интерцептор + `FetchCompanyKEK`/`GetNodeSTSCredentials`), но его
аутентика (YC IAM) и storage-креды (YC STS) не существуют на Selectel.
Статические ключи компаний уже реализованы (ADR-004, `company_storage_key`,
CompanyStorageKeyService) — рендеру не хватает только канала доставки с
Selectel-совместимой аутентикой ноды. Решение владельца 2026-11-01:
передавать нодам статическую пару компании; схема монтирования (прямой
Redis, chroot компании, проверки выключены) не меняется.

## Goals / Non-Goals

- Goal: render-нода на Selectel получает KEK + активную статическую пару
  компании при маунте и пишет/читает S3 напрямую.
- Goal: отзыв ноды отзывает её доступ к материалу (KEK/ключи) без смены
  ключей компании.
- Non-Goal: перевод рендера на meta-proxy/authz (прямой Redis остаётся);
  STS для рендера; изменения data-plane/шифрования; user-путь.

## Decisions

### D1. Node credential — platform-issued статический токен (per-node)

Админ-мутация выпускает ноде токен (32 байта CSPRNG, base64url); в БД —
только SHA-256 хэш + `render_node_id` + статус/ротация. Нода предъявляет
токен в заголовке (gRPC metadata `x-node-credential`); интерцептор
сравнивает хэш. Выбор между per-node токеном и альтернативами: mTLS
(сильнее, но PKI-обвязка непропорциональна угрозе — нода и так держит
KEK в RAM и пароль Redis), общий секрет (проще, но отзыв = ротация у всех
нод). Отзыв ноды = инвалидация строки в БД — KEK/ключи перестают
выдаваться немедленно, выданный материал умирает с выгрузкой ноды.

### D2. `GetNodeStorageCredentials` — отдельный RPC, а не расширение `FetchCompanyKEK`

Ответ `FetchCompanyKEK` — plaintext KEK (TLS); добавлять туда же S3-ключи
значит смешать два жизненных цикла (KEK-ротация ≠ ротация ключей компании).
Отдельный RPC тянет активную пару через `CompanyStorageKeyService`
(расшифровка `company-storage-keys-kek`), volume/bucket — из
facility-конфига компании (тот же резолвер, что в STS-пути); fail-closed:
нет строки/revoked/нет facility — ошибка без материала; аудит записи
`get_node_storage_credentials` с actor = `render_node:<id>`.

### D3. Режим аутентики по конфигу, YC не ломается

`node_auth: yc-iam | node-token` (дефолт `yc-iam` — текущее поведение).
Интерцептор в режиме `node-token` проверяет токен и кладёт в контекст
`render_node:<id>`; `yc-iam` — существующий путь. Секреты только именами в
конфигах; токены нод — через систему секретов деплоя.

### D4. Fork: `--node-credential-file` и static-провайдер

`render-mount`: вместо `--iam-token-file` (Selectel-путь) — файл с токеном
ноды; после `FetchCompanyKEK` выполняется `GetNodeStorageCredentials`, пара
подаётся в существующий static-путь storage-конфига (как volume-format
ключи сегодня, но источником платформа; в память ноды — как и всё
остальное). volume/bucket из ответа платформы — приоритет над Format
(тот же принцип «клиент не называет volume/bucket», что и в user-пути).
subdir-chroot, отключённые проверки, прямой Redis, mlock KEK, LRU — без
изменений (SRS-001 не трогается).

## Risks / Trade-offs

- [Токен ноды скомпрометирован] → отзыв в БД; окно = до первого deny;
  материал на ноде умирает с выгрузкой (тот же класс, что сегодня с YC IAM).
- [Расшифровка пары при каждом bootstrap] → ключи уже в БД/SM, стоимость
  разовая на маунт; LRU на платформе не нужен.
- [Две системы аутентики в интерцепторе] → строго режим по конфигу, без
  fallback'а между режимами (fail-closed при неверной конфигурации).

## Migration Plan

Platform: миграция + деплой (режим `yc-iam` по умолчанию — поведение не
меняется) → выпуск токенов нодам → перевод конфига stage на `node-token` →
обновление render-образа. Откат: вернуть `yc-iam`/старый образ; выданные
токены инвалидируются удалением строк.

## Open Questions

- Формат admin-мутации выпуска токена (GraphQL или CLI `keymanager
  node-token issue/revoke`) — решить на этапе design-ревью platform-стороны.
