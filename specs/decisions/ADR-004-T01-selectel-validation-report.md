# ADR-004 / T0.1 — отчёт о live-валидации Selectel Object Storage

| Поле | Значение |
|---|---|
| Дата | 2026-10-30 |
| Гейт | ADR-004 §Гейты, T0.1 |
| Окружение | проект «agio platform» (прод-аккаунт, scratch-ресурсы `adr004-val-*`, удалены после прогона), пул ru-7, endpoint `s3.ru-7.storage.selcloud.ru`, terraform provider selectel ~> 8.6 (IAM API v1), aws-cli SigV4, форк juicefs agio-drive-v2 @ 41c6740b0 |
| Испытанные роли | `s3.bucket.user`, `s3.user`, `s3.admin` (service users + S3 credentials через `selectel_iam_serviceuser_v1` / `selectel_iam_s3_credentials_v1`) |

## Вердикты по пунктам матрицы

### (а) Prefix-скоуп политики: РАБОТАЕТ, но Principal — UUID пользователя, не условия

- **`Condition aws:username` / подстановка `${aws:username}` в Resource — НЕ работают**: политика
  с `StringEquals aws:username = <name>` прикладывается успешно, но allow никогда не срабатывает
  (пользователь получает AccessDenied даже на разрешённый префикс). Подстановка
  `arn:aws:s3:::bucket/${aws:username}/*` — аналогично.
- **Рабочий формат: `Principal: {"AWS": ["<uuid service user>"]}`** — голый UUID из IAM API
  (`selectel_iam_serviceuser_v1.id`). С ним object-операции на
  `arn:aws:s3:::<bucket>/<volume>/*` разрешаются, чужой префикс — `AccessDenied` (PutObject
  вне префикса, GetObject чужого объекта — 403).
- **`s3:prefix` в Condition на `s3:ListBucket` — работает**: `StringLike s3:prefix = <volume>/*`
  разрешает листинг с префиксом. Бонус сверх AWS-семантики: листинг **без** prefix-параметра
  возвращает только разрешённые записи (чужие ключи не видны) — сервер фильтрует выдачу.
- `ListAllMyBuckets` у `s3.bucket.user` — 403 (список бакетов проекта не виден).
- Политика проходит серверную валидацию при `PutBucketPolicy` (MalformedPolicy при
  несоответствии Resource имени бакета — валидатор строгий); Principal нормализуется к
  `{"AWS": ["*"]}` в хранимом виде.

### (б) Роли: `s3.bucket.user` — правильный выбор для ключей компаний

- `s3.bucket.user` без политики — **полный ноль** (Put/List — AccessDenied; документация
  подтверждена). С политикой — ровно её скоп.
- `s3.user` **видит ListAllMyBuckets всего проекта** (включая имена прод-бакетов) — информационная
  утечка; для ключей компаний не использовать.
- `s3.admin` имеет полный доступ на бакет **без политики**, но — см. (г).

### (в) Пустой SessionToken / статические креды в juicefs: РАБОТАЕТ

- `juicefs format --storage s3 --bucket https://s3.ru-7.storage.selcloud.ru/<bucket>
  --access-key … --secret-key …` против Selectel (path-style) — успех; объекты тома
  (`juicefs_uuid`, `chunks/0/0/...`) легли в префикс под static-кредами.
- Roundtrip 4 MiB через `juicefs gateway` (write+read+cmp) — OK. SessionToken не задействован
  нигде — эквивалент `SessionToken=""` из ADR-004 §4.

### (г) Управление политикой: s3.admin НЕ обходит default-deny

- `PutBucketPolicy` от `s3.admin` на бакете без политики — работает; `GetBucketPolicy` на чистом
  бакете — `NoSuchBucketPolicy` (читать политики админ может).
- **Критично: после применения default-deny политики `s3.admin` тоже теряет доступ** ко всем
  операциям, не разрешённым в политике (включая `GetBucketPolicy`/`PutBucketPolicy`/
  `DeleteBucket`). ** Management-statement для платформенного принципала обязан входить в
  самую первую версию политики** — иначе контейнер запирается необратимо (Swift-удаление тоже
  403; проверено).
- Итоговая форма политики v2: per-company statement'ы (UUID-principal, `<volume>/*` +
  ListBucket `s3:prefix`) + management-statement платформы (объектный + bucket-level `s3:*`).

### (д) Размеры/лимиты

- Рабочая политика (mgmt + per-volume object+list) ≈ 1 КБ; бюджет 20 КБ достаточен для единиц
  volume'ов и десятков компаний (UUID-principal компактнее условий).
- Лимиты числа service users/ключей и rate limits IAM API — не выяснены (нет в документации);
  запрос в support Selectel остаётся открытым пунктом.

### (е) Ревокация/ротация ключей: работает, есть лаг применения

- `DELETE credentials` (IAM API v1, через destroy `selectel_iam_s3_credentials_v1`): старая пара
  **сразу после подтверждения удаления продолжала работать**, через ≤ 45 с —
  `InvalidAccessKeyId` ("does not exist in our records"). Оценка лага применения — до минуты.
- Ротация: пересоздание credentials тому же service user → новая пара работает немедленно;
  политика по UUID не меняется (principal неизменен) — схема ADR-004 §5 подтверждена целиком.
- Клиентский путь при отзыве: записи через juicefs возвращаются за ~1 с без hang (dedup/volume
  кэш делает точный вердикт об ошибке неоднозначным — целевая проверка клиента с
  `stsRefresher`/`NewReloadableStorage` остаётся за T1.1, метрика `sts_refresh_failures`).

## Влияние на ADR-004 (корректировки)

1. §Решение 1/2: Principal в bucket policy = **UUID service user компании** (не username-условия,
   не `${aws:userid}`); условия username-класса не использовать вовсе.
2. §Решение 2: политика v2 = per-company UUID-statement'ы + management-statement платформы,
   входит в первый put целиком.
3. §Решение 6: SLA ревокации = kill-switch meta proxy (мгновенно) + `DELETE credentials`
   (эффект ≤ ~1 мин из-за лага Selectel); break-glass без изменений.
4. §Решение 3: роль ключей компаний — `s3.bucket.user` (не `s3.user` — информационная утечка
   списка бакетов проекта).
5. Гейт T0.1 считается **пройденным**; открытый пункт — лимиты IAM (support).

## Остатки и уборка

- Все scratch-пользователи/креды удалены (7 ресурсов destroy), тестовые контейнеры удалены.
- **Orphan**: контейнер `adr004-val-policy` заперт первой (тестовой) политикой с
  неработающими username-условиями; принципалы удалены — доступ отсутствует у всех. Требуется
  удаление через support Selectel. Урок зафиксирован в (г).
