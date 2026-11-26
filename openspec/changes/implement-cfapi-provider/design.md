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
4. Наполнение верхнего уровня плейсхолдерами; маркер готовности `OK, <имя тома> is ready` — один раз.
5. Запуск двух пулов воркеров (изоляция — D3):

| Пул | Что делает | Чего не делает |
|---|---|---|
| Гидратация | читает `pkg/fs`, вызывает `CfExecute(TRANSFER_DATA)` | не вызывает `CfHydratePlaceholder`, `CfSetPinState`, write-API `pkg/fs` |
| Выгрузка | читает локальный файл, пишет через `pkg/fs`, обновляет плейсхолдер | не обслуживает FETCH_DATA |

`CfHydratePlaceholder` блокирует вызывающего до завершения FETCH_DATA. Если его вызовет воркер гидратации, а пул занят такими же ожиданиями, наступает взаимная блокировка. Поэтому вызов разрешён только из пула выгрузки.

### Гидратация (D7)

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
- **Сверка при старте.** Локальные файлы без плейсхолдера и плейсхолдеры вне in-sync → очередь выгрузки. Записи индекса, отсутствующие на диске в наполненных каталогах, → очередь удаления.
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

Биндинг — `syscall.NewLazySystemDLL("cldapi.dll")` и `syscall.SyscallN`. Структуры `CF_*` — транскрипция `cldapi.h`. cgo не используется, поэтому биндинг не зависит от mingw и от невозможности чистой сборки (D2).

Конструкцию определяют ограничения `syscall.NewCallback`:

- функция уровня пакета, без замыканий; аргументы — размером с машинное слово;
- на стеке callback'а только копирование полей и постановка в очередь;
- адреса thunk'ов хранятся в переменных уровня пакета;
- паника в thunk'е недопустима — она завершает процесс.

Источник эталонного header: SDK 10.0.26100.0 его не содержит (cldapi.lib есть). Берётся пакет `Microsoft.Windows.SDK.CPP` на NuGet, либо заголовки mingw-w64. Источник, версия и SHA-256 файла записываются в разделе Integration Points при закрытии задачи 0.3; header в репозиторий не коммитится. Имена функций дополнительно сверяются с экспортами `C:\Windows\System32\cldapi.dll` (`objdump -p` из mingw). Имена функций, callback-типов и флагов в этом change — рабочие; спека ссылается на список в этом разделе, а не на имена напрямую.

### D2. CGO: вариант α

Чистая сборка невозможна: `pkg/compress/compress.go:23-24` безусловно импортирует CGO-зависимые DataDog/zstd и hungys/go-lz4, а `pkg/chunk`, `pkg/object` и `pkg/fs` тянут этот пакет. Теги `nosqlite` (`pkg/meta/sql_sqlite.go:1`), `nofdb` (`pkg/meta/tkv_fdb.go:1`), `nogspt` этого не снимают (проверено сборкой `CGO_ENABLED=0` — exit 1 на zstd/lz4).

Прототип собирается с `CGO_ENABLED=1` под mingw gcc. Правок форка ради сборки нет. Рецепт уже существует — Makefile-таргет `juicefs.exe` (PATH `C:\mingw64\bin`, `CGO_ENABLED=1`, `CGO_CFLAGS='-IC:/WinFsp/inc/fuse'` — флаг нужен, т.к. `cmd` тянет `pkg/winfsp` через cgofuse). Задача 0.2 — стоп-точка: воспроизвести сборку ветки до написания кода.

Вариант β (замена на klauspost/zstd и pierrec/lz4 ради `CGO_ENABLED=0`) — отдельный change с предусловиями:

1. Подтверждён формат сжатия stage-томов (поле Compression в format).
2. Есть двунаправленные known-answer векторы: CGO-энкодер читается чистым декодером и наоборот.
3. Теги `nosqlite` и `nofdb` дают сборку без cgo-файлов во всём дереве.

### D3. Процессная модель и изоляция пулов

Один процесс `juicefs clouddrive serve`: соединение с sync root, client meta, chunk store, `pkg/fs`, два пула воркеров (см. Execution Flow — таблица изоляции и обоснование запрета `CfHydratePlaceholder` из пула гидратации).

### D4. Регистрация, готовность, владелец

- `register` проверяет, что том корня — NTFS, вызывает `CfRegisterSyncRoot` и создаёт ключ `Explorer\SyncRootManager\<providerId>!<SID>!<accountId>`. Ветка реестра — по пробе U1.
- `serve` при старте проверяет регистрацию; при отсутствии сообщает об ошибке и завершается.
- Единственный экземпляр: именованный mutex (x/sys/windows) и поведение второго `CfConnectSyncRoot` по U10.
- Маркер готовности повторяет адаптер WinFSP (`pkg/winfsp/winfs.go:1205-1218` — ветка `implement-windows-mount`): строка `OK, <имя тома> is ready` выводится один раз, когда соединение установлено и плейсхолдеры верхнего уровня созданы.
- Owner-override (`winfs.go:439-445`) не переносится: плейсхолдеры создаёт процесс провайдера от имени пользователя, владелец получается нужным без переопределения.

### D5. Таблица callback'ов

Регистрируются десять типов. PRE_DELETE и PRE_RENAME не регистрируются: вето на операции прототипу не нужно.

| Callback | Действие | Шаг |
|---|---|---|
| FETCH_DATA | постановка задания гидратации | 3 |
| CANCEL_FETCH_DATA | установка флага отмены | 3 |
| FETCH_PLACEHOLDERS | наполнение каталога, если подтверждается U2 | 2 |
| CANCEL_FETCH_PLACEHOLDERS | отмена наполнения | 2 |
| NOTIFY_FILE_CLOSE_COMPLETION | классификация файла, постановка выгрузки | 5 |
| NOTIFY_FILE_RENAMED | переименование в backend | 6 |
| NOTIFY_FILE_DELETED | удаление в backend | 6 |
| NOTIFY_DIRECTORY_CREATED | Mkdir в backend, плейсхолдер каталога | 6 |
| NOTIFY_DIRECTORY_RENAMED | переименование в backend, обновление индекса | 6 |
| NOTIFY_DIRECTORY_DELETED | Rmdir в backend | 6 |

### D6. Индекс и плейсхолдеры

- Идентификатор плейсхолдера (FileIdentity) — inode в backend, 8 байт little-endian.
- Индекс `ino → путь` держится в памяти и дублируется журналом в каталоге состояния. При промахе inode разрешается в путь подъёмом по родителям через meta (U12); запасной вариант — обход от корня.
- Наполнение: верхний уровень — при старте; дети каталога — при первом обращении. Основной механизм — FETCH_PLACEHOLDERS; запасной (U2) — уведомление об открытии каталога и `CfCreatePlaceholders`.
- Локальный файл, не являющийся плейсхолдером, с именем удалённой записи не перезаписывается и не удаляется. Конфликт пишется в лог, запись пропускается.

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

Целевые значения — индикативные, владелец подтверждает до шага 3: M1 ≥ 10k ops/s; M2 — на уровне линии тестового канала (доля ≥80% от сырого S3-GET той же машины); M3 — GC-паузы > 10 мс отсутствуют, allocation rate hot path ≈ 0 после прогрева пулов.

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
| U1 | Права на `CfRegisterSyncRoot`; должен ли корень быть пустым; какая ветка реестра нужна Explorer | регистрация из обычного и повышенного процесса | 1 | разовая регистрация с повышением на этапе установки |
| U2 | Когда выдаётся FETCH_PLACEHOLDERS и как доставляются записи | трассировка callback'ов при открытии каталога | 2 | уведомление об открытии каталога и `CfCreatePlaceholders` |
| U3 | Передаётся ли FileIdentity в параметрах FETCH_DATA | логирование первого callback'а | 3 | чтение по inode: `meta.Read` и store (cmd/gc.go:96-110) |
| U4 | Выравнивание и максимальный размер буфера TRANSFER_DATA | передача выровненным буфером; замер Length | 3 | VirtualAlloc под запрос целиком |
| U5 | Рекурсивная гидратация по `CfSetPinState`; вызов из другого процесса; встроенное меню Explorer без MSIX | `pin` каталога; вызов из CLI-процесса | 4 | pin через процесс serve по локальному каналу управления |
| U6 | Получение пути файла в NOTIFY-callback'ах, особенно для ещё не-плейсхолдеров | проба на новом файле | 5 | журнал ReadDirectoryChangesW; NOTIFY как барьер |
| U7 | Открытие на запись частично дегидратированного плейсхолдера | проба | 5 | `CfHydratePlaceholder` из пула выгрузки перед чтением |
| U8 | Имена и флаги преобразования в плейсхолдер и пометки in-sync | по `cldapi.h` | 5 | — |
| U9 | Все десять callback-типов существуют с 1709 | по `cldapi.h` и MS Docs | 1 | сократить набор |
| U10 | Второй `CfConnectSyncRoot` на тот же корень | запуск второго `serve` | 1 | именованный mutex как единственная защита |
| U11 | Очистка staging-файлов juicefs после падения | чтение pkg/chunk и pkg/vfs | 5 | очистка при старте в провайдере |
| U12 | Наличие родителя в Attr для подъёма inode → путь | pkg/meta | 3 | обход от корня |
| U13 | Завершает ли запрос ошибку, возвращённую из callback'а | возврат ошибки из thunk'а | 3 | отдельное `CfExecute` с ошибкой из воркера |

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
