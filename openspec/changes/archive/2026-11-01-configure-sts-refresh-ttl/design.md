# Design: configure-sts-refresh-ttl

## Context

`cmd/sts_refresher.go`: тикер создаётся один раз в `loop()` как
`r.newTicker(r.ttl / 2)`, `ttl` — константа `defaultSTSTTL` (30m), передаётся
из `cmd/mount.go` (`newSTSRefresher(provider, holder, defaultSTSTTL)`). Факт
доставки: платформа кладёт синтетический `ExpirationUnix = now + sts_ttl`
(конфиг платформы, прод 60m, stage-приёмка 6m) — см. proposal.md / Why.
Fail-safe ветки (`refresh` → `checkExpiry` → `logExpired`, метрика
`sts_refresh_failures`) и fail-fast `Start` уже покрыты тестами и не меняются.

## Goals / Non-Goals

**Goals**
- Тик = `(Expiry − момент доставки) × margin`, clamp `[5s, Expiry−5s]`;
  перевычисление после каждого свапа.
- `sts-refresh-margin` — опциональный параметр meta URL (парсинг в
  `grpc_client.go` рядом с остальными `sts-*`/oidc-параметрами), дефолт 0.5.
- Ноль изменений контракта с agio-platform и fail-safe семантики.

**Non-Goals**
- Серверные изменения (TTL остаётся конфигом платформы).
- Push-модель доставки (webhook на ротацию) — poll остаётся механизмом
  доставки (ADR-004).
- Изменение `GetSTSCredentials` RPC.

## Architectural Decisions

- **D1: интервал из факта, не из конфига.** Единственный источник истины о
  TTL — ответ платформы (`stsCredentials.Expiry`). Конфиг-маржа — только
  коэффициент. Это устраняет класс расхождений «конфиги разъехались».
- **D2: ticker пересоздаётся в `loop()` после каждого свапа.** `loop()` держит
  канал `retick chan time.Duration`; `swap()` публикует новый интервал; `loop`
  останавливает старый `time.Ticker` и создаёт новый (защита от дрейфа —
  сброс фазы тика при каждой доставке приемлем: доставка и есть событие
  синхронизации). Альтернатива (`time.AfterFunc` на каждый цикл) отклонена —
  тот же эффект при более запутанном владении таймером.
- **D3: clamp `[5s, Expiry−5s]`** — нижняя граница против флуда при вырожденно
  малом TTL (misconfigured platform), верхняя гарантирует тик строго до
  истечения (строгий `Before` в `valid()`).
- **D4: `sts-refresh-margin` парсится в meta URL**, а не флагом mount:
  параметр привязан к конкретному meta-подключению (как oidc-*), попадает в
  те же команды маунта, не расширяет поверхность флагов.

## Component Map

| Компонент | Файл | Изменение |
|---|---|---|
| `stsRefresher` | `cmd/sts_refresher.go` | поле `margin float64`; `loop` — пересоздание тикера по каналу `retick`; `swap` — publish `(Expiry−now)×margin`; clamp |
| meta URL params | `pkg/meta/grpc_client.go` | парсинг `sts-refresh-margin` (дефолт 0.5, clamp (0,1]), передача в mount |
| mount wiring | `cmd/mount.go` | `newSTSRefresher(..., margin)` вместо `defaultSTSTTL` |
| тесты | `cmd/sts_refresher_test.go` | интервал-кейсы (виртуальные часы `ref.now` уже инъецируются) |

## Execution Flow

1. `Start` → первая доставка → `swap` публикует интервал → `loop` тикает.
2. Тик → `refresh` → успех → `swap` → publish нового интервала; провал →
   `checkExpiry` (без смены интервала — тикаем прежним, чтобы успеть до
   истечения живой пары).
3. Истечение без успеха → `logExpired` один раз (как сейчас).

## Invariants

- Fail-safe контракт (spec drive-sts-client) не нарушается: интервал никогда
  не превышает `Expiry−5s`, поэтому при работающей платформе аларм
  `EXPIRED` недостижим.
- `sts_refresh_failures` и однократность аларма — без изменений.

## Integration Points

- agio-platform `sts_ttl` (значение попадает в `ExpirationUnix`) — без изменений.
- ADR-004 клиентский контракт: poll на половине TTL — теперь буквальный.

## Open Questions

- Нужен ли нижний clamp 5s конфигурируемым — решено «нет» (YAGNI, misconfig
  платформы виден в метриках).
