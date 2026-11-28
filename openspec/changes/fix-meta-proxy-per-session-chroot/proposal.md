# Proposal: fix-meta-proxy-per-session-chroot

## Why

MetaProxyServer держит один общий движок (`grpc_server.go:49`), а RPC `Chroot`
(`grpc_server_admin.go:283-287`) мутирует его корень `m.root` (`base.go:2905-2907`)
без лока и возвращает только errno — inode корня в `ChrootResponse` нет. Прокси не
различает соединения: клиентский заголовок `x-session-id` игнорируется
(`grpc_client.go:516-518`, `:538-542`), `NewSession` клиент вызывает один раз за
монтирование (`cmd/mount.go:812`) и при реконнекте не повторяет, а `reChroot`
выполняет строго до replay журнала записи (`grpc_client.go:710-716`; replay —
`ReplayWriteJournal`, `vfs.go:1423`, хук `mount.go:829`).

Следствия — инцидент stage с клиентом `--subdir companies/<code>`: «пустой» корень,
отказ `create` (create в корне чрутнутого клиента проверяется как Write на `/` —
`authz_interceptor.go:368-372` — и приходит клиенту как `EACCES`,
`grpc_client.go:832-833`), silent revert корня другим клиентом.

Отдельно chroot даёт двойное path-пространство: `InodePathCache` пинит inode 1 → `/`
(`inode_path_cache.go:47-55`, `:303-305`) и `BuildChildPath` строит `/name` от
родителя 1 (`:261-275`), тогда как recovery строит полные пути тома
(`inode_path_recovery.go:123`, `:175`). Поэтому Readdir корня чрутнутой сессии идёт
смешанным bulk-check (`grpc_server_fuse.go:876-892`, `:905-909`).

Это ревизия v1 по двум решениям владельца (28.11):

- **R1** — движок на каждый уникальный chroot, а не на каждую сессию: пул движков по
  subdir.
- **R2** — деплой-линия переносится на fork `agio-drive-v2` @`2d672403c`; change
  базируется на этой ветке, а не на `main` @`7062e02e7`.

## What Changes

- **Сессия = gRPC-соединение.** `grpc.StatsHandler` открывает сессию в `TagConn` по
  `stats.ConnTagInfo.RemoteAddr` и завершает её в `HandleConn(ConnEnd)`; interceptor
  привязывает сессию по `peer.FromContext`. У сессии числовой id в логах.
- **Fail-closed.** Meta-RPC без сессии → `codes.Internal`, движок не вызывается.
  STS-RPC без сессии отклоняется. Стриминговые RPC (`meta.proto:120-140`)
  привязываются к сессии тем же механизмом.
- **Пул движков по уникальному chroot** вместо per-session движков (R1). Реестр
  `map[нормализованный subdir]движок`. Ключевые свойства:
  - движок создаётся при первом `acquire(key)`: фабрика (`NewClient` + `Load(true)`),
    затем `Chroot(key)` на свежем движке, затем `NewSession`; публикация — после
    успеха;
  - корень движка пула пишется ровно один раз — при создании;
  - счётчик ссылок; при `refs == 0` — фоновый teardown: `CloseSession` (≤1 раза,
    бюджет 5s) + `Shutdown`, снятие с реестра;
  - сессии без Chroot шарят движок `""` (корень тома);
  - повторное приобретение после teardown — обычное создание.
- **Базовый движок** остаётся отдельным, используется только `pathResolver` и никогда
  не чрутнится. Поле `meta` удаляется из `MetaProxyServer`; обращение обработчиков к
  базовому движку — ошибка компиляции. `NewMetaProxyServer` получает фабрику движков
  третьим аргументом.
- **Авторизация цели Chroot** (authz-режим): Chroot требует `AuthzPermissionView` на
  `"/"+<нормализованный subdir>` через `CheckPermission`. Deny →
  `codes.PermissionDenied`, корень не меняется; путь не вычислим → `PermissionDenied`.
  Несуществующая цель → `ENOENT` без создания каталогов; не-каталог → `ENOTDIR`. Без
  authz — как сегодня (включая создание) + лог.
- **Идемпотентность Chroot.** `subdir = path.Clean("/"+subdir)` без ведущего `/`.
  Повтор с тем же subdir → сохранённый errno первого вызова без обращения к движку;
  другой subdir → `codes.FailedPrecondition` без обращения к движку.
- **Service identity** (база adv2): запросы service-credential идут мимо
  authz-интерцептора, поэтому Chroot без проверки View, pre-check цели не применяется
  (семантика «как сегодня»); сессии и пул применяются так же (gateway = одно
  долгоживущее соединение = одна сессия). В логах `user=service`.
- **Dir-хендлы** привязаны к сессии; чужая сессия → `EBADF`; закрытие соединения
  закрывает хендлы сессии (устраняет утечку `s.handlers`,
  `grpc_server_dir_handler.go:150-159`).
- **Lifecycle.** `NewSession` движка ≤1 раза за жизнь движка (при создании), RPC
  `NewSession` возвращает сохранённый ответ всем сессиям группы (одинаковый sid).
  `CloseSession` движка ≤1 раза за жизнь движка (при teardown); RPC `CloseSession`
  завершает только сессию отправителя.
- **`--max-sessions`** (default 256, `0` — без лимита) ограничивает соединения;
  превышение → `codes.ResourceExhausted` + Warn. Число движков ограничено числом
  уникальных subdir среди живых сессий.
- **Логи** (логгер `pkg/meta`, key=value): события сессии, события пула, события
  chroot; authz-денайлы с `Debugf` на `Warnf` (`authz_interceptor.go:172`, `:180`,
  `:195`).
- **Пути authz под chroot.** Путь inode 1 = корень сессии
  (`rootPathFromContext`); `InodePathCache.Get`/`BuildChildPath` получают ctx;
  кеш хранит только полные пути тома. Движки Redis/SQL/KV не меняются.

## Impact

- **Код:** `grpc_server.go`, `grpc_server_admin.go`, `grpc_server_fuse.go`,
  `grpc_server_dir_handler.go`, `authz_interceptor.go`, `inode_path_cache.go`,
  `cmd/meta_proxy.go`; новый `grpc_server_session.go`; тесты рядом.
- **Ресурсы в рантайме:** движков = |уникальные subdir среди живых сессий| + движок
  `""` (пока нужен) + 1 базовый. Каждый движок пула несёт одну сессию движка
  (~7–10 горутин, запись сессии в redis).
- **Хранилище:** формат и данные не меняются; записи сессий в redis появляются и
  удаляются штатным механизмом движка.
- **Эксплуатация:** новый флаг `--max-sessions`; новые события в логах; при выкатке
  нужен рестарт прокси, клиенты переподключаются штатно.
- **Деплой-линия:** fork `agio-drive-v2` @`2d672403c` (R2).

## Risks

- **Гонка teardown × acquire.** Решено: запись снимается с реестра только под
  мьютексом реестра при `refs == 0` (или при провале создания); одновременные
  приобретения того же ключа ждут одну и ту же создаваемую запись. После снятия
  старый движок разбирается, новый создаётся независимо; у них разные sid, конфликта
  в хранилище нет.
- **Общий sid движка внутри группы.** Все сессии одного subdir работают через одну
  сессию движка. Механизмы, привязанные к сессии движка, в рамках группы общие. Это
  сужает нынешнюю ситуацию (один движок на всех тенантов) до одной группы, но не
  устраняет её; перечень затронутых механизмов — на проверке (design, Open Questions).
- **Пересоздание после `refs == 0`.** Новое создание — десятки мс и новый sid.
  Реконнект клиента, попавший в окно teardown, получает новый движок; благодаря
  сессии движка, открываемой при создании, он работает без повторного `NewSession`.
- **In-flight RPC завершившейся сессии.** Обработчики не держат ссылку на движок;
  RPC, начатый до завершения сессии, может получить ошибку после `Shutdown`. Клиент
  к этому моменту уже отключён.
- **Окно между pre-check и `Chroot`.** `baseMeta.Chroot` умеет создавать каталоги.
  Финальный lookup на самом свежем движке сужает окно с десятков мс до < 1 мс, но не
  закрывает его до конца (нужно изменение движка — вне этого change).
- **Топология подключений.** Модель «сессия = соединение» предполагает отсутствие
  L7-мультиплексирования HTTP/2-соединений между клиентом и прокси.
- **Соседние change на adv2.** Одновременно активны четыре change-каталога:
  `add-meta-proxy-service-identity`, `add-gateway-platform-mode`, `implement-encrypt`,
  `implement-windows-mount`. Риск расхождений при архивации в разном порядке и
  ребейзе; порядок — в design, Migration Plan.
- **Деплой-линия.** На adv2 нет darwin-CI коммитов main (`7c1d4ca82`, `7062e02e7`);
  перенос деплой-линии — отдельные [GO] вне этого change.

## Non-goals

- НЕ менять клиент (`grpc_client.go`, `cmd/mount.go`): сервер продолжает
  игнорировать `x-session-id`, новых RPC и правок `meta.proto` нет.
- НЕ менять движки Redis/SQL/KV, формат хранилища и данные.
- НЕ менять семантику `baseMeta.Chroot` (неявное создание каталогов в не-authz
  режиме остаётся) и НЕ добавлять resolve-only chroot в движок.
- НЕ добавлять распределённый реестр сессий, кластеризацию прокси, idle-grace кеш
  движков и флаг `--max-engines`.
- НЕ менять модель авторизации (набор `AuthzPermission*`), `keyManager`, STS,
  `pathResolver`, behavior service-identity из change `add-meta-proxy-service-identity`.
- НЕ править agio-drive CI (`JUICEFS_REF`, PIN) и линию сборки образа прокси — это
  отдельные [GO] на переносе деплой-линии.
- НЕ переносить darwin-CI коммиты main → adv2 в этом change.
- НЕ трогать инсталлятор NSIS, релиз, docker-образы. Секреты в артефактах — только
  имена.

## Related Requirements

- `Authorization interceptor` — MODIFIED (Chroot → path-checked View).
- `Inode-to-path cache` — MODIFIED (путь inode 1 = корень сессии).
- `gRPC service surface` — состав RPC не меняется; `Chroot` остаётся admin-операцией.
- `Root short-circuit and directory listing filter` — правило «/ + View без authz» сохраняется для пути `/`.
- `Inode path recovery` — recovery остаётся источником полных путей тома.
- `Stateful directory handler` — дескрипторы привязываются к сессии.
- `gRPC metadata client` — зависимость: клиент повторяет `Chroot` после восстановления соединения.
- `Authz decision cache`, `Authz operation context pass-through`, `OIDC authentication` — используются без изменений.
- Прецедент: архивный change `fix-meta-proxy-session-churn`, design D4 — per-session chroot объявлен отдельным change.

Соседний активный change `add-meta-proxy-service-identity` (его дельта ADDED-only, в
SoT на момент ревизии ещё не заархивирована): `Service credential authentication`,
`Service connections skip authz and admin surface`, `Node credential routing to
keymanager`. С нашей MODIFIED `Authorization interceptor` не пересекается по тексту.
Бизнес-документов (BRD/SRS) по meta-proxy нет — зафиксировано в `openspec/specs/index.md:47-49`.

## Capabilities

### New Capabilities

_None._

### Modified Capabilities

- `domain-meta-proxy` — сессии соединений, пул движков по chroot, авторизация цели
  Chroot, пути authz под chroot, привязка dir-хендлов, лимит сессий, логирование.
