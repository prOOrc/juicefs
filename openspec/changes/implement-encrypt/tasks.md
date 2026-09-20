# Tasks: implement-encrypt

Контракт — delta-спека этого change `specs/domain-encrypt/spec.md` (greenfield-капабилити; при архивации change она становится SoT); межэтапные контракты (бинарные форматы, proto, PG-таблицы, параметры кэшей) и решения D1–D12 + stage-решения — design.md. Код-уровневые детали (сигнатуры, тела функций, тесты) — в разделе «Implementation details» каждого этапа ниже. Порядок: 1 → 2 → 3 → (4 ∥ 5 ∥ 6) → 7 → 8 → 9 → 10; в рамках одной сессии — последовательно. Коммиты: префикс `encrypt-stage-N:` по логическим шагам; новый `.go` файл — с Apache 2.0 header; локальные `replace`-директивы в go.mod в коммиты не попадают. Тесты форка: `make test.meta.core` (изменения `pkg/meta`), `make test.pkg` (остальные `pkg`); тесты платформы: `go test ./...` в соответствующих пакетах (`src/`).

## 1. Stage 1 — Фундамент (agio-platform, ветка feature/drive-v2)

**Контекст.** Репозиторий `/Users/i.obukhov/ai/agio/agio-platform`, ветка `feature/drive-v2`. Go-модуль — в `src/` (`Go 1.26`). Что уже есть: gRPC-сервер `AuthzGRPCServer` (`src/api/grpc_server.go`, порт 9090) с `AuthzService`; interceptors: metrics, field-logging, db-context; транспортной аутентификации НЕТ — `user_id` приходит в теле запроса от доверенного Meta Proxy. `DriveAuthorizationService` (`src/internal/drive/infrastructure/adapters/auth.go`) с `CheckBulkPermissionsByPaths`, Company Owner bypass (`PermissionOwn` через SpiceDB), PermissionCache (Redis `driveperm:{userID}:{objectID}`, TTL 5m). Migrations: golang-migrate, `src/infrastructure/db/migrations/` (актуальный максимум на момент этапа — 000202; миграции этого этапа — 000203/000204). Стиль: `BEGIN/COMMIT`, UUID PK `gen_random_uuid()`, audit-колонки, кавычки в идентификаторах. SQLBoiler v4 (модели в `src/infrastructure/boiler/`, регенерация `sqlboiler -c ./src/sqlboiler.toml -o ./src/infrastructure/boiler psql`), hot-path — raw SQL через sqlx. YC SDK: `src/infrastructure/yc/sdk.go` (`NewSdk()`: `YC_TOKEN` env или `InstanceServiceAccount`). Kratos-клиент для identity (ORY): `src/api/auth/auth_service.go` — **IdentityResolver его НЕ использует** — `sub` == `user.id` всегда (S12). KMS / Secret Manager / STS в репозитории НЕТ — всё net-new.

Что делает этап (SRS §17.2, §5.3, §15.1): порты и реализации KMS + Secret Manager (Yandex первыми, D12); `CompanyKEKService`; PG-таблицы + SQLBoiler; `IdentityResolver` (UUID-валидация, FR-ID-3/FR-USR-6/D11); proto `DriveKeyManagerService` + реализация (`CreateFileKey`/`GetFileFEK`/`GetBulkFileFEK` — полностью, `RotateFileFEK`/`RotateCompanyKEK` — stubs `Unimplemented` до stage 7, `FetchCompanyKEK` — полностью); аудит выдачи ключей (FR-USR-8, NFR-AUD-1). Не делает: интеграцию с форком (этап 3), ротацию (этап 7), STS (этап 7), permission generation (этап 7).

Решения этапа: design.md, «Решения Stage 1» (1.1–1.5).

### Tasks

- [x] 1.1 PG-миграции `drive_company_crypto_key` и `drive_key_access_log` + регенерация SQLBoiler. Файлы: `src/infrastructure/db/migrations/000203_add_drive_company_crypto_key.{up,down}.sql`, `000204_add_drive_key_access_log.{up,down}.sql` (следующие свободные номера на 2026-09-19; актуальный максимум в platform — 000202), `src/infrastructure/boiler/*` (generated). Проверка: `migrate up`/`migrate down` на локальном PG; `go build ./...` в `src/`.
- [x] 1.2 Порты криптографии (`KMS`, `SecretStore`, `CompanyKEKService`, `IdentityResolver`, `KeyAccessLogger`). Файл: `src/internal/drive/application/ports/crypto.go`. Проверка: `go build ./...`.
- [x] 1.3 Yandex KMS / Lockbox-адаптеры + fake-реализации для тестов (проверить наличие `services/kms`/`lockbox` в vendor, при отсутствии — добавить зависимость). Файлы: `src/internal/drive/infrastructure/adapters/kms_yandex.go`, `lockbox_yandex.go`, `crypto_fake.go`. Проверка: `go test ./internal/drive/infrastructure/adapters/... -run 'TestFakeKMS|TestFakeLockbox'`.
- [x] 1.4 `CompanyKEKService`: GetKEK (RAM-LRU TTL 5 мин, fail-closed при unwrap), ProvisionKEK (CSPRNG, KMS-wrap, Lockbox, PG-строка). Файл: `src/internal/drive/infrastructure/adapters/company_kek.go`. Проверка: unit-тесты round-trip GetKEK/ProvisionKEK (fake KMS/Lockbox + testcontainers PG).
- [x] 1.5 `IdentityResolver` — только UUID-валидация формата (FR-ID-3), без DB-запросов. Файл: `src/internal/drive/infrastructure/adapters/identity_resolver.go`. Проверка: unit-тесты валидный/невалидный UUID.
- [x] 1.6 Proto `DriveKeyManagerService` + генерация кода (дополнить `generate_protos.sh`). Файлы: `src/application/authz/proto/key_manager.proto`, generated `.pb.go`. Проверка: `go build ./...`; сгенерированный код закоммичен.
- [x] 1.7 `KeyManagerService`: `CreateFileKey`/`GetFileFEK`/`GetBulkFileFEK` (полностью), `FetchCompanyKEK`/`ProvisionCompanyKEK` (полностью), `RotateFileFEK`/`RotateCompanyKEK` (stubs `Unimplemented` до stage 7); singleflight по FEK; аудит каждой выдачи; AGFK-хелперы формата. Файлы: `src/application/authz/service/key_manager_service.go`, `fek_crypto.go`. Проверка: unit-тесты deny/allow/bulk/singleflight (паттерн `authz_service_test.go`): `go test ./application/authz/...`.
- [x] 1.8 IAM-interceptor для `FetchCompanyKEK` (YC IAM-токен; остальные методы — модель доверия proxy). Файл: `src/api/iam_interceptor.go`. Проверка: unit-тест верификации/отклонения токена.
- [x] 1.9 Wire + config + регистрация сервиса в gRPC-сервере (дефолты `kms_key_id`, `lockbox_folder`, `keymanager_kek_cache_ttl`). Файлы: `src/config/config.go`, `src/internal/drive/infrastructure/adapters/wire.go`, `src/application/authz/wire.go`, `src/api/grpc_server.go`. Проверка: `go build ./...`; сервер стартует с флагами. Deployment values для stage/prod — `agio-cloud` (chart `platform-api`).
- [x] 1.10 Репозитории аудита и ключей (append-only; асинхронный batch INSERT, deny-записи синхронно). Файлы: `src/internal/drive/infrastructure/persistence/repositories/company_crypto_key_repository.go`, `key_access_log_repository.go`. Проверка: unit-тесты Insert/GetActive/NextVersion.

### Implementation details

#### Шаг 1. PG-миграции

**Файл:** `src/infrastructure/db/migrations/000203_add_drive_company_crypto_key.up.sql`

```sql
BEGIN;
CREATE TABLE "drive_company_crypto_key" (
    "id" UUID NOT NULL PRIMARY KEY DEFAULT gen_random_uuid (),
    "company_id" UUID NOT NULL REFERENCES "company" (id),
    "key_purpose" VARCHAR(32) NOT NULL,          -- 'fek_wrap'
    "key_version" INT NOT NULL,
    "kms_key_id" VARCHAR(255) NOT NULL,          -- ARN/ID KMS master key
    "secret_ref" VARCHAR(512) NOT NULL,          -- путь в Secret Manager
    "status" VARCHAR(32) NOT NULL,               -- 'active' | 'retiring' | 'retired'
    "created_by_id" UUID NOT NULL REFERENCES "user" (id),
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL,
    "updated_by_id" UUID REFERENCES "user" (id),
    "updated_at" TIMESTAMP WITH TIME ZONE,
    "deleted_by_id" UUID REFERENCES "user" (id),
    "deleted_at" TIMESTAMP WITH TIME ZONE,
    "version" INT NOT NULL,
    "rotated_at" TIMESTAMP WITH TIME ZONE
);
CREATE UNIQUE INDEX idx_drive_company_crypto_key_uniq ON "drive_company_crypto_key" (company_id, key_purpose, key_version);
COMMIT;
```

`.down.sql`: `BEGIN; DROP TABLE IF EXISTS "drive_company_crypto_key"; COMMIT;`

**Файл:** `src/infrastructure/db/migrations/000204_add_drive_key_access_log.up.sql` — по SRS §5.3 (append-only, без soft-delete):

```sql
BEGIN;
CREATE TABLE "drive_key_access_log" (
    "id" BIGSERIAL PRIMARY KEY,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    "actor_type" VARCHAR(32) NOT NULL,           -- 'user' | 'render_node' | 'service'
    "actor_id" VARCHAR(128) NOT NULL,
    "company_id" UUID,
    "drive_file_id" UUID,
    "inode" BIGINT,
    "operation" VARCHAR(32) NOT NULL,            -- 'get_fek' | 'create_fek' | 'rotate' | 'deny' | 'fetch_kek' | 'provision_kek'
    "permission" VARCHAR(16),
    "result" VARCHAR(16) NOT NULL,               -- 'allow' | 'deny' | 'error'
    "reason" TEXT,
    "request_id" UUID,
    "client_ip" INET
);
CREATE INDEX idx_key_access_log_time ON "drive_key_access_log" (created_at DESC);
CREATE INDEX idx_key_access_log_actor ON "drive_key_access_log" ("actor_id", created_at DESC);
COMMIT;
```

Создание: `migrate create -ext sql -dir src/infrastructure/db/migrations -seq <name>` (или вручную, как выше). После миграций — **регенерация SQLBoiler** (`sqlboiler -c ./src/sqlboiler.toml -o ./src/infrastructure/boiler psql`); сгенерированные файлы коммитим.

#### Шаг 2. Порты криптографии

**Файл:** `src/internal/drive/application/ports/crypto.go` (новый)

```go
package ports

import "context"

// KMS оборачивает/разворачивает ключи корневym KMS-ключом (D12).
type KMS interface {
	// WrapKey шифрует plaintext KMS-ключом keyID. Возвращает wrapped blob провайдера.
	WrapKey(ctx context.Context, keyID string, plaintext []byte) ([]byte, error)
	// UnwrapKey обратная операция.
	UnwrapKey(ctx context.Context, keyID string, wrapped []byte) ([]byte, error)
}

// SecretManager — именованные секреты (Company KEK хранятся здесь, обёрнутые KMS).
type SecretManager interface {
	PutSecret(ctx context.Context, name string, data []byte) error
	GetSecret(ctx context.Context, name string) ([]byte, error)
}

// CompanyKEKService — разрешение plaintext Company KEK по companyID.
type CompanyKEKService interface {
	// GetKEK возвращает plaintext KEK (32 байта) активной версии.
	GetKEK(ctx context.Context, companyID string) ([]byte, error)
	// ProvisionKEK генерирует новый KEK, оборачивает KMS, кладёт в SecretManager,
	// пишет строку drive_company_crypto_key. Возвращает key_version.
	ProvisionKEK(ctx context.Context, companyID, actorUserID string) (int, error)
}

// IdentityResolver валидирует OIDC sub как валидный UUID (FR-ID-3, FR-USR-6).
// S12/A6: sub ≡ kratos.identity_id ≡ user.id — маппинга нет, sub возвращается как есть.
type IdentityResolver interface {
	Resolve(ctx context.Context, oidcSub string) (userID string, err error)
}

// KeyAccessEntry — запись аудита выдачи ключей (NFR-AUD-1).
type KeyAccessEntry struct {
	ActorType   string // 'user' | 'render_node' | 'service'
	ActorID     string
	CompanyID   string
	DriveFileID string
	Inode       int64
	Operation   string // 'get_fek' | 'create_fek' | 'rotate' | 'deny' | 'fetch_kek' | 'provision_kek'
	Permission  string
	Result      string // 'allow' | 'deny' | 'error'
	Reason      string
	RequestID   string
	ClientIP    string
}

// KeyAccessLogger — append-only аудит. Ошибка логирования НЕ роняет запрос (log + metric).
type KeyAccessLogger interface {
	Log(ctx context.Context, entry KeyAccessEntry)
}
```

#### Шаг 3. Реализации KMS и Secret Manager (Yandex)

**Файл:** `src/internal/drive/infrastructure/adapters/kms_yandex.go` (новый)

```go
package adapters

import (
	"context"

	kms "github.com/yandex-cloud/go-genproto/yandex/cloud/kms/v1"
	kmsSvc "github.com/yandex-cloud/go-sdk/services/kms"
)

// YandexKMS — KMS-адаптер на Yandex KMS (симметричный ключ keyRing/key).
type YandexKMS struct {
	svc *kmsSvc.Service
}

func NewYandexKMS(sdk *ycsdk.SDK) *YandexKMS {
	return &YandexKMS{svc: kmsSvc.NewService(sdk)}
}

func (k *YandexKMS) WrapKey(ctx context.Context, keyID string, plaintext []byte) ([]byte, error) {
	resp, err := k.svc.Encrypt(ctx, &kms.EncryptRequest{KeyId: keyID, Ciphertext: plaintext})
	if err != nil {
		return nil, fmt.Errorf("kms encrypt: %w", err)
	}
	return resp.Ciphertext, nil
}

func (k *YandexKMS) UnwrapKey(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	resp, err := k.svc.Decrypt(ctx, &kms.DecryptRequest{KeyId: keyID, Ciphertext: wrapped})
	if err != nil {
		return nil, fmt.Errorf("kms decrypt: %w", err)
	}
	return resp.Plaintext, nil
}
```

> Точные имена методов YC SDK проверить при реализации (`vendor/` содержит `yandex-cloud/go-sdk`); если в vendor нет `services/kms` — добавить зависимость и `go mod vendor`.

**Файл:** `src/internal/drive/infrastructure/adapters/secret_manager_yandex.go` (новый) — аналогично через `secrets.NewService(sdk)`: `PutSecret` = CreateSecret + AddSecretVersion (или UpdateLatest), `GetSecret` = GetLatestSecretVersion + расшифровка KMS-ключом секрета. Имя секрета: `drive/kek/{companyID}/v{version}`.

**Файл:** `src/internal/drive/infrastructure/adapters/crypto_fake.go` (новый, только для тестов) — `FakeKMS` (AES-256-GCM с фиксированным тестовым ключом), `FakeSecretManager` (map).

#### Шаг 4. CompanyKEKService

**Файл:** `src/internal/drive/infrastructure/adapters/company_kek.go` (новый)

```go
type CompanyKEKService struct {
	db       *sqlx.DB // или db.GetDbFromContext
	kms      ports.KMS
	sm       ports.SecretManager
	kmsKeyID string
	folder   string // secret manager folder
	kekTTL   time.Duration

	cache *lru.Cache[string, []byte] // companyID -> plaintext KEK
	mu    sync.Mutex
}

func (s *CompanyKEKService) GetKEK(ctx context.Context, companyID string) ([]byte, error) {
	if v, ok := s.cache.Get(companyID); ok {
		return v, nil
	}
	row, err := s.activeKeyRow(ctx, companyID) // SELECT ... WHERE company_id=$1 AND key_purpose='fek_wrap' AND status='active' AND deleted_at IS NULL ORDER BY key_version DESC LIMIT 1
	if err != nil {
		return nil, err // нет активной версии → ошибка (fail-closed)
	}
	wrapped, err := s.sm.GetSecret(ctx, row.SecretRef)
	if err != nil {
		return nil, err
	}
	kek, err := s.kms.UnwrapKey(ctx, row.KmsKeyID, wrapped)
	if err != nil || len(kek) != 32 {
		return nil, errors.New("kek unwrap failed") // fail-closed, plaintext не логируем (NFR-SEC-10)
	}
	s.cache.Add(companyID, kek)
	return kek, nil
}

func (s *CompanyKEKService) ProvisionKEK(ctx context.Context, companyID, actorUserID string) (int, error) {
	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil { // crypto/rand (NFR-SEC-7)
		return 0, err
	}
	wrapped, err := s.kms.WrapKey(ctx, s.kmsKeyID, kek)
	if err != nil {
		return 0, err
	}
	version, err := s.nextVersion(ctx, companyID) // MAX(key_version)+1
	if err != nil {
		return 0, err
	}
	ref := fmt.Sprintf("%s/drive/kek/%s/v%d", s.folder, companyID, version)
	if err := s.sm.PutSecret(ctx, ref, wrapped); err != nil {
		return 0, err
	}
	// INSERT drive_company_crypto_key (status='active', created_by_id=actorUserID, ...)
	// + audit Log{Operation: 'provision_kek', Result: 'allow'}
	return version, nil
}
```

#### Шаг 5. IdentityResolver

**Файл:** `src/internal/drive/infrastructure/adapters/identity_resolver.go` (новый)

```go
// identityResolver — валидация формата UUID (FR-ID-3). Без состояния:
// инвариант S12/A6 гарантирует sub ≡ user.id, маппинга нет.
type identityResolver struct{}

func (identityResolver) Resolve(_ context.Context, sub string) (string, error) {
	if _, err := uuid.Parse(sub); err != nil {
		return "", fmt.Errorf("oidc sub %q is not a valid UUID: %w", sub, err)
	}
	return sub, nil
}
```

> Примечание (FR-ID-4): пользователь, отсутствующий в платформе (есть в Kratos, но не импортирован/удалён), проходит валидацию формата и отклоняется **authz-слоем** — у него нет записей в `drive_object_permission` → `DriveAuthorizationService` возвращает отсутствие прав → deny. Отдельной проверки существования пользователя в KeyManager НЕ требуется. Остаточный риск R12 (дрейф Kratos↔platform) закрывается мониторингом рассинхрона (этап 10).

#### Шаг 6. Proto KeyManagerService

**Файл:** `src/application/authz/proto/key_manager.proto` (новый). Генерация — дополнить `generate_protos.sh` вторым блоком по образцу authz:

```proto
syntax = "proto3";
package agio.platform.drive.crypto.v1;
option go_package = "agio-band.gitlab.yandexcloud.net/agio-band/agio-platform/application/authz/proto";

// DriveKeyManagerService — authz-gated выдача per-file ключей (FEK) и Company KEK.
service DriveKeyManagerService {
  rpc CreateFileKey(CreateFileKeyRequest) returns (CreateFileKeyResponse);
  rpc GetFileFEK(GetFileFEKRequest) returns (GetFileFEKResponse);
  rpc GetBulkFileFEK(GetBulkFileFEKRequest) returns (GetBulkFileFEKResponse);
  rpc RotateFileFEK(RotateFileFEKRequest) returns (RotateFileFEKResponse);        // этап 7
  rpc RotateCompanyKEK(RotateCompanyKEKRequest) returns (RotateCompanyKEKResponse); // этап 7
  rpc FetchCompanyKEK(FetchCompanyKEKRequest) returns (FetchCompanyKEKResponse);   // render-ноды
  rpc ProvisionCompanyKEK(ProvisionCompanyKEKRequest) returns (ProvisionCompanyKEKResponse);
}

message CreateFileKeyRequest {
  string actor_user_id = 1;    // OIDC sub (UUID, = user.id; A6)
  string volume_name = 2;
  string path = 3;             // полный путь для authz
  string drive_file_id = 4;    // client-generated UUID
  uint64 inode = 5;
  string volume_uuid = 6;      // для AAD wrapped_fek
}

message CreateFileKeyResponse {
  bytes  plaintext_fek = 1;    // TLS only
  bytes  wrapped_fek = 2;      // proxy пишет в Redis inode
  int32  fek_version = 3;
  int32  kek_version = 4;
}

message GetFileFEKRequest {
  string actor_user_id = 1;    // OIDC sub (UUID, = user.id; A6)
  string volume_name = 2;
  string path = 3;
  string drive_file_id = 4;
  uint64 inode = 5;
  bool   for_write = 6;        // true → требовать Edit (FR-USR-5)
  bytes  wrapped_fek = 7;      // из attr (решение 1.1)
  string volume_uuid = 8;      // AAD
}

message GetFileFEKResponse {
  bytes  plaintext_fek = 1;
  int32  fek_version = 2;
  int32  kek_version = 3;
}

message GetBulkFileFEKRequest {
  string actor_user_id = 1;    // OIDC sub (UUID, = user.id; A6)
  string volume_name = 2;
  repeated string drive_file_ids = 3;  // до 1000
  repeated string paths = 4;           // решение 1.3: параллельно drive_file_ids
  repeated bytes wrapped_feks = 5;     // параллельно drive_file_ids
  repeated uint64 inodes = 6;
  bool   for_write = 7;
  string volume_uuid = 8;
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

message RotateFileFEKRequest { string admin_user_id = 1; string drive_file_id = 2; }
message RotateFileFEKResponse { int32 new_fek_version = 1; }
message RotateCompanyKEKRequest { string admin_user_id = 1; string company_id = 2; }
message RotateCompanyKEKResponse { int32 new_kek_version = 1; }

// Render-ноды: identity — YC IAM-токен в metadata 'authorization' (решение 1.4).
message FetchCompanyKEKRequest { string company_id = 1; }
message FetchCompanyKEKResponse { bytes plaintext_kek = 1; int32 kek_version = 2; }

message ProvisionCompanyKEKRequest { string admin_user_id = 1; string company_id = 2; }
message ProvisionCompanyKEKResponse { int32 key_version = 1; }
```

#### Шаг 7. Реализация сервиса

**Файл:** `src/application/authz/service/key_manager_service.go` (новый)

```go
type KeyManagerService struct {
	pb.UnimplementedDriveKeyManagerServiceServer

	DriveAuth     drivePorts.DriveAuthorizationService
	Identity      ports.IdentityResolver
	KEK           ports.CompanyKEKService
	Audit         ports.KeyAccessLogger
	JuiceFsConfig juicefsPorts.ConfigService // resolveCompaniesPrefix (паттерн AuthzService)

	prefixCache  sync.Map
	prefixFlight singleflight.Group
	flight       singleflight.Group // коалесценция одновременных запросов одного FEK (FR-USR-7)
}

func (s *KeyManagerService) GetFileFEK(ctx context.Context, req *pb.GetFileFEKRequest) (*pb.GetFileFEKResponse, error) {
	// 1. Identity: валидация формата UUID (FR-ID-3, FR-USR-6). sub ≡ user.id (A6), маппинга нет.
	//    Неверный формат = ошибка конфигурации идентичности → fail-closed InvalidArgument + audit deny.
	userID, err := s.Identity.Resolve(ctx, req.ActorUserId)
	if err != nil {
		s.Audit.Log(ctx, ports.KeyAccessEntry{ActorType: "user", ActorID: req.ActorUserId,
			DriveFileID: req.DriveFileId, Inode: int64(req.Inode), Operation: "get_fek",
			Result: "deny", Reason: "invalid sub format"})
		return nil, status.Errorf(codes.InvalidArgument, "identity: %v", err)
	}

	// 2. Authz: Read или Edit (FR-USR-4/5). Company Owner bypass — внутри DriveAuth (PermissionOwn).
	perm := auth.PermissionRead
	if req.ForWrite {
		perm = auth.PermissionEdit
	}
	prefix := s.resolveCompaniesPrefix(ctx, req.VolumeName)
	path := stripPathPrefix(req.Path, prefix) // тот же хелпер, что в AuthzService (вынести в общий пакет)
	allowed, err := s.DriveAuth.CheckPermissionByPath(ctx, path, perm, userID)
	if err != nil {
		s.Audit.Log(ctx, ports.KeyAccessEntry{..., Operation: "get_fek", Result: "error", Reason: err.Error()})
		return nil, status.Errorf(codes.Internal, "authz check failed: %v", err) // fail-closed
	}
	if !allowed {
		s.Audit.Log(ctx, ports.KeyAccessEntry{ActorType: "user", ActorID: req.ActorUserId,
			DriveFileID: req.DriveFileId, Inode: int64(req.Inode),
			Permission: string(perm), Result: "deny"})
		return nil, status.Errorf(codes.PermissionDenied, "no %s permission on %s", perm, path)
	}

	// 3. Unwrap FEK под Company KEK (singleflight по drive_file_id, FR-USR-7).
	companyID := s.companyIDFromPath(ctx, path) // ExtractCompanyCodeAndPath + resolveCompanyIDs
	v, err, _ := s.flight.Do(req.DriveFileId, func() (interface{}, error) {
		kek, err := s.KEK.GetKEK(ctx, companyID)
		if err != nil {
			return nil, err
		}
		fek, ver, err := crypto.UnwrapFEK(kek, req.WrappedFek, crypto.FEKAAD{
			VolumeUUID: req.VolumeUuid, CompanyID: companyID, DriveFileID: req.DriveFileId,
			Inode: req.Inode,
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "fek unwrap: %v", err) // fail-closed (NFR-SEC-11)
		}
		return fekResult{fek: fek, ver: ver}, nil
	})
	if err != nil {
		s.Audit.Log(ctx, ports.KeyAccessEntry{..., Result: "error", Reason: err.Error()})
		return nil, err
	}

	s.Audit.Log(ctx, ports.KeyAccessEntry{ActorType: "user", ActorID: req.ActorUserId,
		CompanyID: companyID, DriveFileID: req.DriveFileId, Inode: int64(req.Inode),
		Operation: "get_fek", Permission: string(perm), Result: "allow"})
	r := v.(fekResult)
	return &pb.GetFileFEKResponse{PlaintextFek: r.fek, FekVersion: int32(r.ver)}, nil
}
```

`CreateFileKey` — аналогично, но: authz **Write** на path; FEK генерируется `crypto/rand` (32B); `wrapped_fek = crypto.WrapFEK(kek, fek, AAD{..., FekVersion: 1})`; audit `create_fek`.
`GetBulkFileFEK` — один батч `CheckBulkPermissionsByPaths` на все пути; per-entry `allowed` + unwrap для разрешённых (ошибка unwrap в entry → `allowed=false`, остальные не роняем).
`FetchCompanyKEK` — IAM-верификация (interceptor, шаг 8) → `KEK.GetKEK(companyID)` → audit `fetch_kek` (actor_type=`render_node`).
`ProvisionCompanyKEK` — проверка org-admin (паттерн `CheckOrganizationAdmin`: `Authzed.CheckOrganizationPermissionWithToken`) → `KEK.ProvisionKEK`.
`RotateFileFEK`/`RotateCompanyKEK` — `return nil, status.Errorf(codes.Unimplemented, "rotation lands in stage 7")`.

**Крипто-хелперы формата AGFK** — **Файл:** `src/application/authz/service/fek_crypto.go` (новый). Формат строго по design.md, «Межэтапные контракты → Бинарные форматы»:

```go
const (
	fekMagic   = "AGFK"
	fekVersion = 1
	// layout: magic(4) | version(1) | kek_version(4) | nonce(12) | ciphertext(32) | tag(16) = 69 bytes
)

type FEKAAD struct {
	VolumeUUID, CompanyID, DriveFileID string
	Inode     uint64
	FekVersion uint32
}

// AAD: len(volume_uuid):4 || volume_uuid || len(company_id):4 || company_id ||
//      len(drive_file_id):4 || drive_file_id || inode(8B BE) || fek_version(4B BE).
// Length-prefixed строковые поля делают кодировку однозначной.
func aadBytes(a FEKAAD) []byte { ... }

// WrapFEK: AES-256-GCM(kek, nonce=random12, aad). Nonce уникален на запись (NFR-SEC-9).
func WrapFEK(kek []byte, fek []byte, aad FEKAAD, kekVersion uint32) ([]byte, error) { ... }

// UnwrapFEK: проверка magic/version/tag; несовпадение → ошибка (fail-closed, NFR-SEC-11).
func UnwrapFEK(kek []byte, wrapped []byte, aad FEKAAD) (fek []byte, fekVersion uint32, err error) { ... }
```

> **ВАЖНО:** идентичная реализация AGFK/AGCK/AGDF появится в форке JuiceFS (этапы 2–4). Формат зафиксирован в design.md — расхождение между репозиториями = критический баг. В этапе 9 добавим cross-repo тест-вектор (общий файл с known-answer тестами).

#### Шаг 8. Wire + config + регистрация

- `src/config/config.go`: дефолты `kms_key_id`, `kms_key_ring`, `secret_manager_folder`, `keymanager_kek_cache_ttl` (default `5m`); провайдеры `NewKMSConfig()`, `NewSecretManagerConfig()`.
- `src/internal/drive/infrastructure/adapters/wire.go`: `wire.Bind(new(ports.KMS), new(*YandexKMS))` и т.д.; `wire.Struct(new(CompanyKEKService), "*")`; `IdentityResolver` — provider-функция без зависимостей (`func() ports.IdentityResolver { return identityResolver{} }`).
- `src/application/authz/wire.go`: в `AuthzSet` добавить `keyManagerService.NewKeyManagerService`.
- `src/api/grpc_server.go`: `pb.RegisterDriveKeyManagerServiceServer(grpcServer, s.KeyManagerService)` + поле `KeyManagerService *keyManagerService.KeyManagerService` в `AuthzGRPCServer` (wire.Struct).
- **IAM-interceptor** для `FetchCompanyKEK` — **Файл:** `src/api/iam_interceptor.go` (новый): если метод = `.../FetchCompanyKEK` → взять `authorization: Bearer <token>`, верифицировать YC IAM (`iam.NewService(sdk).ExchangeToken` / проверка subject), положить subject в context; остальные методы — пропуск (модель доверия proxy).

#### Шаг 9. Репозитории аудита и ключей

- `src/internal/drive/infrastructure/persistence/repositories/company_crypto_key_repository.go` — CRUD по SQLBoiler-модели (`GetActive(ctx, companyID)`, `NextVersion`, `Insert`).
- `src/internal/drive/infrastructure/persistence/repositories/key_access_log_repository.go` — только `Insert` (append-only). `KeyAccessLogger` пишет асинхронно (канал + batch INSERT, чтобы не держать hot path на PG; при переполнении канала — drop + metric, НО deny-записи писать синхронно).

#### Тесты

**Unit** (`src/application/authz/service/key_manager_service_test.go`, паттерн `authz_service_test.go` — testify + ручные mock'и):

```go
func TestKeyManager_GetFileFEK_Denied(t *testing.T) {
	mockDrive := &mockDriveAuth{}
	mockDrive.On("CheckPermissionByPath", mock.Anything, "company-abc/projects/file.exr",
		auth.PermissionRead, "user-uuid-1").Return(false, nil)

	svc := NewKeyManagerService(mockDrive, identityResolver{}, // реальный: только uuid.Parse
		&mockKEK{}, &mockAudit{}, &mockJuiceFsConfig{})

	_, err := svc.GetFileFEK(context.Background(), &pb.GetFileFEKRequest{
		ActorUserId: "user-uuid-1", VolumeName: "vol", Path: "/companies/company-abc/projects/file.exr",
		DriveFileId: "df-1", Inode: 42, WrappedFek: []byte("x"),
	})
	assert.ErrorIs(t, err, status.Error(codes.PermissionDenied, "")) // или assert.Contains по message
	mockAudit := svc.audit // проверить запись deny
}
```

Полный набор unit-кейсов (таблица):

| Кейс | Ожидание |
|---|---|
| Read-право есть → allow + корректный FEK (known-answer AGFK) | `plaintext_fek` = ожидаемый 32B |
| `for_write=true`, только Read → deny | PermissionDenied + audit deny |
| Company Owner (`PermissionOwn`) → allow без per-file grant | bypass внутри mock DriveAuth |
| FR-TEST-19: `sub` (UUID) передаётся напрямую в DriveAuthorizationService как `subject_id`, без преобразования | mock DriveAuth получает ровно значение `sub`; права резолвятся |
| FR-TEST-20: `sub` не UUID → fail-closed | `codes.InvalidArgument` + audit deny (FR-ID-3) |
| FR-TEST-21: валидный UUID, но пользователь отсутствует в платформе (нет записей прав) → deny | PermissionDenied через authz (FR-ID-4), не через IdentityResolver |
| Authz-ошибка (не deny) → Internal, fail-closed | codes.Internal |
| Повреждённый `wrapped_fek` (битый tag) → Internal | fail-closed, NFR-SEC-11 |
| Bulk: 3 пути, 1 без прав | entries[1].allowed=false, остальные FEK |
| Singleflight: 2 параллельных запроса одного FEK | один вызов KEK.GetKEK (mock counter) |

**Крипто known-answer** (`fek_crypto_test.go`): фиксированный KEK/FEK/AAD → ожидаемый 69-байтовый blob (закоммитить вектор — он же будет в форке, этап 9).

**Integration** (build tag `integration`, testcontainers PG + miniredis по паттерну `src/testutil/setup.go`):
- миграции 000203/000204 применяются;
- `CompanyKEKService.ProvisionKEK` → `GetKEK` round-trip (FakeKMS/FakeSecretManager);
- `KeyAccessLogger` → запись в PG.

### Verification

```sh
cd /Users/i.obukhov/ai/agio/agio-platform/src
go build ./... && go vet ./...
go test ./application/authz/... ./internal/drive/...   # unit
# integration (нужен docker):
migrate -path ./infrastructure/db/migrations -database "$TEST_DATABASE_URL" up
go test -tags=integration ./internal/drive/...
```

### Stage acceptance criteria

- [x] `DriveKeyManagerService` зарегистрирован на gRPC-сервере; `CreateFileKey`/`GetFileFEK`/`GetBulkFileFEK`/`FetchCompanyKEK`/`ProvisionCompanyKEK` работают (grpcurl-проверка в dev).
- [x] Authz-гейтинг: Read/Edit по `DriveAuthorizationService`, Company Owner bypass, fail-closed (AC-15 частично — полный цикл в этапе 3).
- [x] Аудит: каждая выдача/отказ — запись в `drive_key_access_log` (AC-12 частично).
- [x] Identity: FR-ID-1..4 + FR-USR-6 — `sub` используется напрямую как `user.id`, маппинга нет; fail-closed при не-UUID (`InvalidArgument`); отсутствующий в платформе пользователь → deny через authz (FR-TEST-19/20/21, AC-16).
- [x] Plaintext KEK/FEK не логируются (NFR-SEC-10) — проверить логи dev-запуска.
- [x] Миграции up/down проходят; SQLBoiler перегенерирован.
- [x] Unit + integration тесты зелёные.

## 2. Stage 2 — Data path (форк, ветка agio-drive-v2)

**Контекст.** Репозиторий `/Users/i.obukhov/github/juicefs`, ветка `agio-drive-v2`. Этап выполняется ПОСЛЕ этапа 1 (нужны зафиксированные форматы AGFK/AGCK/AGDF из design.md и known-answer векторы).

Факты о коде (проверено по состоянию ветки):
- `ChunkStore` — `pkg/chunk/chunk.go:29-53`: `NewReader(id uint64, length int) Reader`, `NewWriter(id uint64, tierID uint8) Writer`. `id` = **slice ID**.
- Object key блока: `chunks/{a}/{b}/{sliceID}_{indx}_{blockSize}` (`rSlice.key`, `cached_store.go:74-80`) — slice ID восстанавливается из ключа.
- Read: `rSlice.ReadAt` (`cached_store.go:97-180`) → кэш `bcache.load(key)` → либо `store.loadRange` (range GET, без кэша, :706), либо singleflight → `store.load` (:755) — единственный путь полного GET (prefetch и FillCache тоже через него).
- Write: `wSlice.WriteAt` буферизует в RAM → `FlushTo/Finish` → `wSlice.upload` (:400) → `store.upload` (:356) → `store.put` → `storage.Put`. Writeback: `bcache.stage(key, data, tierID)` (plaintext на диск сегодня).
- Disk cache: единая точка записи `flushPage` (`disk_cache.go:510`), чтения — `openCacheFile`/`cacheFile.ReadAt`; staging hardlink'ится в cache. **Изменений в `disk_cache.go` НЕ требуется** (D4): кэш хранит те байты, которые ему дают.
- VFS: `handle` (`pkg/vfs/handle.go:32-60`) — сюда добавляем FEK; `newFileHandle` (:238); handles персистятся для crash recovery (`saveHandle`, :262) — **FEK туда не сериализуется**.
- Read chain: `VFS.Read` → `h.reader.Read` → `fileReader.Read` (`reader.go:521`) → `sliceReader.run` (:157, получает `[]meta.Slice` из `m.Read`) → `dataReader.readSlice` (:660-700) → `store.NewReader(s.Id, s.Size)`.
- Write chain: `VFS.Write` → `h.writer.Write` → `writeChunk` (`writer.go:262`, `store.NewWriter(0, tierID)`) → `sliceWriter.write` → `wSlice.WriteAt`; commit — `commitThread` (:167) → `m.Write(ctx, inode, indx, off, ss, lastMod)`.
- Slice в Redis: фиксированные 24 байта (`slice.go:73`, `marshalSlice` :75, `readSlices` :90 — hard-fail на другой длине).
- `Attr`: variable-length binary (база 68B, +8 ACL, +1 Tier; `Unmarshal` терпит отсутствие хвоста) — паттерн для crypto-полей (D1).
- В `pkg/chunk` и `pkg/vfs` криптографии НЕТ.

Что делает этап: шифрование блоков S3 CEK (AGDF), wrap CEK под FEK (AGCK), wrap/unwrap FEK под KEK (AGFK, для render/migration), plumbing FEK через VFS handle → chunk store, ciphertext-only local cache, предусловия версионирования FR-VER-1/4. Не делает: KeyManager, Redis-интеграцию wrapped_fek, proto-расширения gRPC, render-клиент (этапы 3–4). FEK/CEK в этом этапе — синтетические (тесты) или из attr (механика).

Решения этапа: design.md, «Решения Stage 2» (2.1–2.8).

### Tasks

- [x] 2.1 Крипто-примитивы AGDF/AGCK: `EncryptBlock`/`DecryptBlock`/`IsLegacyBlock`/`WrapCEK`/`UnwrapCEK` (AES-256-GCM, nonce на запись, AAD = sliceID‖blockIndex / driveFileID‖sliceID‖fekVersion). Файл: `pkg/chunk/cek_encrypt.go`. Проверка: `go test ./pkg/chunk/ -run 'TestEncryptBlock|TestDecryptBlock|TestWrapCEK|TestUnwrapCEK'` (round-trip, tamper → fail-closed, nonce uniqueness, AAD mismatch).
- [x] 2.2 AGFK-примитивы форка (`WrapFEK`/`UnwrapFEK`, `FekAAD`) + known-answer векторы из stage 1. Файл: `pkg/meta/fek_crypto.go`. Проверка: `go test ./pkg/meta/ -run 'TestWrapFEK|TestUnwrapFEK'`.
- [x] 2.3 Slice-запись с `wrapped_cek`: 24B base + optional length-prefixed AGCK tail; `readSlices` парсит оба формата; обновление call-sites в `redis.go`. Файлы: `pkg/meta/slice.go`, `pkg/meta/interface.go` (`Slice.WrappedCEK`), `pkg/meta/redis.go`. Проверка: `go test ./pkg/meta/ -run 'TestSlice|TestMarshalSlice'` — legacy 24B байт-в-байт как до изменения.
- [x] 2.4 Crypto-поля `Attr` (persisted suffix, transient `Fek` вне Marshal) + `Format.EncryptionEnabled/KEKVersion`. Файлы: `pkg/meta/interface.go`, `pkg/meta/config.go`. Проверка: `go test ./pkg/meta/ -run 'TestAttr|TestFormat'` — `Marshal(Attr{Fek: x})` == `Marshal(Attr{})` по crypto-части; legacy marshal не изменился.
- [x] 2.5 `ChunkStore.NewReaderWithKey/NewWriterWithKey`; `rSlice.cek`/`wSlice.cek`; decrypt в единой точке после load (кэш/S3), encrypt перед upload/staging; range-GET отключён для CEK-slice; без compressor для ciphertext; double-encrypt guard по magic AGDF. Файлы: `pkg/chunk/chunk.go`, `pkg/chunk/cached_store.go`. Проверка: `go test ./pkg/chunk/ -run 'TestEncryptedStore|TestLegacyStore'` — S3 и disk cache содержат только ciphertext (NFR-SEC-1); legacy (key=nil) не изменился.
- [x] 2.6 VFS plumbing: `handle.fek/fekVer/encrypted` (не сериализуется в saveHandle), CEK-кэш per open file в `fileReader`/`fileWriter`, wrap CEK при commit. Файлы: `pkg/vfs/handle.go`, `pkg/vfs/reader.go`, `pkg/vfs/writer.go`. Проверка: `go test ./pkg/vfs/ -run 'TestFileWriter_WrapsCEKUnderFEK|TestFileReader_UnwrapsCEKOnce'`.
- [x] 2.7 Предусловия версионирования FR-VER-1/4: хук `baseMeta.fileCryptoDeletable` + точки вызова в GC; `fek_version` end-to-end. Файлы: `pkg/meta/base.go`, `pkg/meta/redis.go`. Проверка: `go test ./pkg/meta/ -run 'TestFileCryptoDeletable|TestCanDeleteFileCrypto'`.
- [x] 2.8 Регрессия этапа. Проверка: `go build ./... && go vet ./pkg/...`; `make test.meta.core && make test.pkg` зелёные; `go fmt -l pkg/ cmd/` пусто.

### Implementation details

#### Шаг 1. Крипто-примитивы

**Файл:** `pkg/chunk/cek_encrypt.go` (новый, Apache header)

```go
package chunk

const (
	// AGDF — зашифрованный блок в S3: magic(4)|version(1)|nonce(12)|ciphertext(N)|tag(16)
	chunkMagic   = "AGDF"
	chunkVersion = 1
	// AGCK — wrapped CEK: magic(4)|version(1)|nonce(12)|ciphertext(32)|tag(16) = 65 bytes
	cekMagic   = "AGCK"
	cekVersion = 1
)

// EncryptBlock шифрует блок AES-256-GCM. nonce — новый случайный на каждый вызов (NFR-SEC-9).
// AAD = sliceID(8B BE) || blockIndex(4B BE).
func EncryptBlock(cek, plaintext []byte, sliceID uint64, blockIndex uint32) ([]byte, error) {
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 12)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err = rand.Read(nonce); err != nil { // crypto/rand (NFR-SEC-7)
		return nil, err
	}
	aad := make([]byte, 12)
	binary.BigEndian.PutUint64(aad[:8], sliceID)
	binary.BigEndian.PutUint32(aad[8:], blockIndex)
	ct := gcm.Seal(nil, nonce, plaintext, aad) // ct = ciphertext||tag
	out := make([]byte, 0, 4+1+12+len(ct))
	out = append(out, []byte(chunkMagic)...)
	out = append(out, chunkVersion)
	out = append(out, nonce...)
	return append(out, ct...), nil
}

// DecryptBlock проверяет magic/version/tag; legacy (без magic) → ошибка IsLegacy.
func DecryptBlock(cek, data []byte, sliceID uint64, blockIndex uint32) ([]byte, error) { ... }

// IsLegacyBlock — true если данные не начинаются с AGDF (plaintext, NFR-COMPAT-1).
func IsLegacyBlock(data []byte) bool { return !bytes.HasPrefix(data, []byte(chunkMagic)) }

// WrapCEK: AGCK = FEK оборачивает CEK. AAD = len(driveFileID):4 || driveFileID || sliceID(8B BE) || fekVersion(4B BE).
func WrapCEK(fek, cek []byte, driveFileID string, sliceID uint64, fekVersion uint32) ([]byte, error) { ... }

// UnwrapCEK: проверка magic/version/tag; несовпадение → ошибка (fail-closed, NFR-SEC-11).
func UnwrapCEK(fek []byte, wrapped []byte, driveFileID string, sliceID uint64, fekVersion uint32) ([]byte, error) { ... }
```

**Файл:** `pkg/meta/fek_crypto.go` (новый) — AGFK для форка (render-клиент этап 4, миграция этап 8):

```go
package meta

// FEKMagic = "AGFK"; layout: magic(4)|version(1)|kek_version(4)|nonce(12)|ciphertext(32)|tag(16) = 69 bytes
// AAD = len(volume_uuid):4 || volume_uuid || len(company_id):4 || company_id ||
//       len(drive_file_id):4 || drive_file_id || inode(8B BE) || fek_version(4B BE)
func WrapFEK(kek, fek []byte, aad FekAAD, kekVersion uint32) ([]byte, error) { ... }
func UnwrapFEK(kek []byte, wrapped []byte, aad FekAAD) (fek []byte, fekVersion uint32, err error) { ... }

type FekAAD struct {
	VolumeUUID, CompanyID, DriveFileID string
	Inode     Ino
	FekVersion uint32
}
```

> Known-answer векторы: взять из этапа 1 (platform `fek_crypto_test.go`) — те же 69/65-байтовые blob'ы. Расхождение = баг.

#### Шаг 2. Slice-запись с wrapped_cek (D2, D7)

**Файл:** `pkg/meta/slice.go` — изменения:

```go
// До: const sliceBytes = 24; marshalSlice(pos uint32, id uint64, size, off, len uint32) []byte
// После:
const sliceBytes = 24 // базовая часть; опциональный хвост — wrapped_cek

type slice struct {
	id    uint64
	size, off, len, pos uint32
	wrappedCEK []byte   // НОВОЕ: AGCK blob, пусто для legacy
	left, right *slice
}

func marshalSlice(pos uint32, id uint64, size, off, length uint32, wrappedCEK []byte) []byte {
	w := utils.NewBuffer(sliceBytes + 4 + len(wrappedCEK))
	w.Put32(pos); w.Put64(id); w.Put32(size); w.Put32(off); w.Put32(length)
	if len(wrappedCEK) > 0 {
		w.Put32(uint32(len(wrappedCEK)))
		w.PutBytes(wrappedCEK)
	}
	return w.Bytes()
}

// readSlices: базовые 24 байта как сейчас; если осталось >0 — читать u32 blobLen + AGCK blob.
// len(val) < 24 → "corrupt slice" (как сейчас). Хвост ≠ blobLen → "corrupt slice".
```

**`pkg/meta/interface.go`:** `Slice` (экспортный) += `WrappedCEK []byte`. Все call-sites `marshalSlice`/`readSlices` в `redis.go` (doWrite :3156, doRead :3085, doCloneEntry :5288, doCompactChunk :3782 и др.) — обновить под новую сигнатуру; `sql.go`/`tkv.go` — не трогаем (целевой бэкенд Redis; если компиляция ломается из-за общего кода — минимальные правки с сохранением legacy-поведения).

**Тест:** `pkg/meta/slice_test.go` — round-trip: legacy 24B (байт-в-байт как до изменения), с AGCK-хвостом, corrupt-кейсы.

#### Шаг 3. Attr crypto-поля (D1, D8)

**Файл:** `pkg/meta/interface.go` — `Attr`:

```go
type Attr struct {
	// ... существующие поля ...
	Tier uint8

	// File encryption (persisted suffix; см. Marshal/Unmarshal).
	Encrypted   bool
	DriveFileID string
	FekVersion  uint32
	CryptoAlg   string // "AES-256-GCM"
	WrappedFek  []byte // AGFK blob

	// Transient, client-side only. НИКОГДА не маршализуется (D1).
	Fek []byte `json:"-"`
}
```

`Attr.Marshal`: после существующего хвоста (Tier) — если `Encrypted || len(WrappedFek) > 0`: байт `0x01` + suffix по решению 2.8. `Attr.Unmarshal`: `rb.Left()`-паттерн — отсутствие suffix = legacy (все crypto-поля zero). **Критично:** `Fek` не участвует в Marshal/Unmarshal; добавить unit-тест, что `Marshal(Attr{Fek: ...})` == `Marshal(Attr{})` по crypto-части.

**`pkg/meta/config.go`:** `Format` += `EncryptionEnabled bool \`json:",omitempty"\`` и `KEKVersion int \`json:",omitempty"\`` (паттерн существующих полей).

#### Шаг 4. ChunkStore: ключевые reader/writer (D4)

**Файл:** `pkg/chunk/chunk.go`:

```go
type ChunkStore interface {
	// ... существующие методы ...
	// NewReaderWithKey — Reader, прозрачно дешифрующий блоки slice ключом key (CEK).
	// key == nil → plaintext (legacy).
	NewReaderWithKey(id uint64, length int, key []byte) Reader
	// NewWriterWithKey — Writer, прозрачно шифрующий блоки ключом key. key == nil → plaintext.
	NewWriterWithKey(id uint64, tierID uint8, key []byte) Writer
}
```

**Файл:** `pkg/chunk/cached_store.go`:

```go
type rSlice struct {
	// ... существующие ...
	cek []byte // НОВОЕ: CEK slice; nil → legacy plaintext
}

type wSlice struct {
	// ... существующие ...
	cek []byte // НОВОЕ
}

func (store *cachedStore) NewReaderWithKey(id uint64, length int, key []byte) Reader {
	r := &rSlice{store: store, id: id, length: length, cek: key}
	// ... как в NewReader ...
	return r
}
// NewWriterWithKey аналогично; NewReader/NewWriter → *WithKey(..., nil).
```

**Read path** — `rSlice.ReadAt` (:97):
- range-ветка: `if s.cek == nil && <условие range как сейчас>` — иначе полный блок (решение 2.4).
- `store.load(ctx, key, tmp, cache, s.cek)` — сигнатура `load` расширяется аргументом `cek []byte`; после `io.ReadFull(in, p.Data)`:

```go
if cek != nil {
	if IsLegacyBlock(p.Data) {
		// legacy-блок в зашифрованном файле не ожидается; но mixed-состояние
		// во время миграции (этап 8) допустимо → passthrough.
	} else {
		plain, err := DecryptBlock(cek, p.Data, s.id, uint32(indx)) // sliceID из rSlice
		if err != nil { return 0, err } // fail-closed
		copy(p.Data, plain)
		n = len(plain)
	}
}
```

- Кэш: `bcache.load(key)` возвращает **ciphertext** (то, что мы кэшируем) → тот же decrypt после cache-hit. То есть decrypt вынесен в единое место после получения данных (и из кэша, и из S3).

**Write path** — `store.upload` (:356):

```go
func (store *cachedStore) upload(ctx context.Context, key string, block *Page, s *wSlice) error {
	cek := []byte(nil)
	if s != nil {
		cek = s.cek
	}
	out := block
	if cek != nil && !IsLegacyBlock(block.Data) {
		ct, err := EncryptBlock(cek, block.Data, sliceIDFromKey(key), blockIndexFromKey(key))
		if err != nil { return err }
		out = page.NewPage(ct) // ciphertext page
	}
	// кэш получает CIPHERTEXT (NFR-SEC-1):
	if sync && (...) { store.bcache.cache(key, out, false, false) }
	// put: для зашифрованного — без compressor (решение 2.3):
	if out != block {
		return store.put(ctx, key, out)
	}
	// ... legacy-ветка с compressor как сейчас ...
}
```

`sliceIDFromKey/blockIndexFromKey` — парсинг из `chunks/.../{id}_{indx}_{size}` (хелпер рядом с `parseObjOrigSize`, :1022).

**Writeback staging** — `wSlice.upload` (:400): `bcache.stage(key, ctData, tierID)` — передаём ciphertext (тот же `EncryptBlock` до stage; magic-проверка решения 2.5 против повторного шифрования в `uploadStagingFile` → `store.upload`).

#### Шаг 5. VFS: FEK в handle, CEK в reader/writer (D5)

**Файл:** `pkg/vfs/handle.go`:

```go
type handle struct {
	// ... существующие ...
	tierID uint8

	// encryption — transient, НЕ сериализуется в saveHandle (см. dumpAllHandles).
	fek       []byte
	fekVer    uint32
	encrypted bool
}
```

`newFileHandle(inode, length, flags, tierID)` → `newFileHandle(inode, length, flags, tierID, attr *meta.Attr)`: копирует `attr.Fek/attr.FekVersion/attr.Encrypted`. Вызывающие: `VFS.Open` (vfs.go:549 — attr уже есть), `VFS.Create` (vfs.go:506).

**Crash recovery** (`loadAllHandles`, :352): для зашифрованных файлов (volume `EncryptionEnabled`) повторно резолвить FEK: `v.Meta.Open(ctx, inode, flags, &attr)` → обновить handle.fek. Если FEK не получен (fail-closed) — handle помечается stale, IO → EIO до повторного open.

**Файл:** `pkg/vfs/reader.go`:
- `fileReader` += `fek []byte; fekVer uint32; driveFileID string; cekCache map[uint64][]byte` (D6).
- `dataReader.Open(inode, length)` → `Open(inode, length, attr *meta.Attr)` — FEK из attr.
- `readSlice` (:660): для каждого `s` с `len(s.WrappedCEK) > 0`:

```go
cek, ok := fr.cekCache[s.Id]
if !ok {
	cek, err = chunk.UnwrapCEK(fr.fek, s.WrappedCEK, fr.driveFileID, s.Id, fr.fekVer)
	if err != nil { return 0, err } // fail-closed
	fr.cekCache[s.Id] = cek
}
reader := r.store.NewReaderWithKey(s.Id, int(s.Size), cek)
```

- legacy slice (`WrappedCEK == nil`) → `NewReader` (plaintext passthrough).

**Файл:** `pkg/vfs/writer.go`:
- `fileWriter` += `fek []byte; fekVer uint32; driveFileID string`.
- `sliceWriter`: при создании — `s.cek = make([]byte, 32); rand.Read(s.cek)` (NFR-SEC-7); `store.NewWriterWithKey(0, tierID, s.cek)`.
- `commitThread` (:167) перед `m.Write`:

```go
if len(s.cek) > 0 {
	ss.WrappedCEK = chunk.WrapCEK(fw.fek, s.cek, fw.driveFileID, s.id, fw.fekVer)
}
f.w.m.Write(meta.Background(), f.inode, c.indx, s.off, ss, s.lastMod)
```

#### Шаг 6. Предусловия версионирования (FR-VER-1, FR-VER-4)

**Файл:** `pkg/meta/base.go`:

```go
type baseMeta struct {
	// ...
	// FR-VER-1: политика удаления crypto-метаданных. nil → удаляемо (текущее поведение).
	// Будущее версионирование поставит проверку reference counting здесь.
	fileCryptoDeletable func(inode Ino) bool
}

func (m *baseMeta) canDeleteFileCrypto(inode Ino) bool {
	if m.fileCryptoDeletable != nil {
		return m.fileCryptoDeletable(inode)
	}
	return true
}
```

Точки вызова — в GC-пути удаления inode/chunk (`cleanInode`/`cleanChunk` в redis.go): перед удалением crypto-полей attr и slice-записей с `WrappedCEK` — `if !m.canDeleteFileCrypto(inode) { skip crypto cleanup }`. (Точные call-sites найти по `doCleanInode`/`cleanAll`; если в текущем GC crypto-поля ещё не удаляются отдельно — хук ставится в место, где attr slice'ов очищается.)

**FR-VER-4:** `fek_version` уже поддерживается end-to-end (Attr.FekVersion, AAD AGFK/AGCK, Format.KEKVersion) — покрывается тестами шагов 1–3.

#### Шаг 7. Тесты (полные тела ключевых)

**`pkg/chunk/cek_encrypt_test.go`:**

```go
func TestEncryptBlock_RoundTrip(t *testing.T) {
	cek := make([]byte, 32); require.NoError(t, rand.Read(cek))
	plain := bytes.Repeat([]byte{0xAB}, 4<<20) // BlockSize
	ct, err := EncryptBlock(cek, plain, 42, 3)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(ct, []byte("AGDF")))
	out, err := DecryptBlock(cek, ct, 42, 3)
	require.NoError(t, err)
	assert.Equal(t, plain, out)
}

func TestDecryptBlock_TamperTag(t *testing.T) {
	// ... encrypt ...
	ct[len(ct)-1] ^= 0xFF // бит в tag
	_, err := DecryptBlock(cek, ct, 42, 3)
	assert.Error(t, err) // fail-closed (NFR-SEC-11)
}

func TestEncryptBlock_NonceUniqueness(t *testing.T) {
	// два шифрования одного plaintext → разные ciphertext (NFR-SEC-9)
}

func TestDecryptBlock_AADMismatch(t *testing.T) {
	// decrypt с другим sliceID/blockIndex → ошибка
}

func TestWrapCEK_RoundTrip_AndLegacyPassthrough(t *testing.T) { ... }
```

**`pkg/chunk/cached_store_enc_test.go`** (fake object storage — in-memory map, паттерн тестов pkg/object):

```go
func TestEncryptedStore_CiphertextInS3AndCache(t *testing.T) {
	store := newTestCachedStore(t, fakeBlob, cacheDir) // с disk cache
	w := store.NewWriterWithKey(0, 0, cek)
	_, err := w.WriteAt(plainBlock, 0); require.NoError(t, err)
	require.NoError(t, w.Finish(len(plainBlock)))

	raw, _ := fakeBlob.Get("chunks/.../1_0_4194304") // object key sliceID=1
	assert.True(t, bytes.HasPrefix(raw, []byte("AGDF")))          // S3: ciphertext
	assert.False(t, bytes.Contains(raw, plainBlock[:64]))         // plaintext не в S3

	cacheFile := filepath.Join(cacheDir, "raw", "chunks/...")
	rawCache, _ := os.ReadFile(cacheFile)
	assert.True(t, bytes.HasPrefix(rawCache, []byte("AGDF")))     // NFR-SEC-1: кэш ciphertext
	assert.False(t, bytes.Contains(rawCache, plainBlock[:64]))

	r := store.NewReaderWithKey(1, len(plainBlock), cek)
	buf := make([]byte, len(plainBlock))
	_, err = r.ReadAt(context.Background(), page.NewPage(buf), 0)
	require.NoError(t, err)
	assert.Equal(t, plainBlock, buf) // round-trip
}

func TestLegacyStore_Unchanged(t *testing.T) {
	// NewReader/NewWriter (key=nil): объект без magic, кэш plaintext — как до этапа
}
```

**`pkg/meta/slice_test.go`, `pkg/meta/attr_crypto_test.go`:** round-trip legacy/encrypted; `Marshal(Attr{Fek: x})` не содержит Fek; corrupt-кейсы slice-хвоста.

**`pkg/vfs/enc_test.go`:** stub ChunkStore (запоминает key из NewReaderWithKey/NewWriterWithKey) + stub meta:

```go
func TestFileWriter_WrapsCEKUnderFEK(t *testing.T) {
	// write → commit → проверить, что m.Write получил ss.WrappedCEK,
	// который UnwrapCEK(fek, ...) возвращает тот же CEK, что использовался в store
}

func TestFileReader_UnwrapsCEKOnce(t *testing.T) {
	// два чтения одного slice → unwrap один раз (cekCache), store получил CEK
}
```

**FR-TEST-7 (предусловия):** `pkg/meta/base_test.go` — `fileCryptoDeletable = func(Ino) bool { return false }` → GC не удаляет crypto-поля (fake engine).

### Verification

```sh
cd /Users/i.obukhov/github/juicefs
go build ./... && go vet ./pkg/...
make test.meta.core   # slice/attr/base изменения
make test.pkg         # chunk/vfs
go fmt -l pkg/ cmd/   # пусто
```

### Stage acceptance criteria

- [x] AGDF/AGCK/AGFK примитивы: round-trip, tamper → fail-closed, nonce uniqueness, AAD mismatch (FR-TEST-1).
- [x] Зашифрованный write/read round-trip через cachedStore; S3 и disk cache содержат только ciphertext (NFR-SEC-1, FR-TEST-5).
- [x] Legacy (key=nil / без magic) — поведение байт-в-байт как до этапа (NFR-COMPAT-1, AC-6 частично).
- [x] FEK в handle, CEK unwrap/wrap в reader/writer; CEK-кэш per open file (FR-USR-10).
- [x] Slice-запись: legacy 24B не изменился; новый формат парсится (D2).
- [x] Attr: legacy marshal не изменился; crypto-suffix round-trip; `Fek` не сериализуется (D1).
- [x] FR-VER-1/4: хук `fileCryptoDeletable` + `fek_version` end-to-end (AC-14 частично).
- [x] `make test.meta.core` и `make test.pkg` зелёные.

## 3. Stage 3 — Meta + KeyManager (оба репозитория)

**Контекст.** Что уже есть (после этапов 1–2): Platform: `DriveKeyManagerService` (CreateFileKey/GetFileFEK/GetBulkFileFEK/FetchCompanyKEK/ProvisionCompanyKEK), KMS/Secret Manager, аудит, IdentityResolver. Форк: AGDF/AGCK/AGFK примитивы (`pkg/chunk/cek_encrypt.go`, `pkg/meta/fek_crypto.go`), slice-запись с `WrappedCEK`, `Attr` crypto-поля + transient `Fek`, `Format.EncryptionEnabled/KEKVersion`, ChunkStore `NewReaderWithKey/NewWriterWithKey`, VFS plumbing (handle.fek, CEK unwrap/wrap).

Факты о коде:
- Proto: `pkg/meta/pb/meta.proto` (+ `meta_common.proto`, `meta_lifecycle.proto`); `OpenResponse{errno, attr}`, `CreateRequest{ctx, parent, name, mode, cumask, flags}`; `ProtoAttr` (meta_common.proto:35-54), `ProtoSlice{id,size,off,len}` (:57-62), `ProtoFormat` (:147-178).
- Клиент: `grpcMeta.Open` (`grpc_client_fuse.go:294-315`), `Create` (:265-291); attr/dir LRU-кэши (`expirable.LRU`).
- Сервер: `MetaProxyServer.Open` (`grpc_server_fuse.go:171-180`), `Create` (:150-169, заполняет `inodePathCache`); identity — `extractUserIDFromOIDC(ctx)` (`authz_interceptor.go:82`).
- Конверсии proto↔Go: `grpc_convert.go` (`AttrToProto/ProtoToAttr` :26-70, `SliceToProto/ProtoToSlice` :74-110, `FormatToProtoPtr/ProtoToFormat` :245-345).
- Proxy-команда: `cmd/meta_proxy.go` (флаги `--authz-service`, `--authz-volume-name`, TLS).
- Authz interceptor уже проверяет Open (Read/Write по flags) и Create (Write на parent) — KeyManager-проверка — второй слой (defense in depth, FR-USR-4/5).

Что делает этап: proto-расширения (FR-API-1..3), `redisMeta.SetFileCrypto`, вызовы KeyManager в proxy при Create/Open, клиентский FEK LRU (FR-USR-9), генерация `drive_file_id` клиентом (FR-USR-3), wiring флагов, интеграционные тесты полного цикла. Предусловие: этапы 1–2.

Решения этапа: design.md, «Решения Stage 3» (3.1–3.7).

### Tasks

- [ ] 3.1 Proto-расширения форка (`ProtoFileCrypto`, `ProtoSlice.wrapped_cek`, `OpenResponse.fek/fek_version/encrypted`, `CreateRequest.drive_file_id`, `Format.encryption_enabled/kek_version`) + конверсии proto↔Go. Файлы: `pkg/meta/pb/meta.proto`, `pkg/meta/pb/meta_common.proto`, generated, `pkg/meta/grpc_convert.go`. Проверка: `go build ./...`; `go test ./pkg/meta/ -run 'TestAttrToProto|TestSliceToProto|TestFormatToProto'` (round-trip + legacy).
- [ ] 3.2 `redisMeta.SetFileCrypto` — один Redis txn (GET attr → mutate crypto-поля → SET); интерфейс `fileCryptoSetter` (type-assertion, не в `Meta`). Файл: `pkg/meta/redis_fek.go`. Проверка: интеграционный тест с реальным Redis: `go test -run 'TestSetFileCrypto' ./pkg/meta/` (гейт `make test.meta.non-core`).
- [ ] 3.3 KeyManager-клиент форка: копия proto platform + gRPC-обёртка (TLS по флагам). Файлы: `pkg/meta/keymanager_pb/key_manager.proto`, generated, `pkg/meta/keymanager_client.go`. Проверка: `go build ./...`; комментарий «sync with agio-platform» в копии proto.
- [ ] 3.4 Proxy Create/Open с KeyManager: FEK при Create + rollback `Unlink` (FR-USR-2), FEK при Open только при `cached_fek_version != attr.FekVersion`, plaintext FEK только в `OpenResponse`; UUID-валидация identity (`extractUserIDFromOIDC` → `(string, error)`, interceptor → `Unauthenticated`). Файлы: `pkg/meta/grpc_server_fuse.go`, `pkg/meta/authz_interceptor.go`. Проверка: интеграционные тесты `TestEncryptedFullCycle`, `TestCreateRollback`, `TestUserWithoutPermission_Denied`, `TestNonUUIDSub_Unauthenticated` (Redis + MinIO + fake KeyManager).
- [ ] 3.5 Клиент: FEK LRU 100k/TTL 15 мин в `grpcMeta`, `drive_file_id = uuid.New()` при Create, кэш-хит Open через `cached_fek_version`. Файлы: `pkg/meta/grpc_client.go`, `pkg/meta/grpc_client_fuse.go`. Проверка: `go test -run 'TestFekCache|TestEncrypted' ./pkg/meta/` — второй Open не вызывает KeyManager (counter).
- [ ] 3.6 Wiring proxy: флаги `--keymanager-service`, `--keymanager-tls-*`; warning при отсутствии. Файл: `cmd/meta_proxy.go`. Проверка: `go build ./...`; `./juicefs meta-proxy --help` показывает флаги.
- [ ] 3.7 Интеграционный suite этапа (Redis + MinIO + in-process fake KeyManager): полный цикл, deny, owner bypass, legacy volume без KeyManager-вызовов, cache-hit. Проверка: `make test.meta.non-core` зелёный; `go test -run 'TestEncrypted|TestFekCache|TestOwnerBypass|TestLegacyVolume' ./pkg/meta/`.

### Implementation details

#### Шаг 1. Proto-расширения (форк)

**`pkg/meta/pb/meta_common.proto`:**

```proto
message ProtoFileCrypto {
  bytes  wrapped_fek = 1;
  string drive_file_id = 2;
  bool   encrypted = 3;
  int32  fek_version = 4;
  string crypto_alg = 5;
}

message ProtoAttr {
  // ... существующие поля (1..19) ...
  ProtoFileCrypto file_crypto = 20;   // НОВОЕ
}

message ProtoSlice {
  uint64 id = 1;
  uint32 size = 2;
  uint32 off = 3;
  uint32 len = 4;
  bytes  wrapped_cek = 5;             // НОВОЕ (AGCK)
}

message ProtoFormat {
  // ... существующие ...
  bool   encryption_enabled = <next>; // НОВОЕ
  int32  kek_version = <next>;        // НОВОЕ
}
```

**`pkg/meta/pb/meta.proto`:**

```proto
message CreateRequest {
  // ... существующие (1..6) ...
  string drive_file_id = 7;           // НОВОЕ (FR-API-2)
}

message OpenRequest {
  // ... существующие (1..3) ...
  uint32 cached_fek_version = 4;      // НОВОЕ (решение 3.1)
}

message OpenResponse {
  uint32 errno = 1;
  ProtoAttr attr = 2;
  bytes  fek = 3;                     // НОВОЕ: plaintext FEK, TLS only (FR-API-1)
  int32  fek_version = 4;
  bool   encrypted = 5;
}
```

Регенерация pb-кода (скрипт/команда как для authz_pb — проверить `Makefile`/`generate_protos.sh` форка; сгенерированное коммитим).

**`pkg/meta/grpc_convert.go`:** `AttrToProto` — если `attr.Encrypted || len(attr.WrappedFek) > 0` → заполнить `ProtoFileCrypto`; `ProtoToAttr` — обратное (+ `attr.Fek` НЕ из proto — только из OpenResponse); `SliceToProto/ProtoToSlice` — `WrappedCEK`; `FormatToProtoPtr/ProtoToFormat` — `EncryptionEnabled/KEKVersion`.

#### Шаг 2. redisMeta.SetFileCrypto

**Файл:** `pkg/meta/redis_fek.go` (новый, по SRS §17.1)

```go
// FileCrypto — crypto-метаданные файла для записи в attr inode.
type FileCrypto struct {
	WrappedFek  []byte
	DriveFileID string
	FekVersion  uint32
	CryptoAlg   string // "AES-256-GCM"
}

// SetFileCrypto записывает crypto-поля в attr inode (один Redis txn: GET → mutate → SET).
func (m *redisMeta) SetFileCrypto(ctx Context, inode Ino, c *FileCrypto) syscall.Errno {
	var attr Attr
	if err := m.txn(ctx, func(tx *redis.Tx) error {
		a, err := tx.Get(ctx, m.inodeKey(inode)).Bytes()
		if err != nil {
			return err
		}
		m.parseAttr(a, &attr)
		attr.Encrypted = true
		attr.WrappedFek = c.WrappedFek
		attr.DriveFileID = c.DriveFileID
		attr.FekVersion = c.FekVersion
		attr.CryptoAlg = c.CryptoAlg
		return tx.Set(ctx, m.inodeKey(inode), m.marshal(&attr), 0).Err()
	}, inode); err != nil {
		return errno(err)
	}
	return 0
}
```

> Паттерн `m.txn`/`m.marshal`/`m.parseAttr` — как в существующих redisMeta-методах (проверить точные имена при реализации; в некоторых версиях — `m.rdb` + явный pipeline).

#### Шаг 3. KeyManager-клиент форка

**Файлы:** `pkg/meta/keymanager_pb/key_manager.proto` (копия из platform, пакет `agio.platform.drive.crypto.v1`) + generated `.pb.go`; **`pkg/meta/keymanager_client.go`:**

```go
type KeyManagerClient interface {
	CreateFileKey(ctx context.Context, req *km.CreateFileKeyRequest) (*km.CreateFileKeyResponse, error)
	GetFileFEK(ctx context.Context, req *km.GetFileFEKRequest) (*km.GetFileFEKResponse, error)
}

// platformKeyManager — gRPC-обёртка (TLS по флагам proxy).
func NewKeyManagerClient(addr string, tlsCfg *tls.Config) (KeyManagerClient, error) { ... }
```

#### Шаг 4. Proxy: Create и Open

**Файл:** `pkg/meta/grpc_server.go` — поле `keyManager KeyManagerClient` + setter; type-assertion интерфейс:

```go
type fileCryptoSetter interface {
	SetFileCrypto(ctx Context, inode Ino, c *FileCrypto) syscall.Errno
}
```

**Файл:** `pkg/meta/grpc_server_fuse.go`:

```go
func (s *MetaProxyServer) Create(ctx context.Context, req *pb.CreateRequest) (*pb.CreateResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Create(mctx, Ino(req.Parent), req.Name, uint16(req.Mode),
		uint16(req.Cumask), uint32(req.Flags), &inode, &attr)
	if errno != 0 {
		return &pb.CreateResponse{Errno: uint32(errno)}, nil
	}
	childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
	if childPath != "" {
		s.inodePathCache.Set(inode, withDirSlash(childPath, &attr))
	}

	// Шифрование: FEK через KeyManager (FR-USR-1).
	if s.encryptionEnabled() && attr.Typ == TypeFile && s.keyManager != nil {
		userID, err := extractUserIDFromOIDC(ctx) // FR-ID-3: UUID-валидация (решение 3.7)
		if err != nil {
			_ = s.meta.Unlink(mctx, Ino(req.Parent), req.Name)
			return &pb.CreateResponse{Errno: uint32(syscall.EACCES)}, nil // fail-closed
		}
		resp, err := s.keyManager.CreateFileKey(ctx, &km.CreateFileKeyRequest{
			ActorUserId: userID, VolumeName: s.volumeName, Path: childPath,
			DriveFileId: req.DriveFileId, Inode: uint64(inode), VolumeUuid: s.volumeUUID,
		})
		if err != nil {
			_ = s.meta.Unlink(mctx, Ino(req.Parent), req.Name) // rollback (FR-USR-2, решение 3.3)
			return &pb.CreateResponse{Errno: uint32(syscall.EIO)}, nil
		}
		setter, ok := s.meta.(fileCryptoSetter)
		if !ok {
			_ = s.meta.Unlink(mctx, Ino(req.Parent), req.Name)
			return &pb.CreateResponse{Errno: uint32(syscall.EIO)}, nil
		}
		if st := setter.SetFileCrypto(mctx, inode, &FileCrypto{
			WrappedFek: resp.WrappedFek, DriveFileID: req.DriveFileId,
			FekVersion: uint32(resp.FekVersion), CryptoAlg: "AES-256-GCM",
		}); st != 0 {
			_ = s.meta.Unlink(mctx, Ino(req.Parent), req.Name)
			return &pb.CreateResponse{Errno: uint32(st)}, nil
		}
		attr.Encrypted = true
		attr.DriveFileID = req.DriveFileId
		attr.FekVersion = uint32(resp.FekVersion)
		attr.WrappedFek = resp.WrappedFek
		attr.CryptoAlg = "AES-256-GCM"
	}

	return &pb.CreateResponse{Errno: 0, Inode: uint64(inode), Attr: AttrToProto(&attr)}, nil
}

func (s *MetaProxyServer) Open(ctx context.Context, req *pb.OpenRequest) (*pb.OpenResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.Open(mctx, Ino(req.Inode), uint32(req.Flags), &attr)
	if errno != 0 {
		return &pb.OpenResponse{Errno: uint32(errno)}, nil
	}
	resp := &pb.OpenResponse{Errno: 0, Attr: AttrToProto(&attr)}

	if attr.Encrypted && s.keyManager != nil {
		resp.Encrypted = true
		resp.FekVersion = int32(attr.FekVersion)
		// Кэш-хит клиента: authz уже проверен interceptor'ом, KeyManager не вызываем (решение 3.1).
		if req.CachedFekVersion != attr.FekVersion {
			forWrite := req.Flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0
			path := s.inodePathCache.Get(Ino(req.Inode))
			if path == "" {
				return &pb.OpenResponse{Errno: uint32(syscall.EACCES)}, nil // fail-closed
			}
			userID, err := extractUserIDFromOIDC(ctx) // FR-ID-3 (решение 3.7)
			if err != nil {
				return &pb.OpenResponse{Errno: uint32(syscall.EACCES)}, nil // fail-closed
			}
			fekResp, err := s.keyManager.GetFileFEK(ctx, &km.GetFileFEKRequest{
				ActorUserId: userID, VolumeName: s.volumeName, Path: path,
				DriveFileId: attr.DriveFileID, Inode: uint64(req.Inode), ForWrite: forWrite,
				WrappedFek: attr.WrappedFek, VolumeUuid: s.volumeUUID,
			})
			if err != nil {
				return &pb.OpenResponse{Errno: uint32(syscall.EACCES)}, nil // fail-closed (NFR-AVAIL-3)
			}
			resp.Fek = fekResp.PlaintextFek
			resp.FekVersion = int32(fekResp.FekVersion)
		}
	}
	return resp, nil
}
```

`encryptionEnabled()` — из `s.meta.GetFormat().EncryptionEnabled`; `volumeName`/`volumeUUID` — из конфига proxy (`--authz-volume-name` + Format.UUID).

**Валидация identity (решение 3.7, FR-ID-3)** — `pkg/meta/authz_interceptor.go`:

```go
// extractUserIDFromOIDC возвращает OIDC sub (= user.id UUID, инвариант A6) и
// проверяет формат: неверный UUID = ошибка конфигурации идентичности → fail-closed.
func extractUserIDFromOIDC(ctx context.Context) (string, error) {
	idToken, ok := oidcIDTokenFrom(ctx) // существующая экстракция из interceptor'а
	if !ok {
		return "", errors.New("no oidc id token in context")
	}
	sub := idToken.Subject
	if _, err := uuid.Parse(sub); err != nil {
		return "", fmt.Errorf("oidc sub %q is not a valid UUID: %w", sub, err)
	}
	return sub, nil // без преобразования (FR-ID-2)
}
```

В interceptor: при ошибке → `codes.Unauthenticated` (запрос не доходит до handler'ов; проверка в Create/Open — defense in depth).

#### Шаг 5. Клиент: FEK LRU + drive_file_id

**Файл:** `pkg/meta/grpc_client.go`:

```go
type grpcMeta struct {
	// ... существующие ...
	fekCache *expirable.LRU[uint64, *fekEntry] // inode -> entry; 100k, TTL 15 мин (FR-USR-9)
}

type fekEntry struct {
	fek     []byte
	version uint32
}
```

Eviction callback — обнуление `fek` (memclr-хелпер; полный механизм — этап 6).

**Файл:** `pkg/meta/grpc_client_fuse.go`:

```go
func (m *grpcMeta) Create(...) syscall.Errno {
	// ... как сейчас +
	req.DriveFileId = uuid.New().String() // FR-USR-3 (решение 3.5)
	// ...
}

func (m *grpcMeta) Open(ctx Context, inode Ino, flags uint32, attr *Attr) syscall.Errno {
	ver := uint32(0)
	if e, ok := m.fekCache.Get(uint64(inode)); ok {
		ver = e.version
	}
	req := &pb.OpenRequest{Ctx: c, Inode: uint64(inode), Flags: flags, CachedFekVersion: ver}
	resp, err := m.client.Open(m.withSessionID(ctx), req)
	// ... errno/attr как сейчас ...
	if resp.GetEncrypted() {
		switch {
		case len(resp.GetFek()) > 0:
			m.fekCache.Add(uint64(inode), &fekEntry{fek: resp.GetFek(), version: uint32(resp.GetFekVersion())})
			attr.Fek = resp.GetFek()
			attr.FekVersion = uint32(resp.GetFekVersion())
		case ver == attr.FekVersion: // кэш-хит: FEK из LRU
			if e, ok := m.fekCache.Get(uint64(inode)); ok {
				attr.Fek = e.fek
			} else {
				return syscall.EIO // fail-closed: кэш пропал, а сервер FEK не дал
			}
		default:
			return syscall.EIO // fail-closed
		}
	}
	return 0
}
```

#### Шаг 6. Wiring

**`cmd/meta_proxy.go`:** флаги `--keymanager-service` (string, адрес), `--keymanager-tls-cert/--keymanager-tls-key/--keymanager-tls-ca` (паттерн authz-TLS); создание `NewKeyManagerClient`; `server.SetKeyManager(...)`. Если флаг пуст — warning в лог: «encryption disabled for all volumes».

**`cmd/mount.go`:** изменений не требуется (Format приходит через Load; `EncryptionEnabled` уже в ProtoFormat).

#### Шаг 7. Тесты

**Unit (форк):**
- `grpc_convert_test.go`: round-trip Attr/Slice/Format с crypto-полями; legacy без полей.
- `grpc_client_fek_test.go`: LRU — добавление, TTL-истечение (fake timer), кэш-хит Open (mock pb.MetaServiceClient: второй Open не вызывает KeyManager-ветку на сервере — проверяется через `cached_fek_version` в запросе).

**Интеграционные** (`pkg/meta/`, гейтинг как у существующих Redis-тестов — env-адрес, запуск `make test.meta.non-core`; S3 — MinIO по паттерну `make test.cmd`):

Компоненты: реальный Redis + MinIO; in-process `MetaProxyServer` над `redisMeta`; **fake KeyManager** (in-process gRPC, реализует proto):

```go
type fakeKeyManager struct {
	kek    []byte // фиксированный 32B
	authz  map[string]map[string]bool // userID -> path -> allowed
	denyCreate bool
	mu sync.Mutex
	calls int
}
func (f *fakeKeyManager) GetFileFEK(ctx, req) (*km.GetFileFEKResponse, error) {
	if !f.authz[req.ActorUserId][req.Path] {
		return nil, status.Error(codes.PermissionDenied, "no access")
	}
	fek, _, err := meta.UnwrapFEK(f.kek, req.WrappedFek, aad(req))
	return &km.GetFileFEKResponse{PlaintextFek: fek, FekVersion: 1}, err
}
// CreateFileKey — генерирует FEK, WrapFEK; при f.denyCreate → error
```

Сценарии (FR-TEST):

| Тест | Сценарий | Ожидание |
|---|---|---|
| `TestEncryptedFullCycle` (FR-TEST-8) | mount → create → write 1MB → close → unmount → mount → open → read | байты совпадают; в S3 — AGDF; в Redis attr — wrapped_fek |
| `TestUserWithoutPermission_Denied` (FR-TEST-9) | пользователь B без прав открывает файл A | EACCES; audit-вызов KeyManager с deny |
| `TestCreateRollback` (FR-USR-2) | fake KM `denyCreate=true` | Create → EIO; файла в листинге нет |
| `TestOwnerBypass` (FR-TEST-18) | «owner» (в fake authz — allow на всё) | FEK выдан, файл читается |
| `TestLegacyVolume_Unchanged` | `EncryptionEnabled=false` | Create/Open без KeyManager-вызовов (counter == 0) |
| `TestFekCacheHit_SkipsKeyManager` | два Open одного файла | второй Open: `cached_fek_version` в запросе, KeyManager не вызывается (counter) |
| `TestNonUUIDSub_Unauthenticated` (FR-TEST-20) | токен с `sub` = "not-a-uuid" → любой RPC через interceptor | `codes.Unauthenticated`; в KeyManager-запросах `actor_user_id` всегда UUID (FR-ID-3, AC-16) |

### Verification

```sh
cd /Users/i.obukhov/github/juicefs
go build ./... && go vet ./pkg/...
make test.meta.core
# с локальным Redis + MinIO (docker-compose.test.yml):
make test.meta.non-core   # или точечно: go test -run 'TestEncrypted|TestFekCache' ./pkg/meta/
```

### Stage acceptance criteria

- [ ] FR-USR-1..3, FR-USR-4..9, FR-USR-11, FR-API-1..3 реализованы.
- [ ] Identity на proxy: `sub` валидируется как UUID, `Unauthenticated` при неверном формате; `sub` уходит в KeyManager без преобразования (FR-ID-2/3, AC-16).
- [ ] Полный цикл create→write→read через proxy с шифрованием (FR-TEST-8).
- [ ] Deny без прав (FR-TEST-9), rollback Create (FR-USR-2), owner bypass (FR-TEST-18, AC-15).
- [ ] Кэш-хит FEK не ходит в KeyManager (NFR-PERF-1); authz на каждом Open сохраняется.
- [ ] Plaintext FEK только в OpenResponse; GetAttr/Readdir — только wrapped_fek (T6, решение 3.4).
- [ ] Legacy-том (EncryptionEnabled=false) — поведение без изменений.
- [ ] Тесты зелёные: `make test.meta.core` + интеграционные.

## 4. Stage 4 — Render-клиент (форк)

**Контекст.** Критическое ограничение (SRS §7): render-ноды — тысячи metadata RPS с ноды × сотни нод. Прямой Redis + S3, **без** Meta Proxy, KeyManager-RPC на каждый файл, OIDC и PG. Company KEK — единственный «ключ доверия» ноды (FR-RND-2: из Secret Manager при mount по identity ноды, НЕ через CLI).

Что уже есть: `redisMeta` с crypto-attr и slice-записями (этапы 2–3); `SetFileCrypto`. AGFK примитивы в форке (`pkg/meta/fek_crypto.go`). VFS: handle.fek, CEK unwrap/wrap (этап 2) — работает с любым источником FEK. Platform: `FetchCompanyKEK` RPC (этап 1, IAM-аутентификация). Mount-wiring: `cmd/mount.go:547-700` (`meta.NewClient` → `NewReloadableStorage` → `chunk.NewCachedStore` → `vfs.NewVFS` → fuse); `--subdir` (Chroot) уже существует.

Что делает этап: команда `juicefs render-mount`, декоратор `renderMeta` (локальный FEK unwrap/generate по Company KEK), LRU FEK 100k/1h, aggressive caching, company-prefix изоляция. Предусловие: этапы 1–3.

Решения этапа: design.md, «Решения Stage 4» (4.1–4.6).

### Tasks

- [ ] 4.1 Декоратор `RenderMeta` над `meta.Meta`: Open (FEK из LRU 100k/1h или локальный `UnwrapFEK`, чужая компания → EIO), Create (локальная генерация FEK + `SetFileCrypto` + rollback), остальные методы — делегирование. Файл: `pkg/meta/render_meta.go`. Проверка: `go test ./pkg/meta/ -run 'TestRenderMeta'` (unwrap, cross-company fail-closed, create, TTL).
- [ ] 4.2 Команда `juicefs render-mount`: прямой Redis (subdir = company prefix), `FetchCompanyKEK` по IAM ноды (TLS 1.3; KEK никогда через CLI), mlock KEK + обнуление при unmount. Файл: `cmd/render_mount.go`. Проверка: `go build ./...`; `./juicefs render-mount --help`; интеграционный `TestRenderFullCycle` без OIDC/proxy/PG (AC-4).
- [ ] 4.3 Aggressive caching (attr/entry timeout ≥ 60s) + pipelined readdir для render-режима. Файлы: `cmd/render_mount.go`, `pkg/meta/redis.go` (pipeline-ветка Readdir при необходимости). Проверка: unit/integration-тест таймаутов; проверка round-trips readdir (1 на директорию).
- [ ] 4.4 Интеграционные тесты: render читает/пишет файл user-клиента; cross-company → EIO + chroot. Проверка: `go test -run 'TestRender' ./pkg/meta/` (Redis + MinIO) зелёный.

### Implementation details

#### Шаг 1. renderMeta декоратор

**Файл:** `pkg/meta/render_meta.go` (новый)

```go
// RenderMeta — meta.Meta для render-нод: прямой бэкенд + локальный FEK unwrap/generate
// по Company KEK. Без authz, без OIDC (SRS §7).
type RenderMeta struct {
	inner    Meta          // redisMeta
	kek      []byte        // Company KEK, mlock'нут (FR-RND-3)
	volumeUUID string
	companyID  string
	prefix     string      // "companies/{code}" — для defense in depth

	fekCache *expirable.LRU[uint64, *fekEntry] // 100k, TTL 1h (FR-RND-7)
}

func NewRenderMeta(inner Meta, kek []byte, volumeUUID, companyID, prefix string) *RenderMeta { ... }

// Open: inner.Open → если attr.Encrypted: FEK из LRU или UnwrapFEK(kek, attr.WrappedFek, aad).
func (m *RenderMeta) Open(ctx Context, inode Ino, flags uint32, attr *Attr) syscall.Errno {
	st := m.inner.Open(ctx, inode, flags, attr)
	if st != 0 || !attr.Encrypted {
		return st
	}
	if e, ok := m.fekCache.Get(uint64(inode)); ok && e.version == attr.FekVersion {
		attr.Fek = e.fek
		return 0
	}
	aad := FekAAD{VolumeUUID: m.volumeUUID, CompanyID: m.companyID,
		DriveFileID: attr.DriveFileID, Inode: inode, FekVersion: attr.FekVersion}
	fek, ver, err := UnwrapFEK(m.kek, attr.WrappedFek, aad)
	if err != nil {
		return syscall.EIO // чужая компания / повреждённые метаданные → fail-closed (FR-TEST-12)
	}
	m.fekCache.Add(uint64(inode), &fekEntry{fek: fek, version: ver})
	attr.Fek = fek
	attr.FekVersion = ver
	return 0
}

// Create: inner.Create → генерация FEK (crypto/rand) → WrapFEK → SetFileCrypto (FR-RND-9).
func (m *RenderMeta) Create(ctx Context, parent Ino, name string, mode uint16, cumask uint16, flags uint32, inode *Ino, attr *Attr) syscall.Errno {
	st := m.inner.Create(ctx, parent, name, mode, cumask, flags, inode, attr)
	if st != 0 || attr.Typ != TypeFile {
		return st
	}
	fek := make([]byte, 32)
	if _, err := rand.Read(fek); err != nil {
		return syscall.EIO
	}
	driveFileID := uuid.New().String()
	aad := FekAAD{VolumeUUID: m.volumeUUID, CompanyID: m.companyID, DriveFileID: driveFileID, Inode: *inode, FekVersion: 1}
	wrapped, err := WrapFEK(m.kek, fek, aad, m.kekVersion())
	if err != nil {
		return syscall.EIO
	}
	setter, ok := m.inner.(fileCryptoSetter)
	if !ok {
		return syscall.EIO
	}
	if st := setter.SetFileCrypto(ctx, *inode, &FileCrypto{WrappedFek: wrapped, DriveFileID: driveFileID, FekVersion: 1, CryptoAlg: "AES-256-GCM"}); st != 0 {
		_ = m.inner.Unlink(ctx, parent, name) // rollback как в proxy (FR-USR-2)
		return st
	}
	attr.Encrypted = true
	attr.DriveFileID = driveFileID
	attr.FekVersion = 1
	attr.WrappedFek = wrapped
	attr.CryptoAlg = "AES-256-GCM"
	attr.Fek = fek // для последующего Open в том же handle-цикле
	return 0
}

// GetAttr/Close/остальные методы — проксирование в inner (GetAttr без FEK: FEK выдаётся только в Open).
```

> `kekVersion()` — из `inner.GetFormat().KEKVersion`. Проксирование остальных ~80 методов `Meta` — через embedding (`struct { Meta }` + переопределение Open/Create/Close) или явные делегаты; выбрать по паттерну кода (embedding короче).

#### Шаг 2. Команда render-mount

**Файл:** `cmd/render_mount.go` (новый, по SRS §17.1)

```go
func cmdRenderMount() *cli.Command {
	return &cli.Command{
		Name: "render-mount",
		Usage: "Mount a volume for render nodes: direct Redis + S3, Company KEK from KeyManager (no OIDC/proxy)",
		ArgsUsage: "META_URL MOUNTPOINT",
		Flags: expandFlags(
			append(mountFlags(),
				cli.StringFlag{Name: "company-id", Usage: "company ID (UUID) for KEK fetch and prefix"},
				cli.StringFlag{Name: "keymanager-service", Usage: "gRPC address of DriveKeyManagerService (FetchCompanyKEK)"},
				cli.StringFlag{Name: "iam-token-file", Usage: "path to YC IAM token file (node identity); default: instance metadata service"},
				// + storageFlags/dataCacheFlags как в mount
			), clientFlags(1.0)),
		Action: renderMount,
	}
}

func renderMount(c *cli.Context) error {
	// 1. Meta: ПРЯМОЙ бэкенд (redis://...), subdir = companies/{code} (решение 4.2).
	metaConf := getMetaConf(c, mp, ...)
	metaConf.Subdir = "companies/" + companyCode // из --company-id или из volume-конфига
	metaCli := meta.NewClient(addr, metaConf)
	format, err := metaCli.Load(true)

	// 2. Company KEK: FetchCompanyKEK по identity ноды (FR-RND-2). ЗАПРЕЩЕНО через CLI.
	kek, kekVersion, err := fetchCompanyKEK(c, companyID) // gRPC + IAM token (YC instance metadata / файл)
	if err != nil { return err }
	mlockKEK(kek) // unix.Mlock; best-effort

	// 3. renderMeta над redisMeta.
	inner := metaCli.(interface{ Raw() meta.Meta }) // или NewClient возвращает redisMeta напрямую — проверить
	rm := meta.NewRenderMeta(inner, kek, format.UUID, companyID, "companies/"+companyCode)

	// 4. Дальше — стандартный путь mount(): NewReloadableStorage(format), chunk.NewCachedStore,
	//    vfs.NewVFS (с rm вместо metaCli), fuse.Serve. Aggressive caching: attr/entry timeout ≥ 60s (FR-RND-11).
}
```

> Точная форма получения «внутреннего» redisMeta из `meta.NewClient` проверить при реализации (возможно, `NewClient("redis://...")` возвращает `*redisMeta` как есть — тогда декоратор оборачивает его напрямую; Chroot применить до декоратора).

**IAM-токен ноды:** YC — instance metadata service (`169.254.169.254/computeMetadata/v1/instance/service-accounts/default/identity`) или `--iam-token-file`; AWS (альтернативный провайдер) — SigV4 через SDK. Реализовать YC первыми (D12), интерфейс `iamTokenProvider interface { Token(ctx) (string, error) }`.

**TLS 1.3** для gRPC к KeyManager (NFR-SEC-8): `tls.Config{MinVersion: tls.VersionTLS13}`.

#### Шаг 3. Aggressive caching и pipelining (FR-RND-11/12)

- FUSE: `attr_timeout`, `entry_timeout` ≥ 60s в render-mount (существующие fuseFlags — проверить имена; если жёстко заданы — добавить флаг `--render-cache-ttl`).
- Readdir: проверить `redisMeta.Readdir`/`doReaddir` — если на entry последовательные round-trips, добавить pipeline-вариант для render-режима (Redis pipeline через `rdb.Pipeline()`); цель — 1 round-trip на директорию.

#### Шаг 4. Тесты

**Unit (`pkg/meta/render_meta_test.go`, fake inner Meta):**

```go
func TestRenderMeta_Open_UnwrapsFEK(t *testing.T) {
	kek := testKEK() // 32B
	fek := make([]byte, 32); rand.Read(fek)
	wrapped, _ := WrapFEK(kek, fek, FekAAD{VolumeUUID: "v", CompanyID: "c", DriveFileID: "d", Inode: 7, FekVersion: 1}, 1)

	inner := &fakeMeta{attr: &Attr{Encrypted: true, WrappedFek: wrapped, DriveFileID: "d", FekVersion: 1}}
	rm := NewRenderMeta(inner, kek, "v", "c", "companies/c")

	var attr Attr
	st := rm.Open(Background(), 7, 0, &attr)
	assert.Zero(t, st)
	assert.Equal(t, fek, attr.Fek)
}

func TestRenderMeta_CrossCompany_FailClosed(t *testing.T) {
	// wrapped_fek обёрнут KEK компании A; render-клиент компании B (другой KEK) → EIO (FR-TEST-12, AC-5)
}

func TestRenderMeta_Create_GeneratesFEK(t *testing.T) {
	// Create → inner получил SetFileCrypto с wrapped_fek; UnwrapFEK(kek, wrapped) == attr.Fek (FR-RND-9)
}

func TestRenderMeta_FekCache_TTL1h(t *testing.T) { ... } // второй Open без повторного unwrap (counter)
```

**Интеграционный (Redis + MinIO, `make test.meta.non-core`):**
- `TestRenderFullCycle` (FR-TEST-11): user-клиент (proxy + fake KeyManager с KEK_A) создаёт файл → render-mount (KEK_A, прямой Redis) читает и пишет без OIDC/proxy; S3 — AGDF.
- `TestRenderCrossCompany`: render B (KEK_B) → Open файла компании A → EIO; листинг prefix'а B не содержит файлов A (chroot).
- AC-4: в тесте нет ни OIDC, ни proxy, ни PG — только Redis + S3 + один gRPC-вызов FetchCompanyKEK при mount.

### Verification

```sh
cd /Users/i.obukhov/github/juicefs
go build ./... && go vet ./pkg/... cmd/...
make test.meta.core
# с Redis + MinIO:
go test -run 'TestRender' ./pkg/meta/
./juicefs render-mount --help   // smoke: флаги, описание
```

### Stage acceptance criteria

- [ ] FR-RND-1..14 реализованы (14 — «Render Proxy не требуется» — тривиально: его нет).
- [ ] AC-4: render-клиент работает без OIDC/proxy/PG; один gRPC-вызов KEK при mount.
- [ ] AC-5: cross-company доступ невозможен (криптографически + chroot).
- [ ] KEK: только из FetchCompanyKEK по identity ноды, mlock, обнуление при unmount (FR-RND-2/3, NFR-SEC-5).
- [ ] FEK LRU 100k/1h; CEK per open file (FR-RND-7/8).
- [ ] Aggressive caching ≥60s; readdir pipelining (FR-RND-11/12).
- [ ] Тесты зелёные.

## 5. Stage 5 — Clone/CopyFileRange/Compaction (форк)

**Контекст.** Семантика slice-sharing (проверено): `doCloneEntry` (`redis.go:5288-5414`) копирует chunk lists **verbatim** (`LRange` → `RPush`) и инкрементирует refcount (`HIncrBy sliceRefs`). Slice — глобальный иммутабельный объект; CEK живёт в slice-записи (AGCK, этап 2). Поэтому:
- Clone «из коробки» копирует `wrapped_cek` под **FEK источника** — target не сможет их развернуть своим FEK.
- Решение (D7): post-hoc re-wrap — переобёртка CEK в записях target под FEK target. **Данные в S3 не трогаются** (FR-OP-2, AC-7).

Компакция: `baseMeta.compactChunk` (`base.go:2800-2907`) → `newMsg(CompactChunk, slices, id, tierID)` (in-process) → `vfs.Compact` (`pkg/vfs/compact.go:54-107`): читает source slice'ы через `store.NewReader`, пишет merged через `store.NewWriter(newId)` → атомарная замена chunk list в `doCompactChunk` (`redis.go:3782`). Data-сторона уже проходит через ChunkStore — с CEK (этап 2) компакция «знает» шифрование, НО ей нужен FEK файла (нет пользовательского контекста) — решение D8.

Факты:
- `Meta.Clone` (`interface.go:~505`), `baseMeta.Clone` (`base.go:3335`), `cloneEntry` (:3402), `BatchClone` (:1856).
- Proxy: Clone RPC существует (authz-классификация — Admin); handler вызывает `s.meta.Clone`.
- `VFS.CopyFileRange` (`vfs.go`, FUSE entry `fuse.go:311-327`) — реализовать проверку пути при старте этапа.
- `cmd/gc.go:171` вызывает `vfs.Compact` напрямую (отдельный процесс).

Что делает этап: zero-copy Clone/CopyFileRange через CEK re-wrap, Compaction с CEK, RPC `ResolveFileKey`, предусловия FR-VER-2/3. Предусловие: этап 2 (этап 4 не блокирует — оркестрация общая).

Решения этапа: design.md, «Решения Stage 5» (5.1–5.6).

### Tasks

- [ ] 5.1 `redisMeta.RewrapSlices` — re-wrap всех AGCK в chunk lists dst из-под srcFek в-под dstFek, один txn на chunk list; legacy-записи не трогаются; решение по направлению зависимости pkg/meta→pkg/chunk для AGCK-примитивов (при необходимости — вынос в общий пакет). Файлы: `pkg/meta/redis_fek.go`, при необходимости новый общий пакет. Проверка: `go test -run 'TestRewrapSlices' ./pkg/meta/` (round-trip, legacy untouched, fail-closed на ошибке unwrap).
- [ ] 5.2 RPC `ResolveFileKey` (authz Read; FEK в ответе + запись в клиентский LRU). Файлы: `pkg/meta/pb/meta.proto`, `pkg/meta/grpc_server_fuse.go`, `pkg/meta/grpc_client_fuse.go`. Проверка: интеграционный тест resolve + deny без прав.
- [ ] 5.3 Компакция с CEK: хук `baseMeta.fileKeyResolver` + `compactionAllowed`; `CompactChunk` msg с `{fek, driveFileID, fekVersion}`; `vfs.Compact` (read с CEK → merge → new CEK → `WrapCEK`); `doCompactChunk` += `wrappedCEK` (redis — в запись, sql/tkv — nil legacy); fail-closed skip при ошибке resolve. Файлы: `pkg/meta/base.go`, `cmd/mount.go`, `pkg/vfs/compact.go`, `pkg/meta/redis.go`, `pkg/meta/sql.go`, `pkg/meta/tkv.go`. Проверка: `go test ./pkg/vfs/ -run 'TestCompact_WithCEK'`; `go test ./pkg/meta/ -run 'TestCompaction_SkippedWhenNotAllowed'`; `make test.meta.core`.
- [ ] 5.4 Clone-оркестрация: proxy (`GetFileFEK(src)` → `Clone` → `CreateFileKey(dst)` → `SetFileCrypto` → `RewrapSlices`, ошибка → `Unlink(dst)`) + render path (локально). Файлы: `pkg/meta/grpc_server_fuse.go`, `pkg/meta/render_meta.go`. Проверка: `go test -run 'TestClone_Rewrap_NoS3Rewrite|TestClone_Subset|TestClone_LegacyFile' ./pkg/meta/` — S3 keys идентичны до/после (AC-7).
- [ ] 5.5 CopyFileRange: slice-sharing путь → `RewrapSlices` на диапазоне dst (FEK из handles); data-copy путь — через reader/writer с FEK; разрезание чанка → новый CEK. Файлы: `pkg/vfs/vfs.go`, при необходимости `pkg/meta/redis_fek.go`. Проверка: интеграционный тест CopyFileRange зашифрованных файлов (read обоих файлов после операции).
- [ ] 5.6 Предусловия FR-VER-2/3: хук `baseMeta.sliceDeletable` + точки в GC. Файлы: `pkg/meta/base.go`, `pkg/meta/redis.go`. Проверка: `go test ./pkg/meta/ -run 'TestSliceDeletable_Hook'`.

### Implementation details

#### Шаг 1. RewrapSlices

**Файл:** `pkg/meta/redis_fek.go` (дополнение этапа 3)

```go
// SliceCryptoAAD — AAD для AGCK обёрток (design.md, «Межэтапные контракты»).
type SliceCryptoAAD struct {
	DriveFileID string
	FekVersion  uint32
}

// RewrapSlices переоборачивает все wrapped_cek в chunk lists inode dstIno
// из-под srcFek в-под dstFek. Данные S3 не изменяются. Legacy-записи пропускаются.
func (m *redisMeta) RewrapSlices(ctx Context, dstIno Ino, srcFek, dstFek []byte, src, dst SliceCryptoAAD) syscall.Errno {
	// Для каждого chunk list c/{dstIno}_{i} (i = 0..Length/ChunkSize):
	//   LRange → для каждой записи:
	//     если есть AGCK-хвост:
	//        cek, err := chunk.UnwrapCEK(srcFek, blob, src.DriveFileID, sliceID, src.FekVersion)
	//        if err != nil { return EIO } // fail-closed: не дописывать частично
	//        newBlob = chunk.WrapCEK(dstFek, cek, dst.DriveFileID, sliceID, dst.FekVersion)
	//     заменить запись в списке (Del + RPush, либо перезапись через LSet в txn)
	// Всё в одном txn на chunk list; по chunk lists — последовательно (атомарность на чанк).
}
```

> `chunk.UnwrapCEK/WrapCEK` — из `pkg/chunk/cek_encrypt.go` (этап 2). Проверить направление зависимости: `pkg/meta` уже импортирует `pkg/chunk`? Если нет — вынести AGCK-примитивы в общий пакет (например, `pkg/agio/crypto`) или дублировать минимальный код в meta; предпочтительно вынос. **Зафиксировать решение при реализации.**

#### Шаг 2. RPC ResolveFileKey (user path)

**`pkg/meta/pb/meta.proto`:**

```proto
rpc ResolveFileKey(ResolveFileKeyRequest) returns (ResolveFileKeyResponse);

message ResolveFileKeyRequest { MetaContext ctx = 1; uint64 inode = 2; }
message ResolveFileKeyResponse {
  uint32 errno = 1;
  bytes  fek = 2;        // plaintext, TLS only
  int32  fek_version = 3;
  bool   encrypted = 4;
}
```

**Proxy handler** (`grpc_server_fuse.go`): как Open-ветка шифрования (authz Read через interceptor — классифицировать `ResolveFileKey` → Read; path из InodePathCache; KeyManager.GetFileFEK). **Клиент** (`grpc_client_fuse.go`): `ResolveFileKey(inode)` + запись в FEK LRU.

#### Шаг 3. Компакция с CEK (D8)

**`pkg/meta/base.go`:**

```go
type baseMeta struct {
	// ...
	fileKeyResolver func(ctx Context, inode Ino) ([]byte, string, uint32, error) // fek, driveFileID, fekVersion
	sliceDeletable  func(id uint64) bool   // FR-VER-2 (nil → удаляемо)
	compactionAllowed func(inode Ino) bool // FR-VER-3 (nil → разрешена)
}
```

`compactChunk` (:2800): перед `newMsg`:

```go
if m.compactionAllowed != nil && !m.compactionAllowed(inode) {
	return 0 // версия/снепшот «заморозил» файл — пропустить (FR-VER-3)
}
var fek []byte
var driveFileID string
var fekVer uint32
if m.fileKeyResolver != nil && <inode зашифрован> {
	fek, driveFileID, fekVer, err = m.fileKeyResolver(Background(), inode)
	if err != nil {
		log.Warnf("compaction skipped for %d: key resolve: %v", inode, err) // fail-closed (решение 5.3)
		return 0
	}
}
err := m.newMsg(CompactChunk, slices, id, uint8(tierID), fek, driveFileID, fekVer)
```

**`cmd/mount.go`** (`registerMetaMsg`, :318):

```go
m.OnMsg(meta.CompactChunk, func(args ...interface{}) error {
	return vfs.Compact(*chunkConf, store, args[0].([]meta.Slice), args[1].(uint64), args[2].(uint8),
		args[3].([]byte), args[4].(string), args[5].(uint32)) // fek, driveFileID, fekVer
})
```

**`pkg/vfs/compact.go`:**

```go
func Compact(conf chunk.Config, store chunk.ChunkStore, slices []meta.Slice, id uint64, tierID uint8,
	fek []byte, driveFileID string, fekVersion uint32) (wrappedCEK []byte, err error) {
	// read: для каждого source slice с WrappedCEK → UnwrapCEK(fek, ...) → store.NewReaderWithKey(s.Id, s.Size, cek)
	// write: newCEK := rand 32B; writer := store.NewWriterWithKey(id, tierID, newCEK)
	// merge как сейчас; в конце:
	if len(newCEK) > 0 {
		wrappedCEK = chunk.WrapCEK(fek, newCEK, driveFileID, id, fekVersion) // FR-OP-7
	}
	return wrappedCEK, nil
}
```

**`doCompactChunk`** (engine interface + `redis.go:3782`, `sql.go:3864`, tkv): += аргумент `wrappedCEK []byte`; redis — кладёт в новую slice-запись (`marshalSlice(..., wrappedCEK)`); sql/tkv — nil (legacy).

**Резолверы в `cmd/mount.go`:**

```go
// user mode (grpcMeta):
metaCli.SetFileKeyResolver(func(ctx meta.Context, inode meta.Ino) ([]byte, string, uint32, error) {
	fek, ver, encrypted, st := gm.ResolveFileKey(ctx, inode) // новый клиентский метод
	if st != 0 || !encrypted { return nil, "", 0, st }
	return fek, /*driveFileID из attr*/ ..., ver, nil
})
// render mode: локальный unwrap через RenderMeta (KEK в RAM).
```

#### Шаг 4. Clone-оркестрация

**Proxy** (`grpc_server_fuse.go`, handler Clone):

```go
func (s *MetaProxyServer) Clone(ctx context.Context, req *pb.CloneRequest) (*pb.CloneResponse, error) {
	// ... существующий вызов s.meta.Clone ...
	// Если src зашифрован (attr из GetAttr(srcIno)):
	srcFek, err := s.keyManagerGetFileFEK(ctx, userID, srcPath, srcAttr) // Read на src
	if err != nil { return deny }
	// meta.Clone (копирует lists verbatim + attr с чужим wrapped_fek)
	// dst inode из ответа Clone:
	driveFileID := uuid.New().String()
	kresp, err := s.keyManager.CreateFileKey(ctx, &km.CreateFileKeyRequest{
		ActorUserId: userID, VolumeName: s.volumeName, Path: dstPath,
		DriveFileId: driveFileID, Inode: uint64(dstIno), VolumeUuid: s.volumeUUID})
	if err != nil { rollback: Unlink(dst) }
	setter.SetFileCrypto(ctx, dstIno, &FileCrypto{...kresp...})
	setter.RewrapSlices(ctx, dstIno, srcFek, kresp.PlaintextFek,
		SliceCryptoAAD{srcAttr.DriveFileID, srcAttr.FekVersion},
		SliceCryptoAAD{driveFileID, uint32(kresp.FekVersion)})
	// ошибка RewrapSlices → Unlink(dst) (target с чужими ключами = unusable)
}
```

**Render path:** та же последовательность в `cmd/render_mount.go`/`renderMeta` (FEK локально; `RewrapSlices` — метод redisMeta, доступен напрямую).

#### Шаг 5. CopyFileRange

1. Изучить реализацию `VFS.CopyFileRange` (vfs.go) и meta-путь: делит ли slice'ы или копирует данные.
2. Slice-sharing путь: после meta-операции — `RewrapSlices(dstIno, rangeChunks, fhIn.fek → fhOut.fek)` (FEK из handles; оба файла открыты). Разрезание чанка (FR-OP-5): если создаётся новый slice для части — он получает новый CEK через обычный write path (NewWriterWithKey).
3. Data-copy путь: read через reader с FEK src, write через writer с FEK dst — уже работает (этап 2).

#### Шаг 6. Тесты

**Unit:**

```go
func TestRewrapSlices_RoundTrip(t *testing.T) {
	// redis (реальный Redis по паттерну pkg/meta тестов):
	// файл с 2 чанками × 2 slice'а, wrapped_cek под FEK_A;
	// RewrapSlices(FEK_A → FEK_B);
	// UnwrapCEK(FEK_B, каждая запись) == исходные CEK; UnwrapCEK(FEK_A, ...) → ошибка.
}

func TestRewrapSlices_LegacyUntouched(t *testing.T) { ... } // записи без AGCK не меняются

func TestCompact_WithCEK(t *testing.T) {
	// vfs.Compact с 2 source slice (CEK1, CEK2) → новый slice:
	// данные == concat(plain1, plain2); wrappedCEK разворачивается FEK'ом в тот же newCEK,
	// что использовался в store (stub store проверяет).
}

func TestCompaction_SkippedWhenNotAllowed(t *testing.T) {
	// compactionAllowed = false → compactChunk не вызывает newMsg (FR-VER-3)
}

func TestSliceDeletable_Hook(t *testing.T) {
	// sliceDeletable = false → GC не удаляет slice-объект (FR-VER-2)
}
```

**Интеграционные (Redis + MinIO):**

| Тест | Сценарий | Ожидание |
|---|---|---|
| `TestClone_Rewrap_NoS3Rewrite` (FR-TEST-14, AC-7) | clone зашифрованного файла; снять список object keys до/после | keys идентичны (zero-copy); target читается своим FEK; source — своим |
| `TestClone_Subset` (FR-TEST-15) | clone диапазона (если поддерживается `--range`) или частичное CopyFileRange | target видит только скопированные данные; остальные чанки source через target недоступны |
| `TestCompaction_EncryptedFile` (FR-OP-6..8) | write много slice'ов → компакция → read | данные целы; новый slice с новым CEK под FEK файла; старые slice'ы GC-нуты |
| `TestClone_LegacyFile` | clone plaintext-файла | без re-wrap (нет AGCK), поведение как до этапа |

### Verification

```sh
cd /Users/i.obukhov/github/juicefs
go build ./... && go vet ./pkg/...
make test.meta.core
# с Redis + MinIO:
go test -run 'TestClone|TestRewrapSlices|TestCompaction' ./pkg/meta/
go test ./pkg/vfs/ -run 'TestCompact_WithCEK'
```

### Stage acceptance criteria

- [ ] FR-OP-1..8 реализованы (clone/copyfilerange zero-copy, компакция с CEK).
- [ ] AC-7: clone не переписывает S3-объекты (keys идентичны).
- [ ] Target clone'а читается своим FEK; source — своим.
- [ ] Компакция зашифрованных файлов: данные целы, новый CEK, fail-closed skip при ошибке resolve.
- [ ] FR-VER-2/3: хуки `sliceDeletable`/`compactionAllowed` работают.
- [ ] Тесты зелёные: `make test.meta.core` + интеграционные.

## 6. Stage 6 — Offline + No Residuality (форк)

**Контекст.** Требования (SRS §13, §14): при потере связи с hub клиент переходит в offline-connected (чтение из кэша, запись в journal), после timeout — disconnected; при logout/отзыве прав все plaintext-ключи обнуляются немедленно (NFR-SEC-5, AC-8/9). Что уже есть: FEK LRU в `grpcMeta` (этап 3), handle.fek (этап 2), disk cache с ciphertext (этап 2).

Факты о коде:
- Lifecycle RPC: `FlushSession` (`meta_lifecycle.proto`) — клиент шлёт session state; hub отвечает.
- `grpcMeta` — `pkg/meta/grpc_client.go`; reconnect-логика уже есть (session recovery).
- VFS handles: `dumpAllHandles`/`loadAllHandles` (`handle.go`).
- Disk cache: `bcache` — чтение из кэша в offline работает «из коробки» (кэш локальный).

Что делает этап: memclr/mlock механизмы, state machine hub, write journal, offline-чтение из кэша, fail-closed новые Open. Предусловие: этап 3 (этапы 4–5 не блокируют).

Решения этапа: design.md, «Решения Stage 6» (6.1–6.7).

### Tasks

- [ ] 6.1 `MemClear`/`MlockPage` + обнуление всех plaintext-ключей: eviction LRU, `grpcMeta.WipeKeys`, `VFS.InvalidateAllKeys` (handles → stale EIO), mlock для FEK/KEK (metric при неудаче). Файлы: `pkg/utils/memclr.go`, `pkg/meta/grpc_client.go`, `pkg/vfs/vfs.go`. Проверка: `go test ./pkg/utils/ -run TestMemClear`; `go test ./pkg/meta/ -run 'TestWipeKeys'`; `go test ./pkg/vfs/ -run 'TestInvalidateAllKeys'`.
- [ ] 6.2 State machine hub (online → offline-connected → disconnected, timeout default 15 мин) + триггеры WipeKeys: logout control file `_JFS_LOGOUT`, OIDC expiry, offline timeout. Файлы: `pkg/meta/grpc_client.go`, `pkg/vfs/internal.go`. Проверка: `go test ./pkg/meta/ -run 'TestHubStateMachine'`; интеграционный `TestLogout_CacheUnreadable` (AC-8).
- [ ] 6.3 Write journal offline-записей (append-only, replay по seq при reconnect, truncate) + проверка идемпотентности replay по семантике `redisMeta.doWrite` (результат зафиксировать в решениях). Файлы: `pkg/vfs/write_journal.go`, `pkg/vfs/writer.go`, `cmd/mount.go`. Проверка: `go test ./pkg/vfs/ -run 'TestWriteJournal'`; интеграционный `TestOfflineWrites_ReplayOnReconnect` (NFR-OFF-3).
- [ ] 6.4 Флаги + wiring (`--offline-timeout`, `SetOnWipe`, journal в cache-dir) + offline-чтение из кэша / fail-closed новые Open. Файлы: `cmd/mount.go`, `pkg/meta/grpc_client.go`. Проверка: `go build ./...`; интеграционные `TestOfflineConnected_ReadFromCache` (AC-9), `TestOfflineTimeout_Disconnected`; `make test.pkg`.

### Implementation details

#### Шаг 1. MemClear / MlockPage

**Файл:** `pkg/utils/memclr.go` (новый)

```go
package utils

import "unsafe"

// MemClear обнуляет память b. Не оптимизируется компилятором (runtime.KeepAlive).
func MemClear(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}

// MlockPage фиксирует страницу в RAM (best-effort; при EPERM — metric, не ошибка).
func MlockPage(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	addr := uintptr(unsafe.Pointer(&b[0]))
	length := uintptr(len(b)) + uintptr(pageSize-1) &^ uintptr(pageSize - 1)
	return unix.Mlock((*[1 << 30]byte)(unsafe.Pointer(addr))[:length]...)
}
```

Применение: FEK в `fekEntry` (mlock при Add, MemClear при eviction), KEK в render-mount (этап 4), CEK — не mlock'уем (короткоживущие, в page-буферах).

#### Шаг 2. WipeKeys + InvalidateAllKeys

**`pkg/meta/grpc_client.go`:**

```go
// WipeKeys обнуляет и удаляет все FEK из LRU. Вызывается при logout/отзыве/offline-timeout.
func (m *grpcMeta) WipeKeys() {
	m.fekCache.Range(func(inode uint64, e *fekEntry) bool {
		utils.MemClear(e.fek)
		m.fekCache.Remove(inode)
		return true
	})
}
```

**`pkg/vfs/vfs.go`:**

```go
// InvalidateAllKeys помечает все open handles stale: FEK обнуляется, IO → EIO до повторного Open.
func (v *VFS) InvalidateAllKeys() {
	v.handleMu.Lock()
	defer v.handleMu.Unlock()
	for _, h := range v.handles {
		if h.fek != nil {
			utils.MemClear(h.fek)
			h.fek = nil
		}
		h.stale = true // новое поле: Read/Write → EIO
	}
}
```

#### Шаг 3. State machine hub

**`pkg/meta/grpc_client.go`:**

```go
type HubState int
const (
	HubOnline HubState = iota
	HubOfflineConnected // связь потеряна, в пределах offlineTimeout
	HubDisconnected     // timeout истёк
)

// В reconnect-цикле grpcMeta:
//   ошибка RPC → state = OfflineConnected, lastFail = now
//   timer(offlineTimeout, default 15m): если state == OfflineConnected → Disconnected + WipeKeys + InvalidateAllKeys
//   успешный RPC → Online
```

**Триггеры WipeKeys:**
- **Logout control file**: `pkg/vfs/internal.go` — watcher на `<mountpoint>/_JFS_LOGOUT` (poll 1s или inotify): файл появился → `WipeKeys()` + `InvalidateAllKeys()` + удалить файл.
- **OIDC expiry**: в oidc-login callback — при истечении refresh token → WipeKeys.
- **Offline timeout**: см. выше.

#### Шаг 4. Write journal

**Файл:** `pkg/vfs/write_journal.go` (новый)

```go
// WriteJournal — append-only журнал offline-записей. Формат записи:
// seq(8B BE) | inode(8B) | off(8B) | len(4B) | data(len) | crc32(4B)
type WriteJournal struct {
	path string
	f    *os.File
	seq  uint64
	mu   sync.Mutex
}

func (j *WriteJournal) Append(inode Ino, off int64, data []byte) error { ... } // fsync после записи
func (j *WriteJournal) Replay(fn func(seq uint64, inode Ino, off int64, data []byte) error) error { ... }
func (j *WriteJournal) Truncate() error { ... } // после успешного replay
```

**Интеграция в writer:** при `state != HubOnline` — `wSlice.WriteAt` пишет и в RAM-буфер (как сейчас), и в journal. При reconnect: `Replay` → для каждой записи `m.Write(...)` (идемпотентность: `doWrite` с тем же off/len — overwrite, результат тот же; **зафиксировать в решениях**). После успешного replay — `Truncate`.

> Journal хранит **plaintext** (кэш уже ciphertext, но journal — временный, в cache-dir, права 0600). При WipeKeys journal НЕ обнуляется (данные не ключи) — но при logout-отзыве прав journal очищается (записи больше не авторизованы).

#### Шаг 5. Offline-чтение и fail-closed

- Чтение: `rSlice.ReadAt` → S3 недоступен → `bcache.load(key)` — если в кэше → decrypt → OK; если нет → EIO (fail-closed).
- Новые Open зашифрованных файлов в offline: FEK не получить → EACCES/EIO.
- Legacy-файлы (без шифрования) в offline: чтение из кэша как сейчас, новые Open — как сейчас (meta недоступен → EIO).

#### Шаг 6. Тесты

```go
func TestMemClear(t *testing.T) {
	b := []byte{1, 2, 3, 4}
	MemClear(b)
	assert.Equal(t, []byte{0, 0, 0, 0}, b)
}

func TestWipeKeys(t *testing.T) {
	// LRU с 3 FEK → WipeKeys → LRU пуст; память обнулена (проверка через copy до wipe)
}

func TestInvalidateAllKeys(t *testing.T) {
	// 2 open handles с FEK → InvalidateAllKeys → Read → EIO; повторный Open → OK
}

func TestHubStateMachine(t *testing.T) {
	// fake clock: online → ошибка → offline-connected (чтение из кэша OK) → timeout → disconnected + WipeKeys
}

func TestWriteJournal(t *testing.T) {
	// append 3 записи → replay → те же (seq, inode, off, data); truncate → replay пуст
	// повреждённый crc → ошибка replay (не применять запись)
}
```

**Интеграционные:**
- `TestLogout_CacheUnreadable` (AC-8): mount → write → close → создать `_JFS_LOGOUT` → open зашифрованного файла → EIO; legacy из кэша — тоже EIO (ключи обнулены, но legacy не шифрован — проверить: legacy читается из кэша, если кэш есть).
- `TestOfflineConnected_ReadFromCache` (AC-9): kill hub → чтение закэшированных блоков OK; новые Open → EIO.
- `TestOfflineTimeout_Disconnected`: offline > 15m (fake clock) → WipeKeys + InvalidateAllKeys.
- `TestOfflineWrites_ReplayOnReconnect` (NFR-OFF-3): offline write → reconnect → данные в S3 (ciphertext), journal очищен.

### Verification

```sh
cd /Users/i.obukhov/github/juicefs
go build ./... && go vet ./pkg/...
make test.pkg
# интеграционные:
go test -run 'TestLogout|TestOffline' ./pkg/meta/ ./pkg/vfs/
```

### Stage acceptance criteria

- [ ] NFR-SEC-5: plaintext-ключи обнуляются при logout/отзыве/offline-timeout (MemClear + mlock).
- [ ] AC-8: после logout кэш зашифрованных файлов нечитаем.
- [ ] AC-9: offline-connected — чтение из кэша работает, запись в journal.
- [ ] NFR-OFF-3: offline-записи replay'ятся при reconnect (идемпотентно).
- [ ] State machine: online → offline-connected → disconnected (timeout 15m default, флаг `--offline-timeout`).
- [ ] Тесты зелёные: `make test.pkg` + интеграционные.

## 7. Stage 7 — Revocation + STS (оба репозитория)

**Контекст.** Требования (SRS §15): отзыв прав ≤ 30s (permission generation + heartbeat), STS-креденшелы с prefix-scoped policy для S3-доступа, FEK rotation (offboarding), KEK rotation. Что уже есть: PermissionCache (Redis, TTL 5m — нужно ≤30s в production), `FlushSession` lifecycle RPC, WipeKeys/InvalidateAllKeys (этап 6), `ReloadableStorage` (форк).

Факты о коде:
- Platform: `DriveAuthorizationService.SetRole/DeleteRole/ClearRoles` (`src/internal/drive/infrastructure/adapters/auth.go`) — точки bump generation.
- Форк: `FlushSession` (`meta_lifecycle.proto`, `grpc_server_lifecycle.go`, `grpc_client.go`); `ReloadableStorage` (`pkg/chunk/reloadable_storage.go`) — swap blob'а без remount.
- STS в platform НЕТ (этап 1 — только KMS/Lockbox).

Что делает этап: permission generation + heartbeat, STS-провайдер + RPC, FEK rotation (offboarding), KEK rotation. Предусловие: этап 3 (этап 6 желателен для WipeKeys).

Решения этапа: design.md, «Решения Stage 7» (7.1–7.7).

### Tasks

- [ ] 7.1 Platform: permission generation — `INCR drivepermgen:{userID}` при SetRole/DeleteRole/ClearRoles + явная `Invalidate` PermissionCache; production TTL кэша ≤ 30s; RPC `GetPermissionGeneration`. Файлы: `src/internal/drive/infrastructure/adapters/auth.go`, `src/config/config.go`, `src/application/authz/proto/key_manager.proto`, `key_manager_service.go`. Проверка: `go test ./internal/drive/... -run 'TestDeleteRole_BumpsGeneration|TestGetPermissionGeneration'` (miniredis).
- [ ] 7.2 Platform: STS-провайдер (порты + AWS `AssumeRole` с prefix-scoped inline policy; YC — проверить возможности IAM, ограничение зафиксировать) + RPC `GetSTSCredentials` (TTL ≤ 60 мин, аудит). Файлы: `src/internal/drive/application/ports/sts.go`, `src/internal/drive/infrastructure/adapters/sts_aws.go`, `sts_yc.go`, `key_manager_service.go`. Проверка: `go test ./internal/drive/... -run 'TestGetSTSCredentials_PolicyPrefix'` (fake provider: prefix + duration).
- [ ] 7.3 Форк: heartbeat generation — `FlushSessionResponse.permission_generation`; proxy читает generation на каждом heartbeat; клиент при изменении → `WipeKeys` + `InvalidateAllKeys`. Файлы: `pkg/meta/pb/meta_lifecycle.proto`, `pkg/meta/grpc_server_lifecycle.go`, `pkg/meta/grpc_client.go`. Проверка: unit-тест «generation change → WipeKeys вызван» (mock).
- [ ] 7.4 Форк: `stsRefresher` (refresh на половине TTL, fail-safe до expiry) + `ReloadableStorage.SetCredentials` (локальный swap blob, без записи в Redis Format); флаг `--sts-enabled`; render mode — прямой cloud STS по IAM ноды. Файлы: `cmd/sts_refresher.go`, `cmd/mount.go`, `cmd/render_mount.go`. Проверка: unit-тест refresh/swap (fake clock); интеграционный `TestSTS_RefreshAndExpiry` (FR-REV-3).
- [ ] 7.5 FEK rotation: RPC proxy `RotateFileKey` (admin-gated: GetFileFEK(old) → `GenerateRotatedFileKey` platform → `SetFileCrypto` → `RewrapSlices`, fail-closed на ошибке) + platform handler с org-admin check и аудитом. Файлы: `pkg/meta/pb/meta.proto`, `pkg/meta/grpc_server_fuse.go`, `src/application/authz/proto/key_manager.proto`, `key_manager_service.go`. Проверка: интеграционный `TestFekRotation` — S3 keys не изменились, старый FEK не разворачивает новые wrapped_cek (FR-ROT-4).
- [ ] 7.6 Offboarding: batch RPC proxy `RotateFileKeysByPaths` (rate-limited, resumable с checkpoint, skipped-paths в ответе) + CLI platform `keymanager rotate-user-keys` (user → пути из PG → proxy RPC). Файлы: `pkg/meta/pb/meta.proto`, `pkg/meta/grpc_server_fuse.go`, `src/cmd/keymanager.go`. Проверка: интеграционный `TestRotation_OffboardingBatch` (10 файлов, версии bumped, данные целы; повторный запуск — 0 операций).
- [ ] 7.7 Интеграция отзыва: grant → read OK → DeleteRole → deny ≤ TTL(30s) + heartbeat(12s) + WipeKeys. Проверка: `TestRevoke_LosesFEK` (FR-TEST-10, AC-10); регрессия: форк `make test.meta.core`, platform `go test ./application/authz/... ./internal/drive/...`.

### Implementation details

#### Шаг 1. Permission generation (platform)

**`src/internal/drive/infrastructure/adapters/auth.go`:**

```go
// При SetRole/DeleteRole/ClearRoles — после успешного обновления SpiceDB:
func (s *DriveAuthorizationService) bumpPermissionGeneration(ctx context.Context, userID string) {
	// INCR drivepermgen:{userID} (Redis; key TTL 24h)
	// + s.cache.Invalidate(userID) — явное удаление записей PermissionCache
}

// GetPermissionGeneration — текущее значение (0 если нет).
func (s *DriveAuthorizationService) GetPermissionGeneration(ctx context.Context, userID string) (int64, error) { ... }
```

**Config:** `permission_cache_ttl` — production default `30s` (было 5m; dev может оставить 5m).

**Proto** (`key_manager.proto`):

```proto
rpc GetPermissionGeneration(GetPermissionGenerationRequest) returns (GetPermissionGenerationResponse);
message GetPermissionGenerationRequest { string user_id = 1; }
message GetPermissionGenerationResponse { int64 generation = 1; }
```

#### Шаг 2. STS-провайдер (platform)

**`src/internal/drive/application/ports/sts.go`:**

```go
type STSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// STSProvider — временные креденшелы для S3-доступа, ограниченные prefix'ом.
type STSProvider interface {
	GetCredentials(ctx context.Context, companyID, prefix string, ttl time.Duration) (*STSCredentials, error)
}
```

**`sts_aws.go`:** `AssumeRole` с inline policy:

```json
{
  "Effect": "Allow",
  "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket"],
  "Resource": [
    "arn:aws:s3:::BUCKET",
    "arn:aws:s3:::BUCKET/PREFIX/*"
  ]
}
```

**`sts_yc.go`:** YC IAM — проверить возможности (service account + folder-level policy; prefix-scoping в YC S3 ограничен — **зафиксировать ограничение в решениях**).

**RPC:** `GetSTSCredentials(company_id, prefix, ttl≤60m)` → аудит (`operation: 'sts'`).

#### Шаг 3. Heartbeat generation (форк)

**`pkg/meta/pb/meta_lifecycle.proto`:**

```proto
message FlushSessionResponse {
  // ... существующие ...
  int64 permission_generation = <next>; // НОВОЕ
}
```

**Proxy** (`grpc_server_lifecycle.go`): на каждом `FlushSession` — `GetPermissionGeneration(userID)` (из platform RPC или локальный Redis INCR-read) → в ответ.

**Клиент** (`grpc_client.go`):

```go
// В обработчике FlushSessionResponse:
if resp.PermissionGeneration != m.lastPermGen {
	m.lastPermGen = resp.PermissionGeneration
	m.WipeKeys()
	vfs.InvalidateAllKeys() // через callback SetOnWipe (этап 6)
}
```

#### Шаг 4. stsRefresher (форк)

**Файл:** `cmd/sts_refresher.go` (новый)

```go
type stsRefresher struct {
	provider   kmClient // GetSTSCredentials
	storage    *chunk.ReloadableStorage
	prefix     string
	ttl        time.Duration
	stop       chan struct{}
}

func (r *stsRefresher) Run(ctx context.Context) {
	ticker := time.NewTicker(r.ttl / 2) // refresh на половине TTL
	for {
		select {
		case <-ticker.C:
			creds, err := r.provider.GetSTSCredentials(ctx, ...)
			if err != nil {
				log.Warnf("sts refresh failed: %v", err) // fail-safe: старые креды до expiry
				continue
			}
			r.storage.SetCredentials(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)
		case <-r.stop:
			return
		}
	}
}
```

**`ReloadableStorage.SetCredentials`:** локальный swap blob-конфига (без записи в Redis Format — **критично**: Format общий на все клиенты).

**Render mode:** прямой cloud STS по IAM ноды (YC: instance service account → S3 folder policy; без platform RPC).

#### Шаг 5. FEK rotation

**Proxy RPC** (`meta.proto`):

```proto
rpc RotateFileKey(RotateFileKeyRequest) returns (RotateFileKeyResponse);
message RotateFileKeyRequest { MetaContext ctx = 1; uint64 inode = 2; string admin_user_id = 3; }
message RotateFileKeyResponse { uint32 errno = 1; int32 new_fek_version = 2; }
```

**Последовательность (proxy handler):**
1. Admin check (org-admin через platform).
2. `GetFileFEK(old)` — получить текущий FEK (Read на path).
3. `GenerateRotatedFileKey` (platform RPC, новый) — новый FEK + wrapped_fek v+1.
4. `SetFileCrypto(inode, newWrappedFek, driveFileID, v+1)`.
5. `RewrapSlices(inode, oldFek → newFek)` (этап 5).
6. Ошибка на любом шаге → fail-closed (откат не требуется: старые ключи ещё валидны до шага 4; после шага 4 — RewrapSlices атомарна на чанк).

**Platform:** `GenerateRotatedFileKey` — org-admin check + аудит (`operation: 'rotate'`).

#### Шаг 6. Offboarding batch

**Proxy RPC:**

```proto
rpc RotateFileKeysByPaths(RotateFileKeysByPathsRequest) returns (RotateFileKeysByPathsResponse);
message RotateFileKeysByPathsRequest {
  string admin_user_id = 1;
  repeated string paths = 2;   // до 100 на вызов
  int64 checkpoint = 3;        // для resume
}
message RotateFileKeysByPathsResponse {
  repeated string rotated_paths = 1;
  repeated string skipped_paths = 2; // с причиной (нет файла, не зашифрован, ошибка)
  int64 next_checkpoint = 3;
}
```

Rate-limit: 10 файлов/сек на компанию (конфиг). Resumable: checkpoint = индекс в списке путей.

**CLI platform** (`src/cmd/keymanager.go`): `keymanager rotate-user-keys --user <id>` — user → пути из PG (`drive_object_permission` WHERE subject=user) → батчами proxy RPC.

#### Шаг 7. Тесты

```go
// Platform:
func TestDeleteRole_BumpsGeneration(t *testing.T) {
	// miniredis: gen=0 → DeleteRole → gen=1; GetPermissionGeneration == 1
}

func TestGetSTSCredentials_PolicyPrefix(t *testing.T) {
	// fake STS provider: policy содержит prefix; TTL ≤ 60m
}

// Форк:
func TestHeartbeat_GenerationChange_WipesKeys(t *testing.T) {
	// mock FlushSessionResponse gen=1 → gen=2 → WipeKeys вызван (counter)
}

func TestSTS_RefreshAndExpiry(t *testing.T) {
	// fake clock: refresh на TTL/2; при ошибке refresh — старые креды до expiry
}

func TestFekRotation(t *testing.T) {
	// rotate → S3 keys не изменились; UnwrapCEK(oldFek, newWrappedCEK) → ошибка;
	// UnwrapCEK(newFek, ...) → OK; данные читаются
}

func TestRotation_OffboardingBatch(t *testing.T) {
	// 10 файлов → все rotated (версии bumped); повторный запуск → 0 операций (idempotent)
}

func TestRevoke_LosesFEK(t *testing.T) { // FR-TEST-10, AC-10
	// grant → read OK → DeleteRole → ≤30s: deny (PermissionCache TTL) + heartbeat → WipeKeys
}
```

### Verification

```sh
# Platform:
cd /Users/i.obukhov/ai/agio/agio-platform/src
go build ./... && go test ./application/authz/... ./internal/drive/...
# Форк:
cd /Users/i.obukhov/github/juicefs
go build ./... && make test.meta.core
# Интеграционные (Redis + MinIO):
go test -run 'TestRevoke|TestFekRotation|TestSTS' ./pkg/meta/
```

### Stage acceptance criteria

- [ ] FR-REV-1..5: отзыв ≤ 30s (TTL) + heartbeat (12s) + WipeKeys.
- [ ] AC-10: после DeleteRole пользователь теряет доступ к FEK.
- [ ] STS: prefix-scoped policy, TTL ≤ 60m, refresh на половине TTL, fail-safe до expiry.
- [ ] FEK rotation: S3 keys не изменились, старый FEK не разворачивает новые wrapped_cek (FR-ROT-4).
- [ ] Offboarding: batch rotation resumable, idempotent, rate-limited.
- [ ] Тесты зелёные: оба репозитория + интеграционные.

## 8. Stage 8 — Миграция legacy-данных (форк)

**Контекст.** Требования (SRS §16): включение шифрования на существующем томе без downtime; legacy-файлы читаются как есть (NFR-COMPAT-1); фоновая reencrypt-миграция legacy → encrypted. Что уже есть: `Format.EncryptionEnabled/KEKVersion` (этап 2), `FetchCompanyKEK` (этап 1), `RewrapSlices` (этап 5), AGDF/AGCK примитивы (этап 2).

Факты о коде:
- Chunk list: `c/{inode}_{chunkIdx}` — Redis list slice-записей.
- Slice refcount: `sliceRefs` hash (`HIncrBy`).
- GC: `cleanChunk`/`cleanInode` (redis.go) — удаление slice-объектов с refcount=0.
- `cmd/gc.go` — отдельный процесс GC.

Что делает этап: `ReencryptChunk` (атомарный swap), команда `juicefs reencrypt`, включение шифрования (`enable-encryption`), CEK rotation, rate-limiting. Предусловие: этапы 1–3.

Решения этапа: design.md, «Решения Stage 8» (8.1–8.6).

### Tasks

- [ ] 8.1 `baseMeta.ReencryptChunk` — атомарный swap chunk list одним slice (txn: DEL + RPush + refcount −1 старые/+1 новый) с обязательной проверкой неизменности list с момента Read (конфликт → повтор чанка); реализации engine (redis — полностью, sql/tkv/grpcMeta — ENOSYS/делегирование). Файлы: `pkg/meta/base.go`, `pkg/meta/interface.go`, `pkg/meta/redis.go`. Проверка: `go test -run 'TestReencryptChunk_Refcount' ./pkg/meta/`; `make test.meta.core`.
- [ ] 8.2 Команда `juicefs reencrypt` + worker: walk от root/path, FEK по Company KEK (`FetchCompanyKEK` по service identity; `--kek-file` только dev), per-chunk merge в новый slice с новым CEK, идемпотентность из состояния Redis (all-AGCK = done), resume. Файлы: `cmd/reencrypt.go`, `cmd/reencrypt_worker.go`. Проверка: интеграционные `TestReencrypt_RoundTrip`, `TestReencrypt_Idempotent`, `TestReencrypt_Resume` (Redis + MinIO).
- [ ] 8.3 Включение шифрования на томе: флаг format `--encryption-enabled` + админ-команда `juicefs enable-encryption` (`ProvisionCompanyKEK` при отсутствии KEK → `Format.EncryptionEnabled=true, KEKVersion`; идемпотентно). Файлы: `cmd/format.go`, `cmd/enable_encryption.go` (новый). Проверка: `TestLegacyReadable_AfterEnable` (FR-MIG-1, AC-6); `./juicefs enable-encryption --help`.
- [ ] 8.4 CEK rotation как режим `--file <path> --rotate-cek` (полный проход: decrypt → new CEK → swap; документация FR-ROT-7). Файлы: `cmd/reencrypt.go`, `cmd/reencrypt_worker.go`. Проверка: интеграционный тест rotate-cek (старые S3-объекты GC-кандидаты, данные читаются).
- [ ] 8.5 Rate-limiting (concurrency/IOPS/bandwidth) + доступность файла для чтения/записи во время миграции. Файлы: `cmd/reencrypt_worker.go`. Проверка: `TestRateLimit` (measured IOPS ≤ лимит×tolerance), `TestReencrypt_ConcurrentReadWrite` (FR-MIG-6).

### Implementation details

#### Шаг 1. ReencryptChunk

**`pkg/meta/base.go`:**

```go
// ReencryptChunk — атомарная замена chunk list inode/chunkIdx одним новым slice.
// Проверка неизменности: LLEN + CRC64 (или hash) list до и после — конфликт → EAGAIN.
func (m *baseMeta) ReencryptChunk(ctx Context, inode Ino, chunkIdx uint32, newSlice Slice) syscall.Errno { ... }
```

**`pkg/meta/redis.go`:**

```go
func (m *redisMeta) doReencryptChunk(ctx Context, inode Ino, chunkIdx uint32, newSlice Slice) syscall.Errno {
	key := m.chunkKey(inode, chunkIdx)
	return errno(m.txn(ctx, func(tx *redis.Tx) error {
		// 1. LRange — текущие slice'ы
		// 2. Проверка: все legacy (без AGCK) ИЛИ уже зашифрованы (идемпотентность)
		// 3. HIncrBy sliceRefs -1 для каждого старого slice
		// 4. HIncrBy sliceRefs +1 для нового slice
		// 5. DEL key + RPush key marshalSlice(newSlice)
		// Всё в одном MULTI/EXEC — атомарно.
	}, inode))
}
```

> Конфликт (list изменился между Read и txn): Redis WATCH на key → при конфликте txn падает → повтор чанка.

#### Шаг 2. Команда reencrypt

**Файл:** `cmd/reencrypt.go` (новый)

```go
func cmdReencrypt() *cli.Command {
	return &cli.Command{
		Name: "reencrypt",
		Usage: "Migrate legacy plaintext files to encrypted (background, resumable)",
		ArgsUsage: "META_URL [PATH]",
		Flags: expandFlags(
			cli.StringFlag{Name: "company-id", Usage: "company ID for KEK fetch"},
			cli.StringFlag{Name: "keymanager-service", Usage: "gRPC address (FetchCompanyKEK)"},
			cli.StringFlag{Name: "kek-file", Usage: "dev only: path to KEK file (bypasses KeyManager)"},
			cli.IntFlag{Name: "concurrency", Value: 4, Usage: "parallel chunk workers"},
			cli.Int64Flag{Name: "iops", Value: 100, Usage: "max IOPS (0 = unlimited)"},
			cli.Int64Flag{Name: "bandwidth", Value: 0, Usage: "max bandwidth MB/s (0 = unlimited)"},
			cli.BoolFlag{Name: "rotate-cek", Usage: "rotate CEK for already-encrypted files"},
		),
		Action: reencrypt,
	}
}

func reencrypt(c *cli.Context) error {
	// 1. Meta: прямой Redis (как render-mount).
	// 2. KEK: FetchCompanyKEK по service identity (или --kek-file для dev).
	// 3. Walk от root/path: Readdir → для каждого файла:
	//    - если !attr.Encrypted → reencryptFile
	//    - если attr.Encrypted && --rotate-cek → rotateCEK
	// 4. reencryptFile:
	//    a. FEK = UnwrapFEK(kek, attr.WrappedFek) (или генерация нового для legacy)
	//    b. Для каждого chunk: read slice'ы (plaintext) → merge в новый slice с новым CEK
	//       (EncryptBlock per block) → ReencryptChunk
	//    c. После всех чанков: SetFileCrypto (wrapped_fek, drive_file_id)
	// 5. Идемпотентность: если все chunk'ы уже AGCK → skip.
	// 6. Resume: walk повторяется; уже зашифрованные файлы пропускаются.
}
```

#### Шаг 3. enable-encryption

**Файл:** `cmd/enable_encryption.go` (новый)

```go
func cmdEnableEncryption() *cli.Command {
	return &cli.Command{
		Name: "enable-encryption",
		Usage: "Enable encryption on a volume (admin; idempotent)",
		ArgsUsage: "META_URL",
		Flags: expandFlags(
			cli.StringFlag{Name: "company-id", Usage: "company ID"},
			cli.StringFlag{Name: "keymanager-service", Usage: "gRPC address"},
		),
		Action: enableEncryption,
	}
}

func enableEncryption(c *cli.Context) error {
	// 1. Load format.
	// 2. Если EncryptionEnabled уже true → OK (идемпотентно).
	// 3. ProvisionCompanyKEK (если нет KEK для компании).
	// 4. Format.EncryptionEnabled = true; Format.KEKVersion = kekVersion; Save(format).
}
```

**`cmd/format.go`:** флаг `--encryption-enabled` (bool) для нового format.

#### Шаг 4. CEK rotation

Режим `--rotate-cek` в reencrypt: для зашифрованных файлов — read (decrypt с текущим CEK) → write (new CEK) → ReencryptChunk. Старые S3-объекты становятся GC-кандидатами (refcount → 0).

#### Шаг 5. Rate-limiting

```go
// В worker:
sem := make(chan struct{}, concurrency) // parallel chunk workers
iopsLimiter := rate.NewLimiter(rate.Limit(iops), iops) // golang.org/x/time/rate
bwLimiter := ... // bandwidth limiter (если > 0)
```

**Доступность во время миграции:** файл читается/пишется как есть (legacy chunk'ы — plaintext, новые — AGDF); `IsLegacyBlock` passthrough (этап 2) обеспечивает mixed-состояние.

#### Шаг 6. Тесты

```go
func TestReencryptChunk_Refcount(t *testing.T) {
	// 2 slice'а в chunk list → ReencryptChunk → 1 новый slice;
	// refcount старых → 0, нового → 1; данные целы.
}

func TestReencrypt_RoundTrip(t *testing.T) {
	// legacy файл (3 чанка) → reencrypt → все AGDF; read → данные совпадают;
	// S3: старые объекты ещё есть (GC не запущен), новые — AGDF.
}

func TestReencrypt_Idempotent(t *testing.T) {
	// reencrypt дважды → второй раз 0 операций (все AGCK).
}

func TestReencrypt_Resume(t *testing.T) {
	// reencrypt 2 из 5 чанков → kill → reencrypt → остальные 3; данные целы.
}

func TestLegacyReadable_AfterEnable(t *testing.T) { // FR-MIG-1, AC-6
	// enable-encryption → legacy файл читается (passthrough); новый файл — зашифрован.
}

func TestRateLimit(t *testing.T) {
	// iops=50 → measured IOPS ≤ 50×1.2 (tolerance).
}

func TestReencrypt_ConcurrentReadWrite(t *testing.T) { // FR-MIG-6
	// reencrypt + concurrent read/write → данные целы; нет EIO.
}
```

### Verification

```sh
cd /Users/i.obukhov/github/juicefs
go build ./... && go vet ./pkg/... cmd/...
make test.meta.core
# Интеграционные (Redis + MinIO):
go test -run 'TestReencrypt|TestLegacyReadable' ./cmd/ ./pkg/meta/
./juicefs reencrypt --help
./juicefs enable-encryption --help
```

### Stage acceptance criteria

- [ ] FR-MIG-1..6: включение шифрования без downtime; legacy читаются; фоновая миграция.
- [ ] AC-6: после enable-encryption legacy файлы читаются, новые — зашифрованы.
- [ ] ReencryptChunk: атомарный swap, refcount корректен, идемпотентность.
- [ ] Rate-limiting: IOPS/bandwidth/concurrency лимиты работают.
- [ ] CEK rotation: старые объекты GC-кандидаты, данные читаются.
- [ ] Тесты зелёные: `make test.meta.core` + интеграционные.

## 9. Stage 9 — Тестирование (оба репозитория)

**Контекст.** Требования (SRS §21): cross-repo known-answer векторы, покрытие FR-TEST-1..30, нагрузочные тесты, security-сценарии, stage-прогон, AC-чеклист. Предусловие: этапы 1–8.

### Tasks

- [ ] 9.1 Cross-repo known-answer векторы форматов AGFK/AGCK/AGDF — идентичный `vectors.json` в обоих репозиториях + `TestKnownAnswerVectors` в каждом. Файлы: `pkg/agio/testcrypto/vectors.json` (форк), `src/application/authz/service/testcrypto/vectors.json` (platform) + тесты. Проверка: `go test ./pkg/agio/... -run TestKnownAnswerVectors` (форк) и `go test ./application/authz/... -run TestKnownAnswerVectors` (platform) — оба зелёные.
- [ ] 9.2 Аудит покрытия unit-тестов FR-TEST-1..7, 19..21: таблица «FR → тест», заполнить пробелы (fail-closed KMS/Redis, plaintext не на диске включая staging/journal, identity FR-TEST-19/20/21). Файлы: тесты в `pkg/chunk/`, `pkg/meta/`, `pkg/vfs/` (форк), `src/application/authz/service/` (platform). Проверка: таблица полная; `make test.meta.core && make test.pkg`; platform unit зелёные.
- [ ] 9.3 Интеграционный suite + Makefile-target: `docker-compose.enc-test.yml` (Redis + MinIO), `make test.enc.integration` (compose up → `go test -tags=encintegration ./pkg/meta/... ./cmd/...` → down); включить существующие тесты этапов 3–8 в suite. Файлы: `docker-compose.enc-test.yml`, `Makefile`. Проверка: `make test.enc.integration` зелёный.
- [ ] 9.4 Нагрузочные тесты FR-TEST-22..25: harness `tests/load/` (N клиентов, hit/miss Open, throughput encrypted vs legacy, render RPS) + отчёт с p50/p99 и pass/fail по целям (≤5 мс / ≤100 мс / ≤10% / 0 доп. round-trips). Файлы: `tests/load/main.go`, `tests/load/report-<date>.md`. Проверка: отчёт закоммичен; недостижение целей — зафиксировано как отклонение (не блокирует).
- [ ] 9.5 Security-сценарии FR-TEST-26..30: Go-тесты + скрипты (инсайдер deny, утечка S3/Redis/Redis+S3, компрометация render-ноды) + отчёт. Файлы: `tests/security/*`, `tests/security/report.md`. Проверка: отчёт pass/fail по всем 5 сценариям; AC-3 подтверждён.
- [ ] 9.6 Полный прогон на stage-окружении (manual): реальный platform + KMS/SpiceDB/PG, owner bypass через SpiceDB, revoke с замером времени, STS prefix-policy, render-нода с IAM. Файлы: `tests/stage-runbook.md`. Проверка: чеклист выполнен, результаты записаны.
- [ ] 9.7 AC-чеклист: `tests/acceptance.md` — таблица AC-1..16 → тест(ы) → команда → статус + трассировка «каждый FR/NFR ДОЛЖЕН → тест» (AC-1). Файл: `tests/acceptance.md`. Проверка: все 16 AC со статусом и ссылкой на тест.

### Implementation details

#### Шаг 1. Known-answer векторы

**Файл:** `pkg/agio/testcrypto/vectors.json` (форк) — идентичная копия в platform (`src/application/authz/service/testcrypto/vectors.json`):

```json
{
  "AGFK": {
    "kek": "000102...3f",
    "fek": "a0a1...bf",
    "aad": {"volume_uuid": "vol-1", "company_id": "comp-1", "drive_file_id": "df-1", "inode": 42, "fek_version": 1},
    "kek_version": 1,
    "nonce": "0f1e2d...c3",
    "blob": "<base64 69 bytes>"
  },
  "AGCK": {
    "fek": "a0a1...bf",
    "cek": "c0c1...df",
    "drive_file_id": "df-1",
    "slice_id": 7,
    "fek_version": 1,
    "nonce": "...",
    "blob": "<base64 65 bytes>"
  },
  "AGDF": {
    "cek": "c0c1...df",
    "plaintext": "<base64 4MB>",
    "slice_id": 7,
    "block_index": 3,
    "nonce": "...",
    "blob": "<base64>"
  }
}
```

**Тест** (в каждом репозитории):

```go
func TestKnownAnswerVectors(t *testing.T) {
	vectors := loadVectors(t) // testcrypto/vectors.json
	// AGFK: WrapFEK(kek, fek, aad, kekVersion) == blob; UnwrapFEK → (fek, kekVersion)
	// AGCK: WrapCEK(fek, cek, dfid, sliceID, ver) == blob; UnwrapCEK → cek
	// AGDF: EncryptBlock(cek, plain, sliceID, idx) == blob; DecryptBlock → plain
}
```

> Nonce фиксирован в векторе (для детерминизма); в production — random.

#### Шаг 2. Аудит покрытия FR-TEST

Таблица `tests/fr-test-coverage.md`:

| FR-TEST | Тест | Репозиторий | Статус |
|---|---|---|---|
| FR-TEST-1 | TestEncryptBlock_RoundTrip, TestDecryptBlock_TamperTag | форк pkg/chunk | ✓ (этап 2) |
| FR-TEST-5 | TestEncryptedStore_CiphertextInS3AndCache | форк pkg/chunk | ✓ (этап 2) |
| FR-TEST-7 | TestFileCryptoDeletable | форк pkg/meta | ✓ (этап 2) |
| FR-TEST-8 | TestEncryptedFullCycle | форк pkg/meta | ✓ (этап 3) |
| FR-TEST-9 | TestUserWithoutPermission_Denied | форк pkg/meta | ✓ (этап 3) |
| FR-TEST-10 | TestRevoke_LosesFEK | форк pkg/meta | ✓ (этап 7) |
| FR-TEST-11 | TestRenderFullCycle | форк pkg/meta | ✓ (этап 4) |
| FR-TEST-12 | TestRenderMeta_CrossCompany_FailClosed | форк pkg/meta | ✓ (этап 4) |
| FR-TEST-14 | TestClone_Rewrap_NoS3Rewrite | форк pkg/meta | ✓ (этап 5) |
| FR-TEST-15 | TestClone_Subset | форк pkg/meta | ✓ (этап 5) |
| FR-TEST-18 | TestOwnerBypass | форк pkg/meta | ✓ (этап 3) |
| FR-TEST-19 | TestKeyManager_GetFileFEK_SubDirect | platform | ✓ (этап 1) |
| FR-TEST-20 | TestNonUUIDSub_Unauthenticated | форк pkg/meta | ✓ (этап 3) |
| FR-TEST-21 | TestKeyManager_UserNotInPlatform_Deny | platform | ✓ (этап 1) |

Заполнить пробелы (fail-closed KMS/Redis, plaintext не на диске включая staging/journal).

#### Шаг 3. Интеграционный suite

**Файл:** `docker-compose.enc-test.yml`:

```yaml
services:
  redis:
    image: redis:7-alpine
    ports: ["6390:6379"]
  minio:
    image: minio/minio
    command: server /data --console-address ":9001"
    ports: ["9000:9000", "9001:9001"]
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
```

**Makefile:**

```makefile
test.enc.integration:
	docker compose -f docker-compose.enc-test.yml up -d
	REDIS_ADDR=localhost:6390 S3_ENDPOINT=localhost:9000 go test -tags=encintegration ./pkg/meta/... ./cmd/... -count=1
	docker compose -f docker-compose.enc-test.yml down -v
```

#### Шаг 4. Нагрузочные тесты

**Файл:** `tests/load/main.go`:

```go
// N клиентов (FUSE mount или gRPC client):
// - FR-TEST-22: Open hit/miss latency (p50/p99) — цель ≤5 мс (hit), ≤100 мс (miss)
// - FR-TEST-23: throughput encrypted vs legacy — цель ≤10% degradation
// - FR-TEST-24: render RPS (metadata ops/sec) — цель 0 доп. round-trips
// - FR-TEST-25: concurrent read/write — нет EIO, данные целы
```

Отчёт: `tests/load/report-<date>.md` с p50/p99 и pass/fail.

#### Шаг 5. Security-сценарии

**Файлы:** `tests/security/`:

| Сценарий | Тест | Ожидание |
|---|---|---|
| FR-TEST-26: инсайдер (owner) без прав на файл | Go-тест | deny (owner bypass только для своих файлов) |
| FR-TEST-27: утечка S3 (ciphertext) | Скрипт: скачать S3-объект → попытка decrypt без KEK | fail-closed |
| FR-TEST-28: утечка Redis (wrapped_fek) | Скрипт: прочитать attr → UnwrapFEK без KEK | fail-closed |
| FR-TEST-29: утечка Redis+S3 | Скрипт: wrapped_fek + ciphertext → без KEK | fail-closed |
| FR-TEST-30: компрометация render-ноды | Скрипт: KEK ноды → доступ только к своей компании | cross-company deny |

Отчёт: `tests/security/report.md` — pass/fail по всем 5.

#### Шаг 6. Stage-прогон (manual)

**Файл:** `tests/stage-runbook.md`:

```markdown
# Stage Runbook

## Предусловия
- Platform deployed (stage) с KMS/SpiceDB/PG
- Redis + MinIO (stage)
- 2 компании (A, B), 2 пользователя (user-A, user-B), 1 render-нода

## Шаги
1. ProvisionCompanyKEK для A и B
2. enable-encryption на volume A
3. user-A: create → write → read (encrypted)
4. user-B: open файл A → deny
5. Owner A: open файл A → allow (SpiceDB bypass)
6. DeleteRole(user-A) → замер времени до deny (≤30s)
7. STS: GetSTSCredentials → S3 access с prefix
8. Render-нода: render-mount → read/write файл A
9. Revoke: WipeKeys → cache unreadable

## Результаты
| Шаг | Ожидание | Факт | Статус |
|---|---|---|---|
```

#### Шаг 7. AC-чеклист

**Файл:** `tests/acceptance.md`:

```markdown
# Acceptance Criteria

| AC | Описание | Тест(ы) | Команда | Статус |
|---|---|---|---|---|
| AC-1 | Каждый FR/NFR ДОЛЖЕН → тест | tests/fr-test-coverage.md | — | ✓ |
| AC-2 | ... | ... | ... | ... |
...
| AC-16 | Identity: sub UUID, fail-closed | TestNonUUIDSub_Unauthenticated | go test -run TestNonUUIDSub ./pkg/meta/ | ✓ |
```

### Verification

```sh
# Форк:
cd /Users/i.obukhov/github/juicefs
go test ./pkg/agio/... -run TestKnownAnswerVectors
make test.meta.core && make test.pkg
make test.enc.integration
# Platform:
cd /Users/i.obukhov/ai/agio/agio-platform/src
go test ./application/authz/... -run TestKnownAnswerVectors
# Нагрузочные + security:
go run ./tests/load/ -clients 10 -duration 5m
bash tests/security/run-all.sh
```

### Stage acceptance criteria

- [ ] Cross-repo known-answer векторы: оба репозитория зелёные.
- [ ] FR-TEST-1..30: таблица покрытия полная, пробелы заполнены.
- [ ] `make test.enc.integration` зелёный.
- [ ] Нагрузочные: отчёт с p50/p99; недостижение целей зафиксировано.
- [ ] Security: 5 сценариев pass (AC-3).
- [ ] Stage-прогон: чеклист выполнен.
- [ ] AC-чеклист: все 16 AC со статусом.

## 10. Stage 10 — Production rollout (оба репозитория + инфраструктура)

**Контекст.** Требования (SRS §22, §23): per-company KMS keys, Redis backups, аудит ≥12 мес, метрики/алерты, runbooks, audit package, rollout-план. Предусловие: этап 9, acceptance зелёный.

### Tasks

- [ ] 10.1 Per-company KMS master keys + auto-rotation (проверить поведение Decrypt старых версий у провайдера ДО включения) + CLI `keymanager provision --company <id>` (идемпотентно) + IAM least privilege matrix. Файлы: `src/cmd/keymanager.go` (platform), Terraform `yandex_kms_symmetric_key` per company + rotation_period в `agio-terraform-yc` (изменения — в его собственном openspec), `docs/ops/iam-matrix.md`. Проверка: provision idempotent (повторный запуск — 0 операций); матрица IAM задокументирована (NFR-SEC-12).
- [ ] 10.2 Redis metadata backups (внутренние автоматические бэкапы YC, retention 35 дней — SSE-KMS/S3-экспорт сервисом не поддерживается, OQ1 change `drive-crypto-infra`) + обязательный restore-тест через restore API YC. Файлы: `tests/security/redis-restore-test.sh`. Проверка: скрипт пройден (restore → mount → чтение зашифрованных файлов OK); зафиксировано в runbook (FR-REDIS-5/6).
- [ ] 10.3 Аудит: партиционирование/архивация `drive_key_access_log` ≥ 12 мес (append-only), anomaly detection worker (>N файлов за период T → security event, исключения render_node), daily reconciliation Kratos↔platform (R12). Файлы: platform worker + SQL-миграции, `docs/ops/`. Проверка: `go test ./worker/... -run TestAnomaly` (platform); reconciliation-отчёт.
- [ ] 10.4 Метрики + алерты: форк (`jfs_fek_cache_hits/misses_total`, `jfs_fek_unwrap_latency_seconds`, `jfs_sts_refresh_failures_total`, `jfs_offline_events_total`, `jfs_encrypted_blocks_read/written_total`, `jfs_reencrypt_progress`, `jfs_fek_mlock_failures_total`), platform (`keymanager_*`), Grafana-правила (p99 > 100 мс, STS failures, deny-rate spike, offline events). Файлы: код метрик в этапах 3–8 (проверить наличие), k8s values (`agio-cloud`: chart `platform-api`, values/secrets), `docs/ops/`. Проверка: `curl -s localhost:9091/metrics | grep keymanager` (platform) и `/metrics` форка содержат метрики; алерт-правила закоммичены.
- [ ] 10.5 Runbooks (8 документов): rb-kek-rotation, rb-fek-rotation, rb-cek-rotation, rb-revocation, rb-incident-kek-compromise, rb-incident-redis-loss, rb-incident-s3-leak, rb-enable-company. Файлы: `docs/ops/rb-*.md` (оба репозитория + общий README). Проверка: документы закоммичены и ревьюированы; тайминги отзыва (≤30s / ≤12s+TTL / ≤60 мин) отражены.
- [ ] 10.6 Audit package для SOC 2 / MPAA TPN: threat model с принятыми границами (T8/T9 + компенсирующие контроли), иерархия ключей и форматы как реализовано, traceability «SRS → код → тест», процедуры, допущения A1–A6. Файл: `docs/security/audit-package.md`. Проверка: таблица traceability полная (на основе `tests/acceptance.md`).
- [ ] 10.7 Rollout-план + rollback + пилотная компания: порядок (test → stage → prod per company), предусловия (все клиенты на slice-формате v2, STS, бэкапы), rollback = остановка новых зашифрованных файлов; финальный прогон AC на production-конфигурации. Файлы: `docs/ops/rollout.md`, `tests/acceptance.md`. Проверка: пилотная компания работает с шифрованием; алерты не стреляют 72 часа; все 16 AC закрыты.

### Implementation details

#### Шаг 1. Per-company KMS keys

**Terraform** (`agio-terraform-yc`, отдельный openspec-change):

```hcl
resource "yandex_kms_symmetric_key" "company_kek" {
  for_each = var.companies # map: company_id → {key_ring, name}

  key_ring_id   = each.value.key_ring_id
  name          = "drive-kek-${each.key}"
  rotation_period = "8760h" # 1 год (проверить Decrypt старых версий!)

  labels = {
    company_id = each.key
    purpose    = "drive_fek_wrap"
  }
}
```

**CLI platform** (`src/cmd/keymanager.go`):

```go
// keymanager provision --company <id>
// 1. Проверить: есть ли активный KEK для компании (PG).
// 2. Если да → OK (идемпотентно).
// 3. Если нет → ProvisionKEK (этап 1) + KMS key (Terraform apply).
```

**IAM matrix** (`docs/ops/iam-matrix.md`):

| Роль | KMS | Secret Manager | STS | S3 | Redis |
|---|---|---|---|---|---|
| platform-api | Encrypt/Decrypt (per-company key) | Get/Put (drive/kek/*) | AssumeRole | — | Read/Write (driveperm*, drivepermgen*) |
| render-node | — | — | — | Get/Put/Delete (company prefix) | Read/Write (company prefix) |
| admin | ListKeys | ListSecrets | — | — | — |

#### Шаг 2. Redis backups

**Скрипт** (`tests/security/redis-restore-test.sh`):

```bash
#!/bin/bash
# 1. Сделать бэкап YC Redis (API).
yc managed-database backup create --name test-backup --cluster-id $CLUSTER_ID
# 2. Удалить кластер (или создать новый из бэкапа).
# 3. Restore из бэкапа.
yc managed-database cluster restore --backup-id $BACKUP_ID
# 4. Mount → чтение зашифрованных файлов OK.
./juicefs mount redis://... /tmp/jfs-test
cat /tmp/jfs-test/test-file.exr > /dev/null && echo "OK" || echo "FAIL"
```

> YC Redis: SSE-KMS/S3-экспорт не поддерживается — только внутренние бэкапы (retention 35 дней). Зафиксировано в OQ1 change `drive-crypto-infra`.

#### Шаг 3. Аудит

**SQL-миграции** (platform):

```sql
-- Партиционирование drive_key_access_log по месяцам (PostgreSQL declarative partitioning)
ALTER TABLE drive_key_access_log SET WITH (autovacuum_vacuum_scale_factor = 0);
-- Архивация: >12 мес → drive_key_access_log_archive (или удаление по retention policy)
```

**Anomaly detection worker** (platform):

```go
// Каждые 5 мин: SELECT actor_id, COUNT(*) FROM drive_key_access_log
// WHERE created_at > now() - interval '1 hour' GROUP BY actor_id HAVING COUNT(*) > N
// → security event (Slack/PagerDuty), исключения: actor_type = 'render_node'
```

**Daily reconciliation** (R12):

```go
// Каждые 24ч: Kratos users vs platform users (drive_object_permission)
// Рассинхрон → alert + отчёт
```

#### Шаг 4. Метрики + алерты

**Форк** (проверить наличие в этапах 3–8, добавить если нет):

```go
// pkg/meta/grpc_client.go:
var (
	fekCacheHits   = prometheus.NewCounter(...) // jfs_fek_cache_hits_total
	fekCacheMisses = prometheus.NewCounter(...) // jfs_fek_cache_misses_total
	fekUnwrapLatency = prometheus.NewHistogram(...) // jfs_fek_unwrap_latency_seconds
)

// cmd/sts_refresher.go:
stsRefreshFailures = prometheus.NewCounter(...) // jfs_sts_refresh_failures_total

// pkg/meta/grpc_client.go (offline):
offlineEvents = prometheus.NewCounter(...) // jfs_offline_events_total

// pkg/chunk/cached_store.go:
encryptedBlocksRead   = prometheus.NewCounter(...) // jfs_encrypted_blocks_read_total
encryptedBlocksWritten = prometheus.NewCounter(...) // jfs_encrypted_blocks_written_total

// cmd/reencrypt_worker.go:
reencryptProgress = prometheus.NewGauge(...) // jfs_reencrypt_progress (0..1)

// pkg/utils/memclr.go:
mlockFailures = prometheus.NewCounter(...) // jfs_fek_mlock_failures_total
```

**Platform**: `keymanager_*` (requests, latency, deny-rate, sts-issues).

**Grafana-правила**:
- p99 FEK unwrap > 100 мс (5m) → warning
- STS refresh failures > 3 (5m) → critical
- Deny-rate spike (>2σ от baseline) → warning
- Offline events > 0 (1h) → info

#### Шаг 5. Runbooks

**Файлы** (`docs/ops/rb-*.md`):

| Runbook | Содержание |
|---|---|
| rb-kek-rotation.md | Ротация Company KEK: ProvisionKEK(v+1) → rewrap всех wrapped_fek → retire v |
| rb-fek-rotation.md | Ротация FEK файла: RotateFileKey (этап 7) |
| rb-cek-rotation.md | Ротация CEK: reencrypt --rotate-cek (этап 8) |
| rb-revocation.md | Отзыв прав: DeleteRole → ≤30s deny; WipeKeys |
| rb-incident-kek-compromise.md | Компрометация KEK: rotate KEK + rewrap все FEK |
| rb-incident-redis-loss.md | Потеря Redis: restore из бэкапа (≤35 дней) |
| rb-incident-s3-leak.md | Утечка S3: rotate CEK всех затронутых файлов |
| rb-enable-company.md | Включение шифрования для компании: provision KEK → enable-encryption → reencrypt |

#### Шаг 6. Audit package

**Файл:** `docs/security/audit-package.md`:

```markdown
# Audit Package (SOC 2 / MPAA TPN)

## Threat Model
- T8: компрометация render-ноды → KEK ноды → доступ только к своей компании (cross-company deny)
- T9: утечка S3/Redis → ciphertext/wrapped_fek без KEK → fail-closed
- Компенсирующие контроли: mlock, MemClear, STS prefix-scoped, audit log

## Key Hierarchy
KEK (KMS) → FEK (per file, AGFK) → CEK (per slice, AGCK) → blocks (AGDF)

## Traceability
| SRS | Код | Тест |
|---|---|---|
| FR-USR-1 | pkg/meta/grpc_server_fuse.go:Create | TestEncryptedFullCycle |
...

## Допущения
A1–A6 (см. design.md, «Open Questions»)
```

#### Шаг 7. Rollout-план

**Файл:** `docs/ops/rollout.md`:

```markdown
# Rollout Plan

## Порядок
1. Test: пилотная компания на test-окружении (все AC)
2. Stage: пилотная компания на stage (72h, алерты не стреляют)
3. Prod per company: enable-encryption → reencrypt (background) → мониторинг

## Предусловия
- Все клиенты на slice-формате v2 (WrappedCEK)
- STS включён (--sts-enabled)
- Redis бэкапы настроены (retention 35 дней)
- Метрики/алерты закоммичены

## Rollback
- Остановка новых зашифрованных файлов (Format.EncryptionEnabled = false)
- Legacy файлы читаются как есть (NFR-COMPAT-1)
- Reencrypt не откатывается (идемпотентно, можно продолжить)

## Финальный прогон AC
- Все 16 AC на production-конфигурации
```

### Verification

```sh
# Platform:
cd /Users/i.obukhov/ai/agio/agio-platform/src
go build ./... && go test ./worker/... -run TestAnomaly
# Форк:
cd /Users/i.obukhov/github/juicefs
go build ./... && make test.meta.core
# Metrics:
curl -s localhost:9091/metrics | grep keymanager
curl -s localhost:9567/metrics | grep jfs_fek
# Redis restore:
bash tests/security/redis-restore-test.sh
```

### Stage acceptance criteria

- [ ] Per-company KMS keys: provision idempotent, rotation настроена.
- [ ] Redis backups: restore-тест пройден (FR-REDIS-5/6).
- [ ] Аудит: партиционирование ≥12 мес, anomaly detection, reconciliation.
- [ ] Метрики + алерты: все метрики присутствуют, алерт-правила закоммичены.
- [ ] Runbooks: 8 документов закоммичены и ревьюированы.
- [ ] Audit package: traceability полная.
- [ ] Rollout: пилотная компания работает с шифрованием; алерты не стреляют 72 часа; все 16 AC закрыты.

## Definition of Done

- [ ] D.1 Форк: `go build ./...` и `go vet ./pkg/... cmd/...` — без ошибок.
- [ ] D.2 Platform: `go build ./...` и `go vet ./...` (в `src/`) — без ошибок.
- [ ] D.3 Тесты форка: `make test.meta.core`, `make test.pkg`, `make test.enc.integration` — зелёные.
- [ ] D.4 Тесты platform: `go test ./application/authz/... ./internal/drive/...` — зелёные.
- [ ] D.5 Все 16 AC закрыты (tests/acceptance.md).
- [ ] D.6 Документация: runbooks, audit package, rollout-план закоммичены.
- [ ] D.7 Метрики + алерты: закоммичены и работают в stage.
