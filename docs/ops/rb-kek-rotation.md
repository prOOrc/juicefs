# RB: ротация Company KEK (rb-kek-rotation)

**Сценарий:** плановая годовая ротация KEK компании (KMS `rotation_period=8760h`) или досрочная (компрометация — см. rb-incident-kek-compromise).

**Стороны:** YC KMS ключ `drive-kek-<company>`, platform CompanyKEKService (RAM-LRU, TTL `keymanager_kek_cache_ttl`), render-ноды (KEK в mlock-памяти, LRU 1 ч), reencrypt-воркер.

## Плановая ротация (KMS-версия)

1. Terraform: новый ключ создаётся **автоматически** как новая версия того же ключа (`rotation_period`). Проверить: `yc kms symmetric-key versions list --id <key-id>`.
2. `Decrypt` старых версий остаётся валиден (YC KMS хранит все версии) — существующие wrapped FEK/CEK продолжают разворачиваться без переписывания.
3. Проконтролировать, что platform подхватила текущую версию: `keymanager_kek_cache_entries` (Grafana) или рестарт `platform-api-authz-grpc` после ротации.
4. Re-wrap данных НЕ требуется (Wrap только над новыми FEK). Опционально — плановый re-wrap через `juicefs reencrypt --rotate-cek` (stage 8) в окно низкой нагрузки.

## Досрочная ротация KEK (подозрение на утечку) — см. rb-incident-kek-compromise

Отличие: перед шагами выше — отозвать доступ render-нод и выгнать KEK из кэшей (WipeKeys), rotation без старой версии (YC KMS primary version switch).

## Проверки после ротации

- Пользователь mount: чтение + запись файла компании — OK.
- Render-mount: чтение зашифрованного файла — OK (новый KEK подтягивается по FetchCompanyKEK).
- `keymanager_requests_total{result="error"}` — без роста.

## Метрики/алерты

`docs/ops/grafana-alerts-drive.yaml`: KeyManagerErrorRateHigh, KeyManagerGetFileFEKHighLatency.
