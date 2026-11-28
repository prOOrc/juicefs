# Proposal: fix-copyobject-kgate

## Why

Server-side `CopyObject` шлюза (`pkg/gateway/gateway.go:702`) при различающихся источнике и
получателе идёт через промежуточный файл в `.sys/tmp` (`:723-744`: `fs.Create(tmp)`) →
`CopyFileRange(src→tmp)` (`:746-751`) → etag/tags/`setObjMeta` на tmp (`:753-775`) →
`Rename(tmp→dst)` (`:782-795`). На шифрованном platform-томе этот путь не работает в двух
точках:

1. `provisionFileCrypto` (`pkg/meta/grpc_server_fuse.go:294-345`) при Create запрашивает
   файловый ключ по пути создания; путь под `.sys/tmp` вне whitelist'а платформы →
   Unlink + `syscall.EIO` (reported при приёмке 28.11; воспроизводится первым же
   server-side copy через rclone/mc).
2. Даже при успехе `Open` запрашивает ключ по ТЕКУЩЕМУ пути (`:376-437`); после rename
   из `.sys` в `companies/<code>/…` FEK/KEK-привязка не совпадает → `EACCES` fail-closed.

`PutObject`, `PutObjectPart`, `CompleteMultipartUpload` с 28.11 (fix-цикл, adv2) создают
файл сразу по финальному пути (инвариант «file key derived from creation path»,
`gateway.go:1457-1463`) и от этого не страдают. `CopyObjectPart` делегирует
`PutObjectPart` (`:1383-1393`) — уже безопасен. CopyObject — последний staging-путь.

## What Changes

- `CopyObject` (src ≠ dst) создаёт файл получателя по финальному пути (новая функция
  `copyObjectAtDestination`), содержимое переносится `CopyFileRange` (путевой вызов,
  форма `:746-751`). Staging в `.sys/tmp` и `Rename` из CopyObject удаляются.
- Скелет Create/rollback переносится дословно из `putObjectAtDestination` (`:948-965`,
  `:966-1033`): EEXIST-каталог → ошибка; EEXIST-файл → Delete + повторный Create
  (S3-семантика молчаливой перезаписи); ENOENT → `mkdirAllInBucket` + повторный Create;
  любая ошибка после Create → rollback-Delete получателя.
- Перенос метаданных (`:753-775`: KeepEtag-xattr, tags, `setObjMeta`, `Stat`) —
  логика байт-в-байт, меняется только цель вызовов (tmp → dst); нефатальность
  ошибок метаданных сохраняется как сегодня.
- Источник проверяется `Stat(src)` ДО Create получателя (путевой copy не даёт ранней
  ошибки источника) — «источник исчез → получатель не тронут», включая overwrite.
- Блокировки не меняются: `writeLocks.lock(dstBucket, dstObject)` остаётся на уровне
  `CopyObject` (`:703`) — same-key сериализация (`TestObjectWriteSerialization`,
  кейс «copy») сохраняется, тест остаётся без изменений.
- `checkBucket` src/dst (`:706-711`) даёт `BucketNotFound` до любых Create — не меняется.

## Capabilities

### New Capabilities

Нет.

### Modified Capabilities

- `domain-gateway-platform-mode`: ADDED «Server-side copy writes final object at
  destination» и «Server-side copy failure leaves no destination file». Основной спеки
  capability в `openspec/specs/` ещё нет (change add-gateway-platform-mode не
  заархивирован) — дельта чисто ADDED, синк при архивации создаст/дополнит спеку.

## Non-goals

- src == dst ветка (`:715-722`) — не менять.
- `CopyObjectPart` (`:1383-1393`) и его TODO — вне скоупа.
- `cleanup()`, `AbortMultipartUpload`, trash (`:85`, `:290-291`), multipart upload-пути
  (`:259-267`), фоновая уборка tmp — не менять.
- `putObjectAtDestination`, `putObject`, `PutObject`, `PutObjectPart`,
  `CompleteMultipartUpload` — не рефакторить, общий скелет не выделять (follow-up).
- Whitelist key manager (agio-platform) — вне репозитория.
- Атомарность перезаписи через staging — осознанно не восстанавливается (та же цена,
  что принята для PutObject/multipart 28.11).
- Деплой — stage по GO владельца; master/prod не трогаются («пока не льём» 28.11).

## Consequences

- Перезапись получателя перестаёт быть атомарной (как у PutObject с 28.11): во время
  копирования получатель отсутствует/частично заполнен; при сбое перезаписи прежнее
  содержимое уже удалено и не восстанавливается.
- Аварийное завершение процесса между Create и rollback оставляет частичный файл по
  финальному пути (фоновая уборка tmp его не покрывает).
- Поведение безусловно для всех томов, включая нешифрованные.
- Положительный эффект: server-side CopyObject работает там же, где работает PutObject
  (шифрованные platform-томы), — закрывает класс K-gate/EIO для copy.
