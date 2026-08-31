## Purpose

Позволяет использовать JuiceFS-мета-клиент как Go-библиотеку из внешнего модуля: экспортируемый конструктор `NewMetaClient`, возвращающий ошибку вместо завершения процесса, реализованный без изменения upstream-кода `pkg/meta`. (Сырой Redis-клиент `CreateRedisClient` вынесен на ветку `outbox` — см. design.md.)

## ADDED Requirements

### Requirement: Library-safe meta client constructor

Пакет `pkg/meta` SHALL предоставлять экспортируемую функцию `NewMetaClient(uri string, conf *Config) (Meta, error)`, создающую мета-клиент для любого зарегистрированного драйвера. При ошибке (неизвестный драйвер, некорректный URI, недоступный бэкенд) функция SHALL возвращать ненулевую `error` и SHALL NOT вызывать `logger.Fatalf` или `os.Exit`. При `conf == nil` функция SHALL использовать параметры `DefaultConf()`.

#### Scenario: Valid URI returns a client

- **WHEN** вызывается `NewMetaClient` с корректным URI зарегистрированного драйвера и доступным бэкендом
- **THEN** функция возвращает ненулевой `Meta` и `nil` error

#### Scenario: Unknown driver returns an error without exiting

- **WHEN** вызывается `NewMetaClient` с URI неизвестного драйвера (например, `unknown://host:1`)
- **THEN** функция возвращает ненулевую `error`, содержащую имя драйвера, и процесс НЕ завершается

#### Scenario: Nil conf uses default config

- **WHEN** вызывается `NewMetaClient` с `conf == nil`
- **THEN** клиент создаётся с параметрами `DefaultConf()`

#### Scenario: Works for every registered driver

- **WHEN** вызывается `NewMetaClient` с URI любого из зарегистрированных драйверов (`redis`, `rediss`, `unix`, `mysql`, `postgres`, `sqlite`, `tikv`, `etcd`, `badger`, `fdb`, `grpc`)
- **THEN** функция делегирует создание соответствующему зарегистрированному конструктору и возвращает его результат

### Requirement: Additive, upstream-untouched implementation

Новый конструктор SHALL быть реализован в новом файле пакета `pkg/meta` без изменения существующих функций `newRedisMeta` и `NewClient`. Поведение `newRedisMeta` и `NewClient` SHALL остаться неизменным.

#### Scenario: Existing constructors are unchanged

- **WHEN** выполняется grep по файлу `pkg/meta/redis.go` на определение `func newRedisMeta` и по `pkg/meta/interface.go` на `func NewClient`
- **THEN** оба определения присутствуют в исходных файлах, а `NewMetaClient` определён в другом (новом) файле пакета `pkg/meta`
