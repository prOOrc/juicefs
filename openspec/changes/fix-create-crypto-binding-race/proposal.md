# Proposal: fix-create-crypto-binding-race

## Why

Fix-review Stage 11 (implement-encrypt, design addendum B2 «Известное
ограничение») зафиксировал гонку: два конкурентных `Create` одного пути
(O_CREAT без O_EXCL) оба читают attr до чужого `SetFileCrypto`, оба видят
«биндинга нет» и оба вызывают `CreateFileKey` + `SetFileCrypto`. Проигравший
молча перебинди́т `drive_file_id` — AGCK уже закоммиченных slices победителя
разворачиваются со старым AAD (`drive_file_id‖slice_id‖fek_version`) и файл
становится нечитаемым (EIO). Класс предсуществующий (до Stage 11 перебиндинг
происходил на каждом overwrite), Stage 11 сузил окно до ветки
провижининга; владелец утвердил отдельный фикс (2026-11-01).

## What Changes

- Engine (`pkg/meta/redis_fek.go`): новый метод `SetFileCryptoIfUnbound` —
  тот же единый Redis-txn (GET attr → mutate → SET), но запись выполняется
  ТОЛЬКО если у инода ещё нет крипто-биндинга; при существующем биндинге
  txn завершается без записи с `EEXIST`. Обычный `SetFileCrypto` остаётся
  без изменений (ротация FEK легитимно перезаписывает биндинг).
- Proxy (`pkg/meta/grpc_server_fuse.go`): ветка провижининга в `Create` и
  `Mknod` переходит на `SetFileCryptoIfUnbound`; `EEXIST` (биндинг уже
  выставил конкурентный создатель) → существующая идемпотентная ветка
  Stage 11 — FEK стоящего биндинга через `GetFileFEK`, без перевыпуска.
- Не затрагивается: `RotateFileFEK`, Clone/CopyFileRange dst-биндинг,
  render-Create (там перезапись биндинга осознана или невозможна).

## Capabilities

### Modified Capabilities

- `domain-encrypt` (modified): ADDED-требование «Create-time binding
  allocated exactly once» — CAS-семантика провижининга биндинга при Create.
  Капабилити сейчас живёт дельтой открытого implement-encrypt; дельта
  этого change'а добавляет отдельное требование (бесконфликтный мерж при
  архивации обоих).

## Related Requirements

- implement-encrypt design.md, Addendum 2026-11-01, B2 и блок «Известное
  ограничение (fix-review Stage 11)» (гонка + будущий guard — CAS по
  `WrappedFek` в `SetFileCrypto`).
- SoT (после архивации implement-encrypt): domain-encrypt «User path —
  file creation» (идемпотентность overwrite), «Write fail-closed without
  FEK» (страховка от тихого plaintext сохраняется).

## Impact

- `pkg/meta/redis_fek.go` (+ метод), `pkg/meta/grpc_server_fuse.go`
  (переключение двух call sites), тесты `pkg/meta/*_test.go` +
  интеграционный гонку-тест на Redis.
- Без изменений proto/wire; mixed-version безопасен по обоим направлениям
  (старый прокси сохраняет гонку — известное ограничение; новый прокси
  со старым клиентом — без изменений).
