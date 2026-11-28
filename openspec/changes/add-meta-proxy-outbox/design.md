# Design: add-meta-proxy-outbox

## Context

Факты (explorer, path:line проверены 28.11):

- Эмиссия живёт в redis-движке: `publishEventToPipe` (pkg/meta/redis_event.go:37) — ранний
  выход `if m.outbox == nil || !m.outbox.Enabled()`; вызовы из doMknod/doWrite/doRename/
  doUnlink/doBatchUnlink/doRmdir/doTruncate (pkg/meta/redis.go:1372/1612/1927/2316/2455/2793/3232)
  — в том же redis-пайплайне, что и мета-мутация.
- `cmd/meta_proxy.go:220`: `m := meta.NewClient(metaBackendUrl, meta.DefaultConf())`;
  `DefaultConf()` → `Outbox.Enabled=false` (pkg/meta/config.go:80-88).
- Флаги-прецедент: `cmd/flags.go:347-355` (`--outbox-enabled` bool без default, `--outbox-stream`
  default `juicefs:outbox`); парсинг-прецедент: `cmd/mount.go:378-381` (IsSet-гвард на stream).
- grpc-клиент (pkg/meta/grpc_client*.go) событий не эмитит — на proxy-пути единственный
  emitter — серверный redisMeta.

## Decisions

### D1. Флаги — общий регистратор `outboxFlags()` (single source имён и дефолтов)

Ровно два флага: `--outbox-enabled` (BoolFlag, default false) и `--outbox-stream`
(StringFlag, default "juicefs:outbox") — дословно прежние из `metaFlags()`
(cmd/flags.go:347-355). Регистрация: в cmd/flags.go заводится `outboxFlags() []cli.Flag`,
`metaFlags()` включает её результат (mount/gateway — поверхность и порядок флагов
не меняются), команда meta-proxy добавляет `outboxFlags()` в свой список (`append` —
spread внутри slice-литерала в Go запрещён). Один источник имён/дефолтов — дефолты
mount/gateway/proxy не разъезжаются.

Альтернативы (отклонены): копия двух флагов inline в meta_proxy.go — дефолты разъедутся;
переиспользование metaFlags() целиком — тянет ~20 лишних mount-флагов в поверхность
proxy; env-only конфиг — ломает паритет с остальными командами форка.

### D2. Виринг: локальный conf = DefaultConf() + Outbox-поля, IsSet-гвард на stream

```go
func outboxConfFromContext(c *cli.Context) *meta.Config {
    conf := meta.DefaultConf()
    conf.Outbox.Enabled = c.Bool("outbox-enabled")
    if c.IsSet("outbox-stream") {
        conf.Outbox.StreamName = c.String("outbox-stream")
    }
    return conf
}
// в Action:
m := meta.NewClient(metaBackendUrl, outboxConfFromContext(c))
```

Хелпер вынесен для тестируемости (TestOutboxConfFromContext, cmd/meta_proxy_outbox_test.go;
контексты через flag.FlagSet+Apply+Parse+cli.NewContext — паттерн
cmd/gateway_platform_test.go:52-64). NewClient принимает *Config — подтверждено
компиляцией. IsSet-гвард — паритет с mount.go:378-381. Остальные поля Outbox-конфига
(ConsumerGroup/KafkaTopic/MaxRetries/TrimMaxLen/ReclaimMinIdle) — движковые дефолты.

### D3. Default-off = zero behavior change

`DefaultConf()` в старом коде ≡ `metaConf` в новом при `--outbox-enabled=false`
(Enabled=false, StreamName из DefaultConf). Никаких других отличий в NewClient-пути нет.
Логирование старта не меняется (секреты/URL не логируются — usage-строки флагов это
сохраняют).

### D4. Консьюмер — вне этого change'а

Публикация в S3-очередь — отдельный процесс `juicefs outbox` (существует, change
2026-11-19-add-outbox-s3-sink; все --s3-* флаги имеют JFS_S3_* env-алиасы, redis-URL —
позиционный аргумент). Его деплой sidecar'ом drive-meta-proxy — change
add-hector-outbox-events в agio-cloud. Группу консьюмера не меняем (default
meta-proxy-outbox).

### D5. Дубли и отказы — существующие механизмы

At-least-once при XAutoClaim-реклейме PEL — глотается персистентным dedup консюмера
платформы (processed_message); при недоступном sink — retry + DLQ `<stream>:dead`
+ XTRIM MAXLEN 10000 на продюсере. Ничего нового не требуется.

## Risks/Trade-offs

- События write-heavy тома: XADD на каждую мутацию — издержки существующего контракта
  domain-outbox (работает на HK beta), TrimMaxLen 10000 ограничивает stream.
- gateway-процесс с grpc:// meta URL не эмитит и не должен (иначе дубли с прокси) —
  зафиксировано в proposal Impact.
