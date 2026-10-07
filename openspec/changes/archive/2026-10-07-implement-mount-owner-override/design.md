# implement-mount-owner-override — Design

## Architectural Context

Change целиком в клиентском FUSE-контуре форка: hanwen-бридж `pkg/fuse` → `pkg/vfs` → клиент meta (`pkg/meta` gRPC-клиент). Серверный контур (Meta Proxy `pkg/meta/grpc_server.go`, AuthzService, KEK) не затрагивается — правило parity Redis/SQL/KV и `base_test.go` на change не распространяются (нет семантических изменений pkg/meta).

Проблема — расслоение «предъявленное владение vs stored-владение»: FUSE-клиент сегодня отдаёт kernel'у stored `meta.Attr.Uid/Gid`; kernel с `default_permissions` (pkg/fuse/fuse.go:512, добавляется при `!conf.NonDefaultPermission`) отклоняет операции вызывающего с чужим uid до того, как запрос дойдёт до серверного authz. При выключенном kernel-слое (`NonDefaultPermission`) запрос уходит в meta-слой `Access` (pkg/vfs/vfs_unix.go:77 → `v.Meta.Access`), где проверка снова идёт против stored-владельца. Оба локальных слоя проверяют одно и то же — stored-атрибуты, — тогда как единственным значимым гейтом в agio Drive является серверный authz.

## Component Map

| Компонент | Роль | Точка изменения |
|---|---|---|
| `cmd/mount_unix.go` | регистрация флагов, сборка `vfs.Config` | блок squash-флагов :1161-1172 — рядом добавить парсинг `--owner-override` |
| windows flagset (`implement-windows-mount`) | те же флаги на Windows + `buildServiceCmdLine` | добавить флаг в flagset и в перенос сервисной cmdline |
| `pkg/vfs/vfs.go` `vfs.Config` | конфиг VFS (`AllSquash *AnonymousAccount` :144, `NonDefaultPermission` :145) | новое поле `OwnerOverride *AnonymousAccount` |
| `pkg/vfs/vfs.go` `VFS.Lookup` :183, `VFS.GetAttr` :213 | оба пути возвращают `*meta.Entry` клиенту meta→FUSE | применить override к `entry.Attr.Uid/Gid` после `v.Meta.GetAttr`/`v.Meta.Lookup` |
| `pkg/fuse/fuse.go` `fileSystem.Lookup` :85, `fileSystem.GetAttr` :100, readdir-путь :404 (`AddDirLookupEntry`) | конверсия `meta.Entry` → `fuse.EntryOut`/`fuse.AttrOut` | проверка, что readdir-записи либо attr-less (kernel делает getattr — покрыто VFS.GetAttr), либо проходят через тот же override |
| `pkg/fuse/context.go` :71-74 | маппинг вызывающего (`--all-squash`) | НЕ меняется — композиция |
| `pkg/fuse/fuse.go` :512 | добавление kernel-опции `default_permissions` | НЕ меняется — инвариант |

Типы verified grep-ом: `VFS.Lookup`, `VFS.GetAttr`, `vfs.Config`, `AnonymousAccount`, `meta.Entry`, `meta.Attr`, `fileSystem.Lookup/GetAttr`, `AddDirLookupEntry`, `parseUIDGID` (cmd/mount_unix.go, используется squash-блоком).

## Execution Flow

getattr (включён override):

```
kernel(default_permissions)
  → fileSystem.GetAttr (pkg/fuse/fuse.go:100)
    → VFS.GetAttr (pkg/vfs/vfs.go:213)
      → v.Meta.GetAttr(ctx, ino, attr)          // stored uid/gid из meta
      → applyOwnerOverride(v.Conf, attr)        // NEW: attr.Uid/Gid = override
    → fuse.AttrOut из meta.Entry
  → kernel сравнивает вызывающего с предъявленным владельцем (= вызывающий) → pass
→ операция (create/write/…) → серверный authz решает
```

Lookup — тот же flow через `VFS.Lookup` :183. Побочные внутренние пути (`.stats`, `.config` через `IsSpecialNode`/`getInternalNode`, pkg/vfs) переиспользуют `entry.Attr` — override применяется к ним так же единообразно (решение D6).

## Invariants

1. Override работает ТОЛЬКО на чтении атрибутов: ни одна кодовая ветка change не вызывает `meta.Write`/setattr; stored uid/gid бит-в-бит равны значениям без флага.
2. `default_permissions` остаётся в kernel-опциях при включённом override (условие fuse.go:512 не меняется) — локальный слой не отключается, а тривиально проходит.
3. Результаты серверного authz не зависят от флага (контекст запроса в meta-клиент не модифицируется).
4. Флаг не задан → ни одна новая ветка не выполняется (добавляется один `nil`-чек на путь; поведение идентично текущему).
5. Windows: флаг парсится, в сервисную cmdline переносится, на WinFsp-том не влияет.
6. Композиция: `AllSquash` меняет `ctx.header.Uid/Gid` (pkg/fuse/context.go:73-74), `OwnerOverride` меняет только атрибуты ответов; порядок применения не определён и не важен.

## Decisions & Rationale

- **D1 — override на уровне VFS, а не бриджа или kernel**: kernel-опция `uid=` не работает (fs задаёт per-inode атрибуты — hanwen отдаёт `Attr.Uid` из `meta.Entry`); override в бридже потребовал бы правок в трёх конверсиях (`fuse.EntryOut`, `fuse.AttrOut`, readdir), тогда как `VFS.Lookup`/`VFS.GetAttr` — единственные два источника `meta.Entry` для всех них (readdir-записи при attr-less покрыты GetAttr'ом).
- **D2 — презентация, не перезапись**: перезапись stored-uid при записи/создании меняла бы данные тома глобально (второй клиент увидит чужого владельца), требовала бы политик и миграций. Презентационный override обратим, локален и не ломает существующие тома.
- **D3 — отдельный флаг, не перегрузка `--all-squash`**: squash — идентичность вызывающего (влияет на stored-uid новых файлов и meta-слой Access), override — предъявление. Разные семантики; склейка сломала бы текущих пользователей squash (тесты implement-windows-mount 4/4).
- **D4 — значение по умолчанию = uid:gid процесса**: десктоп-клиент agio Drive однопользовательский на машину; клиент передаёт голый `--owner-override` без значений. Явное `uid:gid` остаётся для сервисных сценариев.
- **D5 — отклонённая альтернатива «просто выставить NonDefaultPermission»**: без `default_permissions` kernel шлёт `ACCESS`, `VFS.Access` (vfs_unix.go:77) → `v.Meta.Access` сравнивает с stored-владельцем — отказ мигрирует в meta-слой; смешанные stored-uid на томе (созданы разными клиентами/сервером) ломают любой фиксированный squash. Override нейтрален к хаосу stored-uid.
- **D6 — спец-узлы под override тоже**: `.stats`/`.config` предъявляются с override-владельцем — единообразие,无 исключений в коде; содержимое read-only, риск отсутствует.

## Integration Points

- **agio-drive клиент** (внешний репозиторий, вне change): супервизор добавит `--owner-override` в `ExtraArgs` (`pkg/jfs` supervisor → `buildMetaURL`/args в pkg/mount/juicefs.go). Форма передачи: `--owner-override=<uid>:<gid>` или `--owner-override=` (пустое значение = uid:gid процесса маунта); «голая» форма без `=` в urfave/cli недопустима (StringFlag съест следующий аргумент). Перекрёстная ссылка в tasks agio-drive change `implement-juicefs-mount`; до интеграции флаг опционален.
- **implement-windows-mount**: общий flagset; `buildServiceCmdLine` обязан переносить флаг (иначе сервис-маунт теряет его) — задача 4.
- **CI/сборки**: сборка не меняется (чистый Go, darwin/linux/windows); Windows-сборка pure-Go без cgo — флаг не тянет зависимостей.

## Known Limitations

- Атрибутные ответы МУТИРУЮЩИХ операций (`Create`/`Mkdir`/`Mknod`/`Symlink`/`Link`/`SetAttr`, `Open`) предъявляют stored uid/gid до следующего refresh'а атрибутов (`conf.AttrTimeout`). Спека перечисляет только Lookup/GetAttr/readdir; для основного сценария (запись в чужой каталог без chmod) этих путей достаточно — создаётся/меняется inode владельцем, чей stored uid совпадает с вызывающим (без squash). Известное ограничение, кандидат на follow-up, если появится сценарий «squash + override с разными значениями».

## Risks / Trade-offs

- **Когерентность кэшей атрибутов** (`conf.AttrTimeout`/`DirEntryTimeout`): override применяется в источнике до попадания в кэш → кэшированные значения уже переопределены; риск рассинхрона Lookup/GetAttr отсутствует по построению, закрывается тестом согласованности.
- **Косметика листингов**: `ls -la` показывает владельца = маунт-пользователь для чужих файлов (как в rclone VFS). Для однопользовательского десктопа это целевое поведение; для общих машин-маунтов — задокументированное ограничение (Non-goal: multiuser-семантика).
- **Маскировка диагностики**: лог `logit` показывает переопределённые атрибуты; при разборе инцидентов stored-uid доступен только на сервере. Принимаем: клиентская диагностика и так оперирует предъявленной картиной.

## Open Questions

- Несут ли readdir-записи hanwen-бриджа `Attr` в этом форке (fuse.go:404 `AddDirLookupEntry(de)`): если `de` attr-less — требование readdir покрывается `VFS.GetAttr` автоматически; пинётся на задаче 2 тестом согласованности.
- Поведение macFUSE с `default_permissions` при совпадающем uid (регрессия стенда agio-drive): подтверждается приёмкой на macOS (задача 5) — ожидается нулевой дифф при включённом флаге.
- Стоит ли agio-drive включать флаг по умолчанию для ВСЕХ маунтов или только при детекте несовпадения uid (задача клиента, не форка) — решить в agio-drive `implement-juicefs-mount`; для форка контракт — «флаг безопасен всегда».
