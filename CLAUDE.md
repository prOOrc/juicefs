# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

JuiceFS is a high-performance POSIX file system designed for cloud-native environments. It stores data in Object Storage (S3, GCS, Azure Blob, etc.) and metadata in various database engines (Redis, MySQL, PostgreSQL, TiKV, etc.). The codebase is written in Go.

## Build Commands

```bash
# Build the main binary
make juicefs

# Build with debug symbols
make debug

# Build with coverage
make juicefs.cover

# Build with all optional features (ceph, fdb, gluster)
make juicefs.all

# Cross-compile for specific architectures
make juicefs.loongarch  # Requires cross-compiler
```

## Test Commands

```bash
# Run meta package core tests
make test.meta.core

# Run meta package non-core tests (Redis Cluster, PostgreSQL, etc.)
make test.meta.non-core

# Run all pkg tests except meta
make test.pkg

# Run cmd package tests (requires sudo)
make test.cmd

# Run tests for specific package
go test -v -cover -count=1 -failfast -timeout=12m ./pkg/meta/...

# Run a single test function
go test -v -run TestRedisClient ./pkg/meta/...

# Run tests with coverage output
go test -cover -coverprofile=cover.out ./pkg/meta/...
go tool cover -html=cover.out
```

## Linting

```bash
# Run golangci-lint
golangci-lint run

# Pre-commit hooks for static analysis
pre-commit install
pre-commit run --all-files
```

## Code Architecture

### Core Packages

- **`cmd/`** - Top-level CLI commands (format, mount, gc, status, etc.)
- **`pkg/meta/`** - Metadata engine implementations and interface
  - `interface.go` - Meta interface definition
  - `base.go` - Base implementation with common functionality
  - `redis.go` - Redis metadata engine
  - `sql.go`, `sql_mysql.go`, `sql_pg.go`, `sql_sqlite.go` - SQL-based engines
  - `tkv.go`, `tkv_tikv.go`, `tkv_badger.go`, `tkv_etcd.go` - KV-based engines
  - `grpc_client_*.go` - gRPC client for meta-proxy architecture
  - `grpc_server_*.go` - gRPC server for meta-proxy
- **`pkg/fs/`** - FUSE filesystem implementation
- **`pkg/vfs/`** - Virtual filesystem (reader.go, writer.go, handle.go)
- **`pkg/chunk/`** - Chunk management (64MB chunks, slices, blocks)
- **`pkg/object/`** - Object storage integrations (s3, gcs, azure, etc.)
- **`pkg/fuse/`** - FUSE abstraction layer
- **`pkg/acl/`** - Access control lists

### Meta Interface

The `meta.Meta` interface in `pkg/meta/interface.go` defines all metadata operations. All backend implementations (Redis, SQL, TKV, gRPC) must implement this interface. Key concepts:

- **Inode** - 64-bit unique file identifier (RootInode = 1)
- **Chunk** - 64MB logical file segments
- **Slice** - Write operations, multiple slices per chunk
- **Block** - 4MB default storage units in object storage

### gRPC Meta-Proxy Architecture

The meta-proxy allows JuiceFS clients to connect via gRPC instead of directly to the metadata backend:

- **Server** (`cmd/meta_proxy.go`, `pkg/meta/grpc_server_*.go`) - Exposes Meta interface over gRPC
- **Client** (`pkg/meta/grpc_client_*.go`) - Implements Meta interface via gRPC calls
- **Protobuf** (`pkg/meta/pb/*.proto`) - gRPC service definitions split by domain:
  - `meta.proto` - Core FUSE operations
  - `meta_lifecycle.proto` - Session management
  - `meta_directory.proto` - Directory operations
  - `meta_streaming.proto` - Streaming RPCs (Readdir, ScanDeletedObject)
  - `meta_locks.proto`, `meta_xattrs.proto`, `meta_acl.proto`, `meta_token.proto`, `meta_admin.proto`

To add a new metadata backend, implement the `Meta` interface and register it in `pkg/meta/interface.go` NewClient factory.

### Data Flow

1. **Mount** (`cmd/mount.go`) → Creates FUSE filesystem (`pkg/fs/fs.go`)
2. **Read/Write** → VFS layer (`pkg/vfs/`) → Meta operations (`pkg/meta/`) + Object storage (`pkg/object/`)
3. **Metadata operations** → Backend-specific implementation (Redis/SQL/TKV/gRPC)

## Key Files for Common Tasks

| Task | Files |
|------|-------|
| Add new CLI command | `cmd/main.go`, new `cmd/<command>.go` |
| Add metadata backend | `pkg/meta/interface.go`, new `pkg/meta/<backend>.go` |
| Add object storage | `pkg/object/`, implement `Storage` interface |
| Modify FUSE behavior | `pkg/fs/fs.go`, `pkg/vfs/` |
| gRPC service changes | `pkg/meta/pb/*.proto`, then regenerate with `protoc` |

## Development Notes

- Go modules are enabled (`GO111MODULE=on`)
- Build tags control optional features (ceph, fdb, gluster, nogateway, etc.)
- Tests use `rapid` library for property-based testing in `pkg/meta/random_test.go`
- Logging via `pkg/utils/logger.go`