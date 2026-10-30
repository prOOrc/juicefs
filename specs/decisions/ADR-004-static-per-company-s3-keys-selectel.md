# ADR-004: Data-plane креденшелы v2 — статические per-company ключи Selectel (замена YC STS)

| Поле | Значение |
|---|---|
| Статус | accepted |
| Дата | 2026-10-30 |
| Контекст | Переезд платформы YC → Selectel (авария YC 2026-10); change `implement-juicefs-mount` (agio-drive v2); решения владельца 2026-10-29/30 (В1–В5) |
| Связанные | ADR-003 (superseded настоящим ADR), SRS-001, `implement-juicefs-mount`, agio-terraform-selectel, agio-cloud (Selectel-контур) |

## Контекст

ADR-003 закрыл FR-REV-3 через platform-issued **YC STS** (TTL 60 мин, session policy
`<volume>/*`, refresh на половине TTL). В Selectel STS нет; статические ключи были отклонены
в ADR-003 исключительно потому, что у провайдера существовала строгая альтернатива — эта
посылка утратила силу вместе с переездом. Целевая архитектура v2 (change
`implement-juicefs-mount`): клиент — внешний процесс `juicefs` (lite-сборка форка) под
супервизией agio-drive, метаданные через gRPC meta proxy (`drive-meta-proxy`), **чанки —
напрямую в Selectel S3**; JuiceFS S3 gateway в v2-пути не участвует (он остаётся за v1).

Ключевые свойства Selectel Object Storage (docs.selectel.ru, проверено 2026-10-29):

- **Bucket policy** — есть (default-deny; Resource `arn:aws:s3:::bucket/prefix/*`; Condition
  `s3:prefix`; переменные `${aws:userid}`; одна на бакет, 20 КБ). Доковского примера
  «ListBucket + s3:prefix для конкретного пользователя» нет — требуется live-валидация (гейт
  T0.1).
- **Роли service users** (project scope): `s3.admin` / `s3.user` / `s3.bucket.user` (без
  политики — ноль доступа). «Пользователь видит только разрешённые ему бакеты» — штатная
  модель. Swift ACL не существуют.
- **Полный программный жизненный цикл**: REST IAM API v1
  (`POST/DELETE /iam/v1/service_users`, `POST/DELETE /iam/v1/service_users/{id}/credentials`)
  под account-scoped IAM token; Terraform-ресурсы (`selectel_iam_serviceuser_v1`,
  `selectel_iam_s3_credentials_v1`) — обёртки над тем же API (провайдер сам написан на Go и
  является референс-реализацией клиента). `DELETE credentials` = мгновенная ревокация без
  пересоздания пользователя; удаление пользователя каскадно отзывает ключи. Лимиты на число
  пользователей/ключей и rate limits IAM API в документации не опубликованы (запросить
  support в T0.1). 2000 бакетов на проект; ≤250 RPS на бакет (рекомендация).
- Секреты платформы — **Selectel Secrets Manager** (адаптер `selectel/secretstore.go` уже в
  эксплуатации: `facility_storage_provider=selectel`, KEK `facility-storage-kek`).

Точка замены в коде форка: pass-through `GetSTSCredentials` (`pkg/meta/grpc_server_sts.go`)
→ platform KeyManager; клиентский `stsRefresher` (`cmd/sts_refresher.go`) и горячая замена
кред (`cmd/mount.go` `NewReloadableStorage`) остаются без изменений. Данные v2: ключи чанков
`<volume>/chunks/...` (`cmd/format.go:292`), company-сегмента в S3-ключах объектов нет.

## Решения владельца (зафиксированы 2026-10-29/30)

- **В1**: AGDF (шифрование) **обязателен** для v2 — платформа даёт клиентам прямой доступ к
  S3. Вопросы «как выдавать/отзывать ключи» и «как разделять компании» решаются настоящим
  ADR (см. Решение и Следствия).
- **В2**: runtime account-scoped token в платформе — **принят** (авто-ревокация за секунды).
- **В3**: отзыв — ручной триггер админом; ротация — плановая фоновая задача платформы.
- **В4**: render-путь (`cmd/render_mount.go`, YC IAM metadata) — **на холде**: Selectel не
  имеет ресурсов под render-ферму, площадка выбирается. В настоящий change не входит.
- **В5**: v1 (gateway + per-user ключи + `DriveCredentials`) **не трогается**; v2 запускается
  **рядом, в отдельных бакетах и томах**.

## Решение

1. **Креды data-plane v2 — статические per-company пары Selectel** (service user + S3
   credentials). ADR-003 superseded: его отклонение статических ключей опиралось на наличие
   STS у провайдера; в Selectel альтернативы нет. Требования FR-REV-3 в исходной
   формулировке (TTL ≤ 60 мин) заменяются моделью «отзывной статический ключ»: единица
   компрометации — ключ компании; окно экспозиции утёкшего ключа не ограничено TTL и
   компенсируется ротацией, AGDF и двухслойной ревокацией.
2. **Модель изоляции v2.** Все компании общего тома живут в одном volume
   (`<volume>/chunks/...` без company-сегмента) ⇒ S3-скоп ключа ниже volume невозможен
   структурно. Граница компания↔компания — **криптографическая (AGDF, В1)**; per-company
   ключ — единица **идентификации и отзыва**, не изоляции. Bucket policy на v2-контейнере
   скопится на `<volume>/*` (объектные операции) + `ListBucket` c `s3:prefix` — statement'ов
   единицы (по volume, не по компании), бюджет 20 КБ не является ограничением. Контейнер v2 —
   отдельный от v1 (В5), политика вводится с первого дня; риск «сломать gateway» снят
   архитектурно, но проверяется в T0.1.
3. **Жизненный цикл ключей — платформа в рантайме, без Terraform в per-company контуре.**
   Terraform остаётся только на facility-bootstrap (контейнеры, gateway). Платформа
   использует Selectel IAM API v1 напрямую (тонкий Go-клиент): выпуск service user +
   credentials при онбординге компании, ротация, ревокация; обновление bucket policy — через
   S3 `PutBucketPolicy`. Аутентификация — выделенный service user платформы с ролью **User
   administrator** (минимально необходимая для управления пользователями), пароль в Selectel
   SM. Выданные пары компании хранятся в Selectel SM (reuse адаптера) и выдаются клиенту
   через существующий gRPC-канал KeyManager.
4. **Клиентский контракт не меняется**: `GetSTSCredentials` возвращает
   `AccessKeyId/SecretAccessKey/SessionToken=""` и **синтетический `ExpirationUnix` = now +
   TTL (60 мин)** — `stsRefresher` продолжает опрашивать на половине TTL, периодический poll
   становится механизмом доставки ротации; горячая замена — через `NewReloadableStorage`.
   Креды живут только в памяти mount-процесса (как при STS); на диск не пишутся.
5. **Ротация**: плановая фоновая задача платформы (каденция 90 дней — параметр конфига) +
   внеплановая по инциденту. Схема: `POST credentials` (новая пара) → платформа переключает
   active-пару → клиенты забирают в пределах TTL → grace ≥ 2×TTL → `DELETE credentials`
   старой пары. Простоя нет.
6. **Ревокация (компрометация/блокировка компании) — двухслойная**:
   (а) kill-switch компании в meta proxy/платформе — мгновенно прекращает metadata-путь и
   выдачу кред;
   (б) `DELETE credentials` через IAM API — мгновенный S3-deny.
   Триггер — админ (В3); break-glass путь (одно-триггерный пайплайн DELETE) обязателен на
   случай недоступности платформы. После отзыва — расследование по аудит-логам KeyManager и
   MinIO/Selectel.
7. **Гибрид «высокие гарантии» — открытая опция** (не блокирует настоящий ADR): отдельные
   компании могут получать собственные volume + bucket (S3-уровневая изоляция целиком).
   Feasibility подтверждена частично: скоп «пользователь ↔ бакет» через роль
   `s3.bucket.user` + policy документирован (проще префиксного); создание volume —
   библиотечно (`Meta.Init`, включая pass-through RPC `Init` через meta proxy); `Init`
   принимает Format с per-volume `Bucket/AccessKey/SecretKey`. Открытые пункты: мульти-volume
   роутинг meta proxy (сегодня прокси fronts один volume; N томов = N деплоев прокси или
   доработка роутинга), meta-namespace на volume (Redis DB на том — планирование ёмкости),
   лимит 2000 бакетов/проект и неопубликованные лимиты IAM. Решение — отдельным change после
   T0.1.

### Альтернативы (отклонены)

| Вариант | Почему отклонён |
|---|---|
| Сохранить YC STS | Провайдер недоступен (переезд); путь выключен. |
| S3-скоп ключа на company-префикс внутри общего volume | Невозможно структурно: ключи чанков `<volume>/chunks/...` без company-сегмента; политика не может выделить компанию ниже volume. |
| Per-company volume для всех компаний | Множит meta-namespace (Redis DB/том) и деплои proxy; ломает модель общего facility-FS (`companies/<code>/` в одном томе, кросс-компанные доступы); лимит 2000 бакетов. Остаётся опцией гибрида (п.7) для избранных компаний. |
| Terraform в per-company контуре | Провайдер — обёртка над тем же IAM API; секреты светятся в TF state; рантайм-ревокация всё равно требует API-токена в платформе — TF-посредник не даёт выигрыша. |
| Только break-glass (без рантайм-токена) | SLA отзыва зависит от дежурного/пайплайна (минуты—часы); отклонено владельцем (В2). |

## Гейты и последствия

- **T0.1 — live-валидация Selectel (гейт всей реализации)**: (а) ListBucket + `s3:prefix` под
  политикой для конкретного пользователя; (б) роль `s3.bucket.user` vs `s3.user`; (в) пустой
  `SessionToken` доезжает до S3-клиента juicefs; (г) default-deny policy на v2-контейнере не
  мешает платформенным операциям (PutBucketPolicy от платформенного пользователя — какая роль
  нужна); (д) фактические лимиты service users/ключей и IAM rate limits (support); (е)
  поведение клиента при `DELETE credentials` в рантайме (диагностируемая ошибка, не hang).
- Форк: T1 — пустой SessionToken сквозь `sts_refresher`/`NewReloadableStorage` (тесты);
  снятие/пересмотр предусловия «encrypted volume» в ветке STS (`cmd/mount.go:734-763`) — при
  В1 шифрование обязательно, предусловие остаётся и ужесточается смыслом; тест сериализации
  ответа KeyManager.
- Платформа: KeyManager — источник статических пар (Selectel SM), ротация (state machine
  active/pending/grace), revoke-эндпоинт (kill-switch + DELETE), admin-триггер; выделенный
  service user User administrator; аудит выдач/ротаций/ревокаций (outbox-события).
- Риски: неопубликованные лимиты Selectel (T0.1); окно экспозиции статического ключа без TTL
  (компенсации: AGDF, ротация, двухслойная ревокация); компрометация платформенного
  User-administrator пользователя (минимальная роль, Selectel SM, аудит, break-glass).
- v1 (`drive-s3-gateway`, per-user MinIO-ключи, `DriveCredentials` — включая хранение их
  платформой в БД через facility_storage_config) — вне настоящего ADR; render-путь — В4 (холд).
