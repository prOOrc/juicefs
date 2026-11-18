# Tasks: add-outbox-s3-sink

## 1. Контракт события и эмиссия

- [ ] 1.1 Расширить `JuiceFsEvent` (pkg/meta/events.go): поля `uuid string`, `mtime int64`,
      `xattrs map[string]string`, все omitempty; mtime = Unix nanoseconds (wire-контракт — ns).
      Проверка: `go build ./pkg/meta/... && go vet ./pkg/meta/...`
- [ ] 1.2 `publishEventToPipe` (pkg/meta/redis_event.go): генерация `uuid` (github.com/google/uuid,
      direct dep), заполнение `mtime` из `Attr.Mtime` (секунды) ×10⁹ + `Attr.Mtimensec` (проброс
      Attr в точки эмиссии в pkg/meta/redis.go: doMknod, doRename, doWrite, doFallocate); в обоих
      Moved-хелперах `publishFileMovedToPipe` (:102) и `publishDirMovedToPipe` (:115) —
      внеконвейерный снимок xattr `s3-meta` (общий хелпер, whitelist-константа; механика как у
      `reconstructPath`).
      Файлы: pkg/meta/redis_event.go, pkg/meta/redis.go.
      Проверка: `go build ./pkg/meta/... && SKIP_NON_CORE=true go test -count=1 ./pkg/meta/... -run 'TestEventUUIDGenerated|TestWritePublishesMtime|TestRenamePublishesS3MetaSnapshot|TestSetXattrEmitsNoEvent'`
- [ ] 1.3 Тесты (реальный Redis, паттерн `newTestRedisMetaWithOutbox` + readOutboxEvents):
      TestEventUUIDGenerated (uuid непуст и уникален), TestWritePublishesMtime (ns-значение),
      TestRenamePublishesS3MetaSnapshot (hSet s3-meta до rename; трансформация moved-from-tmp
      сохраняет xattrs), TestSetXattrEmitsNoEvent, TestEventJSONBackwardCompatibility
      (marshal без новых полей → unmarshal ok; расширенный JSON → старая структура игнорирует).
      Файлы: pkg/meta/redis_event_test.go.
      Проверка: `make test.meta.core`
- [ ] 1.4 Выровнять литерал трансформации в `FilterEvent` (pkg/meta/event_filter.go:46-59) с SoT
      («all fields preserved except `old_path`»): перенести `Size` (сейчас теряется — дрейф от
      SoT) и новые поля `uuid`/`mtime`/`xattrs`. Тест: расширить
      TestFilterEvent_TransformPreservesFields (трансформация сохраняет Size и новые поля).
      Файлы: pkg/meta/event_filter.go, pkg/meta/event_filter_test.go.
      Проверка: `go test -count=1 ./pkg/meta/... -run 'TestFilterEvent'`

## 2. Доставка: EventSink, reclaim

- [ ] 2.1 Rename `KafkaPublisher` → `EventSink` (pkg/meta/redis_outbox.go:32-34) и все
      использования (pkg/meta/redis_outbox.go, cmd/outbox.go); поведение не меняется.
      Проверка: `go build ./... && go vet ./pkg/meta/... ./cmd/...`
- [ ] 2.2 consumeLoop (pkg/meta/redis_outbox.go): reclaim-проход `XAutoClaim` (MinIdle =
      `--reclaim-min-idle`, default 60s, Count 100) → общий `processMessage`; при backpressure
      s3-синка poll и reclaim-проход не выполняются. XACK оригинала в `moveToDeadQueue` уже
      реализован (deleteMessage/XAck) — код не меняется, фиксируется тестом (2.3).
      Проверка: `go build ./pkg/meta/... && SKIP_NON_CORE=true go test -count=1 ./pkg/meta/... -run 'TestReclaimPending|TestMoveToDeadQueue'`
- [ ] 2.3 Тесты (реальный Redis 127.0.0.1:6379/DB15, skip-паттерн
      `TestInitConsumerGroupNoStream`; go-redis v9.18.1 XAutoClaim поддерживает, сервер ≥ 6.2):
      TestReclaimPendingMessages (сообщение старше MinIdle в PEL → redelivery → обработка),
      TestMoveToDeadQueueAcksOriginal (после DLQ-переноса сообщение отсутствует в PEL — фиксация
      существующего поведения).
      Файлы: pkg/meta/redis_outbox_test.go.
      Проверка: `make test.meta.core`

## 3. S3Sink

- [ ] 3.1 Новый pkg/meta/s3_sink.go: `S3SinkConfig` (prefix, spill-dir, spill-max-size),
      `NewS3Sink(cfg, storage object.ObjectStorage)` (инъекция хранилища), `Publish`: ключ
      `<prefix>/<YYYY-MM-DD>/<HH>/<uuid>.json` (UTC по event.Timestamp), uuid-шим для
      legacy-событий, спилл (JSONL-чанки, tmp+rename, лимит байт), дренаж FIFO с переписыванием
      остатка при восстановлении, backpressure-флаг, `Health() error`
      (реализация `SinkHealthChecker`). Ошибки sink возвращаются вызывающему; panic/os.Exit в
      sink-пути запрещены.
      Проверка: `go build ./pkg/meta/... && go vet ./pkg/meta/...`
- [ ] 3.2 Тесты с моком ObjectStorage (встраивание интерфейса, tmpdir как спилл-каталог):
      TestS3SinkKeyFormat, TestS3SinkGeneratesUUIDWhenAbsent, TestS3SinkPutErrorReturned,
      TestSpillOnSinkUnavailable, TestSpillOverflowBackpressure, TestSpillDrainAfterRecovery.
      Файлы: pkg/meta/s3_sink_test.go.
      Проверка: `go test -count=1 ./pkg/meta/... -run 'TestS3Sink|TestSpill'`

## 4. CLI и health

- [ ] 4.1 cmd/outbox.go: флаги `--sink`, `--s3-*`, `--reclaim-min-idle` + env-алиасы (конвенция
      существующих флагов); снять `Required: true` у kafka-brokers (cmd/outbox.go:70-75),
      валидация по режиму (kafka → brokers required; s3 → endpoint+bucket required; неизвестный
      sink → ошибка);
      ветвление конструктора sink (строка :191; s3: `object.CreateStorage` с bucket в path по
      идиоме cmd/sync.go:425-443, `--s3-region` → установка `MINIO_REGION`); /health комбинирует
      `outbox.IsHealthy()` + `SinkHealthChecker` (type-assertion).
      Проверка: `go build ./cmd/... && go vet ./cmd/...`
- [ ] 4.2 Тесты флагов: TestValidateSinkFlags (kafka без brokers → ошибка; s3 без endpoint/bucket
      → ошибка; неизвестный sink → ошибка; s3 не требует kafka-флагов).
      Файлы: cmd/outbox_test.go.
      Проверка: `go test -count=1 ./cmd/... -run TestValidateSinkFlags`

## 5. Документация

- [ ] 5.1 Обновить docs/en/deployment/outbox.md: режим s3, таблица флагов/env, именование
      объектов `events/<date>/<hour>/<uuid>.json`, спилл и backpressure, health-семантика,
      обновлённая схема потока, примечание об at-least-once и дедупе по uuid.
      Проверка: `grep -qi -- '--sink' docs/en/deployment/outbox.md && grep -qi 'spill' docs/en/deployment/outbox.md && grep -qi 'backpressure' docs/en/deployment/outbox.md`

## 6. E2E (apply-фаза)

- [ ] 6.1 Новый docker-compose.outbox-s3-test.yml (корень, рядом с docker-compose.enc-test.yml):
      redis + minio, network_mode: host.
      Проверка: `docker compose -f docker-compose.outbox-s3-test.yml up -d && docker compose -f docker-compose.outbox-s3-test.yml ps`
- [ ] 6.2 E2E-прогон: format → mount `--outbox-enabled` → операции через FUSE (create/write/
      delete/rename) и через gateway PUT с `X-Amz-Meta-*` → `juicefs outbox --sink s3
      --s3-spill-dir /tmp/outbox-spill` → объекты `events/` с валидным JSON
      (created/written/deleted/moved + uuid/mtime/xattrs на gateway-PUT) → `docker stop` minio →
      процесс жив, спилл растёт, health 200 → `docker start` minio → дренаж, объекты на месте;
      отдельный прогон kafka-режима на docker-compose.test.yml (redpanda) — регресс.
      Проверка: `docker compose -f docker-compose.outbox-s3-test.yml up -d` и последовательность
      шагов сценария; финальные проверки: `docker logs <outbox-container> 2>&1 | grep -ci panic`
      → 0, LIST префикса `events/` содержит объекты всех сценариев, health-curl в окно остановки
      minio → 200.

## 7. Образ (apply-фаза, кросс-репо agio-cloud)

- [ ] 7.1 agio-cloud: bump `JUICEFS_REV` в docker/juicefs/Dockerfile (и в variables
      `JUICEFS_REV` джобы juicefs-image-build в .gitlab-ci.yml) на HEAD ветки
      `agio-drive-v2` после мержа; запустить CI-джобу juicefs-image-build (manual); записать
      фактический тег сюда:
      IMAGE_TAG: __TO_BE_FILLED_AT_APPLY__
      Проверка: CI green, тег уникален (manifest inspect guard), smoke `juicefs meta-proxy --help` в джобе.

## Definition of Done

- `go build ./...`
- `go vet ./cmd/... ./pkg/meta/...`
- `go test -count=1 ./pkg/meta/... -run 'TestS3Sink|TestSpill|TestEvent|TestWrite|TestRename|TestSetXattr|TestReclaim|TestMoveToDeadQueue|TestFilter'` — зелёные
- `make test.meta.core` — зелёный (на машине с Redis reclaim/DLQ-тесты не SKIP)
- `golangci-lint run ./cmd/... ./pkg/meta/...`
- Kafka-режим: регресс подтверждён (Stage 6.2 на docker-compose.test.yml)
- E2E Stage 6 пройден; IMAGE_TAG заполнен (Stage 7)
- `openspec validate add-outbox-s3-sink --strict` — без ошибок
