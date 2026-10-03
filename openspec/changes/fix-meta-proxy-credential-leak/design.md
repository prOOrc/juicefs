# Design: fix-meta-proxy-credential-leak

## Architectural Context

`juicefs meta-proxy` (`cmd/meta_proxy.go`) оборачивает metadata-движок (DSN в `--meta-backend`, пароль — часть URL) gRPC-сервером. Пароль уже существует в логах в замаскированном виде: `meta.NewClient` печатает `Meta address: redis://:****@…` через `utils.RemovePassword` (`pkg/meta/interface.go:744`); команда `destroy`, `load`, `sync` и `main` маскируют URL той же функцией. Единственное сырое место — стартовый лог прокси (`cmd/meta_proxy.go:207`). Инцидент подтверждён эксплуатационно: agio-cloud деплоит прокси с `--meta-backend=$(METAURL)` из Lockbox-секрета, и пароль Managed Redis ушёл в pod-логи/Loki stage 2026-10-03.

Мотивация — proposal.md; контракт — delta-спека `domain-meta-proxy` («Credential masking in command logs»).

## Component Map

```
juicefs meta-proxy (cmd/meta_proxy.go)
  Action():
    loggerProxy.Infof("Metadata backend URL: %s", metaBackendUrl)   // <-- сырое, ЕДИНСТВЕННОЕ место
    m := meta.NewClient(metaBackendUrl, meta.DefaultConf())
                         |
                         +--> logger.Infof("Meta address: %s", utils.RemovePassword(uri))  // pkg/meta/interface.go:744
                                                          ^
utils.RemovePassword (pkg/utils/utils.go:132)  -----------+  общая маска: scheme://user:****@host
```

## Execution Flow

1. Оператор/чарт запускает `juicefs meta-proxy --meta-backend=<DSN с паролем>`.
2. `cmdMetaProxy.Action` читает флаг и печатает стартовый лог — после фикса через `utils.RemovePassword(metaBackendUrl)`.
3. Дальше ничего не меняется: `meta.NewClient` продолжает получать **полный** URL (маскируется только вывод в лог, не значение), gRPC-сервер стартует как раньше.

## Invariants

- `utils.RemovePassword` не меняется; его семантика (`:****@`, отсутствие `@`/пароля → URL без изменений) — общая для всех команд.
- В лог никогда не попадает полный DSN: маскировка безусловная, на всех уровнях логирования (включая DEBUG).
- `metaBackendUrl`, передаваемый в `meta.NewClient`, остаётся полным — change затрагивает только логирование.

## Decisions & Rationale

### D1. Переиспользовать `utils.RemovePassword`, не писать новую маску

Функция уже стандарт репозитория (`pkg/meta/interface.go:744`, `cmd/destroy.go:155`, `cmd/load.go:223`, `cmd/main.go:378`, `cmd/sync.go:401,455`). Альтернативы отвергнуты: своя regex-маска — второй источник истины; url.Parse-подход — ломается на DSN-формах вида `redis://:pass@host` и `postgres://` (RemovePassword написан под LastIndex `@`).

### D2. Точка фикса — лог-вызов в `cmd/meta_proxy.go`, а не обёртка логгера

Единственное место; обёртка вокруг `loggerProxy` добавила бы абстракцию ради одной строки и скрыла бы от grep-аудита факт маскировки. Спека требует отсутствия немаскированных лог-вызовов URL — grep-проверка по файлу остаётся простой.

## Integration Points

| Точка | Детали |
|---|---|
| `utils.RemovePassword` (`pkg/utils/utils.go:132`) | используется как есть, без изменений |
| `meta.NewClient` (`pkg/meta/interface.go:744`) | поведение лога не меняется (уже маскирует) |
| agio-cloud `drive-meta-proxy` чарт | после выпуска образа с фиксом обновить тег в values (вне этого change) |

## Open Questions

- Ротация уже утёкшего пароля Redis stage (Yandex Managed Redis) и ретенция логов в Loki — вне репозитория; согласовать с agio-terraform-yc/agio-cloud отдельно.
- Нужен ли audit-grep по `pkg/` (вне `cmd/`) на предмет сырых DSN в логах — беглый grep известных мест находок не дал; при желании можно расширить в отдельном change.
