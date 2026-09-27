# RB: ротация FEK файла (rb-fek-rotation)

**Сценарий:** замена FEK конкретного файла — отзыв доступа у пользователя (offboarding batch), подозрение на утечку файла, плановое требование.

**Инструмент:** `juicefs meta-proxy`-путь RPC `RotateFileKeysByPaths` (org-admin gated) или CLI платформы `keymanager rotate-user-keys --user <id> --company <code>`; единичный файл — `RotateFileKey`.

## Порядок

1. **Ограничение доступа СНАЧАЛА** (ротация не блокирует активные сессии мгновенно):
   - отозвать права пользователя в authz (SpiceDB/платформа) — deny за **~12–15 c** (замер 9.6b: S3 revoke; TTL decision-cache + InodePathCache);
   - если пользователь онлайн с FEK в LRU — FEK уйдёт при heartbeat-generation wipe: следующая `FlushSession` с увеличенным `permission_generation` → клиент очищает FEK-кэш; окно = интервал heartbeat (~12 c) **+ TTL FEK-LRU** (пользователь 15 мин, render 1 ч). До истечения окна пользователь сохраняет доступ через уже открытые хэндлы.
2. Ротация FEK: `RotateFileKey` (ядро: `GetFileFEK(old)` → platform `RotateFileFEK` → `SetFileCrypto(v+1)` → `RewrapSlices`). Частичный сбой RewrapSlices = файл смешанных версий — чинится `juicefs reencrypt` (stage 8) или Unlink.
3. Offboarding-пакет: `rotate-user-keys` (≤100 путей/вызов, 10 файлов/c, checkpoint возобновляем).

## Проверки

- Старый пользователь: открытие файла → deny (после истечения окна выше — гарантированно).
- Остальные пользователи/render: чтение файла OK (`GetFileFEK` по новой версии).
- Аудит: `drive_key_access_log` — операции `rotate_file_fek` с `admin_user_id`.

## Границы

- Активные хэндлы с расшифрованным контентом в памяти клиента НЕ отзываются (write journal уже записанного — тоже; см. disclosure SRS-001 T10).
- Offline-клиент с FEK в LRU: wipe произойдёт при reconnect (heartbeat).
