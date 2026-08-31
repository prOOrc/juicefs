# Proposal: add-go-client-library

## Why

Ветка `outbox` добавила `meta.CreateRedisClient` и вынесла парсинг Redis-опций из `newRedisMeta` в отдельную функцию `createRedisOptions` — **прямо в upstream-файл `pkg/meta/redis.go`**. При каждом ребейзе на upstream JuiceFS это порождает конфликты, потому что upstream тоже правит тело `newRedisMeta` (см. `@pkg/meta/redis.go#L212-247` — блок создания failover/cluster-клиента). Кроме того, единственный экспортированный конструктор мета-клиента `meta.NewClient` (`pkg/meta/interface.go:628`) при ошибке вызывает `logger.Fatalf` (убивает процесс), поэтому его **нельзя использовать как библиотеку** из другого модуля (Meta Proxy, outbox consumer, render-клиент, bench). Нужно: (1) добавить создание мета-клиента и `CreateRedisClient` так, чтобы upstream-файлы не трогать (парсинг опций дублируется в новых файлах), и (2) сделать клиент usable как Go-библиотеку.

## What Changes

- **Новый файл `pkg/meta/redis_client.go`**: экспортируемая `CreateRedisClient(redisURL string) (redis.UniversalClient, error)` + приватный дублирующий парсинг опций (URL, query-параметры `min/max-retry-backoff`, `read/write-timeout`, TLS-файлы, пароль из `REDIS_PASSWORD`/`META_PASSWORD`/`META_PASSWORD_FILE`). Поддерживает single-node, sentinel (failover) и cluster-режимы — как `newRedisMeta`. **Не трогает `newRedisMeta`/`redis.go`.**
- **Новый файл `pkg/meta/client.go`**: экспортируемый library-safe конструктор мета-клиента `NewMetaClient(uri string, conf *Config) (Meta, error)` — поведению аналогичен `NewClient`, но возвращает `(Meta, error)` вместо `logger.Fatalf`; работает со всеми зарегистрированными драйверами (`redis`, `rediss`, `unix`, `mysql`, `postgres`, `sqlite`, `tikv`, `etcd`, `badger`, `fdb`, `grpc`) через существующий `metaDrivers`.
- **`pkg/meta/redis.go` и `pkg/meta/interface.go` НЕ изменяются** — upstream-код остаётся нетронутым, конфликты при ребейзе исчезают (новые файлы не пересекаются с upstream-изменениями).
- **Тесты**: unit-тесты парсинга опций и выбора режима клиента (single/sentinel/cluster) + library-safe поведение `NewMetaClient` (ошибка возвращается, процесс не падает).

**BREAKING:** нет. Изменения чисто аддитивные; существующие `NewClient`/`newRedisMeta` не меняются.

## Capabilities

### New Capabilities

- `domain-go-client`: library-usable JuiceFS client — экспортируемые конструкторы `NewMetaClient` (мета-клиент `meta.Meta`, возвращает ошибку) и `CreateRedisClient` (сырой `redis.UniversalClient`), реализованные в новых файлах `pkg/meta/` без изменения upstream-кода, чтобы ребейз на upstream не порождал конфликтов.

### Modified Capabilities

(нет — изменение аддитивное, требования существующих капабилити не меняются)

## Non-goals

- Не менять upstream-поведение `newRedisMeta` и `NewClient`; не «выносить» парсинг из `newRedisMeta` (именно это вызывало конфликты в `outbox`).
- Не мигрировать существующих вызывающих на этой ветке: `cmd/outbox.go`, `cmd/meta_proxy.go`, `scripts/bench_grpc.go` живут на других ветках (`outbox`, `agio-drive-v2`) — их переход на `NewMetaClient`/`CreateRedisClient` — отдельный change на тех ветках.
- Не создавать полный FS-клиент (meta + chunk + object + vfs) — только мета-клиент и сырой Redis-клиент.
- Не вводить отдельный Go-модуль: используем существующий `github.com/juicedata/juicefs`, потребитель импортирует `pkg/meta`.
- Не менять SQL/KV-бэкенды и не вносить семантических изменений в `pkg/meta/` (parity Redis/SQL/KV не затрагивается — это добавление конструкторов, а не изменение движка).
- Не дублировать логику `Format`/`Load`/object storage — `CreateRedisClient` возвращает только `redis.UniversalClient`, без подключения к метаданным тома.

## Related Requirements

- ADR-001 (accepted): Гибридный spec-driven workflow — процесс, которому следует этот change (`specs/decisions/ADR-001-hybrid-spec-workflow.md`).
- Архитектурный план agio Drive v13 (рабочий документ, `.qwen/plans/agio-drive-full-plan-v13.md`): бизнес-контекст — Meta Proxy, outbox consumer и render-клиент создают клиенты по URL из внешнего модуля; именно это требует library-safe конструкторов.
- Приоритетный прецедент: ветка `outbox` (`meta.CreateRedisClient`, `createRedisOptions` в `pkg/meta/redis.go`) — этот change переосуществляет тот же функционал без изменения upstream-файла.
- **Отдельного BRD/SRS-требования «клиент как библиотека» нет** (в `specs/index.md` только SRS-001 по шифрованию, ADR-001/002). Это инфраструктурный enabler: потребность вытекает из архитектуры AGIO Drive (внешние модули должны создавать `meta.Meta`/Redis-клиент по URL, получая ошибку вместо `os.Exit`). Ссылка на SRS-001 неуместна — шифрование к этому change отношения не имеет.

## Impact

- **Код (форк, ветка `juicefs-go-client`):** новые файлы `pkg/meta/redis_client.go`, `pkg/meta/client.go` + тесты `pkg/meta/redis_client_test.go`, `pkg/meta/client_test.go`. `pkg/meta/redis.go`, `pkg/meta/interface.go` — без изменений.
- **API:** два новых экспортированных символа в пакете `pkg/meta`: `NewMetaClient`, `CreateRedisClient`. Потребитель (другой модуль) импортирует `github.com/juicedata/juicefs/pkg/meta`.
- **Зависимости:** без новых — используется существующий `github.com/redis/go-redis/v9` (`redis.Options`, `redis.FailoverOptions`, `redis.ClusterOptions`, `redis.UniversalClient`).
- **Совместимость с upstream:** улучшена — upstream-файлы не трогаются, ребейз не конфликтует.
- **Паритет движков:** не затрагивается (аддитивные конструкторы; `NewMetaClient` прозрачно работает со всеми драйверами через `metaDrivers`).
