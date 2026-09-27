# Rollout plan — agio Drive encryption (task 10.7)

## Порядок: test → stage → prod (per company)

### Фаза R1 — stage (ВЫПОЛНЕНА 2026-09-27)

1. ✅ Миграции 000205–000207 применены (`migrate up` на rw-хост, 2026-09-27).
2. ✅ Деплой `platform-api-authz-grpc` обновлён до образа `feature-drive-v2-10658` (CI pipeline 10658 = aede63e5, зелёный); rollout успешен.
3. ✅ Scrape-аннотации :9091 наложены на деплой; Prometheus скрейпит (`up{pod=~"platform-api-authz-grpc-.*"}=1`); `/metrics` содержит `keymanager_kek_cache_entries` и `keymanager_reconciliation_mismatches` (gauge-и; counter/histogram материализуются под трафиком).
4. ✅ Алерты: все 8 правил из `grafana-alerts-drive.yaml` созданы в Grafana (folder `agio-drive`) через admin API (MCP-SA не имеет прав alert-rules writer — 403; использован локальный port-forward + admin creds, file-provisioning в чарте не заведён). Состояние — normal, ложных срабатываний нет.
5. Тест-компании tst-a/tst-b: provision + enable-encryption выполнены в 9.6b; перепроверить чтение/запись после рестартов при пилоте.

### Фаза R2 — prod, по одной компании

Предусловия каждой компании (чек-лист rb-enable-company):
- [ ] terraform: ключ `drive-kek-<company>` применён (`drive_crypto_companies`); **создание prod-ключей — только после подтверждения владельцем (стоимость/необратимость)**.
- [ ] `keymanager provision --company <id>` — OK, повтор — 0 операций.
- [ ] все клиенты компании на fork HEAD с шифрованием (slice-формат v2, STS).
- [ ] бэкапы Redis активны; restore-тест выполнен хотя бы раз на stage.
- [ ] gRPC-деплой со stage-10 кодом; метрики/алерты видны.
- Включение: `enable-encryption <meta-url>` → пилотное окно **72 ч** наблюдения (алерты молчат; anomaly-джобы не дают ложных срабатываний на нормальном профиле компании) → следующий заказчик.

## Гейты качества (tasks 10.7)

| Гейт | Статус 2026-09-27 |
|---|---|
| (б) конкурентный FR-TEST-25 после фикса go-redis | **PASS — закрыт** (261–278k ops/s, 0 round-trips, 0 зависаний; `tests/load/report-2026-09-27-linux.md`) |
| (а) FR-TEST-24 перезамер, go/no-go | **NO-GO** (медиана 18–27% > цели ≤10%); решение за product owner: принять DEVIATION / оптимизировать копии в fork / перемерить на prod-топологии (`report-2026-09-27-linux.md` §Decision request) |
| Финальный AC-прогон | после закрытия (а) |

## Rollback

- **Остановка новых зашифрованных файлов** = единственный механизм (зашифрованные файлы читаются всегда, пока жива иерархия ключей): `--encryption-enabled=false` на volume (или rollback деплоя клиентов) — новые записи идут legacy; существующие зашифрованные файлы остаются читаемы.
- Откат деплоя platform: предыдущий образ; миграции 000205–000207 обратимы (down-миграции; 000205 down восстанавливает plain-таблицу — integration-тест есть). Внимание: down 000205 на больших объёмах = блокировка таблицы (выполнять в окно).
- Полный анти-сценарий «расшифровать volume»: reencrypt не поддерживает decrypt-режим — это сознательно (не понижать безопасность данных по требованию одной компании).

## Что осталось до архивации change

1. Решение владельца по гейту 10.7(a) (см. выше) — блокер пилота.
2. Живой прогон `tests/security/redis-restore-test.sh` (создаёт тест-кластер YC — требуется подтверждение; удаление кластера после теста — тоже).
3. Пилотная компания + 72-часовое наблюдение.
4. Обновление клиентских образов (s3-gateway/mounts) до fork HEAD с метриками `juicefs_*` — алерты fork-группы до этого в NoData.
5. Финальный прогон AC на production-конфигурации → закрыть 10.7 → `opsx-archive`.
