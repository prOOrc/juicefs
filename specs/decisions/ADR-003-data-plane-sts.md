# ADR-003: Креденшелы data-plane — platform-issued YC STS (закрытие FR-REV-3)

| Поле | Значение |
|---|---|
| Статус | accepted |
| Дата | 2026-09-27 |
| Контекст | change `implement-encrypt` (tasks 7.8–7.12), SRS-001 v2.4 (FR-REV-3), прогон 9.6b (S4 partial) |
| Связанные | ADR-001, SRS-001 (§7 Отзыв, §15.1, T5, R8) |

## Контекст

Data-plane (чтение/запись чанков в S3 клиентами тома) сегодня работает на статических
`Format.SecretKey`: они не имеют TTL и не отзываются; их обладатель может перезаписать/удалить
чанки всего тома (cross-tenant integrity/availability; конфиденциальность при этом защищена
криптографией — FEK выдаётся только через authz-gated KeyManager). FR-REV-3 (SRS §7) требует
короткоживущие отзывные S3-credentials (TTL ≤ 60 мин); риск R8 — «STS не реализован → отзыв
не работает, Критическое, приоритет №1». Прогон 9.6b (S4) подтвердил только fail-closed ветку.

Консультации архитектора 2026-09-27 (две) дополнительно установили:

1. **Дефект семантики префикса.** Platform `GetSTSCredentials` строит политику от метаданного
   префикса `companies/<code>/`, а реальные ключи чанков = `<имя-тома>/chunks/...`
   (форк: `cmd/format.go:292`, `pkg/chunk/cached_store.go:77-81`). Даже рабочий провайдер выдал
   бы креды, которыми нельзя ни прочитать, ни записать ни один чанк.
2. **`GetSTSCredentials` без аутентификации.** IAM interceptor защищает только `FetchCompanyKEK`
   (`src/api/iam_interceptor.go`); `user_id` — само-заявленный. Процесс, знающий UUID сотрудника
   компании, получает его креды сам.
3. **Threat model render-нод.** На render-нодах исполняются пользовательские процессы
   (render-задачи) — всё, что доступно любому процессу ВМ (metadata-токен), считается
   скомпрометированным на уровне отдельного процесса.

## Решение

Единый механизм для всех классов клиентов — **platform-issued STS** через `sts.yandexcloud.net`
(AWS-совместимый `AssumeRole` + session policy):

- **Render-ноды**: новый RPC `GetNodeSTSCredentials` — аутентификация YC IAM-токеном ВМ
  (расширение IAM interceptor по паттерну `FetchCompanyKEK`, `WithIAMNodeID`), actor
  `render_node` в аудите.
- **Пользователи (художники)**: существующий RPC `GetSTSCredentials` вызывается **только через
  Meta Proxy** (pass-through RPC на стороне форка): прокси аутентифицирует сессию (OIDC) и
  подставляет `user_id` из верифицированного subject; клиентское поле игнорируется. KeyManager
  остаётся на proxy-trust модели (внутренняя сеть) — как остальные файловые RPC.
- **Скоуп политики**: platform деривирует имя тома и бакет сама —
  `company.default_facility_id` → `facility_juicefs_config.juicefs_config` (`Name` = volume,
  `Bucket`); клиент volume/bucket не называет (иначе запросил бы префикс чужого тома). Политика:
  object-действия на `arn:aws:s3:::<bucket>/<volume>/*` + `ListBucket` с условием
  `s3:prefix StringLike "<volume>/*"`; если YC STS не примет условие — конфиг-fallback
  `sts_listbucket_unscoped` (остаточный риск: перечисление имён объектов бакета; имена —
  opaque inode-ключи).
- **Креды** живут только в mount-процессе (SigV4: AccessKey+Secret+SessionToken — проходят через
  существующий static-провайдер `pkg/object/s3.go` и рефрешер `stsRefresher` без изменений);
  TTL 60 мин, refresh на половине TTL, fail-fast без фолбэка на статику.
- **SA render-нод лишаются ролей на S3-бакет** (остаются только права на KMS/Lockbox для
  `FetchCompanyKEK`) — кража metadata-токена перестаёт давать S3-доступ. Static key
  платформенного SA, которым платформа ходит в STS, живёт на платформе (kill-switch — удаление).
- Ops-мера: mount-процесс под отдельным системным uid (защита от `/proc/.../mem` + ptrace
  same-uid).

### Альтернативы (отклонены)

| Вариант | Почему отклонён |
|---|---|
| VM metadata IAM token → Bearer в Object Storage | Токен доступен любому процессу ВМ; на render-нодах с пользовательскими процессами компрометация процесса ⇒ полный бакетный доступ. Плюс потребовал бы Bearer-режима в aws-sdk-go-v2 S3-клиенте (штатного нет, официального YC-форка нет). |
| YC ephemeral keys (`iam.aws-compatibility/v1/ephemeralAccessKeys`; действующий механизм) | Ключ выпускает сам процесс из своего VM-wide IAM-токена, policy выбирает вызывающий (сужение только правами SA), отозвать до истечения нельзя — threat model хуже, чем у STS. Реализован в форке (`ycEphemeralKeyProvider`, task 7.4) — удаляется task 7.11. |
| Статические ключи (status quo) | FR-REV-3 не выполняется; R8 «Критическое»; отзыв S3-доступа невозможен. |

## Последствия

- Позитив: FR-REV-3 закрыт; отзыв = «platform перестаёт выдавать после authz deny» + TTL ≤ 60 мин;
  выдача authz-gated и аудитируется (op `get_sts_credentials` / `get_node_sts_credentials`);
  унификация render/пользовательского путей; KeyManager остаётся внутренним сервисом.
- Изоляция компаний при общем бакете остаётся криптографической (per-company KEK → FEK,
  AAD-привязка); integrity/availability blast radius «весь том при компрометации mount-процесса
  или root ВМ» — принятая граница (T5, MVP). Митигация в будущем — per-company volume/bucket
  (операционная мера, отдельный change).
- ACL Object Storage проверяются после policy — при переходе на STS рекомендуется отключить ACL
  бакета (они не сработают как ожидают).
- Static key платформенного SA — критичный секрет: компрометация = kill-switch удалением ключа +
  ротация; аудит выдач (`drive_key_access_log`) — источник расследования.
- Root-компрометация ВМ данным решением не закрывается (принято, T5); TTL ограничивает окно.
- Публичный контракт для обоих репозиториев зафиксирован в design.md change `implement-encrypt`
  (раздел A6) и tasks 7.8–7.12.
