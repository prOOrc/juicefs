# Design: `fix-cfapi-followups`

## Architectural Context

Change закрывает четыре follow-ups заархивированного прототипа `implement-cfapi-provider` (ADR-005 accepted, живой facility-маунт vector-stage подтверждён владельцем 27.11). Три задачи касаются data-path движка (`pkg/cfprovider`, `pkg/cfapi`, `cmd/clouddrive.go`), одна — тестовой инфраструктуры `pkg/meta`. Криптоцепочка FEK/CEK/AGDF не затрагивается: классификация и metadata-update не читают и не пишут данные файлов.

```
walk (рестарт serve)                        watcher/ingest (не меняются)
   |  classifyOccupied                           ^
   v                                             | события фильтра/ReadDirectoryChangesW
  [A1] index → exists → STANDARD identity        |
   |      own → adopt (idx.Put, skipped)         |
   |      plain/foreign/unknown → conflict       |
   v                                             |
  [A2] freshness: (size, mtime±2s) vs hub        |
   |      equal → skip (ensureDehydrated в adopt)
   |      localNewer → queueUpload (--read-only → Warn)
   |      backendNewer → markSelfEvent → Update(VerifyInSync|Dehydrate)
   |      pinned/out-of-sync → skip+Warn; ambiguous → skip+Warn
   v
  cleanup [A4]: STANDARD-детект → plan → (--apply) revert → (--delete-files) remove
```

## Component Map

| Пакет или файл | Действие |
|---|---|
| `pkg/cfprovider/identity.go` (new) | A1: `classifyIdentity` (виды identityOwn/Foreign/Plain/Unknown) |
| `pkg/cfprovider/freshness.go` (new) | A2: `freshMtimeSlack`, `compareVersion`, `checkFreshness` |
| `pkg/cfprovider/cleanup.go` (new) + `cleanup_test.go` | A4: движок cleanup (классификация, план, apply) |
| `pkg/cfprovider/placeholders.go` | A1/A2: ветки `classifyOccupied` (:713, :732-751) и `buildPopulatedDir` (:514); store-методы `StandardInfoOf`, `StatOf` |
| `pkg/cfprovider/walk.go` | счётчики adopted/fresh; строки `walk adopt:`, `walk fresh:`, `walk classify:`; формат `walk:` байт-в-байт прежний |
| `pkg/cfapi/cfapi.go`, `consts.go`, `operations.go` | A1: аксессор FileIdentity STANDARD-записи + предикат «не плейсхолдер»; A4: `RevertPlaceholderByPath` + `RevertFlagDelete`; A2 (при необходимости): Update-обёртка с метаданными |
| `pkg/meta/base_test.go` + `flock_other_test.go`/`flock_windows_test.go` (new) + `grpc_fuse_integration_test.go` | A3: платформенные flock-алиасы, тег `!windows` |
| `cmd/clouddrive.go` | A1: `status` переиспользует общий хелпер; A4: команда `cleanup`, хелперы NTFS/unregister |

## Execution Flow

### A1. Identity-first classification (classifyOccupied, финал)

1. `idx.Path(e.Inode)` известен → существующая known-ветка (placeholders.go:682-693): Put-обновление пути + skipped; **STANDARD-запроса нет** (ноль новых syscalls на тёплом пути).
2. `StateOf` (:696): используются только `exists`/`err`. `err != nil` → конфликт (как сейчас); `!exists` → существующий блок create (:697-712) без изменений.
3. `exists` → `classifyIdentity(localPath, e.Inode)` через новый store-метод `StandardInfoOf` (STANDARD-запрос `CfGetPlaceholderInfo`, BACKUP_SEMANTICS — работает и для каталогов):
   - идентичность `FileIdentity[:FileIdentityLength]` == 8B LE `e.Inode` → **own**: `idx.Put` (:748) + skipped + `adopted++`; `ensureDehydrated` только при положительном атрибутном флаге StateOf (для класса U20 с чистыми атрибутами adopt без дегидратации — до гейта A2 freshness; при inSync=TrackNone дегидратация до сверки отбросила бы возможную локальную правку);
   - plain / foreign (чужой FileIdentity или длина ≠ 8) / unknown (ошибка STANDARD) → существующий блок конфликта (:713-722) без мутаций.
4. FETCH-путь `buildPopulatedDir` (:514-516) — тот же предикат: own → ветка плейсхолдера, иначе Warn + `conflicts++`.
5. Ошибка STANDARD в classify → **консервативно unknown → конфликт**: без createEntry, без idx.Put, без дегидратации; повтор на следующем walk. Fallback на StateOf запрещён (атрибут-положительный без доказанной идентичности усыновлять нельзя — индекс связал бы путь с чужим inode, и OpDelete удалил бы не тот файл).
6. Гард-ветка воскрешения (:614-622) не трогается.

Логи: формат `walk:` байт-в-байт прежний (adopt учитывается в skipped); `walk adopt: entries=%d` (Info, только при >0); причины классификации — Debug `walk classify: <path>: <reason>` (отдельный префикс, парсеры `walk conflict:` не задеты).

Сверка идентичности — по **FileIdentity** (провайдерская идентичность, которую движок записывает при createEntry как 8B LE inode), НЕ по FileId (платформенный идентификатор) — подтверждается пробой U-F0/task 1.0.

### A2. Restart cross-machine freshness

Сверка для каждой своей записи (known-ветка при совпадении пути индекса с localPath в нормализованном виде; own-ветка adopt), **только файлы** (каталоги исключены: NTFS двигает mtime каталога при любой смене записей, включая наши же createEntry — свалка ложных «локаль новее»).

- `compareVersion(size, mtime, e BackendEntry) → freshEqual | freshLocalNewer | freshBackendNewer | freshAmbiguous`; size строго, mtime с допуском `freshMtimeSlack = 2s` (конвенция грубых меток — rsync `--modify-window=2`, Unison `-fat`; маскирует возможное усечение до секунд на переходах; ужесточение по пробе U-F2 — константа в одном месте).
- `freshEqual` → ничего (в adopt — `ensureDehydrated` → `idx.Put` → skipped). Stat-ошибка → ничего (reconcileDir обрабатывает удаление).
- `freshLocalNewer`: out-of-sync по фильтру → ничего (reconcileDir :93-102 уже ставит в очередь — дедупликация); `--read-only` → Warn без очереди; иначе → `queueUpload` (тот же вызов, что reconcile.go:93-102).
- `freshBackendNewer`: pinned (PinState из STANDARD) → skip + Warn «pinned stale»; out-of-sync по фильтру → skip + Warn (истинный конфликт обеих сторон); иначе `markSelfEvent(localPath)` → `CfUpdatePlaceholder` с новыми size/mtime, флаги `UpdateFlagVerifyInSync|UpdateFlagDehydrate` (DEHYDRATE обязателен: гидратированный stale-контент без него остался бы старым; следующее чтение FETCH'нет новую версию).
- `freshAmbiguous` (size разошёлся, |Δmtime| ≤ допуск) → skip + Warn, без мутаций.
- `markSelfEvent` перед Update обязателен (in-sync-ignore ingest'а покрывает штатный исход; при сбое Update на середине или снятии in-sync фильтром событие родило бы выгрузку устаревшего контента поверх новой версии backend; цена — слепое окно 10с, самоизлечивается следующим walk).
- Идемпотентность: equal не пишет индекс и не логирует; рестарт без изменений → ноль строк `walk fresh:`/`walk adopt:`/`walk conflict:`.
- Лог: `walk fresh: revalidated=%d queued-upload=%d readonly=%d pinned=%d ambiguous=%d out-of-sync=%d` (Info, только при ненулевой сумме).

### A3. pkg/meta flock-алиасы

Платформенная пара (заголовки по прецеденту errno_server_windows.go/errno_server_other.go — обе строки тегов):
- `pkg/meta/flock_other_test.go` (`//go:build !windows`): `flockRDLCK/flockWRLCK/flockUNLCK` = соответствующие `syscall.F_*` (нетипизированные алиасы — существующие `uint32(...)` и сравнения компилируются без правок типов);
- `pkg/meta/flock_windows_test.go` (`//go:build windows`): те же имена = 0/1/2 (Linux flock ABI — единственное, что уходит в `meta.Flock`/`Setlk`; движки интерпретируют сами).

63 вхождения `syscall.F_*` в `base_test.go` (testListLocks с :1334) заменяются на алиасы. `grpc_fuse_integration_test.go` (`syscall.Stat_t` — unix-only тип, компиляция а не рантайм) → тег `!windows` на файл. Правило для мест, найденных полной инвентаризацией (`go test -c -o NUL -gcflags=-e ./pkg/meta`): константы — алиасами, unix-only типы/функции — тегом на файл. Linux-ветка по значениям не меняется (алиасы = те же syscall-константы).

### A4. Cleanup-режим

CLI: `clouddrive cleanup --root X [--apply] [--delete-files] [--unregister]`. Уровни: dry-run (по умолчанию) = отчёт без мутаций; `--apply` = revert плейсхолдеров в plain; `--apply --delete-files` = + удаление только подтверждённо-reverted; `--apply --delete-files --unregister` = + unregister. Plain-файлы и unknown не трогаются никогда; `os.RemoveAll` запрещён; каталоги — только пустые через `os.Remove`; сам `--root` не удаляется и не revert'ится.

Apply pipeline (plan-then-act): (1) статические предусловия — корень существует/каталог/не корень тома/NTFS (хелпер из register :252-258); (2) WalkDir + классификация: symlink/junction → L (не трогается); STANDARD успешен → P (placeholder); ошибка «не плейсхолдер» → N (plain); иная ошибка → по атрибутам `DirEntry.Info()` (RECALL_ON_DATA_ACCESS файл / RECALL_ON_OPEN каталог → P by-attributes; иначе U unknown, только отчёт); (3) предусловия плана — чужой живой корень внутри (mutex probe по вложенным каталогам) → отказ/пропуск; (4) revert P-файлов: `RevertPlaceholderByPath` (доступ GENERIC_READ|GENERIC_WRITE, +DELETE только при RevertFlagDelete; BACKUP_SEMANTICS для каталогов; директорийность по os.Lstat) → post-проверка STANDARD стал «не плейсхолдер»; (5) `--delete-files`: os.Remove только подтверждённо-reverted; (6) revert P-каталогов снизу вверх, каталог revert'ится только если все потомки-плейсхолдеры подтверждённо reverted; (7) os.Remove пустых reverted-каталогов снизу вверх, непустые → kept; (8) `--unregister` если зарегистрирован (общий хелпер из unregister :362-379; незарегистрирован → WARN и продолжение) — ПОСЛЕДНИЙ шаг мутаций, после всех revert'ов/удалений [U-C1 живая проба 28.11: CfUnregisterSyncRoot удаляет дегидратированные скелеты и материализует гидратированные — revert обязан идти на зарегистрированном корне; revert требует живого провайдера (0x80070194); для dehydrated-сирот — fallback прямого удаления (контент в backend), hydrated — ошибка + rescue-указание в отчёте]; (9) сводка, exit code 0 при успехе/dry-run, non-zero при любой неудаче.

Safety: R1 мутации только с `--apply`; R2 провайдер запущен → отказ в обоих режимах — mutex `Local\juicefs-clouddrive-<san>` (provider.go:634-643), предпочтительно создать и держать до конца (закрывает гонку «serve стартовал между проверкой и revert»); R7 apply на зарегистрированном корне требует `--unregister`; state-dir не трогается. Отчёт: заголовок (режим/mutex/ФС/регистрация), счётчики (files/dirs/links, placeholders dehydrated/hydrated, plain, unknown, ошибки), план, ≤20 примеров на класс с FileId/InSyncState/PinState, гистограмма ошибок apply.

Два шага revert→remove вместо `CF_REVERT_FLAG_DELETE` одним вызовом: точка подтверждения «стал plain» (R4), симметрия с dry-run, безопасный отказ (файл остаётся plain); константа `RevertFlagDelete = 0x1` добавляется для пробы U-C3, продукт от неё не зависит.

## Invariants

- **Локальный файл пользователя не перезаписывается и не удаляется**: plain/foreign/unknown → конфликт/пропуск без мутаций (A1, A4 R3).
- **Идентичность прежде атрибутов**: усыновление только по доказанному FileIdentity == inode; атрибутный флаг — не решающий.
- **Форматы `walk:` и `walk conflict:` байт-в-байт прежние** (парсеры приложения V3); новые строки — только с новыми префиксами `walk adopt:`/`walk fresh:`/`walk classify:`.
- **cleanup без `--apply` не мутирует**; `os.RemoveAll` отсутствует; удаление только после подтверждённого revert.
- **Криптоцепочка не затрагивается** (никаких путей FEK/CEK/AGDF).
- **Linux-CI не меняется** (алиасы = syscall-значения; все новые cfapi/cfprovider файлы кроссплатформенные либо под тегами).
- **Движок для приложения V3 не расширяется**: change не добавляет CLI поверх pin/unpin (dehydrate — сторона приложения очереди B).
- Свой файл с чистыми атрибутами (U20)adopt-ится без дегидратации до гейта freshness — дегидратация до сверки отбросила бы возможную локальную правку при inSync=TrackNone.

## Decisions & Rationale

- **D-A1 identity-first, без LookupPath**: индекс ищется по inode (`idx.Path(e.Inode)`); запись по пути с тем же inode означала бы, что known-ветка уже сработала, с другим — всё равно конфликт по идентичности. STANDARD — единственный честный определитель (урок f78e14cd: атрибуты фильтр чистит у in-sync записей; status уже рефайнит через STANDARD).
- **D-A1 conservative-on-error**: unknown → конфликт. Ложно-отрицательный StateOf — сам баг U20; fallback на него воспроизводил бы дефект.
- **D-A2 файлы-только и 2s-допуск**: см. Execution Flow; стоимость слепого окна 2с ниже риска массовой ревалидации при систематическом сдвиге; ужесточение — по U-F2.
- **D-A2 LWW с гейтами**: истинный конфликт (обе стороны правили при выключенном serve) решается порядком mtime, одна сторона теряется — сознательное ограничение (non-goal: conflict-copies/last-synced штамп — follow-up). Гейты pinned/out-of-sync сужают потерю.
- **D-A3 алиасы, не guard**: 63 механические замены, Linux-значения идентичны, grep-проверяемо; guard потребовал бы vars/обёрток в 63 местах. **Поправка (верификация 3.4, 2026-11-28)**: значения на Windows — НЕ Linux ABI 0/1/2, а пакетные константы `pkg/meta/utils_windows.go` (F_UNLCK=1, F_RDLCK=2, F_WRLCK=3): движки (redis_lock/sql_lock/tkv_lock/grpc_client_data) сравнивают параметр ltype напрямую с пакетными константами, поэтому алиасы обязаны зеркалить пакет на каждой платформе (!windows = syscall-значения = utils_linux/darwin; windows = utils_windows.go). Инвентаризация расширила фронт работ: +7 вхождений в subdir_test.go и unix-типы grpc_fuse_integration_test.go (Stat_t/Statfs_t/Flock/LOCK_*/xattr) — тег на файл.
- **D-A4 plan-then-act и два шага**: наблюдаемость и безопасность важнее краткости; revert на незарегистрированном корне — ровно сценарий пробы U-C1.
- **Порядок реализации**: 1.x (A1) → 2.x (A2, поверх A1) → 3.x (A3, изолированный pkg/meta) → 4.x (A4, переиспользует хелперы A1 в pkg/cfapi). Задачи 3.x и 1.x файл-независимы, но кодируются последовательно одним кодером (общий worktree).

## Integration Points

| Точка | Что используется |
|---|---|
| `PlaceholderStandardInfoByPath` (pkg/cfapi/cfapi.go:423-444) | A1/A4: идентификация и orphan-детект без провайдера (буфер +4KiB, указатель на голову — урок f78e14cd) |
| `queueUpload` (reconcile.go:93-102) | A2: ветка localNewer |
| `markSelfEvent` (ingest.go:545, 567-571) | A2: супрессия watcher-события от Update |
| `CfUpdatePlaceholder` (cfapi.go:330-359, UpdatePlaceholderByPath :406-417) | A2: ревалидация (файл-only достаточно — каталоги исключены) |
| mutex `Local\juicefs-clouddrive-<san>` (provider.go:634-643) | A4: отказ при живом провайдере, probe вложенных корней |
| `writeSyncRootRegistry`/`deleteSyncRootRegistry` (cmd/clouddrive.go:1047-1105) | A4: регистрация/чистка реестра |
| `CfRevertPlaceholder` (operations.go:196-207) | A4: revert (биндинг есть, вызывающих не было) |
| маркеры `walk:`/`walk complete:` | контракт приложения V3 — не менять |

## Open Questions (U-пробы)

| ID | Неопределённость | Проба | Запасной вариант |
|---|---|---|---|
| U-F0 | Семантика полей STANDARD: FileIdentity (провайдерская) vs FileId (платформенная); ответ на plain/каталог/вне root | task 1.0 чтение structs.go + живой статус file.txt.txt | Неуспех не отличим → все неуспехи на существующем файле = unknown (исход тот же — конфликт) |
| U-F1 | DEHYDRATE на pinned; metadata-only на гидратированном stale pinned | живая проба (task 2.4) | Skip + Warn «pinned stale» |
| U-F2 | Дрейф mtime на живом корне | Stage 1 (task 2.2): 20 файлов, differs=0 | Дрейф ~1s → оставить 2s + follow-up ns; кратен 3600s → FAIL чинить TZ до действий; ≤1µs на N≥20 → ужесточить |
| U-F3 | STANDARD Modified/ValidatedDataSize выдаёт офлайн-правку | task 2.4 | Нет → порядок mtime + VerifyInSync |
| U-F5 | DEHYDRATE на out-of-sync; при открытом handle | task 2.4 | Успех → обязателен предчек InSyncState (уже в правиле) |
| U-F6 | Watcher-событие/состояние после Update своего файла | task 2.4 | Снят → добавить UpdateFlagMarkInSync; markSelfEvent в обоих случаях |
| U-C1 | STANDARD/revert на незарегистрированном корне | task 4.2 на AgioDrive8 (AgioDrive9/7 запрещены) | STANDARD падает → классификация по атрибутам, остальное unknown; revert hydrated падает → деградация «отчёт + unregister», эскалация владельцу |
| U-C2 | Revert каталога-плейсхолдера (BACKUP_SEMANTICS) | task 4.2 | Каталоги не revert'ятся — только os.Remove пустых, остаток в отчёте |
| U-C3 | CF_REVERT_FLAG_DELETE (файл/каталог) | task 4.2 | Информационная; продукт от флага не зависит |
| U-C4 | Прямое os.Remove плейсхолдера без revert | task 4.2 | Позитив → вторая попытка в --delete-files; негатив → dehydrated-сироты неудаляемы без провайдера (ограничение фиксируется) |

**Живой гейт facility vector-stage (root AgioDrive9 владельца)** — Stage 0 (после 1.x): штатный рестарт serve, PASS = нет `walk conflict:` по file.txt.txt, status прежний «placeholder, dehydrated, in-sync», файл не изменился. Stage 1 (после 2.1, только отчёт): 20 файлов (10 Windows + 10 Mac) под `companies\home-gri-llo\cf-followups\`, рестарт без изменений, PASS = differs=0, drift ≤2s. Stage 2 (после 2.3): remote-edit (другая длина + та же длина) при выключенном serve → рестарт → `walk fresh: revalidated=2` → чтение SHA-256 совпадает с Mac; local-edit гидратированного → рестарт → `queued-upload=1`, Mac видит; два рестарта без изменений → ноль fresh/adopt/conflict строк. Условия: только штатный `--state-dir` владельца, свои файлы только под `companies\home-gri-llo\cf-followups\`, лог каждого запуска в файл, чужие файлы не трогать. FAIL любого stage → стоп, разбор до продолжения.

## Риски

| Риск | Последствие | Митигция |
|---|---|---|
| Неверное поле идентичности (FileIdentity vs FileId) | adopt не срабатывает или усыновляет чужое | U-F0/task 1.0; foreign → конфликт; юнит-тесты |
| Систематический ложный differs у владельца | Массовая ревалидация/выгрузка | Stage 1 только отчёт до действий |
| Истинный конфликт теряет сторону (LWW) | Потеря одной правки | Гейты pinned/out-of-sync; non-goal зафиксирован |
| Дегидратация U20-adopt до freshness | Потеря правки при inSync=TrackNone | Гейт: дегидратация только при equal (1.x+2.x до Stage 2) |
| os.Stat на каждую known-запись | Стоимость walk | Ноль новых syscalls на unknown-пути; замер Stage 1 |
| Поведение cldapi на незарегистрированном корне | Cleanup деградирует | U-C1; запасной «отчёт + unregister» |
| Повторная поломка компиляции pkg/meta на Windows незамеченной Linux-CI | Мёртвые тесты | Держать `go vet ./pkg/meta` в DoD вручную |
| Мьютекс `Local\` не видит чужую logon-сессию | Cleanup при зомби-serve | Runbook: taskkill перед cleanup (прецедент прототипа) |
