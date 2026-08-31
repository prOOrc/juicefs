# Design: add-go-client-library

> **Примечание (после архива):** `CreateRedisClient` + `parseRedisClientOptions` (`pkg/meta/redis_client.go`) вынесены на ветку `outbox`, где они заменяют подход с `createRedisOptions` в `redis.go` (причина конфликтов при ребейзе). На этой ветке остаётся только `NewMetaClient` (`pkg/meta/client.go`). Требования и сценарии по `CreateRedisClient` удалены из спека `domain-go-client`.

## Context

См. proposal.md — Why. Фактическое состояние (точка отсчёта, проверено по ветке `juicefs-go-client` = `main-agio`):

- `pkg/meta/redis.go:110` — `newRedisMeta(driver, addr string, conf *Config) (Meta, error)`: парсинг Redis-опций (URL, query-параметры, TLS, пароль из env, retry/timeout) и создание failover/cluster/single-клиента **инлайн** в теле функции (upstream-код, `@pkg/meta/redis.go#L212-247`).
- `pkg/meta/interface.go:628` — `NewClient(uri string, conf *Config) Meta`: единственный экспортированный конструктор мета-клиента; при ошибке вызывает `logger.Fatalf` (убивает процесс) → непригоден как библиотека.
- `pkg/meta/interface.go:573` — `type Creator func(driver, addr string, conf *Config) (Meta, error)`; `:575` — `var metaDrivers = make(map[string]Creator)`; `:577` — `Register(name, Creator)`. Драйверы регистрируются в `init()` каждого бэкенда (`redis`/`rediss`/`unix` в `redis.go`, SQL/KV в `sql.go`/`tkv.go`, `grpc` в `grpc_client.go`).
- `pkg/meta/interface.go:600` — `readPasswordFromFile(filePath string) (string, error)`; `:610` — `setPasswordFromEnv(uri string) (string, error)` (приватные, доступны внутри пакета).
- `pkg/meta/utils.go:91` — `type queryMap struct{ *url.Values }` с методами `duration(key, originalKey string, d time.Duration) time.Duration`, `getInt(key, originalKey string, def int) int`, `pop(key string) string`.
- `pkg/meta/config.go:38` — `type Config struct`; `:59` — `func DefaultConf() *Config`.
- Redis-библиотека: `github.com/redis/go-redis/v9` (`redis.Options`, `redis.FailoverOptions`, `redis.ClusterOptions`, `redis.UniversalClient`, `redis.NewClient/NewFailoverClient/NewClusterClient`, `redis.ParseURL`) + `github.com/redis/go-redis/v9/maintnotifications`.
- Прецедент: ветка `outbox` — `meta.CreateRedisClient` + `createRedisOptions` **в `pkg/meta/redis.go`** (изменение upstream-файла → конфликты при ребейзе).

## Goals / Non-Goals

**Goals:**
- Экспортируемый library-safe конструктор мета-клиента `NewMetaClient(uri, conf) (Meta, error)` — ошибка вместо `os.Exit`.
- Экспортируемый `CreateRedisClient(redisURL) (redis.UniversalClient, error)` с теми же семантиками подключения, что у мета-движка Redis (single/sentinel/cluster, TLS, пароль из env, retry/timeout).
- Реализация в **новых файлах** `pkg/meta/` без изменения `redis.go`/`interface.go` → ребейз на upstream не конфликтует.

**Non-Goals:**
- Не менять `newRedisMeta`/`NewClient` и не «выносить» из них парсинг (причина конфликтов в `outbox`).
- Не мигрировать вызывающих на этой ветке (`cmd/outbox.go`, `cmd/meta_proxy.go`, `scripts/bench_grpc.go` — другие ветки).
- Не создавать полный FS-клиент (meta+chunk+object+vfs) и не вводить отдельный Go-модуль.
- Не вносить семантических изменений в движки (parity Redis/SQL/KV не затрагивается).

## Architectural Context

Два независимых конструктора в пакете `pkg/meta`, оба аддитивные:

```
Внешний модуль (Meta Proxy / outbox consumer / render-клиент / bench)
        │  import "github.com/juicedata/juicefs/pkg/meta"
        ├───────────────────────────────┬───────────────────────────────┐
        ▼                               ▼                               ▼
  meta.NewMetaClient(uri, conf)   meta.CreateRedisClient(url)      (существующее)
        │                               │                        meta.NewClient(uri, conf)
        │ scheme-default + driver       │ parseRedisClientOptions   (CLI-путь, logger.Fatalf)
        │ lookup в metaDrivers          │  (дублирует парсинг
        ▼                               │   newRedisMeta)
  Creator(driver, addr, conf)          ▼
        │ (redis/sql/tkv/grpc)   детект режима: sentinel | cluster | single
        ▼                               ▼
   Meta (meta.Meta)            redis.UniversalClient
```

Ключевое свойство: `NewMetaClient` **не дублирует** ничего — он переиспользует `metaDrivers`, `setPasswordFromEnv`, `DefaultConf` (всё уже есть в пакете). Дублируется только парсинг Redis-опций для `CreateRedisClient`, потому что переиспользовать его из `newRedisMeta` нельзя без изменения upstream-функции.

## Component Map

| Компонент | Файл | Статус |
|---|---|---|
| `NewMetaClient(uri string, conf *Config) (Meta, error)` | `pkg/meta/client.go` | новый |
| `CreateRedisClient(redisURL string) (redis.UniversalClient, error)` | `pkg/meta/redis_client.go` | новый |
| `parseRedisClientOptions(redisURL string) (*redisClientOptions, error)` (приватный, дублирует парсинг `newRedisMeta`) | `pkg/meta/redis_client.go` | новый |
| `type redisClientOptions struct{ Options *redis.Options; MinRetryBackoff, MaxRetryBackoff, ReadTimeout, WriteTimeout time.Duration }` | `pkg/meta/redis_client.go` | новый |
| Тесты `NewMetaClient` (unknown driver, nil conf, valid URI) | `pkg/meta/client_test.go` | новый |
| Тесты `CreateRedisClient` (single/sentinel/cluster, пароль из env, TLS, invalid URL) | `pkg/meta/redis_client_test.go` | новый |
| `newRedisMeta` | `pkg/meta/redis.go` | **без изменений** |
| `NewClient`, `metaDrivers`, `setPasswordFromEnv`, `readPasswordFromFile` | `pkg/meta/interface.go` | **без изменений** (только используются) |
| `queryMap` | `pkg/meta/utils.go` | **без изменений** (только используется) |

## Execution Flow

**1. `NewMetaClient(uri, conf)`.**
1. Если в `uri` нет `://` — префикс `redis://`.
2. `driver = uri[:index("://")]`; если `driver` не найден в `metaDrivers` → вернуть `error` (имя драйвера), **без** `logger.Fatalf`.
3. Для `mysql`/`postgres` — `setPasswordFromEnv(uri)` (ошибка → вернуть).
4. `conf == nil` → `DefaultConf()`; иначе `conf.SelfCheck()`.
5. Делегирование: `return metaDrivers[driver](driver, uri[p+3:], conf)` — результат `(Meta, error)` возвращается вызывающему.

**2. `CreateRedisClient(redisURL)`.**
1. `parseRedisClientOptions(redisURL)`:
   - scheme-default → `url.Parse` → `queryMap` читает `min/max-retry-backoff`, `read/write-timeout` (дефолты 20ms/10s/30s/5s — как в `newRedisMeta`) и TLS-параметры (`insecure-skip-verify`, `tls-cert-file`, `tls-key-file`, `tls-ca-cert-file`, `tls-server-name`).
   - Client-side caching параметры (`client-cache`, `client-cache-size`, `client-cache-expire`, `client-cache-preload`) разбираются и **потребляются** (pop) идентично `newRedisMeta`, чтобы URL очищался так же и дубликат парсинга оставался в синхроне при ребейзе. Сам JuiceFS-кеш метаданных (`redisCache`, `redis_csc.go`) к сырому клиенту **не применяется**: это слой, привязанный к `redisMeta` и его `prefix`, а `CreateRedisClient` возвращает голый `redis.UniversalClient`.
   - `redis.ParseURL` → `*redis.Options`; применение TLS (cert/key, CA pool, ServerName, InsecureSkipVerify).
   - Пароль: `REDIS_PASSWORD` → `META_PASSWORD` → `META_PASSWORD_FILE` (`readPasswordFromFile`).
   - `MaxRetries = -1` (retry отключены, как в `outbox`-варианте с `conf == nil`); `MaintNotificationsConfig = ModeDisabled`.
2. Детект режима по `u.Host`:
   - **Sentinel**: есть запятая и она раньше двоеточия → `redis.FailoverOptions` (`MasterName` = первый сегмент, `SentinelAddrs` = остальные, дефолтный порт `26379`, `SENTINEL_PASSWORD`) → `redis.NewFailoverClient`.
   - **Cluster**: несколько хостов через запятую → `redis.ClusterOptions` (`Addrs`, `MaxRedirects = 1`) → `redis.NewClusterClient`.
   - **Single**: иначе → `redis.NewClient(opt)`.
3. Возврат `(redis.UniversalClient, nil)`; ошибка парсинга → `(nil, error)`.

## Invariants

1. **Upstream-файлы нетронуты:** `newRedisMeta` (`redis.go`) и `NewClient` (`interface.go`) не модифицируются; новые функции живут в `client.go`/`redis_client.go`. Проверка: grep `func newRedisMeta` в `redis.go` и `func NewClient` в `interface.go` — определения на месте; `CreateRedisClient`/`NewMetaClient` — в других файлах.
2. **Паритет движков:** `NewMetaClient` делегирует зарегистрированным `Creator`'ам → поведение для Redis/SQL/KV/grpc идентично `NewClient`; семантических изменений движков нет (AGENTS.md «Agent boundaries» не нарушается).
3. **Без завершения процесса:** ни одна из функций не вызывает `logger.Fatalf`/`os.Exit`; все ошибки возвращаются.
4. **Паритет опций с мета-движком:** `CreateRedisClient` использует те же дефолты (таймауты 20ms/10s/30s/5s, `MaxRetries = -1`, TLS, пароль из env), что `newRedisMeta`.
5. **Дублирование осознанно и помечено:** `parseRedisClientOptions` дублирует парсинг `newRedisMeta`; в файле — комментарий «deliberate duplicate of newRedisMeta option parsing to avoid upstream rebase conflicts; keep in sync on rebase».

## Decisions & Rationale

| # | Решение | Альтернатива и почему отклонена |
|---|---|---|
| D1 | Новые файлы `pkg/meta/client.go` + `pkg/meta/redis_client.go`; `redis.go`/`interface.go` не трогаем | Подход `outbox` (вынести `createRedisOptions` из `newRedisMeta` в `redis.go`): upstream правит тело `newRedisMeta` → конфликты при каждом ребейзе (именно та проблема, которую решаем) |
| D2 | Дублировать парсинг опций в приватном `parseRedisClientOptions` | Переиспользовать `createRedisOptions`: требует изменения `newRedisMeta` (конфликты). Дублирование ограничено (~90 строк) и изолировано в одном файле — приемлемая цена за rebase-safety |
| D3 | `NewMetaClient` переиспользует `metaDrivers` + `setPasswordFromEnv` + `DefaultConf` (без дублирования) | Дублировать реестр драйверов: не нужно — `metaDrivers` package-level и заполнен `init()`; дублирование дало бы второй источник правды |
| D4 | `NewMetaClient` возвращает `(Meta, error)`; `NewClient` оставляем как есть (CLI-путь) | Менять `NewClient` на возврат ошибки: ломает все CLI-вызывающие (`cmd/*.go`) + трогает upstream `interface.go` (конфликты) |
| D5 | `CreateRedisClient` детектит single/sentinel/cluster как `newRedisMeta` | Только single-node: outbox consumer / meta proxy могут указывать на sentinel/cluster Redis — режим обязан совпадать с мета-движком |
| D6 | `MaxRetries = -1` в `CreateRedisClient` (retry отключены) | Брать `conf.Retries`: сигнатура `CreateRedisClient(redisURL)` без `conf` (совпадает с `outbox`); при необходимости — отдельный `CreateRedisClientWithConf` (Open Question) |

## Integration Points

1. **Внешний модуль → `pkg/meta`:** импорт `github.com/juicedata/juicefs/pkg/meta`, вызов `meta.NewMetaClient` / `meta.CreateRedisClient`. Модуль `github.com/juicedata/juicefs` уже публичный — отдельный Go-модуль не нужен.
2. **`NewMetaClient` → существующие символы пакета:** `metaDrivers` (interface.go:575), `setPasswordFromEnv` (interface.go:610), `DefaultConf` (config.go:59), `Creator` (interface.go:573).
3. **`CreateRedisClient` → существующие символы пакета + go-redis:** `queryMap` (utils.go:91), `readPasswordFromFile` (interface.go:600), `redis.*` + `maintnotifications` (go-redis/v9).
4. **Будущее (другие ветки, отдельные changes):** `cmd/outbox.go` (ветка `outbox`) и `cmd/meta_proxy.go` (ветка `agio-drive-v2`) могут перейти на `CreateRedisClient`/`NewMetaClient`; на этой ветке вызывающих нет.

## Risks / Trade-offs

- [Дрейф дубликата] `parseRedisClientOptions` может расойтись с парсингом `newRedisMeta`, если upstream изменит дефолты → комментарий-маркер дубликата + unit-тест, фиксирующий дефолты (20ms/10s/30s/5s, `MaxRetries = -1`); при ребейзе — ревью этого файла.
- [Два пути парсинга Redis-опций] → осознанный trade-off ради rebase-safety; дублирование изолировано в одном файле и ограничено по объёму.
- [Расхождение `NewMetaClient` ↔ `NewClient`] → `NewMetaClient` повторяет `NewClient` построчно (тот же driver-handling), отличается только обработкой ошибки; покрывается тестами (unknown driver, nil conf, valid URI).
- [Логирование] `NewClient` пишет `logger.Infof("Meta address: ...")`; `NewMetaClient` как библиотека — без side-effect-логирования (тихий) → зафиксировано как решение, не риск.

## Migration Plan

- Изменение аддитивное, миграции данных нет. Деплой — слияние ветки `juicefs-go-client`.
- Потребители переходят на новые функции в своём темпе (отдельные changes на ветках `outbox`/`agio-drive-v2`).
- Rollback: удалить два новых файла (`client.go`, `redis_client.go`) + тесты — на этой ветке на них не зависит ни один другой код.

## Open Questions

1. **Нужен ли `CreateRedisClientWithConf(redisURL string, conf *Config)`?** Сейчас `MaxRetries = -1` жёстко (совпадает с `outbox`). Если вызывающему нужны кастомные retry — добавить вариант с `conf`. Отложено: текущие потребители (outbox consumer) используют форму без `conf`.
2. **Должен ли `NewMetaClient` логировать адрес метаданных** (как `NewClient`)? По умолчанию — нет (библиотека не должна иметь side-effect-логирования). Если потребителю нужен лог — он логирует сам. Отложено до первого реального потребителя.
