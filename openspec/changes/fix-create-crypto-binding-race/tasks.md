# Tasks: fix-create-crypto-binding-race

## 1. Engine

- [ ] 1.1 `pkg/meta/redis_fek.go`: `SetFileCryptoIfUnbound(ctx, inode, c)`
  — клон txn из `SetFileCrypto`; после GET+parseAttr при
  `len(attr.WrappedFek) > 0 || attr.DriveFileID != ""` → `syscall.EEXIST`
  без записи и без `genLog`; иначе запись + `genLog("SETFILECRYPTO(...)")`
  как сейчас.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./pkg/meta/ -run SetFileCryptoIfUnbound`.
- [ ] 1.2 Юнит-тесты `pkg/meta/redis_fek_test.go`: первый вызов пишет
  биндинг; второй по тому же иноду → EEXIST и attr не изменился; обычный
  `SetFileCrypto` поверх биндинга по-прежнему перезаписывает (ротация).
  Проверка: `go test -run SetFileCrypto ./pkg/meta/`.
- [ ] 1.3 Расширить `fileCryptoSetter` (pkg/meta, интерфейс прокси) методом
  с fallback: движки без `SetFileCryptoIfUnbound` → unconditional +
  warning-лог (dbMeta/kvMeta не регрессируют).
  Проверка: `go build ./...`.

## 2. Proxy

- [ ] 2.1 `pkg/meta/grpc_server_fuse.go`: Create и Mknod — ветка
  провижининга через `SetFileCryptoIfUnbound`; `EEXIST` → существующая
  идемпотентная ветка (GetFileFEK стоящего биндинга; для легаси-overwrite —
  как в Stage 11; rollback только по O_EXCL-ветке).
  Проверка: `go test -count=1 ./pkg/meta/ -run 'TestCreate|TestMknod'`.
- [ ] 2.2 Тест `TestCreateRaceLoserServesStandingBinding` (grpc_server_fuse_test.go):
  мок-engine, где первый Create выставил биндинг между чтением attr и CAS —
  проигравший Create не вызывает `CreateFileKey` второй раз и отвечает
  успехом со стоящим биндингом.
  Проверка: `go test -run TestCreateRace ./pkg/meta/`.
- [ ] 2.3 Интеграционный тест `TestConcurrentCreateSingleBinding`
  (encrypt_integration_test.go, Redis DB): N горутин создают один путь
  одновременно → в мете ровно один `drive_file_id`, все ответы успех,
  slices первого писателя читаемы после завершения остальных.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./pkg/meta/ -run TestConcurrentCreateSingleBinding` (локальный Redis).

## Definition of Done

- `GOTOOLCHAIN=go1.26.5 go build ./... && go vet ./pkg/meta/`
- `go test -count=1 ./pkg/meta/` зелёный (внешне-сервисные тесты —
  известный локально падающий набор, не в счёт)
- Stage-приёмка не требуется (изменение уровня движка/прокси, покрыто
  интеграционным тестом); попадает в следующий proxy-образ.
