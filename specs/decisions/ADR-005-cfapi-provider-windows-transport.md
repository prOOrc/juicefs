# ADR-005: Windows-транспорт V3 — провайдер Cloud Files API на Go (замена WinFSP)

| Поле | Значение |
|---|---|
| Статус | accepted (27.11: DoD 57/57, гейт D11 — Go остаётся; владелец подтвердил прототип живьём — facility-маунт vector-stage: OIDC+STS+walk+upload+чтение AGDF через FETCH, кросс-машинная видимость Mac↔Windows; change `implement-cfapi-provider` заархивирован) |
| Дата | 2026-11-26 |
| Контекст | Решение владельца о V3 (нативные плейсхолдеры Windows, pin/hydrate, устойчивость к офлайну); сравнение трёх технологий (Go syscall / C# P/Invoke / C++ native) на базе исследования сэмпла Microsoft CloudMirror, клиента agio-drive (master a67b6ac) и форка (agio-drive-v2); вердикт конвейера explorer→architect 2026-11-26 |
| Связанные | ADR-004 (data-plane креденшелы — не меняются), SRS-001 (криптоцепочка — инвариант), `implement-cfapi-provider` (прототип), follow-up `implement-cfapi-drive-integration`, домен `domain-windows-mount` (линия WinFSP — живёт параллельно) |

## Контекст

V1-клиент (rclone mount через WinFSP, CGO) отдаёт файлы только через процесс смонтированной ФС:
нет нативных плейсхолдеров Windows, нет pin/hydrate, нет чтения гидратированных данных без
участия провайдера. Windows Cloud Files API (cldflt.sys + cldapi.dll) даёт эти свойства
нативно (модель OneDrive). Требуется выбрать язык/механизм реализации провайдера:

1. **Go + биндинг cldapi.dll через `syscall`** (без cgo);
2. **C# + P/Invoke** (готовые community-обёртки: PrimalZed/CloudSync, styletronix/cfapiSync);
3. **Native C++** (как официальный сэмпл Microsoft CloudMirror).

Фактура сравнения (исследование 2026-11-26):

- Поверхность CfAPI для MVP мала: ~7 функций, 2–5 callback'ов; callback'и неблокирующие,
  очереди сообщений не требуют; ядро провайдера в сэмпле — 350–450 строк CfAPI-специфики
  (минимальный скелет 600–900 строк); shell-интеграция — опциональные ~800 строк.
- Регистрация sync root: WinRT-путь сэмпла требует package identity (MSIX); чистый Win32
  путь (`CfRegisterSyncRoot` + реестр `SyncRootManager`, мин. Win10 1709) package identity
  не требует — путь проверен сообществом.
- Единственный код, который нельзя дёшево перенести на другой язык, написан на Go:
  ядро форка (meta grpc/OIDC/TLS, chunk store, `pkg/fs`, криптоцепочка FEK/CEK/AGDF —
  SRS-001) и облако клиента agio-drive (agio-login OIDC, genqlient GraphQL, конфиг, tray).
- Готового Go-биндинга CfAPI не существует (в отличие от Rust `wincs`/`cloud-filter` и
  C#-обёрток); оценка собственного — 600–900 строк (pkg/cfapi).
- Ветки C# и C++ означают либо второй процесс (Go sidecar + IPC-контракт с ACL и
  lifecycle), либо дублирование data path и криптоцепочки на втором языке — для прототипа
  недопустимо (расхождение с V2).
- Чистая сборка форка без CGO невозможна (`pkg/compress` безусловно тянет CGO zstd/lz4),
  но Windows-сборка с CGO под mingw — уже существующий Makefile-таргет `juicefs.exe`.

## Решения владельца (зафиксированы 2026-11-26)

- **В1**: прототип живёт в форке juicefs, ветка от `agio-drive-v2` (== origin/main);
  agio-drive V1 не меняется.
- **В2**: hub-доступ прототипа — OIDC через agio-login + STS (проверенный путь
  `grpc://…?oidc-…&tls=1`, `cmd/render_mount.go`, `cmd/sts_refresher.go`).
- **В3**: write-путь (минимальный upload) входит в скоуп прототипа — отклонение от
  рекомендации архитектора «read-only».
- **В4**: корень sync root — папка в профиле (`%USERPROFILE%\AgioDrive`), не буква диска.

## Решение

**Вариант 1 — Go, биндинг `cldapi.dll` через `syscall.NewLazySystemDLL`/`syscall.SyscallN`,
один процесс.** Ядро форка и облако клиента импортируются напрямую; биндинг пишется свой
(`pkg/cfapi`, транскрипция `cldapi.h`, эталонный header из NuGet-пакета
`Microsoft.Windows.SDK.CPP` или mingw-w64 — в SDK 10.0.26100.0 заголовка нет). Регистрация
sync root — Win32 + реестр, без MSIX. Сборка прототипа — с CGO (вариант α, рецепт
`make juicefs.exe`); чистая сборка без CGO (замена `pkg/compress`) — отдельный change.

**Замер гейта D11 (27.11, прототип live на cldflt.sys + sqlite-том bench10k/5032 записи,
детали — design.md change'а)**: M2 streaming — движок 18–87 GB/s (юнит), через полный
фильтр+FETCH_DATA 8MB/0.130s — упирается в линию канала, не в язык: **PASS**. M3 — GC
8 циклов @1% CPU, паузы >10ms: 0 (юнит-нагрузка 20k walk + 256MB fetch): **PASS**.
M1 create-путь — 2948 ops/s живой convert-walk (цель 10k): **Warn** — узкое место
NTFS-дисковые примитивы (4 syscall/файл ~250µs) + Defender-шторм (10000 FETCH_DATA на
холодном рестарте, тёплый рестарт классификации 35 188 ops/s = 3.5× цели). **Вердикт:
порт в Rust по этим основаниям не обоснован — узкое место дисковая подсистема NTFS,
воспроизводимая в любом языке; 10k create-ops/s на файл за ~100µs недостижимо и в C++/Rust.
Go остаётся в линии V3.** Пути ускорения create-пути при бизнес-потребности: батчевая
отдача (CfCreatePlaceholders при PARTIAL), sparse-file вместо Truncate, отложенный convert —
все не требуют смены языка (design.md D11, замерная таблица).

Отклонённые альтернативы:

- **C# P/Invoke** — экономия на готовом биндинге ≈ цене IPC-сервера и клиента, двух
  рантаймов в дистрибутиве и версионирования IPC-контракта; имело бы смысл только при
  сознательном проектировании клиента как .NET-приложения.
- **Native C++** — движок уровня OneDrive (контроль потоков/приоритетов I/O) избыточен для
  прототипа; объём 2500–3500 строк на втором языке, memory safety, toolchain.
- **Go core как c-shared DLL под C#-хостом** — двухязычность не снимает, добавляет C-ABI
  поверх `pkg/fs`; отвергнуто.

## Следствия

- Прототип — change `implement-cfapi-provider` (лестница 0–7, таблица неопределённостей
  U1–U13 в design.md); интеграция с agio-drive (tray, agio-login v0.0.12, supervisor) —
  follow-up `implement-cfapi-drive-integration` (зависимость направления форк←клиент
  требует отдельного решения по module graph).
- **Путь A (решение владельца 2026-11-26): стратегический язык движка V3 решается по
  замерам прототипа.** Программа замеров M1/M2/M3 (metadata ops/s на каталоге 100k записей;
  streaming MB/s при N=1/8/64 параллельных FETCH_DATA; heap/GC-профиль) — design.md, D11;
  целевые значения подтверждает владелец до шага 3. Гейт: цели достигнуты → Go остаётся в
  линии V3; нет → порт движка в Rust (или C++) по фреймингу «чистый клиент протокола хаба»
  (gRPC MetaService + S3-форматы + AGDF/AGCK/AGFK known-answer + STS, ~5–10k LOC), этот
  прототип — оракул поведения и бенчмарк-стенд. Расход при смене языка — биндинг и
  провайдер (~1.5–2.5k строк), осознанный. Итог гейта фиксируется в настоящем ADR (задача 7.9).
- Линия `implement-windows-mount` (WinFSP) не замораживается и не изменяется; readiness-контракт
  (`OK, <том> is ready`) переиспользуется провайдером для будущего supervisor'а.
- cldflt.sys работает только на NTFS; корень — папка на NTFS-томе (решение В4).
- Ограничения прототипа фиксируются в proposal Non-Goals: конфликт нескольких писателей,
  реактивное обновление дерева, офлайн-доступ к дегидратированным файлам, shell-расширения.
- **ACCEPTED 27.11**: DoD 57/57, гейт D11 (M2/M3 PASS, M1-create Warn — узкое место NTFS+Defender,
  не язык; тёплый рестарт 3.5× PASS), вердикт «Go остаётся»; владелец подтвердил прототип живым
  facility-маунтом (vector-stage) с кросс-машинной видимостью. Ключевые живые находки, укрепившие
  решение: errno-перевод Linux↔Windows (serverErrno), FEK-доставка через meta-Open (fs.Open flags),
  FETCH-fail RequiredLength — всё в стеке прототипа, документировано в design.md U-таблице и
  follow-up `implement-cfapi-drive-integration`.
