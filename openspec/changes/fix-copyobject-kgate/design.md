# Design: fix-copyobject-kgate

## Context

Проверено по коду adv2 @306f6d1e2 (worktree fix-copyobject-kgate):

- `CopyObject` (`pkg/gateway/gateway.go:702-808`): lock `writeLocks.lock(dst)` `:703-704`
  (defer unlock — НА уровне CopyObject, вне staging-блока); `checkBucket` src/dst
  `:706-711`; пути `:712-713`; src==dst `:715-722`; staging `:723-744` (uuid, tmp,
  `Create(tmp)` c ENOENT-ретраем после `mkdirAll(path.Dir(tmp))` — это mkdirAll для TMP);
  `CopyFileRange(src, 0, tmp, 0, 1<<63)` `:746-751` — **путевой** вызов (не хендловый);
  метаданные `:753-775` (KeepEtag `:753-762`, tags `:764-771`, `setObjMeta` `:772-775` —
  ошибки метаданных ЛОГИРУЮТСЯ, НЕ фатальны; `Stat(tmp)` `:776-780` — фатален);
  `Rename`+ENOENT-ретрай после `mkdirAllInBucket(dstBucket,…)` `:782-795`; ObjectInfo
  `:796-807` (из `fi` tmp-Stat + `srcInfo.UserDefined`).
- Прецедент: `putObjectAtDestination` `:946-1033` (скелет Create `:948-965`: EEXIST-каталог
  → ошибка, EEXIST-файл → Delete+re-Create, ENOENT → `mkdirAllInBucket`, иная ошибка —
  как есть; rollback `:966-1033`); `CompleteMultipartUpload` `:1427-1546` (инвариант
  `:1457-1463`, путевой CopyFileRange `:1509`).
- Крипто-механика: `grpc_server_fuse.go:294-345` (ключ по пути при Create, ошибка →
  Unlink+EIO), `:376-437` (ключ по текущему пути при Open → EACCES после rename);
  `render_meta.go:225-254` — путевой CopyFileRange в шифрованный приёмник делает
  RewrapSlicesRange под FEK приёмника → «Create по финальному пути + путевой
  CopyFileRange» криптографически корректен (работает в CompleteMultipartUpload).
- Остальные пользователи `tpath/Rename` не затрагиваются: trash `:85/:290-291`,
  uploads `:259-267`; после фикса `Rename` в gateway.go остаётся только в trash.
- Спека `openspec/specs/domain-gateway-platform-mode/` пока НЕ существует (change
  add-gateway-platform-mode не заархивирован) → дельта чисто ADDED, синк при архивации
  создаст спеку; порядок архиваций не важен (обе стороны — ADDED).

## Decisions

### D1. Отдельная copyObjectAtDestination; putObjectAtDestination не переиспользуется

CopyFileRange путевой (источник — путь, не поток) — адаптер-поток теряет серверное
копирование и RewrapSlicesRange; метаданные переносятся от источника (не из opts);
прецедент CompleteMultipartUpload с тем же типом переноса сделан отдельной функцией.
Из прецедента дословно берутся скелет Create `:948-965` и rollback `:966-1033`;
комментарий-инвариант по образцу `:1457-1463`. Рефакторинг общего скелета — follow-up.

### D2. Поправки к скелету архитектора (по фактическому коду)

1. **Нет шага «открыть источник»**: CopyFileRange путевой. Вместо этого — `Stat(src)`
   ДО любых манипуляций с получателем: ошибка → `jfsToObjectErr`, получатель не тронут
   (в т.ч. при overwrite прежний файл жив). TOCTOU-окно остаётся (как и сегодня).
2. **ENOENT-ветка Create**: `mkdirAllInBucket(ctx, dstBucket, path.Dir(dst))` (прецедент
   dst-иерархии), НЕ tmp-шный `mkdirAll`.
3. **Нефатальность метаданных сохраняется**: `:753-775` переносится байт-в-байт с
   заменой цели tmp→dst — KeepEtag/tags/setObjMeta логируются и НЕ роняют копирование
   (сегодня так же); фатальные шаги — Create (маппинг `objectCommitErr`),
   CopyFileRange (`jfsToObjectErr`), `Stat(dst)` (`objectCommitErr`).
4. **BucketNotFound**: даётся существующим `checkBucket(dst)` `:709` до Create —
   семантика теста AfterBucketDeleted сохраняется без изменений в этой ветке.
5. **Блокировки**: остаются на уровне `CopyObject` (`:703`) — helper вызывается из
   CopyObject под локом; `TestObjectWriteSerialization` (кейс «copy» ждёт in-flight
   PUT на тот же ключ) остаётся зелёным БЕЗ правок — это регресс-гард сериализации.
6. **ObjectInfo**: блок `:796-807` остаётся в CopyObject, собирается из `fi` (Stat dst),
   etag, tagStr, `srcInfo.UserDefined` — helper возвращает (fi, etag, tagStr, err)
   либо ObjectInfo — по выбору coder'а, сигнатуры в этом документе не источник истины.

### D3. At-destination безусловно

Без веток «если не шифрованный — staging»: шлюз не знает, шифруется ли путь (решение
в `pkg/meta` при Create); прецедент — «Complete-at-destination безусловно» (Decision 3
add-gateway-platform-mode).

## Поток (CopyObject после :722)

```
fi, etag, tagStr, err := n.copyObjectAtDestination(ctx, src, srcBucket, srcObject, dst, dstBucket, dstObject, srcInfo)
if err != nil { return minio.ObjectInfo{}, err }
return minio.ObjectInfo{ ...блок :796-807 из fi/etag/tagStr... }, nil
```

copyObjectAtDestination: Stat(src) → Create(dst) со скелетом `:948-965` → deferred
rollback (close + Delete(dst) при любой ошибке после Create) → CopyFileRange(src→dst)
→ метаданные `:753-775` (цель dst) → Stat(dst). Порядок и close — образец
CompleteMultipartUpload `:1509-1546`.

## Risks

1. Атомарность перезаписи теряется (как у PutObject с 28.11); сбой перезаписи
   уничтожает прежний объект (Delete до повторного Create).
2. Аварийное завершение процесса между Create и rollback оставляет частичный файл
   по финальному пути; уборка tmp его не покрывает.
3. K-gate в unit-тестах не воспроизводим (memkv-харнесс без platform-whitelist) —
   отсутствие Create под `.sys/tmp` проверяется тестами листинга tmp, живая
   проверка — stage (rclone copy внутри remote).
4. Конкурентные писатели в один ключ: побеждает последний (nsMutex сериализует,
   содержимое не смешивается) — поведение как у PutObject.
5. Две копии скелета Create/rollback (дублирование с putObjectAtDestination) —
   принятая цена до follow-up-рефакторинга.
