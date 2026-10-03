# Design: implement-grpc-tls

## Context

Канал клиент→Meta Proxy — единственный gRPC-канал agio Drive без TLS: `newGRPCMeta` делает `grpc.Dial(addr, grpc.WithInsecure())` (`pkg/meta/grpc_client.go:190`), сервер в `cmd/meta_proxy.go` создаёт `grpc.NewServer` без `grpc.Creds`. Все прочие каналы (KeyManager, authz→platform) уже имеют TLS из файлов cert/key/CA (`pkg/meta/keymanager_client.go:101-165`, `pkg/meta/authz_client.go:38-40`). Мотивация и границы — proposal.md (Related Requirements: SRS-001#NFR-SEC-8, контракт «plaintext FEK, TLS only»).

## Goals / Non-Goals

**Goals**: опциональный TLS на обеих сторонах клиентского канала; принудительный fail-closed запрет выдачи plaintext FEK по plaintext-соединению; нулевая деструкция существующих развёртываний (дефолт — plaintext).

**Non-Goals**: mTLS (клиентские сертификаты), обязательный TLS, device-code/keyring (OIDC не трогаем), Render-канал TLS 1.3 (уже есть), ротация сертификатов на лету.

## Architectural Context

```
juicefs mount (клиент)                       juicefs meta-proxy (сервер)
+---------------------------+                +---------------------------------+
| newGRPCMeta               |                | meta_proxy server               |
|  query: tls,tls-ca,       |   TLS (опц.)   |  flags: --tls-cert/--tls-key    |
|         tls-server-name   | -------------> |  grpc.NewServer(+grpc.Creds)    |
|  dial: creds | insecure   |                |  interceptors: OIDC -> authz    |
+---------------------------+                |  Open -> KeyManager -> fek      |
                                             +---------------------------------+
TLS-детекция на сервере: grpc.AuthInfoFromContext(ctx) -> credentials.TLSInfo
```

Клиентский и серверный TLS независимы: сервер может слушать TLS, а клиенты — ходить plaintextом (тогда зашифрованные волюмы для них недоступны на чтение — fail-closed), и наоборот (plaintext-сервер + TLS-клиент — handshake не пройдёт).

## Component Map

| Компонент | Файл | Изменение |
|---|---|---|
| Клиент: парсинг query-параметров | `pkg/meta/grpc_client.go` (143-236) | +`tls`, `tls-ca`, `tls-server-name` (в блоке с `oidc-*`, camelCase-алиалоги не нужны — параметры новые) |
| Клиент: dial | `pkg/meta/grpc_client.go:190` | `WithInsecure` → условные transport credentials |
| Клиент: HTTP-прокси переменные | нет новых | `grpc` читает `HTTPS_PROXY` сам; изменений нет |
| Сервер: флаги | `cmd/meta_proxy.go` (75-89, 203-283) | +`--tls-cert`, `--tls-key`; `grpc.NewServer(grpc.Creds(...))` при паре |
| Сервер: fail-closed FEK | `pkg/meta/grpc_server_fuse.go` (228-261) | в ветке выдачи plaintext FEK — проверка TLS-канала |
| Тесты клиента | `pkg/meta/grpc_client_test.go` | парсинг параметров, выбор creds, ошибка CA-файла |
| Тесты сервера FEK | `pkg/meta/grpc_server_fuse_test.go` (при наличии; см. Open Questions) | сценарии plaintext/TLS для `Open` |

Используемые API grpc-go (все существуют в текущей версии go.mod): `credentials.NewServerTLSFromFile(cert, key)`, `credentials.NewClientTLSFromFile(ca, serverName)`, `grpc.AuthInfoFromContext(ctx) (credentials.AuthInfo, bool)`, type assert `credentials.TLSInfo`. Минимальная версия TLS — `tls.VersionTLS12` (NFR-SEC-8); `NewClientTLSFromFile` использует дефолтный `MinVersion` гRPC-стека (1.2+ в текущих версиях), отдельный override не вводим.

## Execution Flow

**Маунт с TLS** (после change):

1. `juicefs mount "grpc://proxy:9561/vol?tls=1&tls-ca=/etc/agio/ca.pem&oidc-issuer=..." Z:`
2. `newGRPCMeta` парсит `tls=1`, `tls-ca`, `tls-server-name` → строит `tls.Config` (RootCAs из файла, ServerName override; при отсутствии `tls-ca` — системный пул).
3. `grpc.Dial` c `grpc.WithTransportCredentials(credentials.NewTLS(cfg))`; канал lazy — handshake при первом RPC (`Load`).
4. Сервер (если запущен с `--tls-cert/--tls-key`) завершает handshake; `Open` зашифрованного файла проходит `AuthInfoFromContext` → `TLSInfo` → FEK выдаётся.

**Маунт без `tls=1`** — поведение идентично текущему во всех ветках, кроме одной: `Open` зашифрованного файла возвращает `Unauthenticated` вместо FEK (новое правило, см. Invariants).

## Invariants

- **Дефолт не меняется**: без `tls=1` клиентский dial и без `--tls-cert` серверный listener ведут себя как сейчас (все существующие тесты stage-топологии проходят без правки конфигурации).
- **Fail-closed FEK**: plaintext FEK никогда не покидает сервер по plaintext-соединению на зашифрованном волюме; ошибка — `codes.Unauthenticated` (клиент маппит в `EACCES` существующей таблицей, нового кода ошибки не вводим).
- **Parity не затрагивается**: change не меняет семантику метаданных движков Redis/SQL/KV (только транспорт и серверную проверку контекста), общие тесты `base_test.go` не затрагиваются.
- **dump/load не затрагивается**: формат метаданных не меняется.

## Decisions & Rationale

1. **Query-параметры meta URL, а не CLI-флаги клиента** — конфигурация OIDC уже живёт в query (`oidc-*`); URL целиком переживает реконструкцию командной строки в Windows service mode (`pkg/winfsp/winfs.go:1365-1424`) и не требует дублирования флагов в `cmd/mount_unix.go`/`cmd/mount_windows.go`. Альтернатива (флаги `--rpc-tls-ca` и т.п.) отклонена: два источника правды для транспортной конфигурации.
2. **`tls=1` вместо авто-детекции** — gRPC-сервер на plaintext-соединении от TLS-client handshake отваливается с невнятной ошибкой; явный флаг даёт предсказуемую диагностику (ошибка CA-файла — на этапе создания клиента). Альтернатива (пробовать TLS, фолбэк на plaintext) отклонена — security: тихий фолбэк на plaintext на зашифрованном волюме маскировал бы реальный риск.
3. **Fail-closed FEK на уровне сервера, а не «рекомендация в доке»** — контракт SRS-001 «TLS-only» без принуждения — просто комментарий; проверка `grpc.AuthInfoFromContext` в единственной точке выдачи FEK (`Open`) закрывает класс ошибок конфигурации. Альтернатива (запрет всего маунта зашифрованного волюма без TLS) отклонена как более ломкая: пользователь увидит осмысленную ошибку на первом `Open`, а диагностика причины остаётся локальной.
4. **`Unauthenticated` как код отказа FEK** — попадает в существующую маппинг-таблицу клиента (`Unauthenticated → EACCES`, требование «gRPC metadata client»), не требует новых errno и правки клиента. Альтернатива (`PermissionDenied`) тоже маппится в `EACCES`, но семантически «нет аутентификации канала» точнее.
5. **`--tls-cert`/`--tls-key` только парой** — однобокая конфигурация почти наверняка ошибка; fail-fast на старте дешевле диагностики «почему клиенты не подключаются».

## Integration Points

- **`implement-windows-mount`**: Windows-клиенты получают TLS бесплатно через query-параметры URL (см. решение 1); e2e-верификация Windows-маунта может гоняться поверх TLS после этого change.
- **`implement-encrypt`**: требование «Plaintext FEK delivery requires TLS» уточняет выдачу FEK из дельты `domain-encrypt` (OpenResponse.fek); взаимных конфликтов нет — там FEK уже помечен «TLS-only».
- **Существующие тесты FEK-пути**: тесты, гоняющие `Open` с FEK через insecure bufconn, после включения fail-closed потребуют TLS-harness (bufconn с `credentials.NewServerTLSFromFile`/`NewTLS`) — см. tasks и Risks.

## Risks / Trade-offs

- [Существующие тесты зашифрованного `Open` используют insecure-соединение] → обновить harness до TLS-bufconn в том же change; допустимость подтверждается тем, что сценарий «plaintext Open» теперь и есть тест-кейс (ожидаем `Unauthenticated`).
- [Опечатка в `tls-ca` на проде] → ошибкаcaught на этапе создания клиента (fail-fast), до первого сетевого RPC.
- [Окно несовместимости: сервер с TLS, клиент без `tls=1` на зашифрованном волюме] → задокументировать в rollout: сначала включаем TLS у клиентов, потом у сервера; `Unauthenticated`+`EACCES` на `Open` — явный сигнал, а не тихая деградация.
- [Перф: TLS-handshake на reconnect] → канал один на маунт, keepalive не рвёт соединение; overhead разовый, замер не требуется (NFR-PERF-3 не затрагивает транспорт).

## Migration Plan

1. Деплой сервера с новым бинарем **без** `--tls-cert/--tls-key` (поведение идентично текущему) — безопасно в любой момент.
2. Выдать клиентам новые URL с `tls=1` (включение по желанию эксплуатации; на plaintext-сервере TLS-клиент получит ошибку handshake — разворачивать в порядке «сначала сервер, потом клиенты»).
3. Для зашифрованных волюмов `tls=1` становится обязательным де-факто: без него `Open` возвращает `EACCES`.
4. Rollback: убрать `tls=1` из URL клиентов и/или перезапустить сервер без TLS-флагов.

## Open Questions

- Нужен ли на зашифрованных волюмах запрет всего маунта (а не только `Open`) без TLS — на старте (`Load`) сервер знает `encryption_enabled` и видит канал; отказ на старте информативнее, но ломает сценарий «маунт для чтения метаданных». Текущее решение — отказ только на `Open` (см. Decisions 3); пересмотреть, если эксплуатация сочтёт позднюю диагностику проблемой.
- Хранить ли тестовый сертификат для TLS-bufconn harness в `hack/` (как уже сделаны WinFsp-заголовки) или генерить в тестах on-the-fly (`crypto/tls` self-signed в `TestMain`) — решить на этапе задач; на спеку не влияет.
