# IAM matrix — agio Drive encryption (task 10.1, NFR-SEC-12)

Least-privilege matrix for the encryption data plane. YC cloud: folder-scoped
service accounts; KMS = YC KMS symmetric keys (per company), secrets = YC Lockbox,
object storage = YC S3-compatible buckets, metadata = YC Managed Redis (mget-кластер
`juicefs-meta-*`), STS = `AssumeRole`/ephemeral access keys (ADR-003).

## Roles and permissions

| Роль | KMS (per-company key) | Lockbox | STS | S3 bucket | Redis | PG (platform) | Kratos admin |
|---|---|---|---|---|---|---|---|
| **platform-api-authz-grpc** (KeyManager) | `kms.keys.encrypterDecrypter` на ключе компании (`drive-kek-<company>`) | read/write секретов `drive/kek/<company>` |_issue STS: SA static key → `AssumeRole` `agio-drive-sts` (prefix-scoped inline policy, ADR-003; YC-вариант требует `Principal` в каждой statement) | — | read/write (формат-ключи, сессии) | read/write (users, `drive_key_access_log`, `drive_security_events`, `drive_identity_reconciliation`, company crypto keys) | read identities (admin API, reconciliation) |
| **meta-proxy** (форк juicefs, user data path) | — | — | pass-through `GetSTSCredentials` (сами креды не хранит) | — | read/write volume meta (DB компании) | — | — |
| **render-node** (native JuiceFS, identity-less) | — | — | consumer: ephemeral creds от `platform-api-authz-grpc` (`GetNodeSTSCredentials`, IAM-токен ноды) | Get/Put/Delete **только** prefix `<volume>/chunks/` (inline policy STS) | read/write meta своего volume | — | — |
| **platform user** (через Meta Proxy/OIDC) | — | — | consumer: volume-scoped creds через proxy (`GetSTSCredentials`, sub из OIDC-сессии) | Get/Put/Delete только company prefix | косвенно (через proxy) | — | — |
| **company admin** (CLI `keymanager provision/rotate-user-keys`) | — (ключ создаёт Terraform) | — | — | — | — | org-admin gate на `ProvisionCompanyKEK`/rotation RPC; `admin_user_id` = OIDC sub | — |
| **terraform (CI/админ)** | `kms.admin` + `kms.keys.encrypterDecrypter` (создание/rotation `drive-kek-<company>`) | admin секретов `drive/kek/*` | `iam.serviceAccounts.admin` (SA для STS) | bucket admin (создание) | cluster admin (создание) | — | — |
| **аудитор/SOC** | `kms.keys.list` (без decrypt) | list (без read) | — | read-only | — | read-only (`drive_key_access_log`, `drive_security_events`) | — |

## Rotation

- Per-company KEK: YC KMS `rotation_period = 8760h` (1 год); `Decrypt` старых версий
  остаётся валиден у провайдера (YC KMS хранит все версии ключа — проверено на stage
  KEK v1↔v2 в 9.6b); wrapped FEK/CEK переписываются только при явной re-wrap ротации
  (rb-kek-rotation).
- STS SA static key (для YC AssumeRole): ручная ротация по rb-kek-rotation §STS;
  живёт только в k8s secret `drive-grpc-creds`.
- Redis/PG креды: managed rotation средствами YC (35-дневные бэкапы, 10.2).

## Cross-references

- Terraform (создание ключей): `agio-terraform-yc`, openspec change `drive-crypto-infra`.
- SRS: NFR-SEC-12 (least privilege), FR-KEY-*, ADR-003 (STS).
- Инцидентные процедуры: `docs/ops/rb-incident-kek-compromise.md`, `rb-revocation.md`.
