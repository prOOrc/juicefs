# Tasks: add-meta-proxy-outbox

## 1. cmd/meta_proxy.go

- [x] 1.1 Зарегистрировать флаги `--outbox-enabled` (BoolFlag) и `--outbox-stream`
  (StringFlag, default juicefs:outbox) в flag-списке команды meta-proxy — usage/дефолты
  дословно из cmd/flags.go:347-355.
- [x] 1.2 В Action: собрать `metaConf := meta.DefaultConf()`; `metaConf.Outbox.Enabled =
  c.Bool("outbox-enabled")`; `if c.IsSet("outbox-stream") { metaConf.Outbox.StreamName =
  c.String("outbox-stream") }`; `meta.NewClient(metaBackendUrl, metaConf)` вместо
  `meta.DefaultConf()` (паритет cmd/mount.go:378-381).

## 2. Тесты

- [x] 2.1 Юнит-тест виринга: таблица (флаг unset / --outbox-enabled / + --outbox-stream=X)
  → ожидаемый meta.Config.Outbox (Enabled/StreamName); парсинг через cli-аппаратом
  (тест Action-логики в изоляции или через cli.Context как в существующих cmd-тестах).
- [x] 2.2 Регресс: `go build ./... && go vet ./cmd/...` зелёные.

## 3. Спека и валидация

- [x] 3.1 Дельта specs/domain-meta-proxy: ADDED Requirement «Outbox emission configuration»
  + сценарии (включено — события в stream при мутациях через прокси-RPC; выключено —
  байт-в-байт прежнее поведение; stream override применяется только при IsSet).
- [x] 3.2 `openspec validate add-meta-proxy-outbox --strict` — valid.

## DoD (подтверждено ревью 28.11: APPROVED, 0 замечаний)

- [x] build/vet зелёные офлайн; тесты 2.1 зелёные; validate --strict valid;
  Verified-by в дельте указывает на тест 2.1.
