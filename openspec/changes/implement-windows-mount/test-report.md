# Windows E2E Test Report — implement-windows-mount

Период: 2026-10-03 → 2026-10-04. Верификация фаз change'а на живых Windows-машинах со stage-топологией JuiceFS Drive (Meta Proxy + authz/KeyManager + OIDC + зашифрованный том).

## Verdict

**BLOCKED** — фичи этого change'а работают (STS-флаги, fail-fast, OIDC 5m-таймаут, service-cmdline реконструкция), E2E-файловые операции не закрыты из-за двух дефектов кодовой базы, найденных 2026-10-04 и лежащих вне scope change'а:

- **D1** — fork `prOOrc/go-redis v9.18.1` (replace в go.mod) не работает под Windows: PING зависает до таймаута. Все redis-мета-пути на Windows мертвы.
- **D2** — grpc-маунт зашифрованного тома: метаданные обрабатываются, но файловые операции падают `ERROR_IO_DEVICE`. Воспроизводится только в agio-drive-v2-дереве (vanilla-сборка тем же тулчейном на той же машине работает).

## Environments

| | itx.lan (2026-10-03) | YC VM jfs-win10 (2026-10-04) |
|---|---|---|
| ОС | Windows 11 Home, build 26200, dual-boot | Windows 10 Pro 22H2 (19045), образ `windows10-pro-01` (fd848ep3pqg7rh77mon3) |
| Доступ | SSH + локальная консоль пользователя | SSH (OpenSSH из Windows-capability), консольная сессия JD через autologon |
| WinFsp | 2.0.23075 / 2.1.25156 / 2.2B4 (перезаливки) | 2.1.25156 (чистая MSI-установка) |
| Топология | meta-proxy на mac (redis DB 25 + port-forward authz-grpc) | meta-proxy на самой VM (redis DB 25 напрямую из YC-сети + port-forward svc/platform-api-authz-grpc:9090) |
| Том | tst-enc-a (DB 25, encryption_enabled, tst-a1@agio.test) | тот же |
| Сборка тестируемого бинаря | mingw-w64 кросс на macOS | mingw-w64 кросс на macOS **и** нативная сборка на VM (Go 1.26.5 + w64devkit, `CGO_CFLAGS=-I.../hack/winfsp_headers`) |

Окружение itx исключено из уравнения полной заменой машины и ОС — симптомы воспроизведены, значит виноват не стенд.

## Verified (change's own scope — OK)

- **1.1 STS flag parity**: флаги в Windows-flagset, `--help` показывает оба; общая обработка в mount.go не тронута.
- **1.2 fail-fast**: `--sts-enabled` на незашифрованном томе → `--sts-enabled requires an encrypted volume`; без grpc-эндпоинта → `--sts-enabled requires a meta proxy (grpc) endpoint`. Подтверждено на обеих машинах, разными бинарями.
- **2.2 повторный маунт из кеша**: без браузера, токен из `.oidc-login` (с оговоркой про ротацию refresh_token: живой маунт делает копии кеша одноразовыми).
- **2.3 контракт «SHALL NOT hang»**: пустой кеш в неинтерактивной сессии → ровно 300s → `oidc: interactive authentication timed out after 5m0s` → `FATAL Unauthenticated`. Подтверждено дважды: itx (после фикса bb026386) и YC VM (JD-сессия, 15:46:10 → 15:51:11 = ровно 300s).
- **3.1/3.2 service-cmdline реконструкция**: unit-тест 4/4 PASS на живой Windows; на реальном реестре CommandLine сервиса сохраняет URL с query-параметрами и оба STS-флага байт-в-байт.
- **STS-выдача в E2E-топологии**: каждый grpc-маунт на VM получает `STS credentials refreshed (expiry +1h)`.

## Finding D1 — fork go-redis v9.18.1 hangs on Windows

**Симптом**: meta-proxy на VM не поднимает gRPC-listener: лог обрывается после `Meta address: redis://... [NewClient]`; redis-маунты висят на NewClient вечно.

**Микротест** (rdtest, PING к MDB redis 10.129.0.5:6379/25, одинаковые бинарники кроме модуля):

| Модуль | Результат |
|---|---|
| upstream `redis/go-redis v9.18.0` | `PING result in 9.9ms: PONG <nil>` |
| fork `prOOrc/go-redis v9.18.1` (replace) | висит до context-deadline 15s, PONG не читается |

На macOS fork работает (вся stage-топология живёт на нём). Дефект Windows-специфичный; предположительно RESP3 push-ридер фикса 7251ee61 блокирует чтение ответа под IOCP-нетполлом — требует разбора в репо fork'а.

**Обход/фикс-кандидаты**: (а) починить fork; (б) conditional replace для windows-сборки (до починки fork'а позволяет гонять Windows-E2E).

**Последствия до фикса**: пересборка на VM без replace (`juicefs-nofork2.exe`) — прокси поднимается за 5s; оставлено на VM.

## Finding D2 — grpc mount: metadata alive, file I/O dead

**Симптом**: `juicefs-nofork2 mount grpc://127.0.0.1:9561/tst-enc-a?... C:\jfs-mnt --as-local-volume --sts-enabled --company-id ...` — NewSession sid, STS refresh, в trace прокси видны `Authz skip: method=/pb.MetaService/Lookup user=tst-a1 path=/` — но `Get-ChildItem C:\jfs-mnt` → `ERROR_IO_DEVICE` с периодом ~1s (WinFsp VolumeInfoTimeout=1000).

**Маркер**: наш mount печатает в stdout `The service juicefs-nofork2 has been started.` — cgofuse/WinFsp уходит в service-путь; у vanilla-сборки на той же машине этого маркера нет.

**Контроль (тот же тулчейн, та же VM, тот же WinFsp)**:
- `vanilla-built.exe` (исходники v1.4.1, собран на VM mingw-w64 + hack/winfsp_headers): redis:// маунт — буква X: и папка работают сразу (mkdir/write/readback OK);
- release `juicefs-1.4.1-windows-amd64`: то же;
- наш код на **sqlite**-мете: mkdir/read работают (FUSE-слой жив);
- наш код на **redis**-мете (незашифрованный b-stage): не проверен чисто — на тот момент D1 валил redis-dial; после nofork2-пересборки grpc-путь воспроизводит I/O error стабильно.

**Вывод**: регрессия ответа FUSE-хендлеров под Windows в agio-drive-v2-дереве на grpc-мете. Бисект по agio-коммитам (e3f21cff..HEAD) не проводился — следующий шаг. Дополнительно проверить: vfs Init/log-пайплайн, logout watcher (`internal.go`), STS refresher goroutine — всё, чего нет в v1.4.1.

## Diagnostics notes (для будущих прогонов)

1. **kubectl port-forward не годится для FUSE-трафика**: мета-RPC (NewSession/flush/STS) через него ходят, а потоки файловых операций зависают. Прокси держать процессом на той же машине, что и маунт.
2. **Ложноположительные результаты probe-скриптов**: старый живой маунт удерживает точку монтирования; после `Stop-Process` папка остаётся обычной NTFS-папкой — все последующие «операции с томом» тихо выполняются на NTFS. Каждый прогон: убивать процессы по имени, удалять папку монтирования и проверять `fsptool lsvol` (том без буквы/с префиксом = живой маунт).
3. **Маркер service-пути**: строка `The service <exe> has been started.` в stdout маунта — быстрый индикатор ненормального пути cgofuse/WinFsp (у здоровой vanilla-сборки отсутствует).
4. **Офлайн-подготовка Windows-образов YC**: `hiberfil.sys`/`pagefile.sys` удалить офлайн обязательно (hiberboot резюмится и игнорирует правки реестра), `ntfsfix -d` перед rw-маунтом; автологон с пустым паролем требует `PasswordLess\Device\DevicePasswordLessBuildVersion=0`; Cloudbase-Init в образе windows10-pro-01 сконфигурён под EC2-metadata и крашится (event 7034) — user-data не работает, sshd ставится через RunOnce.
5. **Раздел из 20GB-образа расширять сразу** (`Resize-Partition -Size ($m=Get-PartitionSupportedSize).SizeMax`): диск забивается мгновенно (Go-сборке нужно ~10GB), обрезанный модуль-cache даёт пустые файлы в `$GOROOT/src`.

## Bench state (сохранён, остановлен)

- **VM `jfs-win10`** (внутренний 10.129.0.56-диапазон, публичный IP на момент остановки 103.76.55.118): OpenSSH (`jfsadmin` / `JD`, пароль `Jf-E2E-Pass-2026`); `C:\jfs` — `juicefs-nofork2.exe` (без redis-fork), `juicefs-native.exe`, ваниль-сборки, Go 1.26.5 + w64devkit (`C:\jfs\go`, `C:\jfs\w64`), все probe-скрипты (`*.ps1`, `*.cmd`) и логи прогонов; `C:\Users\jfsadmin\.oidc-login` и `C:\Users\JD\.oidc-login` — кеш tst-a1 (refresh_token протухнет, обновить перед ретестом); kubeconfig с IAM-токеном (тоже протухнет — пересоздать `yc iam create-token`).
- **VM `jfs-rescue2`** (10.129.0.12): Ubuntu 22.04, офлайн-инструментарий (ntfs-3g, chntpw, hivex).
- **Диски**: `jfs-win10-disk` (40GB), `jfs-win-fixed` (100GB, копия win11pro-01 с офлайн-правками — использовалась для ранних попыток), `rescue2-boot`.
- **SG `jfs-win-test`**: ingress 22/2222/3389/5985 с WAN-IP владельца + 10.0.0.0/8.
- Удалены: deploy `jfs-e2e-proxy` (stage), secret `jfs-e2e-meta` (stage), snapshot `jfs-win-snap`, временные VM jfs-rescue/jfs-win/jfs-win-e2e.

## Next steps

1. D1: починить fork go-redis под Windows (или conditional replace) — разблокирует redis-пути и локальный meta-proxy.
2. D2: бисект agio-drive-v2 по коммитам на VM (e3f21cff..HEAD) — найти регрессию FUSE-ответа; кандидаты для первоочередного взгляда: `pkg/vfs` Init/log, logout watcher, STS refresher.
3. Переиграть 4.1 (md5 roundtrip + `_JFS_LOGOUT` wipe + повторный вход) и 2.1 (first-login браузером) на сохранённом стенде.
