# Design: fix-create-crypto-binding-race

## Context

Гонка описана в proposal.md; точка входа — `redisMeta.SetFileCrypto`
(pkg/meta/redis_fek.go:45-77): единый txn `m.txn` (WATCH ключа инода → GET
attr → mutate → SET) уже сериализует конкурентов; проблема в том, что мутация
безусловна.

## Goals / Non-Goals

- Goal: биндинг при Create/Mknod выдаётся ровно один раз; проигравший
  конкурент переиспользует стоящий биндинг; AAD уже закоммиченных slices
  никогда не остаётся без своего `drive_file_id` в атрибуте.
- Non-Goal: общий compare-and-swap на произвольные поля attr; переработка
  путей ротации/клона; клиентские изменения.

## Decisions

### D1. `SetFileCryptoIfUnbound` — opt-in CAS поверх существующего txn

Новый метод `redisMeta` в том же файле: идентичный `m.txn`, но после
`GET`+`parseAttr` — если `len(attr.WrappedFek) > 0 || attr.DriveFileID != ""`
→ вернуть `syscall.EEXIST` БЕЗ записи (txn откатывается, `genLog` не пишется).
Иначе — запись как в `SetFileCrypto`. Отдельный метод, а не флаг: так
безусловная перезапись остаётся явной в call sites ротации/клона (revirovka
биндинга там осознанна), и сигнатура `SetFileCrypto` не меняется.

### D2. Встраивание: только ветка провижининга Create/Mknod

`grpc_server_fuse.go`: ветка «биндинга нет» (новый файл или легаси) вызывает
`SetFileCryptoIfUnbound`. `EEXIST` → немедленный переход в существующую
идемпотентную ветку Stage 11 (не перевыпускать, `GetFileFEK` стоящего
биндинга, best-effort доставка). Ветви `fileCryptoSetter`-интерфейса не
меняются: у прокси уже есть type-assertion на интерфейс — добавляем метод в
тот же интерфейс (`fileCryptoSetter` расширяется; fallback для движков без
метода — прежний unconditional + warning, поведение как сегодня).

Порядок «сначала CAS, потом fetch FEK» сохраняет fr-USR-2 rollback только
для genuinely-нового инода (O_EXCL): если CAS выигран, но доставка FEK
упала — rollback как в Stage 11; если CAS проигран — rollback невозможен по
определению (инод существовал).

### D3. Неподчекнутые пути остаются на `SetFileCrypto`

`RotateFileFEK`/`RotateFileKeysByPaths` (версия+1 — осознанная перезапись),
Clone/CopyFileRange dst (свежий инод, но пишем безусловным методом — инод
только что создан прокси-хендлером), render-Create. Их семантика не
меняется — это и есть проверка «guard не мешает легитимной перезаписи».

## Risks / Trade-offs

- [Движки без нового метода в `fileCryptoSetter`] → type-assertion
  с fallback на unconditional + warning-лог; целевой бэкенд Redis
  (спека domain-encrypt), dbMeta/kvMeta не регрессируют.
- [EEXIST как сентинель] → коллизии по смыслу нет: в ветке провижининга
  EEXIST от самого `Create` уже обработан раньше; сентинель читается
  только после успешного Create.
- [Смешанные версии прокси за одним Redis] → старый прокси сохраняет гонку
  (известное ограничение B2); новый — устраняет. Клиенты менять не нужны.

## Migration Plan

Деплой прокси; миграций и клиентов не требует. Откат — предыдущий образ
(гонка возвращается, данных не теряет: перебиндинг уже был до Stage 11 и
после).

## Open Questions

- Нет (подход утверждён fix-review'ом Stage 11 как будущий guard).
