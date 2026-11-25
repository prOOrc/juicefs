# D2 Fix Plan: encrypted+STS маунт не обслуживает I/O под Windows

Дата: 2026-11-25. Основание: прогон по `windows-runbook.md` на Win11 Home 26300 (вердикт BLOCKED D2, `test-report.md` → «Run 2026-11-25»). Документ — самодостаточная инструкция для фикс-итерации на другом хосте. Вердикт-коммиты: `09dfa3c8` (прогон), `9418dcf6` (локализация), этот план — `см. git log`.

## 1. Root cause (цепочка до строки)

Симптом: encrypted+STS-маунт (`--sts-enabled --company-id`) через grpc-мету под Windows: NewSession/STS OK, том жив в WinFsp, но **любой** `Opendir` возвращает EIO → буква не материализуется, `dir` → «I/O device error». Bare-том через ту же мету — работает. macOS — работает.

```
WinFsp Opendir
 → pkg/winfsp/winfs.go:789 juice.Opendir
 → pkg/winfsp/winfs.go newContext → meta.NewContext
   → pkg/meta/context.go WrapWithCancel: checkPerm жёстко TRUE (все контексты)
 → pkg/vfs/vfs.go:442 vfs.Opendir: if ctx.CheckPermission() → v.Meta.Access(...)
 → pkg/meta/grpc_client_fuse.go:58 grpcMeta.Access → RPC Access
 → сервер (authz_interceptor) отклоняет gRPC-статусом (точный статус не виден — см. §3)
 → pkg/meta/grpc_client_fuse.go:70-71: if err != nil { return syscall.EIO }  ← МОЛЧА, без лога,
   без D3-статусной таблицы (в отличие от Lookup/NewDirHandler в соседних функциях)
 → Opendir → (-5) EIO → WinFsp VolumeInfoTimeout=1000 ретрай раз в секунду
```

Почему macOS зелёный: `pkg/fuse/context.go:65` — `checkPermission = conf.NonDefaultPermission && uid != 0` → по умолчанию **false** → `Meta.Access` на Opendir не вызывается вовсе. Windows-мост семантику darwin не повторяет.

Ключевой контраст внутри клиента (доказывает, что маппинг — дефект): соседние `grpcMeta.Lookup` и `grpcMeta.NewDirHandler` (`grpc_client_data.go:752`) логируют ошибку и мапят статусы через общую таблицу D3 (PermissionDenied→EACCES, NotFound→ENOENT).

## 2. Evidence (эксперименты 2026-11-25, машина Win11 26300)

| Эксперимент | Результат | Вывод |
|---|---|---|
| L1: sqlite-мета, тот же бинарь/WinFsp/сессия | I/O работает | WinFsp и FUSE-мост здоровы |
| **Изолятор**: bare `juicefs-b-stage`, та же grpc-мета, **без** `--sts-enabled` | листится (9 каталогов) | grpc-мета-клиент + WinFsp под Windows работают; ломает только enc+STS |
| fuse-trace enc-маунта | `Getattr(/_JFS_LOGOUT)` → -2 ENOENT за 7-11ms (сетевой RPC OK); `Opendir(/)` → -5 за 4-8ms раз/сек | транспорт жив; EIO в Opendir-пути |
| Логи enc-маунта | «NewDirHandler error» — 0; любых error/deny — 0 | EIO появляется только через молчаливую ветку Access |
| Код: `isAlwaysAllowed` (`authz_interceptor.go`) | `path=="/" && perm==View → skip` | bare (корень резолвится в "/") — скипается; enc (chroot) — не скипается |

Логи на машине прогона: `C:\jfs\mount21.log`, `mount22.log`, `mount-debug.log`, `fuse.log` (fuse-trace enc), `l1-mount.log`, `l2-sqlite.log`, `l2-bare-grpc.log`, `mount-bare.log` (изолятор). Бинарь: `8b29770818d4` + фикс дубля owner-override.

## 3. Клиентские фиксы (форк juicefs, ветка implement-windows-mount)

### F1 (обязательный, малый, диагностический): `grpcMeta.Access` — убрать молчаливый EIO

Файл: `pkg/meta/grpc_client_fuse.go:68-71` (функция `Access`).

Сейчас:
```go
resp, err := m.client.Access(m.withSessionID(ctx), req)
if err != nil {
    return syscall.EIO
}
```

Сделать по образцу соседних функций: логировать (`logger.Errorf("Access error: %v", err)` — доступен logger этого файла) и мапить через общую статусную таблицу D3 (`grpcStatusToErrno`): PermissionDenied→EACCES, NotFound→ENOENT, остальные — по таблице. Эффект: вместо «I/O device error» — осмысленная ошибка + в логе маунта виден настоящий статус сервера (это же закрывает потребность в серверных логах для диагноза §4).

### F2 (дизайн-вопрос, не блокирует F1): checkPerm-семантика WinFsp-моста

`pkg/winfsp/winfs.go newContext` создаёт контекст через `meta.NewContext` → `WrapWithCancel` (checkPerm=true всегда). Darwin-путь ставит `checkPermission` условно (`pkg/fuse/context.go:65`). Варианты: (а) повторить darwin-условие в Windows-мосте; (б) оставить true, но тогда сервер обязан отвечать на Access корректно (см. §4) — POSIX-проверка на Windows и так суррогатная (owner-override там игнорируется, authz гейтится сервером). Решение за владельцем; для разблокировки E2E достаточно F1 + §4.

Оговорка retest: прогоны 25.11 велись ручным вызовом без `--owner-override=` (собранный клиент agio-drive передаёт всегда) — на эту цепочку не влияет (checkPerm константен), но финальный retest делать через собранного клиента.

## 4. Серверные пункты (authz/path-resolution слоя meta-proxy; серверная ревизия 37163e7e9 — те же функции, сверить при фиксе)

### S1 (диагностика, без кода): что именно отвечает сервер на Access(ino=1) enc-маунта

Включить debug authz-лога proxy (`authzLogger` — строки `Authz check:` / `Authz denied:` / `Cannot resolve path for method=Access`) на деплое `drive-meta-proxy-tst-enc-vk`, повторить Windows-маунт, увидеть один из исходов:

- `access denied: path not resolved` — `resolveChecks` → `lookupPath(ino=1)` не резолвит корень chroot-маунта (`ai.cache.Get(1)` пуст, recovery не отвечает) → кандидаты фикса: (i) сид path-cache корня chroot-маунта при NewSession/Init (path = рабочий каталог компании), (ii) расширить `isAlwaysAllowed` на корень **сессии** (root of the session = chroot-корень) с View;
- `access denied` (чистый) — `CheckPermission(userID, <resolved-path>, Read)` вернул deny/ошибку → смотреть, в какой путь резолвится ino=1 (ожидается `companies/home-gri-llo`) и почему SpiceDB DENY на Read (founder→own должен разрешать); помнить квирк SpiceDB v1.37 (deny на свежезаписанный кортеж до первой мутации — tasks.md 8.2 agio-drive);
- `Unauthenticated` — authn-контекст Access-RPC пуст (клиент не вкладывает user в grpcContext для Access — сравнить с Lookup).

Смысловой критерий: Opendir корня тома — операция, которую пользователь этого маунта безусловно имеет право сделать; сервер должен разрешать её (path not resolved — это fail-closed, но для корня сессии он ложный).

### S2 (код, после S1): корректный ответ на Access корня chroot-маунта

Реализовать выбранный по S1 кандидат (сид кэша или always-allow корня сессии) + unit-тест `authz_interceptor` на Access(inо корня) для chroot- и bare-конфигураций.

## 5. Порядок работ и верификация

Рекомендуемый порядок: **F1 → retest (диагноз окончательный) → S1 (по логам) → S2/F2 по результатам**. F1 сам по себе может разблокировать E2E только если сервер денит EACCES-ом и это окажется серверной недоработкой корня (тогда всё равно S1/S2); если после F1 ошибка станет EACCES — диагноз готов без серверных логов.

Retest-чеклист (машина Win11 26300 готова: Go 1.26.11 в `C:\jfs\toolchains\go`, mingw 14.2.0 в `C:\mingw64`, WinFsp 2.1.25156, заголовки в `C:\WinFsp\inc\fuse`, клон `C:\jfs\src`, remote `github`; команды — `windows-runbook.md` §5-§6):

1. Пересборка после F1 (`go build -ldflags "-s -w" -o C:\jfs\juicefs.exe .`, CGO_CFLAGS `-IC:/WinFsp/inc/fuse`).
2. enc-маунт (L3-команда ранбука; токен-кеш жив, браузера не будет) → `Get-ChildItem Z:\` — должен листаться (или осмысленная ошибка по S1).
3. Если листается → 4.1 целиком: 16MiB md5 ×3 → `_JFS_LOGOUT` wipe → повторный вход → данные на месте → PASS 4.2 → закрытие гейта 0.1 agio-drive.
4. Зафиксировать статусы в tasks.md/test-report.md этого change'а.

Гигиена между попытками: `windows-runbook.md` §7 (убивать процессы, проверять `fsptool lsvol`, ложные NTFS-папки).
