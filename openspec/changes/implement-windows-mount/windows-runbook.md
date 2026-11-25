# Windows Runbook: прогоны 2.1 / 4.1 / 4.2 на отдельной Windows-машине

Дата редакции: 2026-11-25. Аудитория: агент/инженер на **автономной Windows-машине**
(без доступа к внутренним базам знаний проекта — весь контекст здесь). Цель — закрыть
три открытые задачи change `implement-windows-mount` и поставить вердикт 4.2:

| Задача | Что делаем |
|---|---|
| **2.1** | Первый интерактивный OIDC-вход: пустой токен-кеш → маунт → браузер → логин → том читается |
| **4.1** | E2E зашифрованного тома с STS: запись/чтение/md5-roundtrip, wipe по `_JFS_LOGOUT`, повторный вход |
| **4.2** | Вердикт прогона (PASS / BLOCKED со списком дефектов), запись в tasks.md + test-report.md |

Это — **ретест**: 2026-10-04 на YC VM jfs-win10 (Win10 22H2) прогоны были заблокированы
двумя дефектами кодовой базы (**D1**, **D2** — см. §8). С тех пор дерево пересобрано
rebase'ом `41c6740b0` (2026-10-29, ~250 upstream-коммитов, включая windows-фиксы
#7693/#7696/#7697/#7707/#7756/#7768), но `replace go-redis` не менялся — живы ли D1/D2
на текущем дереве, **неизвестно, и ответ на это — часть прогона**.

---

## 1. Что за система (минимальный контекст)

**agio Drive v2** — облачный диск компании AGIO. Клиентское монтирование построено на
**форке JuiceFS** (`prOOrc/juicefs`, ветка `implement-windows-mount` — вы её соберёте).
Форк дополнен: gRPC-мета-клиент (`grpc://` вместо redis://), OIDC-аутентификация
браузером с PKCE (библиотека oidc-login, токен-кеш на диске), per-company STS-креды
данных (`--sts-enabled --company-id <uuid>`), шифрование томов (FEK выдаёт сервер).

**Серверная часть** (вам не нужна, только сетевые эндпоинты): stage-окружение на
VK Cloud. Зашифрованный тестовый том `tst-enc-vk`; его meta-proxy — gRPC-сервис за
TLS (сертификат Let's Encrypt, публичный CA — дополнительных настроек CA не нужно);
авторизация — OIDC через Hydra stage; data-plane креды — STS per-company (ADR-004:
под «STS» — статическая per-company пара за тем же RPC; клиенту без разницы).

При маунте с `--sts-enabled` сервер отдаёт рабочую директорию компании — том **chroot'ится
в `companies/home-gri-llo`** (это не баг: пользователь видит только свою компанию).

## 2. Константы прогона (актуальная VK-stage-топология, live с 2026-11-18)

```
# Основной маунт (зашифрованный том, TLS, OIDC, STS)
grpc://drive-meta-proxy-tst-enc-vk.agio.services:443/tst-enc-vk?tls=1&oidc-issuer=https://platform-stage.agio.services/.ory/hydra/public&oidc-client-id=juicefs-drive-stage&oidc-redirect-url=http://127.0.0.1:18080/callback&oidc-scopes=openid,offline_access,profile,email

# Bare-том (для отказного смоука «--sts-enabled требует encrypted volume»)
grpc://drive-meta-proxy-stage.agio.services:443/juicefs-b-stage?tls=1&oidc-issuer=...&oidc-client-id=juicefs-drive-stage&oidc-redirect-url=...&oidc-scopes=...   (те же oidc-* значения)

--company-id 0f7afc02-718c-479e-9f57-e0cb8d1e60bc     # компания Home пользователя gri-lom
```

- **OIDC-клиент**: `juicefs-drive-stage` (литеральный client_id, public+PKCE, redirect
  `127.0.0.1:8080`/`:18080` — берём **18080**; порт должен совпасть с `oidc-redirect-url`).
- **Идентичность для входа**: `gril-lom@yandex.ru`. **Пароль агенту не передаётся и в
  логи не пишется — браузерный логин выполняет владелец лично** (§9). Маунт сам откроет
  браузер в интерактивной сессии.
- **Том в URL обязателен в path** (`/tst-enc-vk`) и **порт обязателен** (`:443`) —
  grpc-диалер не выводит ни того, ни другого.
- Буквы дисков: `Z:` для основного, `X:`/`Y:` для вспомогательных.
- Токен-кеш: `%USERPROFILE%\.oidc-login` (дефолт библиотеки; параметр `oidc-cache-dir`
  можно не задавать).
- Метрики живого маунта: `http://127.0.0.1:9567/metrics`.
- Проверка discovery до старта: `curl https://platform-stage.agio.services/.ory/hydra/public/.well-known/openid-configuration` → 200 JSON.
- Проверка proxy до старта: `Test-NetConnection drive-meta-proxy-tst-enc-vk.agio.services -Port 443` → TcpTestSucceeded=True.

## 3. Требования к машине

1. **НЕ свежий Windows 11 build 26200** — на нём найден отдельный блокер среды:
   WinFsp монтирует тома без ошибок, но не обслуживает I/O ни в одной logon-сессии
   (воспроизводился на WinFsp 2.0.23075 / 2.1.25156 / 2.2B4; см. tasks.md 4.1 статус
   2026-10-03). Годятся: Win10 22H2 (19045 — эталон прошлого прогона), Win11 других
   билдов, windows-2022-подобные образы. Критерий «среда здорова» ставит §6 (L1).
2. **Интерактивная консольная сессия с браузером** (для 2.1). SSH-сессия НЕ годится
   для первого входа: без браузера/pty маунт завершится по 5-минутному таймауту
   (`oidc: interactive authentication timed out after 5m0s`) — это нормальное
   fail-fast поведение, не поломка.
3. ~15 ГБ свободного места (Go-сборке нужно ~10 ГБ), интернет до `*.agio.services:443`,
   `github.com`, `hackage`-зеркала Go-модулей.
4. Права администратора (установка WinFsp/Go/mingw, registry для сервисного пути).

## 4. Установка среды

| Компонент | Версия | Как |
|---|---|---|
| WinFsp | **2.1.25156** MSI (эталон здорового прогона 2026-10-04) | `winfsp-2.1.25156.msi /quiet /norestart` с github.com/winfsp/winfsp/releases. Замечание: код форка при версии < 2.1.25219 тихо отключает flush-on-cleanup, < 2.1.26036 — AddWriteEaAccess (`pkg/winfsp/winfs.go:54,57`) — Warning в логе это ожидаемо, не ошибка |
| Go | **1.26.x** (go.mod требует 1.26.5) | официальный установщик |
| MinGW-w64 | mingw-builds **14.2.0-rt_v12-rev1** (niXman) или w64devkit | распаковать в `C:\mingw64` (или `C:\w64`), добавить в PATH |
| git | любой свежий | для клонирования |

Код форка использует winfsp-заголовки из репо: `hack\winfsp_headers\` (4 .h-файла).
Скоприруйте их так же, как CI (`.github/workflows/wintest.yml`):

```powershell
mkdir C:\WinFsp\inc\fuse -Force
copy hack\winfsp_headers\* C:\WinFsp\inc\fuse\
```

## 5. Сборка бинаря (нативно на Windows)

```powershell
git clone -b implement-windows-mount https://github.com/prOOrc/juicefs.git C:\jfs\src
cd C:\jfs\src
$env:CGO_ENABLED=1; $env:PATH += ";C:\mingw64\bin"
$env:CGO_CFLAGS='-IC:/WinFsp/inc/fuse'
go build -ldflags "-s -w" -o C:\jfs\juicefs.exe .
go version -m C:\jfs\juicefs.exe > C:\jfs\binary-version.txt   # зафиксировать коммит/зависимости
```

**Что должно получиться**: `C:\jfs\juicefs.exe mount --help` показывает флаги
`--sts-enabled` и `--company-id` (Windows-паритет из задачи 1.1 — уже был подтверждён,
это контроль сборки). Если флагов нет — собралась не та ветка.

**Про выбор дерева** (обновлено 25.11 после rebase от `agio-drive-v2`): ветка
`implement-windows-mount` ребейзнута на tip `agio-drive-v2` (`31355c614`) — 8
коммитов change'а поверх текущей дев-линии форка (STS-флаги, 5m-фикс
OIDC-таймаута, owner-override forwarding, unit-тест service-cmdline, docs+этот
ранбук). Серверная ревизия meta-proxy `37163e7e9` — **предок** этого дерева
(клиент = потомок сервера — безопасное направление совместимости протокола);
первый успешный NewSession в §6-L3 остаётся контрольной точкой. Верификация
rebase: range-diff 8/8 коммитов идентичен (только новые sha), PE32+ бинарь и
winfsp-тест-бинарь кросс-собраны на macOS (mingw-w64; на arm64-хосте обязателен
`GOARCH=amd64`, `CGO_CFLAGS` — абсолютный путь к заголовкам).

## 6. Лестница смоуков — сначала среда, потом E2E

### L1 — среда + FUSE-слой нашего дерева (без сети, без OIDC)

Локальный sqlite-мета (go-redis не участвует — D1 сюда не относится):

```powershell
C:\jfs\juicefs.exe format sqlite3://C:/jfs/meta.db winvol
C:\jfs\juicefs.exe mount sqlite3://C:/jfs/meta.db X:
mkdir X:\t1; "hello" > X:\t1\a.txt; type X:\t1\a.txt        # должно быть hello
C:\jfs\juicefs.exe umount X:
```

- **PASS** → среда и FUSE-слой дерева здоровы (значит, блокер itx.lan здесь не
  воспроизводится). Идите в L2.
- **FAIL (том монтируется, I/O не работает, буква не материализуется)** → у вас
  воспроизведён **средовой** блокер itx.lan-типа. Дальше не идите: вердикт «blocked
  (environment)», зафиксируйте ОС-билд, версию WinFsp, `fsptool lsvol` — это ценные
  данные (см. §8, гигиена).
- Контроль при сомнении: release-бинарь `juicefs-1.4.1-windows-amd64` с GitHub
  (vanilla, без agio-правок) — тот же sqlite-маунт. Если vanilla работает, а наш —
  нет, это код-дефект, не среда.

### L2 — отказные смоуки STS (fail-fast, без маунта)

```powershell
# (а) не-grpc мета + --sts-enabled → мгновенная именованная ошибка, сети не нужно
C:\jfs\juicefs.exe mount redis://127.0.0.1:6379/1 Y: --sts-enabled
# ожидаемо: FATAL ... --sts-enabled requires a meta proxy (grpc) endpoint
```

Второй негатив (уже с токеном после 2.1, см. §7): maунт **bare**-тома с
`--sts-enabled` → ожидаемо `FATAL ... --sts-enabled requires an encrypted volume`
(проверено на redis-мете дважды в октябре; на grpc-мете — репетируем впервые, факт
зафиксировать дословно).

### L3 — основной маунт (задача 2.1)

```powershell
# кеш обязан быть пуст для первого входа:
if (Test-Path $env:USERPROFILE\.oidc-login) { Write-Error "cache not empty"; }
C:\jfs\juicefs.exe mount "grpc://drive-meta-proxy-tst-enc-vk.agio.services:443/tst-enc-vk?tls=1&oidc-issuer=https://platform-stage.agio.services/.ory/hydra/public&oidc-client-id=juicefs-drive-stage&oidc-redirect-url=http://127.0.0.1:18080/callback&oidc-scopes=openid,offline_access,profile,email" Z: --sts-enabled --company-id 0f7afc02-718c-479e-9f57-e0cb8d1e60bc --cache-dir C:\jfs\cache > C:\jfs\mount21.log 2>&1
```

Запускать **в интерактивной консоли** (не через SSH-редирект): маунт сам откроет
браузер на страницу Hydra; **владелец логинится `gril-lom@yandex.ru` лично** (пароль
агенту не сообщается). После логина callback на `127.0.0.1:18080`, маунт завершает
инициализацию.

**Критерии успеха 2.1** (все зафиксировать с таймингами в лог):
1. Браузер открылся автоматически; логин завершён; маунт прошёл без FATAL.
2. В логе: `NewSession sid=…`, `STS credentials refreshed (expiry …)`, рабочая директория
   `companies/home-gri-llo` (chroot).
3. `Get-ChildItem Z:\` работает; виден каталог `companies/` (внутри — `home-gri-llo`);
   **чужие компании не видны** (авторизационная фильтрация сервером).
4. Токен-кеш создан: `%USERPROFILE%\.oidc-login\<хеш>` существует.

Повторный маунт из кеша (бонус, дешёво — это по сути 2.2): umount → mount той же
командой → браузер НЕ открывается, маунт готов за секунды.

### 4.1 — E2E файловых операций (выполняется только если L3 прошёл)

```powershell
# в корне тома видна companies/home-gri-llo; работаем внутри неё
$f = "Z:\companies\home-gri-llo\win-e2e-$(Get-Date -Format yyyyMMdd-HHmmss).bin"
$src = "C:\jfs\test16m.bin"
# 16 MiB случайных данных у себя на диске:
$b = New-Object byte[] (16MB); (New-Object Random).NextBytes($b); [IO.File]::WriteAllBytes($src,$b)
$md5src = (Get-FileHash $src -Algorithm MD5).Hash
Copy-Item $src $f                       # запись через том (шифрование на сервере)
$md5dst = (Get-FileHash $f -Algorithm MD5).Hash         # чтение №1 (расшифрование)
$md5dst2 = (Get-FileHash $f -Algorithm MD5).Hash        # чтение №2
$md5dst3 = (Get-FileHash $f -Algorithm MD5).Hash        # чтение №3
"$md5src / $md5dst / $md5dst2 / $md5dst3"               # все четыре должны совпасть
Remove-Item $f                           # чистка за собой (чанки в S3 подтверждать не нужно)
```

**Wipe по logout + повторный вход** (вторая половина 4.1):

```powershell
"" | Set-Content Z:\_JFS_LOGOUT -NoNewline   # sentinel: маунт стирает ключи из RAM и завершает сессию
# маунт-процесс должен завершиться; перезапустить той же командой маунта:
#   токен-кеш жив (браузер НЕ нужен), но STS/FEK переизвлекаются → том снова читается
```

Критерии: md5 совпадает во всех чтениях (шифрование прозрачно), `_JFS_LOGOUT` корректно
гасит маунт, повторный вход из кеша восстанавливает том, данные после всего цикла
на месте.

## 7. Если что-то идёт не так — диагностика (это РАВНОЦЕННЫЙ результат прогона)

### Маркеры дефекта D2 (grpc-маунт: метаданные живы, файловый I/O мёртв)

- `Get-ChildItem Z:\` → **`ERROR_IO_DEVICE`** с периодом ~1 c (это `VolumeInfoTimeout=1000`
  из `pkg/winfsp/winfs.go:1116-1119`, не рандом).
- В stdout маунта строка **`The service <exe> has been started.`** — маркер ухода в
  service-путь cgofuse/WinFsp (у vanilla-сборки его нет).
- При этом в логе маунта: `NewSession`, `STS credentials refreshed` — метаданные живут.

Если видите эту картину — **D2 жив на текущем дереве** (upstream-фиксы rebase 41c6740b0
не помогли). Зафиксировать: полный mount-лог (`--fuse-trace-log C:\jfs\fuse.log`),
версию бинаря (`go version -m`), WinFsp-версию, ОС-билд. Бисект — отдельное кодовое
решение, не в этом прогоне.

### Контрольный опыт для D2 (если есть время, ~15 минут)

Локальный grpc-proxy на этой же машине: October-протокол — на здоровом redis это
не работало из-за D1, поэтому сейчас проще так: поднять прокси НЕЛЬЗЯ без redis
(D1), вместо этого достаточно L1 (sqlite — FUSE жив) + L3 (grpc — I/O мёртв):
эта пара сама по себе локализует дефект в grpc-пути нашего дерева. Записать.

### D1 (go-redis fork) — затрагивает ли ваш прогон

**Нет**: основной путь — `grpc://`, go-redis не участвует. D1 бьёт только redis://
маунты и локальные meta-proxy сборки на Windows (replace `go.mod:374` —
`prOOrc/go-redis v9.18.1`, не починен до сих пор, проверено по дереву 2026-11-25).
Фиксировать его статус в этом прогоне не нужно.

### Гигиена между попытками (ложноположительные пробы — главная ловушка)

1. Убивать процессы по имени: `Get-Process juicefs* | Stop-Process -Force`.
2. Удалять папку/букву монтирования: после смерти маунта точка может остаться
   **обычной NTFS-папкой** — все «операции с томом» тогда тихо идут на NTFS и всё
   «работает» ложно.
3. Проверять реальные тома: `fsptool lsvol` (том без буквы = живой маунт).
4. Буквы дисков в WinFsp **per-logon-session**: из другой сессии `Z:` не виден
   (`net use` к чужому тому = 67/64) — это платформенное ограничение, не дефект.
5. Живой маунт непрерывно ротирует refresh_token: копии токен-кеша с машины на
   машину делают одноразовыми; токен-кеш жить на той машине, где логин.
6. Первый вход после сброса кеша может занять до ~10 минут на пересборке login-цепочки
   (AuthenticationTimeout); в неинтерактивной сессии — ровно 300 s и `FATAL
   Unauthenticated` (контракт «SHALL NOT hang», задача 2.3 — уже верифицирован дважды).

## 8. История вопроса (почему прогон выглядит так)

- **2026-10-03, itx.lan** (Win11 26200, dual-boot, SSH+консоль): WinFsp 2.0/2.1/2.2B4 —
  маунты стартуют, «STS credentials refreshed», registry-конфигурация сервисов
  корректна, но буква не материализуется ни в одной сессии, net use = 67/64/85.
  Вердикт: блокер **среды** (WinFsp × свежий Win11), не change'а.
- **2026-10-04, YC VM jfs-win10** (Win10 22H2 19045, чистая MSI WinFsp 2.1.25156):
  среда здорова — но найдены 2 дефекта кодовой базы:
  - **D1**: fork `prOOrc/go-redis v9.18.1` (replace в go.mod) под Windows: PING виснет
    до таймаута (upstream v9.18.0 — 10 ms PONG). Валит redis-мета-пути и локальный
    meta-proxy под Windows. **Не починен по сей день** (replace не менялся).
  - **D2**: grpc-маунт зашифрованного тома: NewSession/STS проходят, файловые операции —
    `ERROR_IO_DEVICE`; только в agio-дереве (vanilla v1.4.1 тем же тулчейном на той же
    машине работает). Бисект не проводился.
- **2026-10-29, rebase `41c6740b0`**: ~250 upstream-коммитов, среди них windows-фиксы
  vfs/fuse (#7693, #7696, #7697, #7707, #7756, #7768) — вошли и в эту ветку. **Возможно,
  D2 уже не воспроизводится — прогон это и проверяет.**
- Отдельно: **macOS-эталон (2026-11-24)** — тот же контур (grpc-мета + OIDC + STS +
  encrypted том) прошёл E2E полностью (roundtrip 16 MiB md5 ×3, wipe, re-login) на
  бинаре серверной ревизии `37163e7e9`. То есть серверная сторона и протокол живы;
  вопросы только к Windows-слою.

## 9. Правила безопасности (обязательны)

1. **Пароли никогда**: агент не запрашивает, не записывает и не логирует пароли.
   Браузерный логин `gril-lom@yandex.ru` владелец выполняет лично на машине.
   Альтернатива (если владелец недоступен): тест-юзер `proorc@inbox.ru` — но его
   компания `d6394c03-cb5a-4e2a-8475-4a3f91bba126` требует предварительного выпуска
   S3-ключа админом (GraphQL Issue-мутация, рецепт в agio-cloud archive
   `2026-11-24-add-authz-grpc-encrypted-volume-vk-stage`) — согласовать с владельцем
   до прогона, `--company-id` тогда меняется на её UUID.
2. Логи маунта **не содержат** токенов/паролей (проверено эксплуатацией) — их можно
   коммитить; токен-кеш `%USERPROFILE%\.oidc-login` **не коммитить и не пересылать**.
3. Stage можно тестировать и ломать тестовыми данными; **prod-эндпоинты и чужие
   компании — не трогать**; Issue-мутации и серверные изменения — только с владельцем.
4. В коммитах/отчётах: имена, UUID, ARN — можно; креденшелы — никогда.

## 10. Что сделать по итогам (закрытие задач)

1. **tasks.md** этого change'а — обновить статусы 2.1 / 4.1 / 4.2 (по образцу
   существующих записей: дата, машина (ОС+билд), бинарь (коммит из `go version -m`),
   WinFsp-версия, результат, дословные ошибки).
2. **test-report.md** — дописать секцию прогона по образцу существующих (Verdict /
   Environment / Verified / Findings — при новых дефектах).
3. `git commit -m "windows-mount: run <дата> on <машина> — <вердикт>"` в ветку
   `implement-windows-mount`, `git push`.
4. Сообщить владельцу вердикт одной строкой (PASS / BLOCKED D1 / BLOCKED D2 / BLOCKED
   environment / proto-mismatch) — он обновит гейт 0.1 в agio-drive
   (`openspec/changes/implement-juicefs-mount/tasks.md`).

## Приложение: быстрая шпаргалка команд

```powershell
# установка заголовков WinFsp (после git clone)
mkdir C:\WinFsp\inc\fuse -Force; copy hack\winfsp_headers\* C:\WinFsp\inc\fuse\

# сборка
$env:CGO_ENABLED=1; $env:PATH += ";C:\mingw64\bin"; $env:CGO_CFLAGS='-IC:/WinFsp/inc/fuse'
go build -ldflags "-s -w" -o C:\jfs\juicefs.exe .

# L1 sqlite-смоук среды
C:\jfs\juicefs.exe format sqlite3://C:/jfs/meta.db winvol
C:\jfs\juicefs.exe mount sqlite3://C:/jfs/meta.db X:

# L2 негатив (немедленный fail-fast)
C:\jfs\juicefs.exe mount redis://127.0.0.1:6379/1 Y: --sts-enabled

# L3/4.1 основной маунт (браузер-логин выполняет владелец)
C:\jfs\juicefs.exe mount "grpc://drive-meta-proxy-tst-enc-vk.agio.services:443/tst-enc-vk?tls=1&oidc-issuer=https://platform-stage.agio.services/.ory/hydra/public&oidc-client-id=juicefs-drive-stage&oidc-redirect-url=http://127.0.0.1:18080/callback&oidc-scopes=openid,offline_access,profile,email" Z: --sts-enabled --company-id 0f7afc02-718c-479e-9f57-e0cb8d1e60bc --cache-dir C:\jfs\cache

# гигиена
Get-Process juicefs* | Stop-Process -Force ; fsptool lsvol
```
