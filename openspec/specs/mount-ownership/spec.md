# mount-ownership Specification

## Purpose

Предъявляемое владение файлами в FUSE-маунте agio Drive: при включённом маунт-флаге клиент предъявляет все inode принадлежащими заданному uid:gid независимо от stored-метаданных, что исключает локальные POSIX-отказы до серверного authz. Капабилити охватывает клиентские пути darwin/linux (pkg/vfs, pkg/fuse); серверный контур и stored-метаданные не затрагиваются.

## Requirements

### Requirement: Override предъявляемого владения

Система SHALL предоставлять маунт-флаг `--owner-override[=<uid>:<gid>]`; при заданном флаге без значения MUST использоваться uid:gid процесса клиента. При включённом флаге атрибутные FUSE-ответы (Lookup, GetAttr, атрибуты записей readdir) SHALL предъявлять uid/gid = значениям флага для всех inode; stored-метаданные MUST оставаться неизменными.

Verified-by: pkg/vfs/owner_override_test.go::TestGetAttrOwnerOverride

#### Scenario: Клиент с чужим stored-uid пишет в серверный каталог
- **WHEN** том смонтирован с `--owner-override` клиентом с uid, не совпадающим с stored-uid каталога `0755`, и серверный authz разрешает запись
- **THEN** создание файла внутри каталога завершается успешно (локальные POSIX-проверки проходят: предъявленный владелец = вызывающий)

#### Scenario: Stored-метаданные не изменяются
- **WHEN** включён `--owner-override` и клиент выполняет операции чтения/записи/создания
- **THEN** значения uid/gid в meta-хранилище для существующих и новых inode не изменяются относительно операции без флага

#### Scenario: Флаг не задан — upstream-поведение
- **WHEN** маунт запущен без `--owner-override`
- **THEN** атрибутные ответы предъявляют stored uid/gid (поведение идентично текущему коду)

### Requirement: Override покрывает все атрибутные пути

При включённом `--owner-override` override uid/gid SHALL применяться единообразно во всех путях выдачи атрибутов FUSE-клиенту: `VFS.Lookup` (pkg/vfs/vfs.go), `VFS.GetAttr` (pkg/vfs/vfs.go) и атрибуты записей каталога в FUSE-бридже (pkg/fuse/fuse.go, путь readdir/AddDirLookupEntry).

Verified-by: pkg/fuse/owner_override_test.go::TestOwnerOverrideLookupConsistency

#### Scenario: Атрибуты согласованы между Lookup и GetAttr
- **WHEN** включён `--owner-override` и клиент запрашивает атрибуты одного inode через lookup (после поиска по имени) и через getattr (по inode)
- **THEN** оба ответа предъявляют одинаковые uid/gid = значениям флага

### Requirement: Композиция с squash-флагами

Флаг `--owner-override` SHALL быть композиционным с `--all-squash`/`--root-squash`: squash изменяет идентичность вызывающего в операциях (pkg/fuse/context.go), override — только предъявление атрибутов; один флаг MUST NOT влиять на действие другого.

Verified-by: pkg/vfs/owner_override_test.go::TestOwnerOverrideSquashComposition

#### Scenario: Squash не меняет предъявление, override не меняет вызывающего
- **WHEN** маунт запущен с `--all-squash 1000:1000 --owner-override=1000:1000`
- **THEN** операции выполняются от uid:gid 1000 (squash) и все атрибуты предъявляются как 1000:1000 (override); при изменении значения override без изменения squash предъявление меняется, а идентичность операций в meta — нет

### Requirement: Платформенный скоуп

Флаг SHALL приниматься на всех платформах; на darwin и linux override SHALL применяться, на Windows (WinFsp) флаг SHALL игнорироваться без ошибки запуска.

#### Scenario: Windows принимает флаг без ошибки
- **WHEN** клиент Windows запускает маунт с `--owner-override=<uid>:<gid>`
- **THEN** маунт стартует штатно, флаг не влияет на WinFsp-том

### Requirement: Инвариант авторизации

Включение `--owner-override` MUST NOT изменять результаты серверного authz (platform AuthzService) и SHALL сохранять kernel-опцию `default_permissions` активной; локальное устранение отказа достигается только предъявлением владельца, равного вызывающему.

#### Scenario: Отказ authz не обходится флагом
- **WHEN** включён `--owner-override`, а серверный authz запрещает запись в каталог
- **THEN** попытка записи завершается ошибкой авторизации (серверный отказ), а не успешной записью

