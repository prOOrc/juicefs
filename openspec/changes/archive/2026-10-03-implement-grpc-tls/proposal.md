# Proposal: implement-grpc-tls

## Why

Канал клиент→Meta Proxy жёстко plaintext: `grpc.Dial(addr, grpc.WithInsecure())` (`pkg/meta/grpc_client.go:190`), а Meta Proxy слушает без creds. При этом контракт шифрования требует защищённый транспорт: plaintext FEK передаётся в `OpenResponse` с пометкой «TLS only» (SRS-001, proto `key_manager.proto`), и NFR-SEC-8 требует «TLS 1.2+ для всех gRPC». Для Windows-рабочих станций (маунт вне доверенной сети, отдельный change `implement-windows-mount`) plaintext FEK и Bearer-токен в сети — прямой риск. Нужна опция TLS на обеих сторонах, без поломки существующих stage-развёртываний.

## What Changes

- **Server**: `juicefs meta-proxy` получает флаги `--tls-cert` / `--tls-key`; при их наличии сервер слушает с `grpc.Creds(credentials.NewServerTLSFromFile(...))`, минимум TLS 1.2 (NFR-SEC-8). Без флагов — текущее поведение (plaintext).
- **Client**: `grpcMeta` принимает query-параметры meta URL (в стиле `oidc-*`): `tls=1` (включить TLS), `tls-ca=<path>` (кастомный CA pool), `tls-server-name=<name>` (override SNI/верификации). Клиентский dial использует `credentials.NewClientTLSFromFile` / кастомный `tls.Config` вместо `WithInsecure` при `tls=1`.
- **Fail-closed FEK**: на зашифрованном волюме сервер не возвращает plaintext FEK (`OpenResponse.fek`) по insecure-соединению — требование «TLS-only FEK» из SRS-001 получает механизм принуждения, а не только соглашение в proto-комментарии.
- **BREAKING**: нет — дефолт остаётся plaintext, существующие развёртывания не затронуты.

## Capabilities

### New Capabilities

(нет)

### Modified Capabilities

- `domain-meta-proxy`: требование «juicefs meta-proxy command» — добавляются флаги `--tls-cert`/`--tls-key`; требование «grpcMeta metadata driver» — insecure-коннект становится опциональным (`tls`/`tls-ca`/`tls-server-name`); новое требование — plaintext FEK выдаётся только по TLS-соединению (fail-closed на зашифрованных волюмах).

## Impact

- **Код**: `pkg/meta/grpc_client.go` (парсинг query-параметров, dial), `cmd/meta_proxy.go` (флаги сервера, `grpc.NewServer` creds), `pkg/meta/grpc_server_fuse.go` (проверка TLS в `Open`).
- **Тесты**: `pkg/meta/grpc_client_test.go` (парсинг, creds), тесты FEK-пути — существующие тесты через insecure bufconn потребуют TLS-harness (см. design.md, Open Questions).
- **Конфигурация**: развёртывания stage — включение TLS опционально, по решению эксплуатации.
- **Документация**: примеры meta URL с `tls=1` для клиентов (docs/, спеки agio Drive).

## Related Requirements

- **SRS-001#NFR-SEC-8** — «TLS 1.2+ для всех gRPC; TLS 1.3 для Render-клиента»: настоящий change закрывает клиентский канал клиент→Proxy (последний plaintext gRPC-канал).
- **SRS-001#§Assumptions A2** — «Все gRPC-каналы — TLS 1.2+».
- **SRS-001#§proto contract** (`key_manager.proto`: `fek // plaintext FEK, TLS only`) — контракт, который настоящее требование делает принудительным.
- **domain-encrypt** (дельта change `implement-encrypt`, требование «FEK delivery») — «plaintext FEK only in `OpenResponse` (TLS-only field)».
- **ADR-001** — гибридный workflow (слой specs/ + openspec/).
- **BRD отсутствует** (реестр `specs/index.md`, раздел «Замечания»): бизнес-контекст живёт в архитектурном плане v13; ссылка на SRS-001 и ADR-001 прямая, отсутствие BRD объясняется явно.

## Non-goals

- **mTLS (клиентские сертификаты) на клиентском канале** — только server-side TLS с опциональным кастомным CA; клиентские сертификаты не вводим.
- **Обязательный TLS** — plaintext остаётся дефолтом; принуждение только для выдачи plaintext FEK на зашифрованных волюмах. Отказ от insecure-маунтов нешифрованных волюмов не делаем.
- **TLS 1.3-only** — минимум 1.2 по NFR-SEC-8; TLS 1.3 остаётся прерогативой Render-клиента (SRS-001).
- **Device-code flow / pre-populated кеш токенов / keyring** — OIDC-аутентификация не меняется (см. change `implement-windows-mount`).
- **Другие gRPC-каналы** — KeyManager и authz-клиенты уже имеют TLS; канал outbox и прочие не затрагиваются.
- **Windows-специфика маунта** — отдельный change `implement-windows-mount`.
