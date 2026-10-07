# implement-mount-owner-override

## Why

Том хранит uid/gid создателей файлов: серверные каталоги компаний созданы админ-маунтом (service uid), а файлы — клиентами с их локальными uid. Клиент с uid, не совпадающим с stored-uid, теряет запись ещё ДО авторизации платформы: kernel-проверка `default_permissions` (pkg/fuse/fuse.go:512) сравнивает вызывающего с предъявленными stored-атрибутами на каталогах `0755` — EACCES при полностью разрешённом authz. Подтверждено на headless-Linux (задача 4.4 agio-drive, itx.lan 2026-10-06): запись 1 MiB отклонена локально, при том что SpiceDB-права founders-доступа активны. Существующие механизмы не решают: `--all-squash` мапит только вызывающего (pkg/fuse/context.go:71-74) и при несовпадении с stored-uid режет операции уже в meta-слое Access; поле `vfs.Config.NonDefaultPermission` отключает только kernel-слой. Для десктоп-клиента agio Drive (один пользователь на машину, произвольный uid) это блокер Linux-фазы и скрытый риск macOS-прода.

## What Changes

- Новый маунт-флаг `--owner-override[=<uid>:<gid>]` (по умолчанию при пустом значении — uid/gid процесса-клиента): FUSE-ответы (Lookup, GetAttr, атрибуты записей readdir) предъявляют все inode принадлежащими указанному uid:gid, stored-метаданные не изменяются.
- Ядро POSIX-проверки при включённом флаге тривиально проходит (владелец = вызывающий), реальным гейтом доступа остаётся серверный authz платформы; kernel `default_permissions` остаётся включённым.
- Флаг не меняет поведение по умолчанию: без флага — upstream-семантика (предъявляются stored uid/gid).
- Платформенный скоуп: darwin и linux FUSE-пути (общий код pkg/vfs + pkg/fuse); на Windows (WinFsp) флаг принимается и игнорируется — POSIX-владение там не проверяется.
- agio-drive клиент (вне этого change) будет передавать флаг в buildMetaURL extra-args — перекрёстная ссылка, не задача этого репозитория.

## Capabilities

### New Capabilities
- `mount-ownership`: предъявляемое (презентационное) владение файлами в FUSE-маунте — override uid/gid в атрибутных ответах при неизменных stored-метаданных.

### Modified Capabilities
<!-- Нет: pkg/meta и серверный контур не затрагиваются. -->

## Non-goals

- Не менять stored uid/gid в метаданных (никаких миграций, chown-путей, записи через meta).
- Не менять серверную часть: Meta Proxy (pkg/meta/grpc_server.go), AuthzService-контракт, parity Redis/SQL/KV не затрагиваются — change целиком клиентский.
- Не менять и не ослаблять серверный authz: доступ по-прежнему определяется platform AuthzService; флаг только устраняет ЛОКАЛЬНЫЕ POSIX-отказы.
- Не вводить мультпользовательскую семантику на одном маунте (флаг на одну машину = один предъявляемый владелец).
- Не трогать Windows-специфику владения (WinFsp ACL) — только принять и проигнорировать флаг.
- Не заменять `--all-squash`/`--root-squash`: их семантика (маппинг вызывающего) сохраняется, флаг композиционен.

## Related Requirements

- SRS-001 (specs/srs/SRS-001-agio-drive-encryption.md, схема FR-*/NFR-*): системный контур agio Drive, FEK/authz-гейты — change не меняет ни одно из требований SRS-001, но снимает локальный барьер перед authz-гейтом (блокер эксплуатации Linux-клиента, задача 4.4 agio-drive).
- ADR-003 (specs/decisions/ADR-003-data-plane-sts.md): data-plane доступ через platform STS и серверный authz — настоящий change сохраняет модель (authz — единственный гейт), фиксируя её инвариантом в design.
- Прямого раздела про POSIX-владение в SRS-001 нет — требование вводится новой capability `mount-ownership`; при синхронизации specs реестр index.md дополняется.
