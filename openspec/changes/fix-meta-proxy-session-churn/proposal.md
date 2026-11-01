# Proposal: fix-meta-proxy-session-churn

## Why

Stage-наблюдение (2026-10-31, том tst-v2/tst-v3): рестарт meta-proxy при живом
grpcMeta-клиенте делает всю ФС недоступной до перемонтирования — readdir
отдаёт 0 entries (errno 0), open → EACCES. Метаданные целы: deny происходит
в прокси до обращения к engine. Механизм (source-инвестигация 2026-11-01):

1. inode→path cache прокси — производная in-memory структура; после рестарта
   в нём остаётся только `{1:"/"}` (pinned корень).
2. Клиент при реконнекте не пере-устанавливает ничего: sid/chroot/кэши
   заморожены, heartbeat оживает, и kernel (entry_timeout 1s) честно
   пере-Lookup'ает — но сам Lookup денится, потому что его parent не в кэше.
   Цикл восстановления разорван по построению.
3. Readdir по каталогу с неизвестным путём возвращает пустой листинг с
   errno 0 (fail-closed как «пустой каталог») — тихая потеря данных
   с точки зрения пользователя.
4. Сопутствующий дефект: denied Lookup/GetAttr/NewDirHandler транслируются
   клиентом в EIO вместо EACCES (`grpcStatusToErrno` применяется только
   в openOnce/Chroot) — deny недиагностируем на stage.

Класс дефекта шире рестартов: eviction из кэша при лимите даёт ту же
картину без всякого рестарта.

## What Changes

- Прокси: серверная реконструкция пути по parent chain через metadata engine
  (источник истины — `attr.Parent`) на cache miss, ДО fail-closed deny.
  Рекавери — внутренние чтения engine, не клиентская операция: authz-решение
  после реконструкции принимает сервис ровно как раньше (fail-closed
  семантика сохранена; чужой инод → путь найден → deny → EACCES).
- Readdir: нерезолвимый даже после рекавери путь каталога → EACCES вместо
  тихого пустого листинга; пустой листинг легитимен только при известном
  пути и 0 авторизованных детях.
- Клиент: errno-маппинг через `grpcStatusToErrno` для Lookup/GetAttr/
  NewDirHandler (PermissionDenied/Unauthenticated → EACCES);
  EBADF от DirHandler-операций → дроп хендла (пересоздание на следующем
  off==0 уже существует в vfs); re-Chroot при реконнекте (только при
  сконфигурированном subdir, до replay write journal).
- Инвалидация: Rename/Unlink/Rmdir точечно сбрасывают закэшированные пути
  (resolver.Invalidate).

Не входит (отдельные change'и): client-provided paths (долгосрочное решение,
комментарий в inode_path_cache.go:34-37); per-session chroot-изоляция
общего engine; fsck-детектор записанных plaintext-файлов.

## Capabilities

### Modified Capabilities

- `domain-meta-proxy`: MODIFIED «Authorization interceptor» (рекавери перед
  deny), MODIFIED «Inode-to-path cache» (кэш — производная, источник истины
  engine), MODIFIED «Root short-circuit and directory listing filter»
  (EACCES вместо пустого листинга при нерезолвимом пути), MODIFIED
  «gRPC metadata client» (errno-маппинг, re-Chroot, EBADF-дроп), ADDED
  «Inode path recovery» (свойства реконструкции).

## Related Requirements

- domain-meta-proxy SoT: «Authorization interceptor» (fail-closed на
  неразрешённом пути), «Inode-to-path cache», «Root short-circuit and
  directory listing filter», «gRPC metadata client».
- Наблюдение зафиксировано в tasks.md архивного add-selectel-kms-backend
  (agio-platform) и в приёмке T0.1 (пилот 9.5).

## Impact

- `pkg/meta/inode_path_recovery.go` (новый), `pkg/meta/authz_interceptor.go`,
  `pkg/meta/grpc_server.go`, `pkg/meta/grpc_server_fuse.go`,
  `pkg/meta/grpc_client_fuse.go`, `pkg/meta/grpc_client_data.go`,
  `pkg/meta/grpc_client.go` / `grpc_client_lifecycle.go`, `pkg/vfs/vfs.go`.
- Engine не меняется; wire-протокол не меняется (фикс полностью серверный
  для ядра проблемы — деплой нового прокси чинит живых клиентов без
  перемонтирования).
- Наблюдаемые errno меняются (EIO → EACCES на deny; пустой листинг → EACCES)
  — compatibility-примечание в design.md.
