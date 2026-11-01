# Tasks: configure-sts-refresh-ttl

## 1. Refresher

- [x] 1.1 `cmd/sts_refresher.go`: поле `margin float64` (дефолт 0.5), вычисление
  интервала `(Expiry − now) × margin` с clamp `[5s, Expiry−5s]`; канал `retick`
  и пересоздание тикера в `loop()` после каждого свапа.
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 -run TestSTSRefresher ./cmd/`.
- [x] 1.2 `swap()` публикует новый интервал; при провале `refresh` интервал не
  меняется (тик прежним до истечения живой пары).
  Проверка: те же тесты + ручной прогон TestSTSRefresher_FailSafeUntilExpiry.
- [x] 1.3 Тесты: `TestSTSRefresher_IntervalFollowsDeliveredTTL` (6м TTL → тик 3м),
  `TestSTSRefresher_IntervalRecomputedAfterSwap` (смена TTL между доставками),
  `TestSTSRefresher_IntervalClampedBeforeExpiry` (тик строго до Expiry−5s),
  регресс `TestSTSRefresher_FailSafeUntilExpiry` без изменений семантики.
  Проверка: `-run 'TestSTSRefresher' -count=5` (стабильность, виртуальные часы).

## 2. Параметр и wiring

- [x] 2.1 `pkg/meta/grpc_client.go`: парсинг `sts-refresh-margin` / `sts_refresh_margin`
  (дефолт 0.5, значения вне (0,1] — ошибка парсинга meta URL).
  Проверка: `GOTOOLCHAIN=go1.26.5 go test -count=1 ./pkg/meta/ -run MetaURL`.
- [x] 2.2 `cmd/mount.go`: передача margin в `newSTSRefresher` (вместо
  `defaultSTSTTL`); `defaultSTSTTL` остаётся дефолтом до первой доставки.
  Проверка: `GOTOOLCHAIN=go1.26.5 go build ./... && go vet ./cmd/ ./pkg/meta/`.

## 3. Приёмка

- [ ] 3.1 Stage-проверка с `sts_ttl=6m`: маунт без EXPIRED-алармов, тики каждые
  ~3м, rotate доставляется следующим тиком.
  Проверка: лог маунта (два последовательных «STS credentials refreshed»
  с дельтой ≈ TTL×margin).

## Definition of Done

- `GOTOOLCHAIN=go1.26.5 go build ./... && go vet ./cmd/ ./pkg/meta/`
- `GOTOOLCHAIN=go1.26.5 go test -count=1 ./cmd/ ./pkg/meta/`
- Прод-конфигурация (sts_ttl=60m): тик станет 30м (сейчас 15м) — это и есть
  контрактная «половина TTL»; до истечения пары остаётся один запасной тик.
  Дельту задокументировать в change-логе.
- Render-путь (render_mount.go) передаёт дефолтную маржу; при текущем
  фиксированном render-TTL 30м тик = 15м — поведение идентично до-change.
  Параметричность включится автоматически, если провайдер станет доставлять
  иной TTL.
