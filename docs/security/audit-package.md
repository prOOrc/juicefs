# Audit package — agio Drive encryption (SOC 2 / MPAA TPN)

Task 10.6, change `implement-encrypt`. Этот документ — вход для внешнего аудита:
границы trust, реализованная иерархия ключей, traceability «требование → код → тест»,
заявленные допущения.

## 1. Система в двух словах

Пользователи работают с файлами через Meta Proxy (gRPC, OIDC + authz); render-ноды —
нативные JuiceFS-FUSE клиенты с прямым доступом к Redis+S3, без идентичности, с KEK
в памяти. Все новые данные компании шифруются: FEK на файл (128-bit, CSPRNG) →
wrap под Company KEK (AGFK) → KEK wrap в YC KMS (`drive-kek-<company>`); CEK слайса
(wrapped FEK) — в slice-record; чанки — AES-256-GCM per-block (AGDF, nonce 96-bit
случайный, AAD = sliceID+blockIndex). Ciphertext живёт только в S3 и дисковом кэше.

## 2. Иерархия ключей (как реализовано)

| Слой | Что | Где живёт | Формат |
|---|---|---|---|
| L0 | YC KMS master key `drive-kek-<company>` (AES-256, rotation 8760h) | YC KMS, выдача material только через API | — |
| L1 | Company KEK (wrapped KMS) | Lockbox `drive/kek/<company>`; RAM-LRU в platform (TTL); render: mlock-память, LRU 1 ч | 32 байта |
| L2 | FEK файла, wrapped под KEK (AGFK: AES-GCM, nonce, AAD = volume/company/drive_file_id/inode/version) | Redis attr-crypto suffix (500-байтный лимит учтён), versioned | AGFK 69 B |
| L3 | CEK слайса = wrapped FEK (AGCK, AAD = drive_file_id+sliceID+version) | slice-record в Redis (tail) | AGCK 65 B |
| L4 | Block ciphertext | S3 object / disk cache | AGDF: magic+ver+nonce+GCM (1 MiB blocks) |

Rotation: L0/L1 планово (rb-kek-rotation), L2 по файлу (rb-fek-rotation), L3 —
reencrypt --rotate-cek (rb-cek-rotation). Decrypt старых версий L0 валиден всегда.

## 3. Trust boundaries (threat model, T-numbers SRS-001)

| # | Граница | Угроза | Контроли | Статус |
|---|---|---|---|---|
| T1 | Пользователь ↔ Meta Proxy | подмена identity, эскалация | OIDC (sub=UUID, strict interceptor), authz по путям (denay fail-closed), audit | closed |
| T2 | Meta Proxy ↔ Redis | прямой доступ к meta | сетевые ACL, креды per-volume; offline-клиенты не проходят через эту границу | closed |
| T3 | Render-node ↔ Redis/S3 | identity-less клиент | узкий контракт: только KEK-unwrap по company-prefix, кросс-компания EIO (FR-TEST-30/S29); IAM-токен ноды → STS | closed |
| T4 | Platform ↔ KMS/Lockbox | кража SA-кред | least-privilege IAM (iam-matrix), rotation SA | closed |
| T5 | Redis steal (полный) | wrapped-слои утекли | без KEK нечитаемо (S28); KEK не в Redis | closed |
| T6 | S3 bucket leak | ciphertext | без KEK/FEK нечитаемо (S27); имена slice-id раскрывают только топологию | closed |
| T7 | Redis+S3 leak | всё кроме KEK | нечитаемо без KEK (S30); KEK только в KMS/Lockbox/памяти | closed |
| T8 | **Render-node память** | KEK+FEK в RAM ноды | mlock (best-effort), WipeKeys, LRU 1 ч; но администратор/руты ноды — в границе доверия. Компенсирующие: аудит выдачи KEK (fetch_company_kek в `drive_key_access_log`), anomaly detection, revoke ноды (rb-revocation) | **ACCEPTED** (компенсирующие контроли) |
| T9 | **Пользовательская память** | расшифрованный контент у легитимного пользователя | неотзываемо по определению; компенсирующие: authz-deny ≤30 c (факт 12–15 c), FEK-wipe ≤12 c+TTL, FEK-ротация (≤60 мин на пакет) | **ACCEPTED** |
| T10 | **Write journal** (offline-режим) | plaintext payload на диске клиента (`pkg/vfs/write_journal.go`) | файл 0600, truncate при logout/replay; терминирование при disconnected; **граница принята** (SRS-001 v2.3 T10): journal пишет plaintext чтобы offline-записи пережили reconnect; компенсирующие: FDE клиента (вне системы), wipe при `_JFS_LOGOUT`, путь в tmp-домене пользователя | **ACCEPTED** |

Инсайдер-модель: org-admin может инициировать ротацию/чтение метаданных, но не
получает plaintext (FR-TEST-26/S26 — owner не читает чужие файлы компании).

## 4. Допущения

A1 — YC KMS/Lockbox соответствуют SOC 2 (наследуемые контроли провайдера).
A2 — платформа (PG, Kratos) администрируется ограниченным кругом; admin actions
аудируются платформой.
A3 — аудитор не требует HSM-уровня FIPS 140-2 L3: YC KMS — HSM-backed (декларация
провайдера), KEK material не покидает KMS в plaintext без авторизации.
A4 — render-ноды физически контролируются заказчиком (farm), образы — подписанные.
A5 — криптопримитивы: Go stdlib AES-256-GCM (AES-NI), CSPRNG crypto/rand; nonce
уникальность — случайный 96-bit per block (риск коллизии < 2^-32 при < 2^32 блоков
на ключ-версию; ротация FEK снижает).
A6 — identity-инвариант: OIDC sub ≡ platform user.id ≡ Kratos identity_id (UUID);
daily reconciliation (task 10.3) мониторит дрейф.

## 5. Traceability (SRS → код → тест)

Полная матрица FR/NFR → тестов: `tests/fr-test-coverage.md`, сводка статусов —
`tests/acceptance.md` (AC-1..16). Ключевые якоря:

| Требование | Реализация | Тест/пруф |
|---|---|---|
| FR-CRYPTO-* (форматы) | `pkg/agio` + `pkg/chunk/cek_encrypt.go`, `pkg/meta/fek_crypto.go` | known-answer vectors (`pkg/agio/testcrypto/vectors.json`, byte-identical в platform) |
| FR-KEY-* (иерархия, wrap) | platform `CompanyKEKService`, `KeyManagerService` | `key_manager_service_test.go`, integration stage-9 |
| FR-ACCESS (authz/deny) | authz interceptor + platform AuthzService | S26/S29, `TestInsiderCrossFileFEK_Denied` |
| FR-REV (отзыв) | heartbeat generation + RotateFileKey(s) | S3-замер 9.6b: 12–15 c; `TestRevoke_LosesFEK` |
| FR-OFFLINE (journal) | `pkg/vfs/write_journal.go` | TestWriteJournal, TestOfflineWrites_ReplayOnReconnect |
| FR-REDIS (бэкапы) | YC backups + restore-тест | `tests/security/redis-restore-test.sh` (task 10.2; прогон — после подтверждения создания кластера) |
| FR-AUDIT (≥12 мес) | `drive_key_access_log` monthly partitions + retention 24 мес (000205), append-only trigger | миграционный integration-тест (000205) |
| FR-SEC (anomaly) | cron `detect-key-access-anomalies` → `drive_security_events` | integration-тесты порога/render_node-исключения |
| R12 (reconciliation) | cron `reconcile-identities` → `drive_identity_reconciliation` | unit+integration тесты |
| NFR-AVAIL-1 (SLO 99.9%) | метрики `keymanager_*`, алерты | `docs/ops/grafana-alerts-drive.yaml` |
| NFR-SEC-12 (IAM) | `docs/ops/iam-matrix.md`, `drive_crypto.tf` | terraform change `drive-crypto-infra` (D.1–D.4 [x]) |
| FR-PERF (нагрузка) | `tests/load/` | `tests/load/report-2026-09-27-linux.md`: FR-TEST-25 concurrent PASS; FR-TEST-24 — DEVIATION pending decision (см. отчёт, §Decision request) |
| FR-REV-3 (STS) | `GetNodeSTSCredentials` (YC AssumeRole, ADR-003) | S4 re-run pass 2026-09-27 |

## 6. Заявленные отклонения

- AC-11 (деградация ≤10%): конкурентный замер на linux дал медиану ~18–27%
  (`tests/load/report-2026-09-27-linux.md`); цель SRS не ослаблена, решение —
  за product owner. Причина — копии буферов в enc-пути на in-memory микробенче;
  продакшн-топология (S3-bound) ожидаемо ниже.
- Stage-деплой новых метрик/миграций 000205–000207 на момент отчёта не выполнен
  (VPN-недоступность k8s API в сессии) — rollout.md фиксирует шаги.
