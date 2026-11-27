# Design: `implement-cfapi-provider`

## Architectural Context

Прототип добавляет Windows-провайдер Cloud Files API поверх существующего data path форка. Читаются и пишутся те же тома hub, что и через `juicefs mount`; меняется только адаптер между `pkg/fs` и операционной системой.

```
Windows Explorer / приложения
        |  обычные файловые вызовы (CreateFile, FindFirstFile...)
        v
  cldflt.sys  (Cloud Files Filter, cldapi.dll — user-mode API)
        ^  FETCH_DATA / NOTIFY_* callbacks          ^ CfExecute(TRANSFER_DATA),
        |                                            |  CfCreatePlaceholders,
  +--------------------------------------------------+------------------+
  |  juicefs clouddrive serve (ОДИН процесс Go)                          |
  |    pkg/cfapi       — биндинг cldapi.dll через syscall (без cgo)      |
  |    pkg/cfprovider  — движок: индекс, плейсхолдеры, пулы, журнал      |
  |    pkg/fs          — path-based API (чтение и write-API)             |
  |    pkg/chunk + pkg/object — chunk store + S3 (VK hb.bizmrg)          |
  |    pkg/meta grpc   — meta proxy hub (OIDC bearer, TLS)               |
  |    cmd/sts_refresher — STS-креды object storage (как в mount)        |
  +----------------------------------------------------------------------+
        |
        v
  meta proxy (hub)  +  S3 bucket  +  KeyManager (FEK unwrap на сервере)
```

Транспорт FUSE/WinFSP (`pkg/fuse`, `pkg/winfsp`) не затрагивается: провайдер садится на тот же уровень, где уже работают gateway/webdav — `pkg/fs` поверх meta+chunk.

## Component Map

| Пакет или файл | Действие |
|---|---|
| `pkg/cfapi` | Новый: биндинг `cldapi.dll`, thunk'и, пул буферов |
| `pkg/cfprovider` | Новый: движок провайдера (индекс, плейсхолдеры, гидратация, выгрузка, журнал) |
| `cmd/clouddrive.go` | Новый: команда `juicefs clouddrive` (register/unregister/status/serve/pin/unpin/ls/hash) |
| Корневой командный файл `cmd` | Регистрация подкоманды |
| `cmd` (обвязка store с STS) | Выделение общего хелпера для mount и clouddrive, если копирование неизбежно |
| `pkg/meta`, `pkg/chunk`, `pkg/object`, `pkg/compress` | Не меняются |
| `pkg/fs` | Не меняется, кроме добавляющего метода при неполном write-API (задача 5.1) |

## Execution Flow

### Старт (`serve`)

1. Обвязка data path повторяет gc-паттерн и STS-выбор mount (блок кода в Integration Points).
2. Проверка регистрации sync root; при отсутствии — ошибка и выход.
3. Единственный экземпляр: именованный mutex; соединение `CfConnectSyncRoot` с таблицей callback'ов (D5).
4. Наполнение верхнего уровня плейсхолдерами; маркер готовности `OK, <имя тома> is ready` — один раз (см. D6: маркер = верхний уровень, не полный namespace).
5. Запуск фонового walk (D6, такт 2) и двух пулов воркеров (изоляция — D3):

| Пул | Что делает | Чего не делает |
|---|---|---|
| Гидратация | читает `pkg/fs`, вызывает `CfExecute(TRANSFER_DATA)` | не вызывает `CfHydratePlaceholder`, `CfSetPinState`, write-API `pkg/fs` |
| Выгрузка | читает локальный файл, пишет через `pkg/fs`, обновляет плейсхолдер | не обслуживает FETCH_DATA |

`CfHydratePlaceholder` блокирует вызывающего до завершения FETCH_DATA. Если его вызовет воркер гидратации, а пул занят такими же ожиданиями, наступает взаимная блокировка. Поэтому вызов разрешён только из пула выгрузки.

### Полный walk (upfront population, D6 — такт 2)

1. Регистрация sync root выполнена (ALWAYS_FULL + управление 0x7).
2. Provider устанавливает соединение с sync root.
3. Такт 1: `populateTopLevel` — листинг корня hub → конфликт-детект → `CfCreatePlaceholders` → индекс + журнал. Сбой или недоступность: retry 2с→30с, к шагу 4 не переходим.
4. Маркер `OK, <имя тома> is ready` (sync.Once).
5. Старт walk: очередь = каталоги верхнего уровня; пул из 4 воркеров BFS; таймер прогресса.
6. Рабочий цикл каталога D:
   6.1. листинг D из hub до конца; транзиентная ошибка → backoff 1с→30с и повтор; перманентная → Warn, `failed++`, поддерево пропускается;
   6.2. локальное перечисление D (case-insensitive); классификация to-create / existing / conflict (D6.5);
   6.3. `CfCreatePlaceholders` порциями ≤256; сбой на i → изоляция записи i одиночным вызовом (лог, skip), остаток снова батчем; «уже существует» = existing;
   6.4. индекс + журнал (только новые/изменённые), flush после порции;
   6.5. подкаталоги (to-create и existing) → в очередь; conflict-каталог и его поддерево не обходятся;
   6.6. счётчики; конфликт → один Warn на каталог.
7. Прогресс `walk:` не чаще раза в 5с, пока очередь или повторы не пусты.
8. Очередь пуста, нет каталогов в работе и в повторе → `walk complete:` ровно один раз.
9. Остановка `serve`: cancel → воркеры дорабатывают текущий вызов → flush → `walk cancelled:` → disconnect.
10. Рестарт: шаги 2–8; существующие имена пропускаются по индексу без Warn и без записи в журнал, недостающие (в том числе прерванные) создаются — идемпотентный дожим заменяет отдельную сверку при старте.

Параллельно с шагами 5–8 пользовательские операции: create в существующем каталоге завершается платформой без FETCH; create по несуществующему пути → `ERROR_PATH_NOT_FOUND`; гидрация созданных плейсхолдеров — FETCH_DATA как раньше (этап 3). Записи индекса, которых нет в hub, этапом 2 не удаляются (U16).

### Гидратация (D7)

Контракт этапа 1 (по замечанию ревью): реестр активных заданий диспетчера адресуется TransferKey; при двух конкурентных FETCH_DATA по одному файлу (общий TransferKey, разные RequestKey) отмена доходит только к последнему — на этапе 3 движок добавляет адресацию по RequestKey (FetchJob уже несёт оба ключа). Контракт `CfConnectSyncRoot` (закрытие в 1.6): таблица callback'ов и callback-контекст ДОЛЖНЫ удерживаться всё время соединения — объект соединения провайдера хранит их, а не передаёт временные значения.

1. Thunk копирует параметры FETCH_DATA в задание и ставит его в буферизованную очередь. При переполненной очереди thunk возвращает ошибку (U13).
2. Воркер берёт страница-выровненный буфер из пула (VirtualAlloc) и разрешает inode в путь.
3. Файл читается через `pkg/fs` порциями не больше 4 MiB; между порциями проверяется флаг отмены.
4. Каждое задание завершается ровно одним `CfExecute(TRANSFER_DATA)` — с данными или с кодом ошибки.

### Выгрузка (D8)

Write-путь берём через write-API `pkg/fs`, а не обращение к `pkg/vfs` writer'у напрямую: `pkg/fs` — тот же публичный слой, что используется для чтения; он уже управляет writer'ом, staging и подтверждением метаданных. Прямой вызов writer'а требует собственной обвязки `vfs.VFS`, inode и handle — дублирование того, что `pkg/fs` делает сам. Сигнатуры write-методов сверяются в `pkg/fs/fs.go` на шаге 5.1.

1. NOTIFY_FILE_CLOSE_COMPLETION: путь разрешается по U6. Файл без плейсхолдера и плейсхолдер вне in-sync ставятся в очередь выгрузки; прочие игнорируются.
2. Задание объединяется по пути и задерживается на интервал 2 с — чтобы исключить повторные закрытия от индексатора и антивируса.
3. Локальный файл открывается только на чтение с `FILE_SHARE_READ`: параллельная запись на время выгрузки исключена. При отказе в совместном доступе задание повторяется.
4. Содержимое пишется через `pkg/fs` блоками, кратными BlockSize из format. `Flush` — подтверждение метаданных.
5. Новый файл преобразуется в плейсхолдер с идентификатором inode (U8). Существующий — обновляется и помечается in-sync.

Write-back выключен: задание считается выполненным только после завершённого `Flush`.

### Отказоустойчивость write-пути (D9)

- **Атомарность.** Версия файла видна в backend только целиком: `Flush` writer'а подтверждает срезы одной записью метаданных.
- **Журнал.** Невыполненная операция (выгрузка, удаление, переименование) сохраняется в каталоге состояния и повторяется с нарастающей паузой, в том числе после перезапуска.
- **Обрыв выгрузки.** Локальный файл не трогается до подтверждения. После перезапуска файл выгружается повторно; повторная выгрузка той же версии меняет только идентификаторы срезов.
- **Удаление локального файла с незавершённой выгрузкой.** Задание отменяется. Если версия уже подтверждена в backend, в журнал ставится удаление по inode.
- **Сверка при старте (поправка пивота 27.11).** Namespace-часть сверки выполняется самим walk (D6: по-каталожное сравнение локальных имён с hub — create недостающих, skip+индекс существующих, skip+Warn конфликтов; идемпотентный дожим после прерывания). Write-часть остаётся: плейсхолдеры вне in-sync после walk → очередь выгрузки (этап 5); записи индекса, отсутствующие на диске в обработанных walk каталогах, → очередь удаления (U16: решение за владельцем).
- **Защита удалений.** В backend передаётся только удаление известного плейсхолдера. Sync root не передаётся никогда: `serve` при следующем старте пересоздаёт каталог и перерегистрирует.

## Invariants

- **Криптоцепочка не затрагивается.** FEK/CEK/AGDF, KeyManager, `meta.NewRenderMeta` — только через существующие слои (`pkg/fs`, write-API). Код расшифровки/шифрования в прототипе не пишется (инвариант SRS-001).
- **Thunk без ввода-вывода.** Внутри callback'а только копирование полей и постановка в очередь; hub, диск и `CfExecute` недоступны. Паника в thunk'е недопустима — она завершает процесс.
- **Ровно один `CfExecute` на задание гидратации** — с данными или с кодом ошибки.
- **Локальный файл пользователя не перезаписывается и не удаляется** провайдером при наполнении и сверке; конфликт — запись в лог и пропуск.
- **Удаление sync root никогда не передаётся в backend**; в backend уходят только удаления известных плейсхолдеров.
- **Write-back выключен**: задание выгрузки завершено только после `Flush` (подтверждение метаданных одной записью).
- **Биндинг без cgo**: `pkg/cfapi` и `pkg/cfprovider` не содержат cgo-файлов (проверка `go list -f "{{.CgoFiles}}"`).
- **Движок проверяется без CfAPI**: обращения к `cldapi.dll` только через интерфейс `pkg/cfapi`, тестируемый на подменной реализации.

## Decisions & Rationale

### D1. Go и биндинг через syscall

Биндинг — `windows.NewLazySystemDLL("cldapi.dll")` (x/sys, LOAD_LIBRARY_SEARCH_SYSTEM32; в stdlib `syscall` такой функции нет — замечание ревьюера подтверждено по x/sys v0.47.0) и вызовы через `LazyProc.Call`. Структуры `CF_*` — транскрипция `cldapi.h`. cgo не используется, поэтому биндинг не зависит от mingw и от невозможности чистой сборки (D2).

Конструкцию определяют ограничения `syscall.NewCallback`:

- функция уровня пакета, без замыканий; аргументы — размером с машинное слово;
- на стеке callback'а только копирование полей и постановка в очередь;
- адреса thunk'ов хранятся в переменных уровня пакета;
- паника в thunk'е недопустима — она завершает процесс.

Источник эталонного header (закрыто задачей 0.3, 2026-11-26): **NuGet `Microsoft.Windows.SDK.CPP` версии 10.0.26100.7175**, файл `c/Include/10.0.26100.0/um/cfapi.h`, **SHA-256 `c50fb112b31a38e91151b84c46fcfefeb395aa12faa60cdc270103cdce7c4ae7`** (1175 строк; 35 Cf-функций, 15 CF_CALLBACK_TYPE_*; все функции биндинга и write-пути присутствуют). В mingw-w64 (GitHub master) cfapi.h отсутствует — NuGet единственный источник. Локальный путь заголовка (вне репозитория): `C:/Users/iobukhov/jfs-tmp/cfapi-headers/cfapi.h`; архив nupkg удалён после извлечения. Header в репозиторий не коммитится. Имена функций сверены с экспортами `C:\Windows\System32\cldapi.dll` (`objdump -p`, задача 0.4): все функции D1/D5 и write-пути в экспортах присутствуют — CfRegisterSyncRoot, CfUnregisterSyncRoot, CfConnectSyncRoot, CfDisconnectSyncRoot, CfCreatePlaceholders, CfExecute, CfReportProviderProgress, CfHydratePlaceholder, CfDehydratePlaceholder, CfSetPinState, CfSetInSyncState, CfConvertToPlaceholder, CfUpdatePlaceholder, CfGetPlaceholderInfo, CfGetPlaceholderStateFromFileInfo, CfGetTransferKey/CfReleaseTransferKey, CfRevertPlaceholder, CfOpenFileWithOplock. Имена функций, callback-типов и флагов в этом change — рабочие; спека ссылается на список в этом разделе, а не на имена напрямую.

### D2. CGO: вариант α

Чистая сборка невозможна: `pkg/compress/compress.go:23-24` безусловно импортирует CGO-зависимые DataDog/zstd и hungys/go-lz4, а `pkg/chunk`, `pkg/object` и `pkg/fs` тянут этот пакет. Теги `nosqlite` (`pkg/meta/sql_sqlite.go:1`), `nofdb` (`pkg/meta/tkv_fdb.go:1`), `nogspt` этого не снимают (проверено сборкой `CGO_ENABLED=0` — exit 1 на zstd/lz4).

Прототип собирается с `CGO_ENABLED=1` под mingw gcc. Правок форка ради сборки нет. Рецепт уже существует — Makefile-таргет `juicefs.exe` (PATH `C:\mingw64\bin`, `CGO_ENABLED=1`, `CGO_CFLAGS='-IC:/WinFsp/inc/fuse'` — флаг нужен, т.к. `cmd` тянет `pkg/winfsp` через cgofuse). Задача 0.2 — стоп-точка: воспроизвести сборку ветки до написания кода.

Факт 0.2 (2026-11-26, стоп-точка пройдена): `PATH="/c/mingw64/bin:$PATH" CGO_ENABLED=1 CGO_CFLAGS='-IC:/WinFsp/inc/fuse' go build -ldflags='-s -w' -o juicefs.exe .` → `juicefs.exe` 137 MB; `go version go1.26.5 windows/amd64` (совпадает с go.mod), gcc из `C:\mingw64\bin`. Build-теги pkg/meta (задача 0.5): `pkg/meta/sql_sqlite.go` = `//go:build !nosqlite` (mattn/go-sqlite3, CGO), `pkg/meta/tkv_fdb.go` = `//go:build fdb` — опциональны для сокращения сборки, на CGO-вариант α не влияют.

Вариант β (замена на klauspost/zstd и pierrec/lz4 ради `CGO_ENABLED=0`) — отдельный change с предусловиями:

1. Подтверждён формат сжатия stage-томов (поле Compression в format).
2. Есть двунаправленные known-answer векторы: CGO-энкодер читается чистым декодером и наоборот.
3. Теги `nosqlite` и `nofdb` дают сборку без cgo-файлов во всём дереве.

### D3. Процессная модель и изоляция пулов

Один процесс `juicefs clouddrive serve`: соединение с sync root, client meta, chunk store, `pkg/fs`, два пула воркеров (см. Execution Flow — таблица изоляции и обоснование запрета `CfHydratePlaceholder` из пула гидратации) плюс фоновый walk-воркер population (D6, 4 воркера BFS).

**Поправка 27.11 (пивот)**: дефолт `--population-policy` в `register` — `ALWAYS_FULL(3)` (было PARTIAL). PARTIAL/FULL остаются для диагностики; при явном выборе — Warn «FETCH-режим: namespace неполный, пользовательский create не гарантируется» (эксперименты 1–5, U2/U14). Управление плейсхолдерами — маска 0x7 (флаг `--management-policy`, default 7). Политика фиксируется при регистрации: корни с прежним дефолтом пересоздаются (unregister → register; осиротевшие плейсхолдеры — U15).

### D4. Регистрация, готовность, владелец

- `register` проверяет, что том корня — NTFS, вызывает `CfRegisterSyncRoot` и создаёт ключ `Explorer\SyncRootManager\<providerId>!<SID>!<accountId>`. Ветка реестра — по пробе U1 (реализация пока HKCU). Фактические значения (задача 1.4, обновлено пивотом 27.11): providerId=`AgioDrive`, accountId=`Agio`, GUID провайдера `{3F7A2C5D-8E41-4B6A-9C2D-5A7E1F0B8C4D}`; политики: hydration=Progressive+StreamingAllowed, **population=ALWAYS_FULL(3) — дефолт с пивота 27.11** (флаг `--population-policy`, PARTIAL/FULL — диагностика с Warn), **management=UNRESTRICTED 0x7** (CREATE|CONVERT|UPDATE — write-путь В3; DEFAULT блокирует пользовательские операции — проба 0x80070781), inSync=TrackNone, hardlink=None; identity при регистрации — nil (плейсхолдеры получают inode-идентичность при создании, D6); повтор register → ретрай `RegisterFlagUpdate`. Идентичности могут уточниться по U1.
- `serve` при старте проверяет NTFS корня (требование proposal, рисковая таблица), затем регистрацию; при отсутствии сообщает об ошибке и завершается.
- Единственный экземпляр: именованный mutex `Local\juicefs-clouddrive-<санитизированный корень>` (initialOwner=false — существование как признак; ReleaseMutex не используется из-за thread-affinity горутин, «release» = CloseHandle в Stop; поведение задокументировано в pkg/cfprovider/provider.go); поведение второго `CfConnectSyncRoot` — по пробе U10.
- **Маркер готовности (поправка пивота 27.11)**: `OK, <имя тома> is ready` означает «соединение установлено, верхний уровень заселён» — НЕ полноту namespace. Выводится один раз (sync.Once), до завершения фонового walk и независимо от его хода; выводится до маркера-аналога WinFSP-паттерна (`winfs.go:1205-1218`). Полнота namespace — строка `walk complete:`, прогресс — строки `walk:`. Потребители маркера (tray, supervisor будущего integration-change) не должны читать его как окончание индексирования.
- Owner-override (`winfs.go:439-445`) не переносится: плейсхолдеры создаёт процесс провайдера от имени пользователя, владелец получается нужным без переопределения.
- **Наблюдение live 27.11**: в зарегистрированном, но НЕ обслуживаемом (provider не подключён) sync root создание обычного файла — Permission denied (cldflt fail-closed на неконнектнутом корне); при живом serve запись локального файла работает (проверяется в сценарии «Одноимённый локальный файл»). Для прототипа — задокументированное поведение окружения, не bug: работать с корнем вне serve нельзя, это цена регистрации.

### D5. Таблица callback'ов

Реальность CfAPI (подтверждено по `cldapi.h`, проба U9 закрыта): **13 callback-типов; отдельного NOTIFY_DIRECTORY_CREATED не существует** — создание каталога попадает в NOTIFY_RENAME/NOTIFY_DELETE с флагом `*_IS_DIRECTORY`. Поэтому «10 OS-thunk'ов» физически нереализуем: реализовано **7 OS-thunk'ов на 9 OS-точек** — RENAME и DELETE маршрутизируют файл/каталог по флагам `CF_CALLBACK_NOTIFICATION_FLAG_*_IS_DIRECTORY`. Десятая точка движка (`OnNotifyDirectoryCreated`) остаётся в интерфейсе `CallbackDispatcher` — при отрицательном исходе U9 её наполняет запасной механизм (ReadDirectoryChangesW-журнал). PRE_DELETE и PRE_RENAME не регистрируются: вето на операции прототипу не нужно. Thunk'и возвращают 0 — `CF_CALLBACK` в header есть `VOID`-функция, результат фильтром игнорируется; возврат ошибки фильтру — предмет пробы U13 (этап 3).

| Точка D5 | OS-callback (реальный) | Действие | Шаг |
|---|---|---|---|
| Гидратация: постановка задания | FETCH_DATA | задание в очередь гидратации | 3 |
| Гидратация: отмена | CANCEL_FETCH_DATA | флаг отмены по TransferKey | 3 |
| Наполнение каталога | FETCH_PLACEHOLDERS | наполнение, если подтверждается U2 | 2 |
| Отмена наполнения | CANCEL_FETCH_PLACEHOLDERS | флаг отмены | 2 |
| Выгрузка нового/изменённого | NOTIFY_FILE_CLOSE_COMPLETION | классификация, постановка выгрузки | 5 |
| Переименование файла / каталога | NOTIFY_FILE_RENAMED | переименование в backend, обновление индекса (флаг IS_DIRECTORY) | 6 |
| Удаление файла / каталога | NOTIFY_FILE_DELETED | удаление в backend (флаг IS_DIRECTORY) | 6 |
| Создание каталога | *OS-типа нет* — из NOTIFY-флагов или запасной механизм U9 | Mkdir в backend, плейсхолдер каталога | 6 |
| Переименование каталога | NOTIFY_FILE_RENAMED (IS_DIRECTORY) | то же, точка 6 | 6 |
| Удаление каталога | NOTIFY_FILE_DELETED (IS_DIRECTORY) | Rmdir в backend | 6 |

### D6. Population: полный upfront-walk hub при старте (пивот 27.11 [GO владельца])

Namespace sync root строится целиком из метаданных hub рекурсивным обходом (walk). FETCH-популяция остаётся в коде как fallback: воркер зарегистрирован и покрыт тестами, но при дефолтной политике `ALWAYS_FULL` платформа его не вызывает (эксперимент 6) и завершает пользовательский create сама. Основание — экспериментальная матрица 26–27.11 (Open Questions, U2/U14): режимы PARTIAL/FULL не дают фильтру завершённого namespace для user-create при любом содержимом FETCH-ответов.

**6.1. Два такта.**
- Такт 1 — синхронно, до маркера: `populateTopLevel` без изменений (листинг корня, конфликт-детект, `CfCreatePlaceholders`, индекс + журнал). Ошибка hub — retry 2с→30с, маркера нет.
- Такт 2 — фоново, сразу после маркера: walk глубины от каталогов верхнего уровня, goroutine с контекстом Provider. Остановка `serve`: cancel → доработка текущего вызова → flush журнала → disconnect.

**6.2. Маркер после верхнего уровня, а не после полного walk.** Полный walk на 1M записей — десятки минут (см. Риски); блокировать готовность нельзя. Цена — фронтир: каталог-плейсхолдер существует, записи появляются по мере дохода walk. Перечисление необработанного каталога возвращает пусто; FETCH не приходит. Create в существующем каталоге завершается платформой (одноимённая hub-запись станет конфликтом на walk — skip+Warn, локальная остаётся). Create по несозданному пути → `ERROR_PATH_NOT_FOUND` — детерминированный отказ, «папки ещё нет в облаке»; дерево наполняется прогрессивно, как индексирование LucidLink; признак полноты — строка `walk complete:`.

**6.3. Обход.** BFS (FIFO), пул из 4 воркеров, листинг hub той же функцией, что в populateTopLevel, с пагинацией до конца. Каталог обрабатывается после создания своего плейсхолдера (создаётся в батче родителя). BFS, а не DFS: уровень заполняется целиком (все компании, затем все проекты). Логика walk не зависит от `pkg/cfapi` напрямую — через интерфейсы «листинг + создатель» (юнит-тесты на любой ОС).

**6.4. Создание — массив, не поштучно.** Один каталог — один вызов `CfCreatePlaceholders`, порциями ≤256 записей (поштучно = 1M round-trip против ~4k вызовов). Батч останавливается на первой сбойной записи — изоляция обязательна: границы успешных по `EntriesProcessed`, per-entry `Result` читается как сейчас; сбойная запись i — одиночным вызовом для точной ошибки, лог, skip (без повторов); остаток i+1..n — снова батчем; «уже существует» = existing.

**6.5. Классификация** — по локальному перечислению имён без учёта регистра (Windows-семантика): **to-create** (имени нет → в батч), **existing** (есть, известна индексу → skip без Warn, индекс актуализируется), **conflict** (есть, не в индексе → skip, один Warn на каталог, в индекс не попадает).

**6.6. Сбои листинга.** Транзиентные (сетевые, 408/429/5xx) — retry backoff 1с→30с без ограничения; перманентные (прочие 4xx) — Warn, `failed++`, поддерево пропускается.

**6.7. Индекс и журнал.** Запись после каждой порции, только новые/изменившиеся записи: повторный walk на неизменном hub добавляет 0 строк.

**6.8. Лог** (стабильные форматы для grep): `walk: dirs=%d files=%d skipped=%d conflicts=%d failed=%d pending=%d elapsed=%s` (Info, не чаще раза в 5с); `walk complete: ...` — ровно один раз; `walk cancelled: ...` — при остановке; `walk conflict: <dir>: skipped %d hub entries, e.g. %v`; `walk failed: <dir>: %v (subtree skipped)`.

**6.9. Non-goals этапа 2**: resume-cursor, приоритет recently-used, удаление stale-плейсхолдеров (U16), актуализация namespace в сессии (U17), walk-статус в `status`. Решения — после M1.

Сохраняются: идентификатор плейсхолдера FileIdentity = inode (8B LE); индекс `ino→путь` + журнал + ResolveIno-подъём (U12); флаги создания `MARK_IN_SYNC` (сверка старта не ставит копии backend в очередь выгрузки; сочетание с `inSync=TrackNone` — проба U8, этап 5); локальный не-плейсхолдер никогда не перезаписывается/не удаляется (конфликт → skip+лог).

### D8. Write-путь и отказоустойчивость

Алгоритм и правила отказоустойчивости — в Execution Flow (разделы «Выгрузка», «Отказоустойчивость write-пути»); решением здесь фиксируется выбор слоя: write-API `pkg/fs` (обоснование — там же) и отказ от write-back.

### D10. Интеграция с agio-drive — отдельный change

1. Требует зависимости от модуля `agio-band.gitlab.yandexcloud.net/agio-band/agio-drive` и agio-login v0.0.12. GitLab сейчас недоступен с машины, а в кэше только v0.0.8 с другим API. Включение в этот change заблокировало бы его DoD.
2. Module path форка — `github.com/juicedata/juicefs`. agio-drive не может импортировать форк без `replace`; направление зависимости требует отдельного решения.
3. Tray и supervisor общие для линий WinFSP и CfAPI. Этот change передаёт им только контракт — маркер готовности и единственный экземпляр.

### D11. Программа замеров и языковой гейт (решение владельца 2026-11-26, Путь A)

Стратегическая цель за прототипом — движок уровня LucidLink для CG/VFX (миллионы мелких файлов, частые metadata-операции из многих потоков). Решение «Go остаётся языком движка V3 / порт движка в Rust (или C++)» принимается **по замерам прототипа**, не по вкусу. Прототип — бенчмарк-стенд; наработанные U1–U13 и семантика переносимы на любой язык, биндинг и провайдер (~1.5–2.5k строк) — осознанный расход в случае смены языка.

| # | Метрика | Метод | Задача |
|---|---|---|---|
| M1 | metadata ops/s: наполнение плейсхолдерами + readdir каталога backend с 100k записей | таймер вокруг прохода наполнения/листинга | 6.6 |
| M2 | streaming MB/s гидратации при N=1/8/64 параллельных FETCH_DATA на файле ≥1 GiB | таймер + provider progress; очередь и число воркеров фиксируются | 3.5 |
| M3 | heap/GC: размер heap, частота и латентность GC-пауз, allocation rate в hot path | `runtime/metrics` во время прогонов 3.5/6.6 и циклов 7.5 | 7.8 |

Целевые значения **подтверждены владельцем 2026-11-26** (решение до шага 3, условие D11 выполнено): M1 ≥ 10k ops/s; M2 — на уровне линии тестового канала (доля ≥80% от сырого S3-GET той же машины); M3 — GC-паузы > 10 мс отсутствуют, allocation rate hot path ≈ 0 после прогрева пулов.

Гейт: все цели достигнуты → Go остаётся в линии V3; цели не достигнуты → порт движка в Rust по фреймингу «чистый клиент протокола хаба» (gRPC MetaService + S3-форматы + AGDF/AGCK/AGFK c known-answer векторами + STS), этот прототип — оракул поведения. Итог гейта фиксируется в ADR-005 (задача 7.9).

## Integration Points

### Обвязка data path (повторяет gc-паттерн и STS-выбор mount)

```go
// cmd/clouddrive.go — обвязка повторяет gc-паттерн (cmd/gc.go:96-110)
// и выбор STS-провайдера mount (cmd/render_mount.go:346-381, cmd/sts_refresher.go:219).
m := meta.NewClient(metaURI) // grpc://…?tls=1&oidc-issuer=… ; pkg/meta/grpc_client.go:325
format := m.Load(true)
chunkConf := getDefaultChunkConf(format)
store := chunk.NewCachedStore(createStorage(format), chunkConf, registry) // cached_store.go:1006, cmd/format.go:240
vfsConf := &vfs.Config{Format: &format, Chunk: &chunkConf /* Meta, Security, HubState — как в mount */}
fsys := fs.NewFileSystem(vfsConf, m, store, nil) // pkg/fs/fs.go:180, pkg/vfs/vfs.go:124
```

Хранилище в реальном коде получает обновляемые STS-креды той же последовательностью, что и mount. Общая часть выделяется в хелпер `cmd`, чтобы mount и clouddrive не разошлись. `WriteJournalPath` в прототипе не заполняется, пока U11 не покажет необходимости.

### Существующие точки интеграции

| Точка | Что используется |
|---|---|
| `fs.NewFileSystem(conf, m, store, registry)` — `pkg/fs/fs.go:180` | path-based API: `Open` (:401), `Stat` (:467), `Readdir` (:1524), `File.Pread` (:1377); write-API сверяется на шаге 5.1 |
| `meta.NewClient("grpc://…")` — `pkg/meta/grpc_client.go:325` | URI query: `oidc-issuer`, обязательный `oidc-client-id`, `tls=1`, `tls-ca`; Bearer — `oidc.TokenManager` (`pkg/oidc/token_manager.go:89`) |
| STS — `cmd/render_mount.go:346-381`, `cmd/sts_refresher.go:219` | `platformNodeSTSProvider` / `newSTSRefresher`; креды object storage как в mount |
| `chunk.NewCachedStore` — `pkg/chunk/cached_store.go:1006`, `createStorage` — `cmd/format.go:240` | chunk store поверх S3; bucket/параметры из format |
| Маркер готовности — паттерн `pkg/winfsp/winfs.go:1205-1218` | `OK, <имя тома> is ready` — контракт для будущего supervisor'а agio-drive |

### Скелет ключевых блоков

```go
// pkg/cfapi/callbacks.go — thunk'и: функции уровня пакета, без замыканий.
var (
	fetchDataThunk   = syscall.NewCallback(onFetchData)
	cancelFetchThunk = syscall.NewCallback(onCancelFetchData)
	// по одному на каждый тип из D5; всего десять
)

func onFetchData(info *CFCallbackInfo, params *CFCallbackParameters) uintptr {
	return dispatcher.OnFetchData(newFetchJob(info, params)) // копия полей, без I/O
}
```

```go
// pkg/cfprovider/provider.go
type Provider struct {
	root    string              // %USERPROFILE%\AgioDrive
	conn    cfapi.ConnectionKey // результат CfConnectSyncRoot
	api     cfapi.SyncRootAPI   // реальный биндинг или подменная реализация в тестах
	vfs     *fs.FileSystem      // pkg/fs/fs.go:180
	idx     *Index              // ino → путь, журнал плейсхолдеров
	journal *Journal            // невыполненные операции над backend
	fetches *Fetches            // активные задания и их отмена
	fetch   chan FetchJob       // очередь пула гидратации
	ingest  chan JournalOp      // очередь пула выгрузки
	bufs    *BufferPool         // страница-выровненные буферы, VirtualAlloc
}
```

```go
// pkg/cfprovider/fetch.go
type FetchJob struct {
	Conn   cfapi.ConnectionKey
	FileID uint64 // FileIdentity плейсхолдера = inode
	Off    int64
	Len    int64
	Req    cfapi.RequestKey
	Cancel atomic.Bool // устанавливается CANCEL_FETCH_DATA
}

func (p *Provider) runFetch(ctx context.Context, job *FetchJob) {
	defer p.fetches.Remove(job)
	buf, free := p.bufs.Get(int(job.Len)) // страница-выровнено
	defer free()
	if job.Cancel.Load() {
		p.finishFetch(job, 0, cfapi.StatusCancelled) // ровно один CfExecute на задание
		return
	}
	path, ok := p.idx.Path(job.FileID)
	if !ok {
		path, ok = p.idx.ResolveIno(ctx, job.FileID) // подъём по родителям; U12
	}
	if !ok {
		p.finishFetch(job, 0, cfapi.StatusNoSuchFile)
		return
	}
	f, errno := p.vfs.Open(ctx, path, 0) // pkg/fs/fs.go:401
	if errno != 0 {
		p.finishFetch(job, 0, cfapi.StatusFromErrno(errno))
		return
	}
	defer f.Close()
	for done := int64(0); done < job.Len; done += p.cfg.FetchBlock {
		if job.Cancel.Load() {
			p.finishFetch(job, 0, cfapi.StatusCancelled)
			return
		}
		end := min(done+p.cfg.FetchBlock, job.Len)
		if _, errno := f.Pread(ctx, buf[done:end], job.Off+done); errno != 0 { // pkg/fs/fs.go:1377
			p.finishFetch(job, 0, cfapi.StatusFromErrno(errno))
			return
		}
	}
	p.finishFetch(job, job.Len, cfapi.StatusSuccess)
}
```

```go
// pkg/cfprovider/ingest.go
func (p *Provider) runIngest(ctx context.Context, op JournalOp) error {
	src := openLocalSharedRead(op.Path) // CreateFile: GENERIC_READ, FILE_SHARE_READ — исключает запись при выгрузке
	defer src.Close()
	dst, errno := p.vfs.Open(ctx, op.Path, vfs.O_WRONLY|vfs.O_CREAT|vfs.O_TRUNC) // write-API pkg/fs; сверка на 5.1
	if errno != 0 {
		return errno
	}
	// Запись блоками, кратными BlockSize из format.
	// Flush — подтверждение метаданных; write-back выключен.
	// Затем преобразование нового файла в плейсхолдер либо обновление существующего с in-sync (U8).
	return nil
}
```

## Open Questions

Результаты каждой пробы вписываются в таблицу; изменение решения после пробы — в Decisions & Rationale.

| ID | Неопределённость | Проба | Шаг | Запасной вариант |
|---|---|---|---|---|
| U1 | Права на `CfRegisterSyncRoot`; должен ли корень быть пустым; какая ветка реестра нужна Explorer | регистрация из обычного и повышенного процесса | 1 | разовая регистрация с повышением на этапе установки — **ЗАКРЫТО 26.11 (пробы на живой машине): регистрация из ОБЫЧНОГО процесса ОК (exit 0); корень непустой (probe.txt) — не мешает; ветка HKCU достаточна (Explorer\SyncRootManager\AgioDrive!SID!Agio: DisplayName/IconResource/UserSyncRoots\<SID>); идемпотентный повтор ОК (RegisterFlagUpdate, ключ один); unregister удаляет ключ, status → not registered; identity=nil возражений от cldapi нет** |
| U2 | Когда выдаётся FETCH_PLACEHOLDERS и как доставляются записи | трассировка callback'ов при открытии каталога | 2 | уведомление об открытии каталога и `CfCreatePlaceholders` — **ЗАКРЫТ ПИВОТОМ 27.11 [GO владельца]: 6 живых экспериментов показали, что FETCH-режимы (PARTIAL=0/FULL=2) НЕ дают фильтру завершённого namespace для пользовательского create независимо от содержимого ответа (existing-в-ответе → цикл ~15 req/5ms; total=EOF → 60с-таймаут-циклы; fresh-only+processed=0 → пенд вечно; даже instant-complete на пустом backend → мгновенный отказ 0x80070781). Единственный рабочий режим — ALWAYS_FULL(3): create завершается платформой, FETCH не вызывается. Решение — D6: полный upfront-walk; FETCH-механизм остаётся в коде как fallback. Установленная попутно семантика: NormalizedPath = путь без буквы диска (`\Users\...`); ParamSize = sizeof(CF_OPERATION_PARAMETERS) (union); EntriesProcessed — кумулятивное условие processed>=total; существующие-в-ответе отвергаются** |
| U3 | Передаётся ли FileIdentity в параметрах FETCH_DATA | логирование первого callback'а | 3 | чтение по inode: `meta.Read` и store (cmd/gc.go:96-110) |
| U4 | Выравнивание и максимальный размер буфера TRANSFER_DATA | передача выровненным буфером; замер Length | 3 | VirtualAlloc под запрос целиком |
| U5 | Рекурсивная гидратация по `CfSetPinState`; вызов из другого процесса; встроенное меню Explorer без MSIX | `pin` каталога; вызов из CLI-процесса | 4 | pin через процесс serve по локальному каналу управления |
| U6 | Получение пути файла в NOTIFY-callback'ах, особенно для ещё не-плейсхолдеров | проба на новом файле | 5 | журнал ReadDirectoryChangesW; NOTIFY как барьер |
| U7 | Открытие на запись частично дегидратированного плейсхолдера | проба | 5 | `CfHydratePlaceholder` из пула выгрузки перед чтением |
| U8 | Имена и флаги преобразования в плейсхолдер и пометки in-sync | по `cldapi.h` | 5 | — |
| U9 | Все десять callback-типов существуют с 1709 | по `cldapi.h` и MS Docs | 1 | сократить набор — **ЗАКРЫТО 26.11: типов 13, NOTIFY_DIRECTORY_CREATED не существует; 7 OS-thunk'ов на 9 OS-точек через *_IS_DIRECTORY-флаги (см. D5); 10-я точка движка наполняется запасным механизмом** |
| U10 | Второй `CfConnectSyncRoot` на тот же корень | запуск второго `serve` | 1 | именованный mutex как единственная защита — **ЗАКРЫТО 26.11: второй serve → exit=1 FATAL «another provider instance ... already running (mutex Local\juicefs-clouddrive-\<san>)» ДО Connect; первый жив (mutex удержан). Также первый живой CfConnectSyncRoot прошёл (connected, loggingDispatcher, idle)** |
| U11 | Очистка staging-файлов juicefs после падения | чтение pkg/chunk и pkg/vfs | 5 | очистка при старте в провайдере |
| U12 | Наличие родителя в Attr для подъёма inode → путь | pkg/meta | 3 | обход от корня |
| U13 | Завершает ли запрос ошибку, возвращённую из callback'а | возврат ошибки из thunk'а | 3 | отдельное `CfExecute` с ошибкой из воркера |
| U14 | Population-семантика пользовательского create (пиво 27.11) | матрица population/mgmt × содержимое FETCH-ответов (6 экспериментов) | 2 | — — **ЗАКРЫТ для этапа 2: create завершается ТОЛЬКО при ALWAYS_FULL + management 0x7, платформой, без FETCH. Во фронтире (D6.2): существующий каталог → create OK (одноимённая hub-запись станет конфликтом на walk); отсутствующий путь → ERROR_PATH_NOT_FOUND. Остаток к write-пути (В3): приходят ли NOTIFY на такие create** |
| U15 | Orphan-placeholder cleanup (пиво 27.11) | unregister+kill → попытка удаления плейсхолдера | 6 | — **ОТКРЫТ: осиротевшие плейсхолдеры блокируют удаление («cloud file provider exited unexpectedly»), NOTIFY_DELETE требует ответа. Кандидаты: cleanup при повторной регистрации (serve отвечает и удаляет) либо команда `clouddrive revert` (CfRevertPlaceholder). До этапа 6 — тесты на чистом root** |
| U16 | Stale-плейсхолдеры (локальный чистый плейсхолдер, отсутствующий в hub) | walk-сравнение | 6 | удалять на walk или нет; защита локальных «грязных» записей — решение за владельцем — **ОТКРЫТ** |
| U17 | Актуальность namespace в сессии (изменения hub от других клиентов) | наблюдение | post-write | periodic re-walk или change feed — **ОТКРЫТ, этап после write-пути** |

## MVP-лестница

| Шаг | Результат | Закрывает |
|---|---|---|
| 0 | Предусловия: сборка, header, экспорты | — |
| 1 | Биндинг, регистрация, подключение, один экземпляр | U1, U9, U10 |
| 2 | Плейсхолдеры, индекс, маркер готовности | U2 |
| 3 | Гидратация и отмена (+замер M2) | U3, U4, U12, U13 |
| 4 | Pin и unpin | U5 |
| 5 | Выгрузка новых и изменённых файлов | U6, U7, U8, U11 |
| 6 | Удаление, переименование, каталоги, офлайн, сверка (+замер M1) | — |
| 7 | Проход DoD (+M3 и языковой гейт D11) | таблица U закрыта |
