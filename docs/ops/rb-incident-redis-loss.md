# RB: инцидент — потеря Redis-метаданных (rb-incident-redis-loss)

**Триггеры:** недоступность/деградация кластера `juicefs-meta-*` (KeyManagerUnavailable + mount offline), потеря данных (сбой кластера без актуального бэкапа).

**Почему это критично для шифрования:** meta Redis хранит wrapped FEK/CEK (attr-crypto suffix, slice-records). Без Redis данные в S3 есть, но FEK-иерархия недоступна.

## Сценарий A — отказ кластера, бэкап есть

1. Зафиксировать время сбоя → выбрать последний READY-бэкап: `yc managed-redis backup list --cluster-id <id>`.
2. Restore: `tests/security/redis-restore-test.sh` в режиме проверки НЕ запускать (он создаёт тест-кластер для валидации) — выполнить прямой restore: `yc managed-redis cluster restore --backup-id <id> --name juicefs-meta-b-restored` (аддитивно).
3. Обновить meta-URL клиентов (deploy/env) на новый хост; render-ноды через перекат (mount = redis://new-host:6379/<db>).
4. RPO = бэкап-интервал YC (автобэкапы, retention 35 дней, task 10.2). Потерянные после бэкапа файлы: метаданных нет → S3-объекты-сироты чистит `gc`; зашифрованные сироты нечитаемы (нет FEK) — безопасны.
5. Валидация после restore — чтение зашифрованного файла (аналог restore-теста).

## Сценарий B — потеря бэкапов (RPO-инцидент)

Данные S3 остаются зашифрованными и НЕчитаемыми (FEK утрачен) — это fail-closed, не утечка. Recovery: пересоздать volume, файлы-сироты в S3 удалить по lifecycle/`gc` после инвентаризации.

## Онлайн-поведение клиентов (проверено stage 6/9)

- Отказ meta: клиент входит в offline-connected → (default 15 мин `--offline-timeout`) → disconnected (терминально до remount); новые Open — fail-closed EIO; записи — write journal (`pkg/vfs/write_journal.go`, 0600) с replay при reconnect.
- Алерты: KeyManagerUnavailable, JuiceFSOfflineEvents.

## Метрики

`juicefs_offline_events` (рост), `keymanager_requests_total{result="error"}` (KeyManager зависит от PG, не Redis — рост здесь говорит о параллельной деградации платформы).
