# JuiceFS Agent Guidelines

## Project Overview
JuiceFS is a high-performance POSIX file system built on Redis and object storage, written in Go 1.23+. It consists of a client that coordinates object storage and metadata engines.

## Build Commands

### Build the binary
```bash
make              # Build release binary
make debug        # Build debug binary with symbols
make juicefs.lite # Build minimal binary without optional storage backends
```

### Cross-compilation
```bash
make juicefs.loongarch  # LoongArch64 cross-compile
make juicefs.linux      # Linux from macOS (requires musl-cross)
make juicefs.exe        # Windows from macOS (requires mingw-w64)
```

## Test Commands

### Run all unit tests
```bash
make test.meta.core     # Core meta tests
make test.meta.non-core # Non-core meta tests (Redis cluster, PostgreSQL, etc.)
make test.pkg           # All pkg tests except meta
make test.cmd           # Command tests
make test.fdb           # FoundationDB tests
```

### Run a single test
```bash
# Run specific test function in a package
go test -v -run TestFileName ./pkg/fs/...

# Run with coverage
go test -v -cover -run TestFileName ./pkg/fs/...

# Example: run TestFileStat only
go test -v -run TestFileStat ./pkg/fs/...
```

### Random filesystem tests
```bash
make unit-random-test meta=memkv seed=123 checks=100 steps=1000
```

## Lint Commands

```bash
# Run golangci-lint (configured in .golangci.yml)
golangci-lint run

# Setup pre-commit hooks for automatic linting
pre-commit install
pre-commit run --all-files
```

## Code Style Guidelines

### Imports
- Standard library imports first, then third-party, then local packages
- Group imports by source with blank lines between groups
- Use aliases for same-package imports (e.g., `aclAPI "github.com/juicedata/juicefs/pkg/acl"`)
- No unused imports allowed

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
- Follow [Effective Go](https://go.dev/doc/effective_go)
- Follow [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)

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

## Key Packages
- `pkg/meta`: Metadata engine implementations (Redis, SQL, KV stores)
- `pkg/fs`: FUSE filesystem implementation
- `pkg/chunk`: Data chunk management
- `pkg/object`: Object storage backends
- `pkg/utils`: Utilities (logging, errors, helpers)
- `cmd/`: CLI commands

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

## Contributing
- Search existing issues before starting work
- Major features require design documents
- PRs need unit tests and maintainer approval
- Sign CLA on first contribution