# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build Commands

```bash
make juicefs          # Standard build
make juicefs.lite     # Lightweight build (excludes optional storage backends)
make juicefs.ceph     # Build with Ceph support
make juicefs.all      # Build with ceph, fdb, and gluster support
make debug            # Build debug binary with symbols
```

### Cross-compilation

```bash
make juicefs.loongarch  # LoongArch64 cross-compile
make juicefs.linux      # Linux from macOS (requires musl-cross)
make juicefs.exe        # Windows from macOS (requires mingw-w64)
```

Build tags control optional backends: `gateway`, `webdav`, `cos`, `bos`, `hdfs`, `ibmcos`, `obs`, `oss`, `qingstor`, `sftp`, `swift`, `azure`, `gs`, `ufile`, `b2`, `nfs`, `dragonfly`, `sqlite`, `mysql`, `pg`, `tikv`, `badger`, `etcd`, `cifs`.

## Test Commands

```bash
make test.meta.core       # Core metadata tests (fastest, no external services)
make test.meta.non-core   # Redis, PostgreSQL, Etcd, KeyDB tests
make test.pkg             # All pkg/ tests except meta
make test.cmd             # Command tests (requires sudo + MinIO running)
make test.fdb             # FoundationDB tests

# Run a specific test
go test -v -count=1 ./pkg/meta/ -run TestSpecificName
go test -v -count=1 ./pkg/chunk/... -run TestChunk

# Run with coverage
go test -v -cover -run TestFileName ./pkg/fs/...
```

### Random filesystem tests

```bash
make unit-random-test meta=memkv seed=123 checks=100 steps=1000
```

## Lint

```bash
golangci-lint run ./...

# Setup pre-commit hooks for automatic linting
pre-commit install
pre-commit run --all-files
```

## Architecture

JuiceFS is a distributed filesystem with three primary layers:

**1. Access Layer** (`cmd/`, `pkg/fuse/`, `pkg/vfs/`, `pkg/gateway/`)
- CLI entry: `main.go` → `cmd.Main()` using `urfave/cli/v2`
- 26 CLI commands including `mount`, `gateway`, `webdav`, `sync`, `gc`, `fsck`, `dump`, `load`
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

## Code Style

### Imports
- Standard library first, then third-party, then local packages
- Group imports with blank lines between groups
- Use aliases for same-package imports (e.g., `aclAPI "github.com/juicedata/juicefs/pkg/acl"`)

```go
import (
    "context"
    "fmt"
    "syscall"

    aclAPI "github.com/juicedata/juicefs/pkg/acl"
    "github.com/juicedata/juicefs/pkg/utils"
)
```

### Formatting
- Always run `go fmt` before committing
- Follow [Effective Go](https://go.dev/doc/effective_go) and [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)

### License Header
Every new source file must begin with the Apache 2.0 license header:
```go
/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
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

### Types and Naming
- Use `syscall.Errno` for error codes throughout the codebase
- Type aliases for meta types: `type Ino = meta.Ino`, `type Attr = meta.Attr`
- Exported types/functions: PascalCase; private variables: lowercase
- Constants: `const` for compile-time, `var` for runtime values

### Error Handling
- Use `syscall.Errno` for POSIX-style errors
- Wrap errors with context using `github.com/pkg/errors`
- Check errors immediately; don't defer error handling

### Logging
```go
var logger = utils.GetLogger("juicefs")
logger.Info("message")
logger.Error(err)
```

### Testing
- Use table-driven tests for multiple cases
- Mock external dependencies with `github.com/agiledragon/gomonkey/v2`

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
- Use `sync.Mutex` / `sync.RWMutex` for synchronization
- Use `sync/atomic` for atomic operations
- Use `errgroup` from `golang.org/x/sync/errgroup` for concurrent tasks
- Always handle context cancellation in goroutines

## Contributing
- Search existing issues before starting work
- Major features require design documents
- PRs need unit tests and maintainer approval
- Sign CLA on first contribution

## CI/CD

Tests run in GitHub Actions (`unittests.yml`) on `ubuntu-22.04` with external services (Redis, MySQL, PostgreSQL, TiKV, Etcd, MinIO, SFTP, CIFS, NFS, Gluster, HDFS). Coverage is tracked to S3-compatible storage. Integration tests for S3 gateway and WebDAV are in `integration/Makefile`.
