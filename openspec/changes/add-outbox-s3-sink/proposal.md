# Proposal: add-outbox-s3-sink (CRSS change 0c)

## Why

Программа CRSS (кросс-регионная репликация VK↔HK↔BJ, `docs/roadmap-crss.md` в agio-platform) строит
конвейер метаданных juicefs → брокер. На HK брокер не развёртывается: критический путь предусматривает
S3 bucket-sink — события мета-слоя пишутся напрямую в S3-бакет под префикс `events/`, откуда их позже
дренирует drainer (change 2, agio-platform) в Redpanda. Сегодня outbox умеет только Kafka-издателя
(`NewWatermillKafkaPublisherWithConfig`), а недоступность Kafka в легаси-alpha укладывала релей в
crash-loop — доставка обязана переживать недоступность sink'а, не теряя события молча.

Change 0c — первый узел критического пути: 0c → 0a (HK-стек terraform: lifecycle `events/` 7d, пин
образа) → 0b (регистрация facility beta). HK-потребление: `juicefs outbox --sink s3` на gateway VM.

Дополнительная мотивация вне скоупа: тем же sink'ом оживают alpha-события BJ→VK (alpha сейчас на
kafka-моде; миграция в этот change не входит).

## What Changes

1. Селектор sink в CLI `juicefs outbox`: `--sink kafka|s3` (default `kafka`) + флаг-набор `--s3-*`
   с env-алиасами `JFS_S3_*`; интерфейс консюмера обобщён (`KafkaPublisher` → `EventSink`).
2. S3 object sink (`pkg/meta/s3_sink.go`): одно событие = один JSON-объект
   (`JuiceFsEvent` as-is) под ключом `events/<YYYY-MM-DD>/<HH>/<uuid>.json` (UTC по `timestamp`
   события).
3. Доставка at-least-once: периодический reclaim PEL (`XAutoClaim`) — чинит латентный дефект
   (PEL никогда не перечитывался, retry/DLQ были недостижимы); локальный спилл-буфер с лимитом и
   backpressure при переполнении — недоступность sink'а не убивает процесс и не теряет события
   молча (XACK оригинала в DLQ-переносе уже реализован — фиксируется требованием и тестом).
4. Расширение контракта события: `uuid` (генерация на продюсере, в момент эмиссии), `mtime`
   (Unix nanoseconds, из `Attr.Mtime` + `Attr.Mtimensec`), `xattrs` (whitelist-снимок xattr
   `s3-meta` при move/rename — носитель метаданных X-Amz-Meta-* gateway; полный дамп xattr
   запрещён конструктивно). Обратная совместимость JSON в обе стороны.
5. Health: в s3-режиме 503 при backpressure; kafka-режим не меняется.
6. Gateway: флаги `--outbox-enabled`/`--outbox-stream` (по схеме mount) — без них gateway-процесс
   не эмитит события, а HK-писатели = gateway (условие verify-сценария «gateway PUT с
   X-Amz-Meta-* → объекты в events/»).
7. Документация outbox (`docs/en/deployment/outbox.md`) и E2E-compose (redis + minio).

## Capabilities

### New Capabilities

(нет — новая функциональность укладывается в существующую capability outbox.)

### Modified Capabilities

- `domain-outbox`: расширение модели события (uuid/mtime/xattrs) и эмиссии (снимок `s3-meta`
  при Moved), reclaim PEL и ack-политика доставки, новые требования «S3 object sink» и
  «S3 spill and backpressure», CLI-флаги и health-семантика s3-режима.

## Non-goals

- Drainer `events/` → Redpanda (change 2, agio-platform).
- Деплой HK (HK-compose) и lifecycle-правила бакета (change 0a, agio-terraform-alibaba).
- Миграция alpha BJ→VK на s3-sink; правки minio-to-kafka (легаси до cleanup №8).
- Эмиссия событий для SQL/KV мета-движков (граница redis-only сохраняется) и события по дочерним
  объектам при move/rename/delete директории (ADR-002, отдельный change).
- Новые типы событий XattrSet/XattrRemoved (отвергнуто дизайном: gateway ставит `s3-meta` на
  tmp-путь `/.sys/tmp/`, такое событие отфильтруется и полезной нагрузки не несёт; снимок при
  rename покрывает все gateway-записи).
- Публикация/пин тега образа в CI (apply-фаза; см. Cross-repo impact).

## Related Requirements

Бизнес-документов (BRD/SRS) для этого изменения нет: CRSS-роадмап живёт в `docs/roadmap-crss.md`
репозитория agio-platform и не оформлен как BRD в `specs/index.md` — ссылаться не по чему, создание
BRD владельцем не запрашивалось. Смежные документы реестра: ADR-002 (draft) «Outbox: события по
дочерним объектам при move/rename/delete директории» — пересекается по зоне (эмиссия событий), но не
дублирует: ADR-002 о дочерних событиях при move дерева, этот change — о sink'е, контракте события и
доставке; ADR-001 (accepted) — процессный; SRS-001 (шифрование) — нерелевантен, но учтён в
ограничении безопасности: в `xattrs` события попадает только whitelist-ключ `s3-meta`, полный дамп
xattr запрещён.

## Impact

- Код: `cmd/outbox.go` (флаги, валидация режима, ветвление конструктора sink, health),
  `cmd/gateway.go` + `cmd/flags.go` (outbox-флаги, аменд при apply), `pkg/meta/redis_outbox.go`
  (rename `KafkaPublisher`→`EventSink`, reclaim), `pkg/meta/events.go` (поля расширения),
  `pkg/meta/redis_event.go` + `pkg/meta/redis.go` (генерация uuid, mtime, снимок `s3-meta`),
  `pkg/meta/event_filter.go` (выравнивание литерала трансформации moved-from-tmp
  → Created с SoT: сейчас `Size` не переносится; + новые поля), `pkg/meta/s3_sink.go` (новый),
  документация `docs/en/deployment/outbox.md`, `docker-compose.outbox-s3-test.yml` (новый).
- Зависимости: `github.com/google/uuid` v1.6.0 (уже direct dep); Redis-команда `XAUTOCLAIM`
  (Redis-сервер ≥ 6.2; клиент go-redis v9.18.1 поддерживает).
- Системы: потребители Kafka-топика `juicefs.events` (в т.ч. консюмер платформы) должны быть
  идемпотентны к повторным доставкам — reclaim легализует at-least-once для обоих режимов.
- Кросс-репо: см. раздел Cross-repo impact (claims).

## Cross-repo impact (claims)

- **agio-platform, change 2 (drainer)**: JSON-контракт события = `JuiceFsEvent` as-is (раздел
  Cross-repo contract в design.md этого change: поля, семантика omitted, at-least-once,
  latest-wins по (volume, path|inode) с `timestamp` как tiebreak, дедуп по `uuid`); ключи объектов
  `events/<YYYY-MM-DD>/<HH>/<uuid>.json`, водяной знак LIST'ом по часовому префиксу.
- **agio-terraform-alibaba, change 0a**: lifecycle 7d по префиксу `events/`; пин образа через
  `var.hk_outbox_image_tag` (имя переменной при необходимости ренеймится в 0a-пропозале); HK-compose
  передаёт `--sink s3`, креды бакета и `--s3-spill-dir` (спилл включён явно — default выключен).
- **agio-cloud**: bump `JUICEFS_REV` в `docker/juicefs/Dockerfile` на HEAD ветки `agio-drive-v2`
  после мержа; CI-джоба `juicefs-image-build` собирает уникальный тег (конвенция
  `agio-v1.4.x_<date><letter>_<rev>`); публикация тега повторно запрещена правилом CI
  (manifest inspect guard). Точный ref фиксируется в tasks.md этого change на apply-фазе.
- **agio-platform, консюмер `juicefs.events`**: подтвердить идемпотентность к повторным доставкам
  (reclaim делает at-least-once фактической для kafka-режима тоже).
