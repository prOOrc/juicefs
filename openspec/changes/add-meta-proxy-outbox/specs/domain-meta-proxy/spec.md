## ADDED Requirements

### Requirement: Outbox emission configuration

Команда `meta-proxy` SHALL регистрировать флаги `--outbox-enabled` (bool, default false) и
`--outbox-stream` (string, default `juicefs:outbox`) с той же семантикой, что у команд
mount/gateway. При включённом флаге прокси SHALL создать meta-движок с
`Outbox.Enabled=true`, и события мета-мутаций всех клиентских RPC (нативный контракт
domain-outbox: doMknod/doWrite/doRename/doUnlink/doRmdir/doTruncate) SHALL попадать в
указанный Redis stream. Значение `--outbox-stream` SHALL применяться только при явной
установке флага. При выключенном флаге (default) поведение SHALL быть идентично прежнему
(`Outbox.Enabled=false`): события не эмитятся, никаких иных изменений в старте прокси.
Значения флагов не содержат секретов и логированию не запрещаются.

Verified-by: cmd/meta_proxy_outbox_test.go::TestOutboxConfFromContext

#### Scenario: Proxy emits events for client mutations when enabled

- **WHEN** прокси запущен с `--outbox-enabled` и клиент выполняет мутацию (create/write/rename/delete)
- **THEN** событие доменного контракта domain-outbox добавляется в Redis stream прокси-движка
  (тот же формат, что у mount/gateway с прямым redis://)

#### Scenario: Default off preserves behavior

- **WHEN** прокси запущен без `--outbox-enabled`
- **THEN** meta-движок создаётся с `Outbox.Enabled=false` (как `meta.DefaultConf()`),
  события не эмитятся

#### Scenario: Stream name override only when explicitly set

- **WHEN** прокси запущен с `--outbox-enabled --outbox-stream custom:stream`
- **THEN** события идут в `custom:stream`
- **WHEN** прокси запущен с `--outbox-enabled` без `--outbox-stream`
- **THEN** события идут в дефолтный `juicefs:outbox`
