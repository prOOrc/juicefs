# AGENTS.md

JuiceFS is a POSIX-compatible distributed file system written in Go
(`module github.com/juicedata/juicefs`). A client coordinates a **metadata engine**
and **object storage**, exposing POSIX (FUSE) and an S3 gateway, plus Java/Hadoop
(`sdk/java/`) and Python (`sdk/python/`) SDKs.

The metadata engine has three implementation families under `pkg/meta/`:

- **Redis** — `redisMeta` (`redis.go`); also KeyDB.
- **SQL/DB** — `dbMeta` (`sql.go`): MySQL, PostgreSQL, SQLite.
- **KV (TKV)** — `kvMeta` (`tkv.go`): TiKV, etcd, BadgerDB, FoundationDB.

## Spec-Driven Workflow

Проект использует гибридный spec-driven workflow: два слоя спецификаций (ADR-001).

**Человеческий слой `specs/`** — для людей и стейкхолдеров:
- `specs/brd/` — бизнес-требования (зачем делаем)
- `specs/srs/` — системные требования (что система должна делать), стабильные ID `REQ-*`, `NFR-*`, `SEC-*`
- `specs/decisions/` — ADR (архитектурные решения)
- `specs/index.md` — реестр всех документов

**Машинный слой `openspec/`** — для AI-агентов:
- `openspec/specs/` — Source of Truth: как система работает СЕЙЧАС (capability specs, SHALL + WHEN/THEN)
- `openspec/changes/` — Delta Specs: что МЕНЯЕТСЯ (propose → apply → archive)

Процесс: Идея → BRD → SRS → `/opsx-propose` → `/opsx-apply` → `/opsx-archive`

Правила для всех агентов:
1. Не реализуй продакшен-код без готового change в `openspec/changes/`.
2. В proposal.md обязательно ссылайся на BRD/SRS/ADR по ID из `specs/index.md` (раздел "Related Requirements").
3. Не меняй утверждённые BRD/SRS без обновления документа и реестра.
4. Не добавляй новую архитектуру, внешние сервисы или модели метаданных без ADR в `specs/decisions/`.
5. Не спекай upstream JuiceFS — Source of Truth покрывает только AGIO-специфичные капабилити (Meta Proxy, outbox, шифрование).
6. Не сохраняй в спеках секреты, токены и персональные данные.
7. Инструкции агента-архитектора: `.qwen/agents/architect.md`. Описание процесса: `specs/README.md`.

Ветки: workflow-файлы (`specs/`, `openspec/`, `.qwen/skills/`, `.qwen/commands/`) живут на `main-agio`; фичевые ветки (`agio-drive-v2`, `outbox`) ребейзятся от неё.

## Repository map

Entry points: `main.go` (root) and `cmd/main.go` (CLI commands live in `cmd/`).

| Path           | Responsibility                                                  |
| -------------- | --------------------------------------------------------------- |
| `cmd/`         | CLI subcommands (`mount`, `gateway`, `sync`, `format`, `gc`, …) |
| `pkg/meta/`    | Metadata engine abstraction + per-engine implementations        |
| `pkg/vfs/`     | Virtual filesystem layer (POSIX semantics)                      |
| `pkg/fuse/`    | FUSE bindings (Linux/macOS); `pkg/winfsp/` for Windows          |
| `pkg/fs/`      | High-level filesystem logic                                     |
| `pkg/chunk/`   | Chunk / slice / block data management and caching               |
| `pkg/object/`  | Object storage backend abstraction                              |
| `pkg/gateway/` | S3-compatible gateway                                           |
| `pkg/sync/`    | Data synchronization (`juicefs sync`)                           |
| `pkg/acl/`     | POSIX ACL support                                               |
| `docs/`        | Documentation: `docs/en/` (English), `docs/zh_cn/` (Chinese)    |

## Build

```sh
make juicefs                 # standard build -> ./juicefs
STATIC=1 make juicefs        # static binary (needs musl-gcc)
make BUILD=debug all         # debug build (-N -l)
make juicefs.lite            # minimal build, most backends disabled
make juicefs.ceph            # -tags ceph
make juicefs.fdb             # -tags fdb (FoundationDB)
```

Run a local volume (SQLite metadata) for manual testing:

```sh
./juicefs format sqlite3://test.db myjfs    # create a volume
./juicefs mount  sqlite3://test.db /tmp/jfs # mount it
```

## Test

Use the smallest target that covers your change. All targets are in the `Makefile`
and mirror CI (`.github/workflows/unittests.yml`).

```sh
make test.meta.core          # ./pkg/meta/... core (no external services)
make test.meta.non-core      # Redis/PostgreSQL/etcd/KeyDB engine tests
make test.pkg                # all ./pkg/... except meta (-tags gluster)
make test.cmd                # ./cmd/... (needs MinIO env, runs under sudo)
make test.fdb                # FoundationDB tests (-tags fdb)
```

| Change scope       | Run                                                               |
| ------------------ | ----------------------------------------------------------------- |
| `pkg/meta/**`      | `make test.meta.core` (+ `test.meta.non-core` if engine-specific) |
| `cmd/**`           | `make test.cmd`                                                   |
| any other `pkg/**` | `make test.pkg`                                                   |

When fixing a bug, add a regression test that fails before the fix and passes after.

## Lint & format

- Run `go fmt` before committing.
- Linting uses `golangci-lint` per `.golangci.yml`; pre-commit pins v1.52.2 and CI runs v2.6 (see `.github/workflows/verify.yml`).
- Install hooks once with `pre-commit install` (config in `.pre-commit-config.yaml`).

## Code style & license header

- Follow [Effective Go](https://go.dev/doc/effective_go) and
  [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments).
- Every new `.go` file MUST start with the Apache 2.0 header:

```go
/*
 * JuiceFS, Copyright <year> Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
```

## Agent boundaries

- Correctness first: this is a distributed file system; small changes can affect data
  integrity or consistency. Do not invent APIs, defaults, or behavior — verify against
  the code, and don't bypass safety checks.
- Metadata-engine parity: a semantic change in `pkg/meta/` must behave identically
  across all three families (Redis, SQL/DB, KV) and be covered by their shared tests.
- Backward compatibility: keep the `dump`/`load` metadata format backward compatible,
  and forward compatible where feasible (tolerate unknown/new fields).
- When writing or reviewing changes, check that behavior changes have matching unit
  tests, and that user-facing changes update the docs.
- Keep diffs minimal and scoped; avoid unrelated refactors or formatting-only churn.
- Do not hand-edit generated code or vendored dependencies.
- Match existing conventions in the file you are editing.
- Confirm before destructive or hard-to-reverse actions (deleting files, force pushes,
  schema/data changes).

### Types and Naming
- Use `syscall.Errno` for error codes throughout the codebase
- Type aliases for meta types: `type Ino = meta.Ino`, `type Attr = meta.Attr`
- Function names: camelCase (e.g., `IsExist`, `checkInodeName`)
- Exported types/functions: PascalCase
- Private variables: lowercase with package context
- Constants: `const` for compile-time, `var` for runtime values

### Error Handling
- Use `syscall.Errno` for POSIX-style errors
- Wrap errors with context using `github.com/pkg/errors`
- Define custom errors in `pkg/utils/errors.go`
- Check errors immediately; don't defer error handling

```go
err := someFunction()
if err != nil {
    return err
}
```

### Logging
- Use the centralized logger from `pkg/utils`:
```go
var logger = utils.GetLogger("juicefs")
logger.Info("message")
logger.Error(err)
logger.Fatal(err)
```

### Testing
- Test files: `*_test.go`
- Test functions: `Test*` prefix
- Use table-driven tests for multiple cases
- Mock external dependencies with `github.com/agiledragon/gomonkey/v2`
- Include coverage annotations: `// mutate_test_job_number: N`

```go
func TestExample(t *testing.T) {
    cases := []struct {
        name     string
        input    string
        expected int
    }{
        {"case1", "input1", 1},
        {"case2", "input2", 2},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            result := function(c.input)
            if result != c.expected {
                t.Fatalf("expected %d, got %d", c.expected, result)
            }
        })
    }
}
```

### Concurrency
- Use `sync.Mutex` or `sync.RWMutex` for synchronization
- Use `sync/atomic` for atomic operations
- Use `errgroup` from `golang.org/x/sync/errgroup` for concurrent tasks
- Always handle context cancellation in goroutines

### Build Tags
Use build tags for optional features:
- `ceph`, `fdb`, `gluster` for storage backends
- `nogateway`, `nowebdav`, etc. for excluding features
- Platform-specific: `_linux.go`, `_windows.go`, `_darwin.go`

## Architecture

JuiceFS is a distributed filesystem with three primary layers:

**1. Access Layer** (`cmd/`, `pkg/fuse/`, `pkg/vfs/`, `pkg/gateway/`)
- CLI entry: `main.go` → `cmd.Main()` using `urfave/cli/v2`
- 27 CLI commands including `mount`, `gateway`, `webdav`, `sync`, `gc`, `fsck`, `dump`, `load`, `outbox`
- FUSE mount via `hanwen/go-fuse/v2`; S3-compatible gateway via MinIO; WebDAV server

**2. Metadata Layer** (`pkg/meta/`)
- `interface.go` defines the `Meta` interface — all filesystem operations go through it
- Pluggable backends: Redis, MySQL, PostgreSQL, SQLite, TiKV, BadgerDB, FoundationDB, Etcd
- `base.go` provides shared logic; each backend (e.g. `redis.go`, `sql.go`, `badger.go`) implements the `Meta` interface
- `base_test.go` contains the core test suite used across all backends

**3. Data Layer** (`pkg/chunk/`, `pkg/object/`, `pkg/compress/`)
- Files split into Chunks (64 MiB default) → Slices → Blocks (4 MiB default)
- Blocks stored in object storage; metadata stored in a metadata engine
- `pkg/object/interface.go` abstracts over S3, Azure, GCS, Alibaba OSS, Ceph, MinIO, and many more
- `pkg/compress/` provides LZ4 and Zstandard compression

**Key packages:**

| Package | Role |
|---|---|
| `pkg/meta` | Metadata interface + all backend implementations |
| `pkg/fs` | Core FS logic bridging VFS and chunk/meta layers |
| `pkg/vfs` | Virtual filesystem abstraction |
| `pkg/fuse` | FUSE mount integration |
| `pkg/chunk` | Chunk management, caching, read/write pipeline |
| `pkg/object` | Object storage abstraction layer |
| `pkg/gateway` | S3-compatible HTTP gateway |
| `pkg/sync` | Cross-filesystem synchronization |
| `pkg/acl` | Access control lists |
| `pkg/metric` | Prometheus metrics |
| `pkg/meta/events.go` | `JuiceFsEvent` struct and `EventType` constants (FileCreated, FileDeleted, FileMoved, FileWritten, DirCreated, DirDeleted) |
| `pkg/meta/redis_outbox.go` | Redis Streams outbox — writes events to stream, consumer group, retry logic, dead-letter queue |
| `pkg/meta/redis_event.go` | Helper functions that publish events from meta operations (doMknod, doRename, doUnlink, doRmdir, doWrite, doFallocate) |
| `pkg/meta/watermill_kafka.go` | Kafka publisher via Watermill + Sarama; reads from Redis stream, publishes to `juicefs.events` topic |

## Outbox Feature

Filesystem events (file/dir create, delete, move, write) are published via Redis Streams to Kafka.

**Mount flags** (writes events to Redis stream):
```bash
--outbox-enabled                  # Enable event publishing
--outbox-stream juicefs:outbox    # Redis stream name (default: juicefs:outbox)
```

**Standalone consumer** (reads stream → publishes to Kafka topic `juicefs.events`):
```bash
juicefs outbox REDIS-URL --kafka-brokers localhost:9092 [--kafka-topic juicefs.events] [--consumer-group outbox]
```

**Local testing environment** (`docker-compose.test.yml` — Kafka, Redis, TiKV, MySQL):
```bash
docker compose -f docker-compose.test.yml up -d
```

Docs: `docs/en/deployment/outbox.md`

## Common Patterns

### Context handling
```go
ctx, trace := context.WithTrace(context.Background(), "operation")
defer trace.Finish()
```

### Error type checking
```go
func IsExist(err error) bool {
    return err == syscall.EEXIST || err == syscall.EACCES || err == syscall.EPERM
}
```

### Constants definition
```go
const (
    inodeBatch     = 1 << 10
    sliceIdBatch   = 4 << 10
    maxSymCacheNum = int32(10000)
)
```

## CI/CD

Tests run in GitHub Actions (`unittests.yml`) on `ubuntu-22.04` with external services (Redis, MySQL, PostgreSQL, TiKV, Etcd, MinIO, SFTP, CIFS, NFS, Gluster, HDFS). Coverage is tracked to S3-compatible storage. Integration tests for S3 gateway and WebDAV are in `integration/Makefile`.

## Contributing
- Search existing issues before starting work
- Major features require design documents
- PRs need unit tests and maintainer approval
- Sign CLA on first contribution
