# Software Requirements Specification
## Подсистема шифрования agio Drive

| Поле | Значение |
|---|---|
| **Версия** | 2.2 |
| **Статус** | Final draft |
| **Продукт** | agio Drive — корпоративная файловая система для CG/VFX |
| **Платформа** | agio-platform |
| **Основа** | Форк JuiceFS (ветка `agio-drive-v2`) |
| **Конкурентный ориентир** | LucidLink (паритет по модели шифрования) |
| **Ключевые слова** | ДОЛЖЕН / НЕ ДОЛЖЕН / ЗАПРЕЩЁН / РЕКОМЕНДУЕТСЯ / МОЖЕТ (RFC 2119) |

---

## История версий

| Версия | Изменение |
|---|---|
| 0.13 (план) | Per-file FEK + PG `file_keys`/`subject_keys` + KEK=Argon2id(password) |
| 1.0 | Ревью: отказ от Argon2id, KMS, authz-гейтинг выдачи FEK |
| 1.1 | Отказ от Render Proxy в пользу прямого Render-клиента; FEK только в Redis |
| 2.0 | Двухуровневая CEK + FEK; encrypted local cache; offline-connected режим; revocation; LucidLink parity |
| 2.1 | Корректная модель администратора компании (убран `CheckOrganizationAdmin`); раздел «Версионирование и снепшоты» — только криптографические предусловия |
| **2.2** | **Зафиксирован инвариант идентичности**: OIDC `sub` ≡ `kratos.identity_id` ≡ `user.id` (UUID). Маппинг `sub → user.id` исключён; риск R12 заменён на остаточный R12' (дрейф идентичности). |

---

## 1. Введение

### 1.1 Назначение документа

Документ определяет требования к подсистеме шифрования данных agio Drive на основе модели **Per-File FEK + Per-Chunk CEK** (двухуровневое envelope encryption) с хранением обёрнутых ключей в Redis как части JuiceFS metadata.

Подсистема обеспечивает:
1. Криптографическую защиту содержимого файлов при хранении в S3.
2. Изоляцию: субъект с правами на файл A не может расшифровать файл B.
3. Сохранение zero-copy семантики JuiceFS (`Clone`, `CopyFileRange`, `Compaction`).
4. Поддержку render-нод без OIDC через прямой доступ к Redis с company service key.
5. Отзыв доступа, эквивалентный по эффекту LucidLink.
6. Криптографические предусловия для будущего версионирования/снепшотов.

### 1.2 Область применения

Подсистема охватывает:
- Генерацию, хранение, выдачу и ротацию ключей (CEK/FEK/Company KEK/KMS Master Key).
- Шифрование/дешифрование чанков в S3.
- Интеграцию в форк JuiceFS (`pkg/vfs`, `pkg/chunk`, `pkg/object`, `cmd/mount.go`).
- KeyManager в agio-platform (authz-gated выдача FEK для пользователей).
- Прямой Render-клиент для render-нод.
- Encrypted local cache и offline-connected режим.
- Миграцию существующих нешифрованных данных.

**Вне области:**
- Замена статических S3 credentials на STS (описана как зависимость, реализуется отдельным подэтапом).
- Шифрование метаданных (имена файлов в Redis остаются видимыми).
- Zero-knowledge режим (опциональный premium, §13).
- **Реализация** версионирования, снепшотов и корзины — закладываются только криптографические предусловия (§9).

### 1.3 Определения и сокращения

| Термин | Определение |
|---|---|
| **CEK** | Chunk Encryption Key — случайный AES-256-GCM ключ на каждый чанк; шифрует байты в S3; «путешествует» вместе с чанком |
| **FEK** | File Encryption Key — AES-256-GCM ключ на каждый файл (inode); оборачивает CEK-и чанков файла; гейтится авторизацией |
| **Company KEK** | Симметричный ключ компании; оборачивает FEK-и; хранится в Secret Manager, обёрнут KMS |
| **KMS Master Key** | Корневой ключ (AWS KMS / Yandex KMS / Vault); оборачивает Company KEK |
| **wrapped_fek** | FEK, обёрнутый Company KEK; хранится в Redis в метаданных inode |
| **wrapped_cek** | CEK, обёрнутый FEK; хранится в slice-метаданных чанка |
| **Meta Proxy** | gRPC-прокси JuiceFS для внешних пользователей (`pkg/meta/grpc_server.go`) |
| **Render-клиент** | Прямой FUSE-клиент для render-нод: Redis + S3 без прокси |
| **KeyManager** | gRPC-сервис в agio-platform для authz-gated выдачи FEK |
| **Company Owner** | Пользователь с `PermissionOwn` на компанию; bypass всех проверок в `DriveAuthorizationService` |
| **No residuality** | Принцип: после logout/disconnect локальные данные нечитаемы |
| **Kratos** | Ory Kratos — сервис управления идентичностями; источник правды о пользователях, точка регистрации |
| **Hydra** | Ory Hydra — OIDC/OAuth2-провайдер; аутентифицирует пользователей через Kratos |
| **Инвариант идентичности** | Гарантированное равенство `OIDC sub` = `kratos.identity_id` = `user.id` (UUID) |

### 1.4 Ссылки на фактическое состояние кода

| Компонент | Файл / сущность | Релевантность |
|---|---|---|
| Authz gRPC | `authz_service.go` → `AuthzService.CheckPermission`, `CheckBulkPermissions` | Точка гейтинга выдачи FEK |
| Движок прав | `auth.go` → `DriveAuthorizationService.CheckBulkPermissionsByPaths` | Переиспользуется для проверки Read перед выдачей FEK |
| Company Owner bypass | `auth.go` → `checkBulkPermissions`, `companyOwnerMap` (`auth.PermissionOwn`) | Механизм «администратора компании» |
| Уровни прав | `permission_level.go` → `PermissionLevel{None,View,Read,Edit,Manage}` | FEK выдаётся при уровне ≥ `PermissionLevelRead` |
| Объекты Drive | `000099_add_drive_file.up.sql` → `drive_object`, `drive_facility_file` | Бизнес-проекция (eventual consistency) |
| Права | `000163_add_drive_object_permission.up.sql` → `drive_object_permission` | Authz-модель `(subject_type, subject_id UUID)` |
| Кэш прав | `auth.go` → `PermissionCache` (Redis `driveperm:{userID}:{objectID}`, TTL) | Паттерн для кэша FEK |
| LRU path→object | `auth.go` → `driveObjectCache` (LRU 500k) | Паттерн для FEK-кэша |
| Identity | OIDC `sub` ≡ `kratos.identity_id` ≡ `user.id` (UUID) | Маппинг не требуется; используется напрямую как `drive_object_permission.subject_id` |

### 1.5 Стратегические решения (зафиксированы)

| ID | Решение | Обоснование |
|---|---|---|
| **S1** | Per-File FEK обязателен | Требование аудита (SOC 2, MPAA TPN); паритет с LucidLink |
| **S2** | Готовность к глубокому форку JuiceFS | Совместимость с upstream не требуется |
| **S3** | Безопасность > дедупликация | Для CG/VFX приемлемо |
| **S4** | Паритет с LucidLink по модели шифрования | Минимальное конкурентное требование |
| **S5** | Внешние пользователи — первичный класс; render-ферма — вторичный | Render через изолированный прямой клиент |
| **S6** | Zero-knowledge НЕ в MVP | Требует паролей/RSA; ломает render + recovery |
| **S7** | FEK хранится только в Redis (не в PG) | Redis = source of truth для JuiceFS metadata; снимает eventual consistency проблему |
| **S8** | Render-ноды без прокси: прямой Redis + company service key | Минимальная latency, отсутствие bottleneck при тысячах RPS × сотни нод |
| **S9** | Двухуровневая модель CEK + FEK | Сохраняет zero-copy Clone/CopyFileRange/Compaction без утечек |
| **S10** | «Администратор компании» = пользователь с `PermissionOwn`, не отдельный механизм | Уже реализовано байпасом в `DriveAuthorizationService` |
| **S11** | Версионирование/снепшоты не реализуются на этом этапе | Закладываются только криптографические предусловия (§9) |
| **S12** | Инвариант идентичности: OIDC `sub` ≡ `user.id` (UUID) | Источник идентичности один (Kratos); маппинг не нужен |

---

## 2. Общее описание

### 2.1 Контекст продукта

```
Metadata plane:  AGIO Client → Meta Proxy gRPC → Redis (JuiceFS meta)
Data plane:      AGIO Client → S3 (прямой доступ)
Authz plane:     Meta Proxy → agio-platform AuthzService → PostgreSQL
Render plane:    Render Client → Redis (прямой) + S3 (прямой)
```

**Критические факты:**
1. Данные НЕ проходят через gRPC Meta Proxy. Шифрование — только на клиенте.
2. Render-ноды: тысячи metadata RPS с ноды × сотни нод. Прямой Redis без прокси.
3. `drive_object` в PG — асинхронная проекция (Kafka). Source of truth — Redis.
4. **Идентичность:** пользователи регистрируются в Kratos и импортируются на платформу; Hydra выполняет OIDC-аутентификацию через Kratos. Гарантируется **инвариант**: OIDC `sub` = `kratos.identity_id` = `user.id` (UUID). Отдельный маппинг `sub → user.id` НЕ требуется; `sub` используется напрямую как `drive_object_permission.subject_id` (тип UUID).

### 2.2 Классы пользователей

| Класс | Идентификация | Путь доступа | Права |
|---|---|---|---|
| **Внешний пользователь (художник)** | OIDC `sub` (= `user.id` UUID) | Meta Proxy → KeyManager | Per-file, authz-gated (`DriveAuthorizationService`) |
| **Администратор компании (Company Owner)** | OIDC `sub` (= `user.id` UUID) | Meta Proxy → KeyManager | Пользователь с `PermissionOwn` на компанию → **bypass всех проверок** в `DriveAuthorizationService` (`companyOwnerMap`, always true). Отдельного криптографического механизма не требует |
| **Render-нода** | Company service key | Прямой Redis + S3 | Полный доступ в рамках компании |

### 2.3 Идентичность

| ID | Требование |
|---|---|
| **FR-ID-1** | Цепочка идентичности: Kratos (регистрация) → импорт в платформу (user.id) → Hydra (OIDC, sub = identity_id). Система ПОЛАГАЕТСЯ на этот инвариант. |
| **FR-ID-2** | OIDC `sub` ДОЛЖЕН интерпретироваться напрямую как `user.id` (UUID) во всех компонентах. Маппинг/преобразование НЕ выполняется. |
| **FR-ID-3** | Meta Proxy и KeyManager ДОЛЖНЫ валидировать, что `sub` является валидным UUID. При неверном формате — fail-closed (InvalidArgument/Unauthenticated), так как это признак ошибки конфигурации идентичности. |
| **FR-ID-4** | Если `user.id` из токена отсутствует в платформе (пользователь в Kratos есть, но ещё не импортирован, либо удалён), `DriveAuthorizationService` возвращает отсутствие прав → доступ ОТКЛОНЯЕТСЯ (fail-closed). |

### 2.4 Административные операции платформы

| ID | Требование |
|---|---|
| **FR-ADMIN-1** | `CheckOrganizationAdmin` и связанные административные RPC JuiceFS Meta engine (`Init`, `GetFormat`, `Compact*`, `Quota`, `DumpMeta`, `Remove`) НЕ являются частью пользовательской модели доступа и ИСКЛЮЧАЮТСЯ из настоящего SRS. Это проверка **админа AGIO-платформы**, а не администратора компании. |
| **FR-ADMIN-2** | Административные операции над томом ДОЛЖНЫ выполняться либо через **отдельный привилегированный клиент** (по аналогии с render-node mount клиентом, с сервисным ключом), либо быть удалены из публичного gRPC-интерфейса. |
| **FR-ADMIN-3** | «Администратор компании» в контексте шифрования — это пользователь с `PermissionOwn`; выдача FEK ему идёт через тот же KeyManager-путь, но authz-проверка возвращает `true` байпасом. Отдельного криптографического механизма НЕ требуется. |

### 2.5 Допущения и зависимости

- **A1.** KMS-провайдер: AWS KMS / Yandex KMS / HashiCorp Vault (интерфейс абстрагирован).
- **A2.** Все gRPC-каналы — TLS 1.2+; Render-клиент — TLS 1.3.
- **A3.** Redis в доверенной сети; компрометация Redis — угроза (см. §3).
- **A4.** `drive_object.id` генерируется клиентом при Create (client-side UUID) и синхронизируется в PG через Kafka.
- **A5.** **ЗАВИСИМОСТЬ:** замена статических `Format.SecretKey` на STS — обязательное условие для реального отзыва доступа (см. FR-REV-3). Реализуется отдельным подэтапом.
- **A6.** **ИНВАРИАНТ ИДЕНТИЧНОСТИ:** OIDC `sub` ≡ `kratos.identity_id` ≡ platform `user.id` (UUID). Обеспечивается схемой: регистрация в Kratos → импорт в платформу → Hydra аутентифицирует через Kratos. Все компоненты (Meta Proxy, KeyManager, DriveAuthorizationService) используют `sub` напрямую как `user.id` без преобразования.

---

## 3. Threat Model

### 3.1 Защищаемые активы

| Актив | Расположение | Ценность |
|---|---|---|
| Содержимое файлов (чанки) | S3 | Критическая (IP клиентов) |
| CEK/FEK | RAM клиентов, Redis (wrapped) | Критическая |
| Company KEK | Secret Manager | Критическая |
| Метаданные (имена, структура) | Redis, PG | Высокая |

### 3.2 Угрозы и контрмеры

| ID | Угроза | Контрмера | Статус |
|---|---|---|---|
| T1 | Утечка S3-бакета | Per-file FEK + per-chunk CEK; ключей в S3 нет | ✅ Защищено |
| T2 | Компрометация Redis | FEK обёрнуты Company KEK; CEK обёрнуты FEK | ✅ Защищено |
| T3 | Пользователь читает чужой файл | Per-file FEK + authz-gated KeyManager | ✅ Защищено |
| T4 | Инсайдер: массовая выгрузка через легитимный доступ | Audit + anomaly detection + STS + rate-limit | ⚠️ Компенсирующие |
| T5 | Компрометация render-ноды | Изоляция per-company (company key) | ⚠️ Ограничено компанией |
| T6 | Компрометация Meta Proxy / KeyManager | FEK не хранится дольше запроса; TLS; аудит | ⚠️ Доверенный компонент |
| T7 | Атака на KMS | HSM; ротация; IAM least privilege | ✅ Облачный KMS |
| T8 | **Легитимный пользователь сохранил ключи + данные offline** | **НЕ защищено криптографически** | ❌ Принятая граница |
| T9 | **Дамп памяти подключённого клиента** | **НЕ защищено программно** | ❌ Принятая граница |
| T10 | Потерянное устройство | Encrypted local cache (no residuality) | ✅ Защищено |
| T11 | Случайное/вредительское удаление данных | Будущее версионирование/корзина (§9); на текущем этапе — только криптографические предусловия | ⚠️ Отложено |

### 3.3 Фундаментальный предел (зафиксировать для аудита)

> Ни одна криптографическая схема не может помешать авторизованному пользователю, который легитимно получил и сохранил ключи И данные, расшифровать их позже. Это свойство любой системы с клиентской расшифровкой (включая LucidLink, Dropbox, все E2EE).

**Компенсирующие контроли:** audit, anomaly detection, STS, encrypted cache, no-full-replication, юридические меры. Для top-secret контента — pixel streaming / remote review (данные не покидают доверенную зону).

### 3.4 Границы доверия

| Зона | Компоненты | Доверие |
|---|---|---|
| Доверенная | Meta Proxy, KeyManager, agio-platform, PG, Redis, KMS, Secret Manager | Полное |
| Полу-доверенная | Клиент художника (после OIDC) | Ограничено per-file FEK + STS |
| Изолированная | Render-нода (company key) | Только своя компания |
| Недоверенная | S3 | Шифрование обязательно |

---

## 4. Иерархия ключей

### 4.1 Схема

```
KMS Master Key (facility или per-company)
    │ encrypt
    ▼
Company KEK (хранится в Secret Manager)
    │ encrypt (wrap)
    ▼
FEK (per file/inode) ──── stored wrapped in Redis inode metadata
    │ encrypt (wrap)
    ▼
CEK (per chunk) ──── stored wrapped in slice metadata
    │ AES-256-GCM
    ▼
Chunk data in S3
```

### 4.2 Требования к ключам

| ID | Требование |
|---|---|
| **FR-KEY-1** | CEK ДОЛЖЕН быть уникальным случайным AES-256-GCM ключом (32 байта, CSPRNG) на каждый чанк. |
| **FR-KEY-2** | FEK ДОЛЖЕН быть уникальным случайным AES-256-GCM ключом (32 байта, CSPRNG) на каждый файл (inode). |
| **FR-KEY-3** | FEK ДОЛЖЕН быть обёрнут Company KEK и сохранён в Redis в метаданных inode (поле `wrapped_fek`). |
| **FR-KEY-4** | Каждый CEK ДОЛЖЕН быть обёрнут FEK файла и сохранён в slice-метаданных чанка (поле `wrapped_cek`). |
| **FR-KEY-5** | Company KEK ДОЛЖЕН быть один на компанию. Хранится в Secret Manager, обёрнут KMS Master Key. |
| **FR-KEY-6** | Plaintext CEK/FEK/Company KEK НЕ ДОЛЖНЫ сохраняться ни в одном стойком хранилище (Redis, PG, S3, файлы, логи). Plaintext допустим только в RAM на время операции. |
| **FR-KEY-7** | KMS Master Key ДОЛЖЕН поддерживать automatic rotation. |
| **FR-KEY-8** | РЕКОМЕНДУЕТСЯ per-company KMS Master Key для строгой tenant-изоляции в production. |

### 4.3 Формат обёрток

**wrapped_fek** (в Redis inode metadata):
```
{
  magic:       "AGFK" (4 bytes)
  version:     uint8
  kek_version: uint32       // версия Company KEK
  nonce:       12 bytes
  ciphertext:  encrypted FEK (32 bytes)
  tag:         16 bytes     // GCM auth tag
}
AAD = volume_uuid || company_id || drive_file_id || inode || fek_version
```

**wrapped_cek** (в slice metadata):
```
{
  magic:       "AGCK" (4 bytes)
  version:     uint8
  nonce:       12 bytes
  ciphertext:  encrypted CEK (32 bytes)
  tag:         16 bytes
}
AAD = drive_file_id || chunk_id || fek_version
```

**Зашифрованный чанк в S3:**
```
{
  magic:       "AGDF" (4 bytes)
  version:     uint8
  nonce:       12 bytes     // уникален на каждую запись
  ciphertext:  N bytes
  tag:         16 bytes
}
AAD = chunk_id || slice_index
```

---

## 5. Хранение ключей в Redis

### 5.1 Что хранится

| ID | Требование |
|---|---|
| **FR-REDIS-1** | `wrapped_fek` ДОЛЖЕН храниться в метаданных inode (расширение attr или xattr, читаемый в том же round-trip что и `GetAttr`). |
| **FR-REDIS-2** | `wrapped_cek` ДОЛЖЕН храниться в slice-метаданных чанка (расширение структуры slice). |
| **FR-REDIS-3** | Metadata ДОЛЖНА включать поля: `drive_file_id` (UUID), `encrypted` (bool), `fek_version` (uint32), `crypto_alg` (string). |
| **FR-REDIS-4** | `wrapped_fek` НЕ ДОЛЖЕН включать path в AAD (файл может быть переименован). |
| **FR-REDIS-5** | Redis metadata backups ДОЛЖНЫ считаться критичными для восстановления данных и храниться в зашифрованном виде. |
| **FR-REDIS-6** | Потеря `wrapped_fek` ДОЛЖНА рассматриваться как потеря доступа к содержимому файла. |

### 5.2 Что НЕ хранится в Redis

- Plaintext FEK/CEK.
- Company KEK.
- Любые ключи субъектов (UEK/GEK — исключены из модели).

### 5.3 Что хранится в PG (только аудит и управление)

```sql
-- Управление Company KEK (метаданные, не сами ключи)
CREATE TABLE drive_company_crypto_key (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id      UUID NOT NULL REFERENCES company(id),
    key_purpose     VARCHAR(32) NOT NULL,    -- 'fek_wrap'
    key_version     INT NOT NULL,
    kms_key_id      VARCHAR(255) NOT NULL,   -- ARN/ID KMS master key
    secret_ref      VARCHAR(512) NOT NULL,   -- путь в Secret Manager
    status          VARCHAR(32) NOT NULL,    -- 'active' | 'retiring' | 'retired'
    created_by_id   UUID NOT NULL REFERENCES "user"(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at      TIMESTAMPTZ,
    UNIQUE(company_id, key_purpose, key_version)
);

-- Аудит выдачи ключей
CREATE TABLE drive_key_access_log (
    id              BIGSERIAL PRIMARY KEY,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_type      VARCHAR(32) NOT NULL,    -- 'user' | 'render_node' | 'service'
    actor_id        VARCHAR(128) NOT NULL,   -- UUID пользователя (user.id) или service id
    company_id      UUID,
    drive_file_id   UUID,
    inode           BIGINT,
    operation       VARCHAR(32) NOT NULL,    -- 'get_fek' | 'create_fek' | 'rotate' | 'deny'
    permission      VARCHAR(16),
    result          VARCHAR(16) NOT NULL,    -- 'allow' | 'deny' | 'error'
    reason          TEXT,
    request_id      UUID,
    client_ip       INET
);
CREATE INDEX idx_key_access_log_time ON drive_key_access_log (created_at DESC);
CREATE INDEX idx_key_access_log_actor ON drive_key_access_log (actor_id, created_at DESC);
```

Таблицы `file_keys` и `subject_keys` из плана v13 **исключены**.

---

## 6. Пользовательский path (внешние клиенты)

### 6.1 Create файла

```
Client → Meta Proxy Create
  → redisMeta.Create inode
  → KeyManager.CreateFileKey(user, path, volume)
      → authz check (authz_interceptor: Write permission)
      → generate FEK
      → wrap FEK with Company KEK
      → return {plaintext_fek, wrapped_fek}
  → Meta Proxy writes wrapped_fek + drive_file_id into Redis inode
  → return FEK to client
Client generates CEK per chunk, wraps by FEK, encrypts, writes to S3
```

| ID | Требование |
|---|---|
| **FR-USR-1** | При `Create` файла Meta Proxy ДОЛЖЕН вызвать KeyManager для генерации FEK и записать `wrapped_fek` в inode атомарно с созданием. |
| **FR-USR-2** | Если запись `wrapped_fek` не удалась, файл ДОЛЖЕН быть помечен как unusable или удалён (rollback). |
| **FR-USR-3** | `drive_file_id` (UUID) ДОЛЖЕН генерироваться клиентом при Create и передаваться в Meta Proxy. |

### 6.2 Open файла

```
Client → Meta Proxy Open
  → AuthzInterceptor: authz check (Read или Edit в зависимости от flags)
  → KeyManager.GetFileFEK(user, file_id, path, volume)
      → DriveAuthorizationService.CheckBulkPermissionsByPaths (Read/Edit)
      → если Company Owner (PermissionOwn): bypass → true
      → if denied: audit deny; return PermissionDenied
      → read wrapped_fek from Redis
      → unwrap FEK with Company KEK
      → audit allow
      → return plaintext FEK
  → Meta Proxy returns FEK + wrapped_cek list to client
Client caches FEK in RAM (LRU), unwraps CEKs on demand
```

| ID | Требование |
|---|---|
| **FR-USR-4** | При `Open` с флагами чтения FEK ДОЛЖЕН выдаваться только при уровне прав ≥ `PermissionLevelRead`. |
| **FR-USR-5** | При `Open` с флагами записи FEK ДОЛЖЕН выдаваться только при уровне прав ≥ `PermissionLevelEdit`. |
| **FR-USR-6** | KeyManager ДОЛЖЕН валидировать, что OIDC `sub` является валидным UUID (FR-ID-3), и использовать его напрямую как `user.id` (FR-ID-2). Маппинг НЕ выполняется. |
| **FR-USR-7** | KeyManager ДОЛЖЕН использовать singleflight для коалесценции одновременных запросов одного FEK. |
| **FR-USR-8** | KeyManager ДОЛЖЕН вести audit log каждой выдачи FEK. |
| **FR-USR-9** | Клиент ДОЛЖЕН кэшировать FEK в LRU (100k записей, TTL 15 мин). |
| **FR-USR-10** | Клиент ДОЛЖЕН кэшировать развёрнутые CEK в RAM на время жизни открытого файла. |
| **FR-USR-11** | Для Company Owner (`PermissionOwn`) authz-проверка возвращает `true` байпасом; FEK выдаётся через тот же путь без отдельного механизма (FR-ADMIN-3). |

### 6.3 Revoke прав

| ID | Требование |
|---|---|
| **FR-USR-12** | При отзыве прав (`DeleteRole`/`ClearRoles`) система ДОЛЖНА удалить `drive_object_permission` и инвалидировать кэши (см. §11). |
| **FR-USR-13** | FEK/CEK в Redis НЕ удаляются при отзыве одного пользователя (они нужны другим авторизованным пользователям). |

---

## 7. Render path (прямой клиент)

### 7.1 Архитектура

```
Render Node (FUSE)
    │
    ├── Redis (прямой доступ): metadata + wrapped_fek + wrapped_cek
    │
    ├── S3 (прямой доступ): зашифрованные чанки
    │
    └── Company KEK (в RAM, из Secret Manager при mount)
```

**НЕТ:** Meta Proxy, KeyManager RPC, OIDC, PG, authz-проверки.

### 7.2 Требования

| ID | Требование |
|---|---|
| **FR-RND-1** | Render-клиент ДОЛЖЕН монтировать volume напрямую в Redis, без Meta Proxy. |
| **FR-RND-2** | Company KEK ДОЛЖЕН запрашиваться из Secret Manager при mount с использованием identity ноды (IAM/SA). ЗАПРЕЩЕНО передавать ключ через CLI-аргументы. |
| **FR-RND-3** | Company KEK ДОЛЖЕН храниться только в RAM (mlock, no swap). |
| **FR-RND-4** | Render-клиент ДОЛЖЕН блокировать доступ к путям вне company prefix (defense in depth; криптографическая изоляция обеспечивается разными Company KEK). |
| **FR-RND-5** | Render-клиент НЕ ДОЛЖЕН выполнять per-file authz. Доступ ограничен компанией криптографически. |
| **FR-RND-6** | `wrapped_fek` ДОЛЖЕН читаться в том же round-trip что и attr. |
| **FR-RND-7** | Render-клиент ДОЛЖЕН кэшировать развёрнутые FEK (LRU 100k, TTL 1 час). |
| **FR-RND-8** | Render-клиент ДОЛЖЕН кэшировать развёрнутые CEK на время жизни открытого файла. |
| **FR-RND-9** | При Create render-клиент ДОЛЖЕН генерировать FEK и записывать `wrapped_fek` в Redis самостоятельно (без KeyManager). |
| **FR-RND-10** | PG-запись `drive_object` для render-created файла создаётся асинхронно. Пользовательский доступ к render-выводу появляется после синхронизации. |
| **FR-RND-11** | Render-режим ДОЛЖЕН поддерживать увеличенные кэши метаданных (attr TTL ≥ 60s, dir TTL ≥ 60s). |
| **FR-RND-12** | Render-клиент ДОЛЖЕН поддерживать batch/prefetch при readdir (Redis pipelining). |
| **FR-RND-13** | Общий Redis между компаниями ДОПУСТИМ: изоляция обеспечивается разными Company KEK. |
| **FR-RND-14** | Render Proxy (gRPC-прокси для render) НЕ требуется и НЕ реализуется. |

### 7.3 Пользовательский доступ к render-выводу

| ID | Требование |
|---|---|
| **FR-USR-14** | При первом пользовательском доступе к render-created файлу KeyManager ДОЛЖЕН прочитать `wrapped_fek` из Redis, развернуть Company KEK, проверить authz и вернуть FEK пользователю. |

---

## 8. Обработка JuiceFS-операций (CEK + FEK)

### 8.1 Общий принцип

- Данные в S3 зашифрованы **CEK**-ами.
- CEK обёрнуты **FEK**-ом файла.
- При шаринге чанков между файлами (Clone/CopyFileRange) CEK **переоборачивается** под FEK target-файла. Это операция над ключом, а НЕ над данными. Zero-copy сохраняется.

### 8.2 Операции

| Операция | Требования |
|---|---|
| **Clone** | FR-OP-1: target получает новый FEK. FR-OP-2: для каждого переносимого чанка создаётся `wrap(FEK_target, CEK)`; данные в S3 не переписываются. FR-OP-3: при клонировании подмножества CEK остальных чанков source НЕ доступны через target. |
| **CopyFileRange** | FR-OP-4: re-wrap CEK под FEK target. FR-OP-5: при разрезании чанка — новый чанк с новым CEK. |
| **Compaction** | FR-OP-6: в рамках одного inode → один FEK. FR-OP-7: read → unwrap CEKs → decrypt → merge → new CEK → encrypt → wrap under FEK → update slices. FR-OP-8: нормализует чанки от Clone под собственный FEK. |
| **Truncate / SetAttr / Rename** | FR-OP-9: FEK не меняется. |
| **Write** | FR-OP-10: новые CEK оборачиваются текущим FEK. |

### 8.3 Дедупликация

| ID | Требование |
|---|---|
| **FR-OP-11** | Глобальная content-based дедупликация в JuiceFS отсутствует. Независимые записи одинакового контента создают разные `chunkId`. Конфликта CEK не возникает. |
| **FR-OP-12** | Шаринг чанков возникает ТОЛЬКО через Clone/CopyFileRange и обрабатывается re-wrap CEK. |

---

## 9. Версионирование и снепшоты: криптографические предусловия

### 9.1 Контекст и статус

- **Потребность:** защита от случайного и вредительского удаления данных, история изменений файлов, восстановление — паритет со снепшотами LucidLink.
- **У LucidLink:** мгновенные снепшоты всего filespace на основе log-structured design + copy-on-write; дельта между снепшотами; защита от рансомвари.
- **В JuiceFS:** нативных снепшотов и per-file версионирования **нет**; есть корзина (`.trash`, ограниченная, неудобная).
- **Статус в настоящем SRS:** версионирование, снепшоты и корзина **НЕ проектируются и НЕ реализуются**. Корзина не прорабатывается (отложена). Закладываются **только криптографические предусловия**, чтобы будущее версионирование не потребовало переделки шифрования.

### 9.2 Почему это важно зафиксировать сейчас

Модель CEK + FEK хорошо совместима с версионированием/снепшотами:
1. Чанки иммутабельны и зашифрованы своим CEK — не меняются между версиями.
2. Восстановление прозрачно для шифрования (пока сохранены `wrapped_fek` и чанки).
3. `wrapped_cek` живут в slice-метаданных — версия естественно несёт свои ключи.

Но есть **четыре** места, где шифрование должно «пережить» версионирование. Если их не заложить сейчас, потом потребуется переделка криптографического слоя.

### 9.3 Криптографические предусловия (обязательны на этом этапе)

| ID | Требование |
|---|---|
| **FR-VER-1** | **FEK reference counting / lifecycle.** Схема метаданных и политика удаления ДОЛЖНЫ допускать ситуацию, когда FEK не может быть удалён, пока на него ссылается хотя бы одна активная версия, запись корзины или снепшот. Конкретный механизм версионирования при этом не реализуется. |
| **FR-VER-2** | **Version-aware GC (предусловие).** Логика сборки мусора ДОЛЖНА проектироваться с учётом того, что в будущем появятся ссылки на чанки из версий/снепшотов, и такие чанки (вместе с их `wrapped_cek`) и соответствующие FEK не должны удаляться. |
| **FR-VER-3** | **Version-aware компакция (предусловие).** Компакция ДОЛЖНА проектироваться так, чтобы в будущем её можно было ограничить для чанков, на которые ссылаются версии/снепшоты (либо исключить версионированные файлы, либо «замораживать» версии). |
| **FR-VER-4** | **Версии FEK.** Поле `fek_version` в `wrapped_fek`/`wrapped_cek` (см. §4.3, §5.1) ДОЛЖНО поддерживаться, чтобы в будущем файл мог иметь несколько версий FEK (для чтения старых версий после ротации) без изменения формата. |

### 9.4 Что явно НЕ входит в этот раздел

- Реализация корзины, self-service восстановления, политик удержания, прав на очистку корзины.
- Реализация per-file версионирования.
- Реализация снепшотов тома.
- Защита от вредительского удаления (политическая, не криптографическая задача).

Эти темы фиксируются как **будущая функциональность** (дорожная карта ниже) и в настоящем документе не прорабатываются.

### 9.5 Дорожная карта (справочно, вне текущего этапа)

| Фаза | Содержание | Статус |
|---|---|---|
| 1 | Корзина + self-service восстановление | Не прорабатывается |
| 2 | Версионирование файлов | Не прорабатывается |
| 3 | Снепшоты тома (LucidLink-стиль) | Не прорабатывается |

---

## 10. Encrypted Local Cache и Offline-Connected режим

### 10.1 No Residuality

| ID | Требование |
|---|---|
| **NFR-SEC-1** | Локальный дисковый кэш чанков ДОЛЖЕН содержать только ciphertext (encryption before caching). Plaintext-кэш на диске ЗАПРЕЩЁН. |
| **NFR-SEC-2** | Все plaintext-ключи (FEK, CEK) ДОЛЖНЫ храниться только в RAM процесса клиента. |
| **NFR-SEC-3** | При явном logout, истечении OIDC-сессии, получении revoke-сигнала, или превышении таймаута недоступности hub, клиент ДОЛЖЕН обнулить plaintext-ключи в RAM (`memclr`, не GC) и перейти в состояние disconnected. |
| **NFR-SEC-4** | После disconnected локальный кэш ДОЛЖЕН стать нечитаемым. |
| **NFR-SEC-5** | Страницы памяти с ключами ДОЛЖНЫ быть `mlock`-нуты (запрет swap). |

### 10.2 Offline-Connected режим

| ID | Требование |
|---|---|
| **NFR-OFF-1** | При недоступности Meta Proxy / KeyManager клиент продолжает читать/писать данные из локального кэша в пределах TTL сессии и STS-credentials. |
| **NFR-OFF-2** | В offline-connected режиме клиент НЕ запрашивает новые FEK (fail-closed для новых файлов). |
| **NFR-OFF-3** | При восстановлении связи клиент возобновляет полную работу (re-fetch FEK, sync dirty writes). |
| **NFR-OFF-4** | Таймаут недоступности hub конфигурируем (по умолчанию 15 минут); по истечении — disconnected (NFR-SEC-3). |

### 10.3 Три режима клиента

| Режим | Сеть | Ключи в RAM | Кэш доступен |
|---|---|---|---|
| Online | есть | есть | ✅ |
| Offline-connected | нет / hub недоступен | есть (сессия жива) | ✅ |
| Disconnected / logged out | любое | нет (обнулены) | ❌ |

---

## 11. Отзыв доступа (Revocation Process)

### 11.1 Процесс

```
Триггер: отзыв прав пользователя X / offboarding X.

1. CONTROL PLANE (мгновенно)
   └─ soft-delete drive_object_permission для X
   └─ bump «permission generation» счётчика компании в Redis
   └─ publish событие в Kafka

2. ИНВАЛИДАЦИЯ КЭШЕЙ
   └─ Redis permission cache (driveperm:{user}:{object}) — TTL ≤ 30s
   └─ FEK-кэш в KeyManager — удалить записи X
   └─ Клиентские FEK/CEK кэши — по revoke-сигналу

3. DATA PLANE (требует STS!)
   └─ STS-токен X истекает в пределах TTL (15–60 мин)
   └─ X теряет доступ к S3

4. РОТАЦИЯ КЛЮЧЕЙ (для чувствительных данных / offboarding)
   └─ FEK rotation: re-wrap CEKs под новый FEK (дёшево, без перешифровки S3)
   └─ CEK rotation: перешифровка данных в S3 (дорого, для критичных файлов)

5. СИГНАЛ КЛИЕНТАМ
   └─ revoke-signal → клиент обнуляет FEK/CEK в RAM → disconnected
```

### 11.2 Требования

| ID | Требование |
|---|---|
| **FR-REV-1** | При отзыве прав KeyManager ДОЛЖЕН немедленно отклонять новые запросы FEK от отозванного пользователя (fail-closed). |
| **FR-REV-2** | Инвалидация кэшей ДОЛЖНА происходить в пределах TTL кэша (≤ 30s для authz, ≤ 15 мин для клиентского FEK-кэша). |
| **FR-REV-3** | **ЗАВИСИМОСТЬ:** Data-plane ДОЛЖЕН использовать STS (короткоживущие S3 credentials, TTL ≤ 60 мин). Без STS отзыв доступа к S3 невозможен. |
| **FR-REV-4** | FEK rotation (re-wrap CEKs) ДОЛЖНА поддерживаться как операция без перешифрования данных в S3. |
| **FR-REV-5** | CEK rotation (перешифровка данных) ДОЛЖНА поддерживаться для критичных файлов / инцидентов. |
| **FR-REV-6** | При offboarding пользователя из компании РЕКОМЕНДУЕТСЯ FEK rotation всех файлов, к которым пользователь имел доступ. |
| **FR-REV-7** | Клиент ДОЛЖЕН обрабатывать revoke-signal от Meta Proxy и обнулять ключи в RAM. |

### 11.3 Что НЕ защищает (зафиксировать)

| Сценарий | Защита |
|---|---|
| Пользователь сохранил plaintext CEK + ciphertext offline | ❌ Не защищено (фундаментальный предел) |
| Дамп памяти подключённого клиента | ❌ Не защищено программно |
| Пользователь скачал данные ДО отзыва | ❌ Не защищено для уже скачанного |
| Пользователь пытается читать ПОСЛЕ отзыва | ✅ Защищено (authz deny + STS expiry + cache invalidation) |
| Потерянное устройство после logout | ✅ Защищено (encrypted cache, no residuality) |

---

## 12. Ротация ключей

### 12.1 Виды ротации

| Тип | Что делает | Данные в S3 | Аннулирует |
|---|---|---|---|
| **KMS Master Key** | Re-wrap Company KEK в Secret Manager | Не затрагиваются | — |
| **Company KEK** | Re-wrap всех `wrapped_fek` в Redis под новый KEK (`kek_version`) | Не перешифровываются | Сохранённые старые Company KEK |
| **FEK** | Генерация нового FEK + re-wrap всех CEK под новый FEK (`fek_version`) | Не перешифровываются (CEK те же) | Сохранённые старые FEK |
| **CEK** | Генерация новых CEK + перешифровка данных в S3 + обновление `wrapped_cek` | **Перешифровываются** | Старые CEK для S3-копии |

### 12.2 Требования

| ID | Требование |
|---|---|
| **FR-ROT-1** | Ротация Company KEK выполняется через re-wrap `wrapped_fek`; данные в S3 не перешифровываются. |
| **FR-ROT-2** | Во время ротации старый KEK в статусе `retiring` до завершения re-wrap. |
| **FR-ROT-3** | `kek_version` в `wrapped_fek` указывает версию KEK. |
| **FR-ROT-4** | Ротация FEK = re-wrap CEK; данные в S3 не перешифровываются. |
| **FR-ROT-5** | Ротация FEK аннулирует сохранённые FEK, но НЕ сохранённые CEK. |
| **FR-ROT-6** | Ротация CEK = перешифровка данных; аннулирует старые CEK для S3-копии. |
| **FR-ROT-7** | Ротация CEK НЕ защищает offline-копию атакующего. |
| **FR-ROT-8** | KMS Master Key поддерживает automatic rotation. |
| **FR-ROT-9** | Ротация с учётом версий: при наличии версий файла система хранит версии FEK (`fek_version`) или re-wrap CEK всех версий (см. FR-VER-4). |

---

## 13. Опциональный premium: Zero-Knowledge режим

### 13.1 Описание

Для клиентов, требующих LucidLink-уровень zero-knowledge:
- Каждый пользователь имеет RSA keypair.
- FEK оборачивается public RSA key каждого авторизованного субъекта (subject-bound wrapping).
- Private RSA key шифруется паролем пользователя (PBKDF2/Argon2id).
- Платформа НЕ имеет доступа к plaintext FEK.

### 13.2 Ограничения

| Ограничение | Пояснение |
|---|---|
| Требует паролей | Конфликт с OIDC-only моделью |
| Нет password reset | Потеря пароля = потеря данных |
| Нет render-поддержки | Render-ноды без пароля не могут получить FEK |
| Нет platform recovery | Платформа не может восстановить данные |

### 13.3 Требования

| ID | Требование |
|---|---|
| **FR-ZK-1** | Zero-knowledge режим МОЖЕТ быть реализован как опциональная надстройка для отдельных компаний. |
| **FR-ZK-2** | Zero-knowledge режим НЕ ДОЛЖЕН быть активирован одновременно с render-доступом для той же компании. |
| **FR-ZK-3** | Zero-knowledge режим НЕ входит в MVP. |

---

## 14. Нефункциональные требования

### 14.1 Безопасность

| ID | Требование |
|---|---|
| **NFR-SEC-6** | Алгоритм: AES-256-GCM для данных и обёрток ключей. |
| **NFR-SEC-7** | Генерация ключей: CSPRNG (`crypto/rand`). |
| **NFR-SEC-8** | TLS 1.2+ для всех gRPC; TLS 1.3 для Render-клиента. |
| **NFR-SEC-9** | Nonce GCM уникален для каждой операции записи с тем же ключом. |
| **NFR-SEC-10** | Plaintext ключи НЕ попадают в логи, метрики, трейсы, core dumps. |
| **NFR-SEC-11** | GCM auth tag проверяется при чтении; несовпадение → fail-closed. |
| **NFR-SEC-12** | Company KEK в Secret Manager доступен только сервисным субъектам agio-platform и render-нодам (IAM least privilege). |

### 14.2 Производительность

| ID | Требование | Цель |
|---|---|---|
| **NFR-PERF-1** | Latency `GetFileFEK` при кэш-хите | ≤ 5 мс (p99) |
| **NFR-PERF-2** | Latency `GetFileFEK` при кэш-промахе | ≤ 100 мс (p99) |
| **NFR-PERF-3** | Overhead шифрования на throughput | ≤ 10% |
| **NFR-PERF-4** | LRU-кэш FEK на клиенте | 100k, TTL 15 мин |
| **NFR-PERF-5** | LRU-кэш FEK в Render-клиенте | 100k, TTL 1 час |
| **NFR-PERF-6** | `wrapped_fek` читается в том же round-trip что и attr | 0 доп. Redis-запросов |
| **NFR-PERF-7** | Batch `GetBulkFileFEK` | до 1000 ключей |
| **NFR-PERF-8** | FEK/CEK unwrap один раз на файл за сессию | Обязательно |

### 14.3 Доступность

| ID | Требование |
|---|---|
| **NFR-AVAIL-1** | Доступность KeyManager ≥ 99.9%. |
| **NFR-AVAIL-2** | При недоступности KeyManager чтение открытых файлов продолжается (FEK в RAM). |
| **NFR-AVAIL-3** | При недоступности KeyManager новые `Open` — fail-closed. |
| **NFR-AVAIL-4** | При недоступности KMS: операции с кэшированными ключами работают; новые — fail-closed. |

### 14.4 Аудируемость

| ID | Требование |
|---|---|
| **NFR-AUD-1** | Каждая выдача FEK логируется: `{timestamp, actor, file_id, operation, permission, result, client_ip, request_id}`. |
| **NFR-AUD-2** | Audit-логи append-only, хранение ≥ 12 месяцев. |
| **NFR-AUD-3** | Детекция аномалий: запрос FEK к > N файлов за период T — событие безопасности. |
| **NFR-AUD-4** | Модель ДОЛЖНА проходить аудит SOC 2 (CC6.1, CC6.3) и MPAA TPN. |

### 14.5 Совместимость

| ID | Требование |
|---|---|
| **NFR-COMPAT-1** | Legacy plaintext файлы читаются без ошибок (`encrypted=0`). |
| **NFR-COMPAT-2** | Формат зашифрованного чанка версионирован (magic + version). |
| **NFR-COMPAT-3** | Совместимость с upstream JuiceFS НЕ требуется (S2). |

---

## 15. gRPC API

### 15.1 KeyManagerService (agio-platform)

```protobuf
package agio.platform.drive.crypto.v1;

service DriveKeyManagerService {
    rpc CreateFileKey(CreateFileKeyRequest)     returns (CreateFileKeyResponse);
    rpc GetFileFEK(GetFileFEKRequest)           returns (GetFileFEKResponse);
    rpc GetBulkFileFEK(GetBulkFileFEKRequest)   returns (GetBulkFileFEKResponse);
    rpc RotateFileFEK(RotateFileFEKRequest)     returns (RotateFileFEKResponse);
    rpc RotateCompanyKEK(RotateCompanyKEKRequest) returns (RotateCompanyKEKResponse);
}

message CreateFileKeyRequest {
    string actor_user_id = 1;       // OIDC sub (UUID, = user.id)
    string volume_name = 2;
    string path = 3;
    string drive_file_id = 4;       // client-generated UUID
    uint64 inode = 5;
}

message CreateFileKeyResponse {
    bytes  plaintext_fek = 1;       // TLS only
    bytes  wrapped_fek = 2;         // Meta Proxy writes to Redis
    int32  fek_version = 3;
    int32  kek_version = 4;
}

message GetFileFEKRequest {
    string actor_user_id = 1;       // OIDC sub (UUID, = user.id)
    string volume_name = 2;
    string path = 3;                // для authz
    string drive_file_id = 4;
    uint64 inode = 5;
    bool   for_write = 6;           // true → требовать Edit
}

message GetFileFEKResponse {
    bytes  plaintext_fek = 1;
    int32  fek_version = 2;
    int32  kek_version = 3;
}

message GetBulkFileFEKRequest {
    string actor_user_id = 1;
    string volume_name = 2;
    repeated string drive_file_ids = 3;   // до 1000
}

message GetBulkFileFEKResponse {
    message Entry {
        string drive_file_id = 1;
        bool   allowed = 2;
        bytes  plaintext_fek = 3;
        int32  fek_version = 4;
    }
    repeated Entry entries = 1;
}

message RotateFileFEKRequest {
    string admin_user_id = 1;
    string drive_file_id = 2;
}

message RotateFileFEKResponse { int32 new_fek_version = 1; }

message RotateCompanyKEKRequest {
    string admin_user_id = 1;
    string company_id = 2;
}

message RotateCompanyKEKResponse { int32 new_kek_version = 1; }
```

### 15.2 Расширения MetaService proto (форк JuiceFS)

| ID | Требование |
|---|---|
| **FR-API-1** | `OpenResponse` расширен: `{bytes fek, int32 fek_version, bool encrypted}`. |
| **FR-API-2** | `CreateRequest` передаёт client-generated `drive_file_id`. |
| **FR-API-3** | `LoadResponse` (Format) содержит `encryption_enabled` и `kek_version`. |

### 15.3 Authz-гейтинг

KeyManager переиспользует существующую логику `AuthzService`:
1. `resolveCompaniesPrefix(volume)` → facility → companies prefix.
2. `stripPathPrefix(path, prefix)`.
3. `DriveAuthorizationService.CheckBulkPermissionsByPaths` с `PermissionRead` или `PermissionEdit`.
4. Company Owner bypass через `companyOwnerMap` (`auth.PermissionOwn`) — всегда true.
5. `actor_user_id` (OIDC `sub`) используется напрямую как `user.id` (инвариант A6); маппинг не выполняется.
6. Fail-closed при ошибке.

---

## 16. Миграция существующих данных

| ID | Требование |
|---|---|
| **FR-MIG-1** | Существующие нешифрованные чанки читаются прозрачно (`encrypted=0`). |
| **FR-MIG-2** | Все новые файлы после включения шифрования шифруются по умолчанию. |
| **FR-MIG-3** | Флаг `encrypted` хранится в inode metadata. |
| **FR-MIG-4** | Фоновый инструмент ре-энкрипции: read plaintext → generate FEK/CEK → encrypt → update S3 → update Redis metadata. Идемпотентный, возобновляемый. |
| **FR-MIG-5** | Ре-энкрипция rate-limited. |
| **FR-MIG-6** | Во время ре-энкрипции файл доступен для чтения (старая версия) и записи (новая версия уже зашифрована). |

---

## 17. Реализация: модификации

### 17.1 Форк JuiceFS

| Файл | Изменение |
|---|---|
| `pkg/meta/config.go` | `Format.EncryptionEnabled`, `Format.KEKVersion` |
| `pkg/meta/grpc_client.go` | Получение FEK при Open, LRU-кэш, revoke-signal |
| `pkg/meta/grpc_server.go` | Вызов KeyManager при Create/Open, запись `wrapped_fek` в inode |
| `pkg/meta/redis_fek.go` (новый) | Чтение/запись `wrapped_fek` в inode (один round-trip с GetAttr) |
| `pkg/vfs/vfs.go` | `FileContext{fileID, FEK, CEKs}` в handle |
| `pkg/chunk/store.go` | `ReadWithFileContext`, `WriteWithFileContext` |
| `pkg/chunk/cek_encrypt.go` (новый) | AES-256-GCM шифрование чанков CEK-ом |
| `pkg/chunk/fek_wrap.go` (новый) | Wrap/unwrap CEK под FEK |
| `pkg/object/fek_encrypt.go` (новый) | Обёртка ObjectStorage: encrypt before caching |
| `pkg/chunk/cache.go` | Encrypted local cache (ciphertext only) |
| `cmd/mount.go` | Wiring: user mode и render mode |
| `cmd/render_mount.go` (новый) | Render-клиент |

### 17.2 agio-platform

| Файл | Изменение |
|---|---|
| `application/authz/proto/key_manager.proto` (новый) | KeyManagerService proto |
| `application/authz/service/key_manager_service.go` (новый) | Реализация KeyManager |
| `internal/drive/infrastructure/adapters/kms.go` (новый) | KMS-адаптер |
| `internal/drive/infrastructure/adapters/secret_manager.go` (новый) | Secret Manager для Company KEK |
| Миграции PG | `drive_company_crypto_key`, `drive_key_access_log` |

### 17.3 Инфраструктура

| Компонент | Требование |
|---|---|
| KMS Master Key | Создан, IAM least privilege |
| Secret Manager | Company KEK per company |
| STS | **Обязательно:** замена `Format.SecretKey` на STS |
| Redis backups | Encrypted, регулярные |
| Audit storage | Append-only, ≥ 12 месяцев |
| Kratos + Hydra | Обеспечивают инвариант идентичности (A6) |

---

## 18. Тестирование

### 18.1 Unit-тесты

| ID | Покрытие |
|---|---|
| **FR-TEST-1** | CEK/FEK wrap/unwrap, GCM auth tag failure, nonce uniqueness |
| **FR-TEST-2** | Clone re-wrap CEK (без перешифровки данных) |
| **FR-TEST-3** | Compaction с CEK |
| **FR-TEST-4** | Fail-closed при ошибках KMS/Redis |
| **FR-TEST-5** | Encrypted local cache: plaintext не пишется на диск |
| **FR-TEST-6** | memclr ключей при logout |
| **FR-TEST-7** | FEK reference counting / `fek_version` (предусловия версионирования) |

### 18.2 Интеграционные тесты

| ID | Сценарий |
|---|---|
| **FR-TEST-8** | Полный цикл: mount → create → write → close → unmount → mount → open → read |
| **FR-TEST-9** | Пользователь B без прав → FEK deny |
| **FR-TEST-10** | Grant → B получает FEK → revoke → B теряет FEK после TTL |
| **FR-TEST-11** | Render-клиент читает зашифрованный файл без OIDC |
| **FR-TEST-12** | Cross-company: render-клиент A не разворачивает FEK компании B |
| **FR-TEST-13** | Legacy plaintext файл читается после включения шифрования |
| **FR-TEST-14** | Clone: re-wrap CEK, S3 не переписывается |
| **FR-TEST-15** | Clone подмножества: остальные чанки source недоступны через target |
| **FR-TEST-16** | Offline-connected: работа по кэшу без hub |
| **FR-TEST-17** | Logout → кэш нечитаем (no residuality) |
| **FR-TEST-18** | Company Owner (`PermissionOwn`) получает FEK байпасом |
| **FR-TEST-19** | OIDC `sub` (UUID) передаётся напрямую в DriveAuthorizationService как `subject_id`; права резолвятся корректно без маппинга |
| **FR-TEST-20** | Токен с `sub` не-UUID → fail-closed (InvalidArgument) |
| **FR-TEST-21** | Валидный токен для пользователя, отсутствующего в платформе (не импортирован) → доступ отклонён |

### 18.3 Нагрузочные тесты

| ID | Сценарий | Цель |
|---|---|---|
| **FR-TEST-22** | 1000 одновременных Open с кэш-хитом | p99 ≤ 5 мс |
| **FR-TEST-23** | 1000 одновременных Open с кэш-промахом | p99 ≤ 100 мс |
| **FR-TEST-24** | Throughput с шифрованием vs без | Деградация ≤ 10% |
| **FR-TEST-25** | Render: 5000 metadata RPS с ноды × 100 нод | Redis выдерживает, 0 доп. round-trips на FEK |

### 18.4 Security-тесты

| ID | Сценарий |
|---|---|
| **FR-TEST-26** | Инсайдер с Read на X пытается получить FEK Y → deny |
| **FR-TEST-27** | Утечка S3: чанки нечитаемы без CEK |
| **FR-TEST-28** | Утечка Redis: `wrapped_fek` нечитаем без Company KEK |
| **FR-TEST-29** | Компрометация render-ноды: доступ только к своей компании |
| **FR-TEST-30** | Утечка Redis + S3 одновременно: данные нечитаемы без Company KEK |

---

## 19. План внедрения

| Фаза | Содержание | Срок | Зависимости |
|---|---|---|---|
| **1. Фундамент** | KMS-адаптер, Secret Manager, миграции PG, KeyManager skeleton | 2 нед | Инфраструктура KMS |
| **2. Data path** | `pkg/object/fek_encrypt.go`, `pkg/chunk/cek_encrypt.go`, `FileContext`, encrypted cache, **FEK lifecycle/refcount предусловия (FR-VER-1,4)** | 2 нед | Фаза 1 |
| **3. Meta + KeyManager** | `wrapped_fek` в Redis inode, KeyManager, интеграция в Meta Proxy, authz-гейтинг | 2 нед | Фазы 1–2 |
| **4. Render-клиент** | `cmd/render_mount.go`, Company KEK из Secret Manager, FEK unwrap, aggressive caching | 2 нед | Фаза 3 |
| **5. Clone/Compaction** | CEK re-wrap при Clone/CopyFileRange, Compaction с CEK, **version-aware предусловия (FR-VER-2,3)** | 1 нед | Фаза 2 |
| **6. Offline + No Residuality** | Encrypted local cache, offline-connected режим, memclr при logout | 1 нед | Фаза 2 |
| **7. Revocation + STS** | STS на data-plane, permission generation, revoke-signal, FEK rotation | 2 нед | Фаза 3, **STS инфраструктура** |
| **8. Миграция** | Фоновая ре-энкрипция legacy данных | 1 нед | Фазы 1–3 |
| **9. Тестирование** | Unit + интеграционные + нагрузочные + security | 2 нед | Фазы 1–8 |
| **10. Production rollout** | Per-company KMS keys, мониторинг, runbooks, MPAA TPN prep | 1 нед | Фаза 9 |

**Итого: 12–14 недель** при 2–3 инженерах.

---

## 20. Риски

| # | Риск | Вероятность | Влияние | Митигация |
|---|---|---|---|---|
| R1 | Latency при кэш-промахе FEK | Высокая | Среднее | LRU, singleflight, batch GetBulkFileFEK |
| R2 | Потеря Redis = потеря FEK = потеря данных | Низкая | Критическое | Encrypted backups Redis, репликация, AOF |
| R3 | Компрометация Company KEK | Низкая | Критическое | Secret Manager, IAM, ротация, per-company KEK |
| R4 | Компрометация render-ноды | Средняя | Высокое | Изоляция per-company, Network Policy |
| R5 | Пользователь с cached FEK после revoke | Средняя | Высокое | Короткий TTL, STS expiry, FEK rotation, revoke-signal |
| R6 | **Инсайдер: сохранил ключи + данные offline** | Средняя | Критическое | **Не защищено криптографически.** Audit, anomaly detection, STS, legal, DLP |
| R7 | **Дамп памяти подключённого клиента** | Средняя | Критическое | **Не защищено программно.** Managed endpoints, pixel streaming для top-secret |
| R8 | STS не реализован → отзыв не работает | Высокая | Критическое | **Приоритет №1:** STS подэтап |
| R9 | Redis load от render-нод | Высокая | Среднее | Aggressive caching, batch, реплики; 0 доп. round-trips на FEK |
| R10 | Сложность форка | Высокая | Среднее | Выделенный инженер, автотесты |
| R11 | Eventual consistency PG для render-created файлов | Средняя | Низкое | Lazy re-wrap при первом пользовательском доступе |
| R12 | **Дрейф идентичности**: пользователь в Kratos (валидный OIDC-токен) но отсутствует/удалён в платформе | Низкая | Среднее | Fail-closed (FR-ID-4): нет записей прав → доступ отклонён; мониторинг рассинхрона Kratos↔platform |
| R13 | **FEK lifecycle/refcount не заложен → дорогостоящая переделка при добавлении версионирования** | Средняя | Высокое | Заложить предусловия сейчас (FR-VER-1…4) |

---

## 21. Конкурентный анализ: LucidLink Parity

### 21.1 Parity-таблица

| Требование LucidLink | Статус agio Drive | Комментарий |
|---|---|---|
| Per-file encryption, isolated keys | ✅ FEK per file | FR-KEY-2 |
| AES-256-GCM + tamper detection | ✅ | NFR-SEC-6, NFR-SEC-11 |
| Unique IV per write, no key/IV reuse | ✅ | NFR-SEC-9 |
| Streaming, no full replication | ✅ | JuiceFS chunk streaming |
| Encrypted local cache / no residuality | ✅ | NFR-SEC-1..5 |
| Offline-connected mode | ✅ | NFR-OFF-1..4 |
| Instant revocation | ✅ | FR-REV-1..7 (при условии STS) |
| SSO + auto offboarding | ✅ | OIDC/Hydra + Kratos |
| File-level audit | ✅ | NFR-AUD-1..4 |
| Snapshots / versioning | ⚠️ Предусловия заложены | §9; реализация отложена |
| Zero-knowledge | ❌ (осознанно) | Опциональный premium (FR-ZK) |
| Render/headless support | ✅ **Превосходим** | LucidLink не имеет |
| Platform recovery | ✅ **Превосходим** | У LucidLink потеря пароля = потеря данных |
| Zero-copy Clone | ✅ | CEK re-wrap (FR-OP-1..3) |
| SOC 2 / ISO 27001 / MPAA TPN | Org roadmap | NFR-AUD-4 |

### 21.2 Позиционирование

> agio Drive сознательно выбирает **platform-managed keys** вместо zero-knowledge. Это позволяет: не иметь паролей (OIDC), поддерживать render-ферму (headless), обеспечивать recovery и ротацию силами платформы. Для клиентов, требующих zero-knowledge, предусмотрен опциональный premium-режим (FR-ZK).

---

## 22. Acceptance Criteria

1. **AC-1.** Все FR/NFR с приоритетом ДОЛЖЕН реализованы и покрыты тестами.
2. **AC-2.** Пользователь с Read на файл X не может расшифровать файл Y.
3. **AC-3.** Утечка S3 + Redis одновременно не раскрывает данные.
4. **AC-4.** Render-клиент читает/пишет без OIDC, без прокси, без PG.
5. **AC-5.** Cross-company доступ невозможен.
6. **AC-6.** Legacy файлы читаются.
7. **AC-7.** Clone сохраняет zero-copy через CEK re-wrap.
8. **AC-8.** Logout → кэш нечитаем.
9. **AC-9.** Offline-connected работает.
10. **AC-10.** Revocation: после отзыва + TTL + STS expiry пользователь теряет доступ.
11. **AC-11.** Нагрузочные тесты пройдены.
12. **AC-12.** Audit log пишется для каждой выдачи FEK.
13. **AC-13.** Threat model задокументирована с принятыми границами (T8, T9).
14. **AC-14.** Криптографические предусловия версионирования заложены: `fek_version` поддерживается, схема удаления учитывает будущий reference counting (FR-VER-1…4).
15. **AC-15.** Company Owner получает доступ байпасом через `PermissionOwn`; `CheckOrganizationAdmin` не используется в пользовательской модели.
16. **AC-16.** OIDC `sub` (UUID) используется напрямую как `user.id` во всех компонентах; маппинг не выполняется; fail-closed при неверном формате (FR-ID-1..4).

---

## Приложение A. Traceability: план v13 → SRS v2.2

| Требование плана v13 | Статус | Новое требование |
|---|---|---|
| Per-file FEK | ✅ Сохранено | FR-KEY-2 |
| FEK → encrypt(GEK) для групп | 🔄 Заменено на CEK + FEK | FR-KEY-1..4 |
| FEK → encrypt(UEK) для личных | 🔄 Заменено | FR-KEY-1..4 |
| GEK/UEK → encrypt(KEK) | 🔄 KEK = Company KEK (KMS) | FR-KEY-5 |
| KEK = Argon2id(password) | ❌ Отклонено | KMS/Secret Manager |
| `file_keys` в PG | ❌ Исключено | Redis inode metadata |
| `subject_keys` в PG | ❌ Исключено | — |
| `KeyManager.GenerateFEK` | ✅ Сохранено | FR-USR-1 (CreateFileKey) |
| `KeyManager.GetFEK` | ✅ Сохранено + authz | FR-USR-4..6 |
| `KeyManager.GrantAccess` | 🔄 Не нужен (FEK в Redis, не per-subject) | Authz в PG |
| `KeyManager.RotateGroupKey` | 🔄 Заменено на RotateCompanyKEK | FR-ROT-1 |
| Render Proxy | ❌ Исключён | Прямой Render-клиент (FR-RND) |
| Ротация без перешифрования S3 | ✅ FEK rotation | FR-ROT-4 |
| Админ компании = `CheckOrganizationAdmin` | ❌ Исправлено | `PermissionOwn` байпас (FR-ADMIN) |
| Маппинг OIDC sub → user.id | ❌ Устранён инвариантом | FR-ID-1..4, A6 |

## Приложение B. Глоссарий операций

| Операция | Поведение с CEK + FEK |
|---|---|
| **Create** | Generate FEK → wrap by Company KEK → Redis. Generate CEK per chunk → wrap by FEK → slice metadata. Encrypt chunks → S3. |
| **Open (read)** | Authz Read → KeyManager → unwrap FEK → client caches. Unwrap CEKs on demand. |
| **Open (write)** | Authz Edit → KeyManager → unwrap FEK → client caches. |
| **Write** | New CEK → wrap by FEK → encrypt → S3. |
| **Clone** | New FEK for target. Re-wrap CEKs under target FEK. No S3 rewrite. |
| **CopyFileRange** | Same as Clone. Partial chunk → new CEK for split part. |
| **Compaction** | Read CEKs → decrypt → merge → new CEK → encrypt → wrap under FEK. |
| **Truncate/SetAttr/Rename** | No key change. |
| **Unlink** | Soft-delete metadata. Keys remain until GC (с учётом будущего reference counting). |
| **Revoke** | Remove permission → invalidate caches → STS expiry → optional FEK/CEK rotation. |
