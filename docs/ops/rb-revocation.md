# RB: отзыв доступа (rb-revocation)

**Сценарий:** увольнение/перевод пользователя, отзыв доступа к компании/файлу, экстренный logout устройства.

**Замеренные тайминги (stage-runbook 9.6b, 2026-09-27):**

| Слой | Механизм | Граница | Факт (9.6b) |
|---|---|---|---|
| Authz-deny (метаданные/открытие) | SpiceDB решение + TTL decision-cache (~12 c) + InodePathCache | ≤ 30 c | **~12–15 c** (S3-revoke замер) |
| FEK-кэш онлайн-клиента | heartbeat `FlushSession` → `permission_generation` increase → wipe FEK | ≤ 12 c + TTL FEK-LRU (user 15 мин / render 1 ч) | heartbeat ~12 c подтверждён |
| Полный доступ к контенту | после истечения окна FEK-LRU + закрытия хэндлов | ≤ 15 мин (user) / ≤ 1 ч (render) | — |
| Крипто-отзыв (файл нечитаем даже с копией wrapped FEK) | FEK/CEK-ротация (rb-fek-rotation / rb-cek-rotation) | ≤ 60 мин на пакет (rate 10 файлов/c) | — |

## Порядок (offboarding пользователя)

1. Deactivate identity (Kratos `active=false`) + revoke прав (authz).
2. Дождаться authz-deny (≤30 c, факт 12–15 c).
3. Инициировать FEK-ротацию файлов пользователя: `keymanager rotate-user-keys --user <id> --company <code>` (пакет ≤100 путей/вызов, checkpoint возобновляем).
4. При экстренном logout устройства: `_JFS_LOGOUT` в volume → VFS poll (1 c) → wipe кэшей, write journal truncate, hub → disconnected (терминально до remount).

## Проверки

- Пользователь: `Open` → deny; после окна TTL — чтение невозможно (FEK нет ни в кэше, ни у KeyManager).
- Аудит: `keymanager_requests_total{result="deny"}` рост; `drive_key_access_log` — deny записи.
- Алерты: KeyManagerDenyRateSpike (шум при массовом offboarding — ожидаем).

## Ограничения (disclosure)

- Уже прочитанный контент отозвать нельзя (T9/T10 границы, см. audit-package).
- Offline-клиент с валидным FEK в LRU сохраняет чтение до TTL/Terms — сминимизировано терминальным disconnected-состоянием.
