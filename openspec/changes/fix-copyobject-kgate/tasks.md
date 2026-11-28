# Tasks: fix-copyobject-kgate

## 1. Реализация (`pkg/gateway/gateway.go`)

- [x] 1.1 Добавить сразу после `CopyObject` функцию `copyObjectAtDestination` по design D2:
  Stat(src) (ошибка → `jfsToObjectErr`, до Create); Create(dst) со скелетом дословно
  `:948-965` (EEXIST-каталог → ошибка; EEXIST-файл → Delete+re-Create; ENOENT →
  `mkdirAllInBucket(ctx, dstBucket, path.Dir(dst))` + re-Create; иная ошибка —
  `objectCommitErr` как есть); deferred rollback close+Delete(dst) при любой ошибке
  после Create (образец `:966-1033`); путевой `CopyFileRange(src, 0, dst, 0, 1<<63)`
  (форма `:746-751`, ошибка → `jfsToObjectErr`); метаданные `:753-775` байт-в-байт
  с целью dst (KeepEtag/tags/setObjMeta — НЕфатальные, как сегодня); `Stat(dst)`
  (`objectCommitErr`); комментарий-инвариант по образцу `:1457-1463`.
- [x] 1.2 В `CopyObject` заменить блок `:723-795` вызовом helper'а; блок `:796-807`
  (ObjectInfo) собрать из возвращённых fi/etag/tagStr. НЕ ТРОГАТЬ: лок `:703-704`,
  `checkBucket` `:706-711`, пути `:712-713`, src==dst `:715-722`. Убрать ставшие
  ненужными uuid/tmp-локали и импорты.
- [x] 1.3 Смоук `TestObjectWriteSerialization` остаётся без правок (регресс-гард
  same-key сериализации; лок на уровне CopyObject сохранён).

## 2. Тесты (`pkg/gateway/gateway_test.go`, паттерны `:1077-1400`)

- [x] 2.1 `TestCopyObjectAtDestination`: новый получатель — содержимое/ETag равны
  источнику, подслучаи: вложенный `x/y/z` (ENOENT-ветка), размер 0, листинг
  `<bucket>/.sys/tmp` до/после — без новых файлов (каталог может отсутствовать).
- [x] 2.2 `TestCopyObjectAtDestinationReplacesExisting`: существующий получатель с
  иным содержимым/метаданными заменён; ETag/метаданные от источника по директивам;
  xattr прежнего объекта отсутствуют.
- [x] 2.3 `TestCopyObjectAtDestinationDestinationIsDirectory`: получатель-каталог →
  ошибка (ожидание как `:1344`), каталог и источник читаются.
- [x] 2.4 `TestCopyObjectAtDestinationFailureRemovesPartial`: инъекция ошибки в
  CopyFileRange и в Stat (механизм как `:1363`); получатель отсутствует, включая
  подслучай перезаписи; НЕфатальность метаданных не проверяется здесь (осталась
  как в `:753-775`).
- [x] 2.5 Порт `TestCopyObjectTempCreateAfterBucketDeleted` →
  `TestCopyObjectAtDestinationAfterBucketDeleted`: бакет удалён → `BucketNotFound`,
  бакет не пересоздан.
- [x] 2.6 CopyObject-часть `TestObjectCommitAfterBucketDeleted` удалить с комментарием:
  «покрыта TestCopyObjectAtDestinationAfterBucketDeleted и …FailureRemovesPartial»;
  подслучаи других операций не трогать (способ — как в прецеденте 28.11 для PutObject).
- [x] 2.7 `TestCopyObjectAtDestinationKeepsSource` (источник не изменён, повторное
  копирование проходит) и `TestCopyObjectAtDestinationSourceMissingKeepsDestination`
  (Stat(src) до Create: несуществующий источник → ошибка, существующий получатель
  цел, включая overwrite-подслучай).

## 3. Проверки

- [x] 3.1 `go build ./...`
- [x] 3.2 `go vet ./pkg/gateway/...`
- [x] 3.3 `go test ./pkg/gateway/... -count=1` (с ldflags
  `protoregistry.conflictPolicy=ignore` — окружение форка с replace minio, см.
  5d35e449d; без флага пакет паникует на init и на базовом HEAD)
- [x] 3.4 Таргет-регресс: `go test ./pkg/gateway/... -run 'CopyObject|PutObject|CompleteMultipartUpload|ObjectWriteSerialization|BucketDeleted' -count=1`
- [x] 3.5 `grep`-гард: в теле `CopyObject` нет `tpath(` и `Rename(`; `git diff`
  не затрагивает `putObjectAtDestination`/`putObject`/`PutObject`/`PutObjectPart`/
  `CompleteMultipartUpload`/`CopyObjectPart`/`cleanup`/`AbortMultipartUpload`/
  trash-путь `:290-291`; изменены только `pkg/gateway/gateway.go` и
  `pkg/gateway/gateway_test.go`.

## Definition of Done

- 3.1–3.3 зелёные; 3.5 чистый.
- Все сценарии дельты покрыты тестами из Verified-by.
- `openspec validate fix-copyobject-kgate --strict` valid (основная сессия).
- Stage-приёмка (rclone copy внутри remote, `.sys/tmp` пуст, ListBuckets — отдельный
  change) — основная сессия после деплоя; master/prod не трогаются.
