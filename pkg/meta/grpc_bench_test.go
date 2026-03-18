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

package meta

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/juicedata/juicefs/pkg/meta/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

var benchCounter uint64

const bufConnSizeBench = 1024 * 1024

type bufconnServerBench struct {
	lis    *bufconn.Listener
	grpc   *grpc.Server
	meta   Meta
	server *MetaProxyServer
}

func newBufconnServerBench(backend Meta) *bufconnServerBench {
	lis := bufconn.Listen(bufConnSizeBench)

	grpcServer := grpc.NewServer()
	server := &bufconnServerBench{
		lis:  lis,
		grpc: grpcServer,
		meta: backend,
	}

	server.server = NewMetaProxyServer(backend)
	pb.RegisterMetaServiceServer(grpcServer, server.server)

	go grpcServer.Serve(lis)

	return server
}

func (s *bufconnServerBench) dial() (*grpc.ClientConn, error) {
	return grpc.DialContext(
		context.Background(),
		"",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return s.lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)),
		grpc.WithDisableServiceConfig(),
	)
}

func (s *bufconnServerBench) stop() {
	s.grpc.Stop()
	s.lis.Close()
}

func setupGRPCBench() (*GRPCClient, *bufconnServerBench, func()) {
	conf := testConfig()

	// Clean up memkv persistence file to ensure fresh backend
	_ = os.Remove("/tmp/juicefs.memkv.setting.json")

	backend := NewClient("memkv://test", conf)
	if backend == nil {
		panic("failed to create memkv backend")
	}

	format := testGRPCBenchFormat()
	if err := backend.Init(format, false); err != nil {
		panic(fmt.Sprintf("failed to init backend: %v", err))
	}

	server := newBufconnServerBench(backend)

	conn, err := server.dial()
	if err != nil {
		panic(err)
	}

	opts := DefaultGRPCOptions()
	client, err := NewGRPCClient("", conf, opts)
	if err != nil {
		panic(err)
	}
	client.client = pb.NewMetaServiceClient(conn)

	cleanup := func() {
		client.CloseConn()
		conn.Close()
		server.stop()
	}

	return client, server, cleanup
}

func BenchmarkGRPCStatFS(b *testing.B) {
	client, _, cleanup := setupGRPCBench()
	defer cleanup()

	client.Init(testGRPCBenchFormat(), false)

	ctx := Background()
	var space, inodes uint64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = client.StatFS(ctx, RootInode, &space, &inodes, nil, nil)
	}
}

func BenchmarkGRPCCreate(b *testing.B) {
	client, _, cleanup := setupGRPCBench()
	defer cleanup()

	client.Init(testGRPCBenchFormat(), false)

	ctx := Background()
	var inode Ino
	var attr Attr
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name := fmt.Sprintf("file%d", i)
		_ = client.Create(ctx, RootInode, name, 0644, 0, 0, &inode, &attr)
		_ = client.Unlink(ctx, RootInode, name)
	}
}

func BenchmarkGRPCLookup(b *testing.B) {
	client, _, cleanup := setupGRPCBench()
	defer cleanup()

	client.Init(testGRPCBenchFormat(), false)

	ctx := Background()
	var inode Ino
	var attr Attr
	_ = client.Create(ctx, RootInode, "testfile", 0644, 0, 0, &inode, &attr)

	b.ResetTimer()
	var lookupIno Ino
	var lookupAttr Attr
	for i := 0; i < b.N; i++ {
		_ = client.Lookup(ctx, RootInode, "testfile", &lookupIno, &lookupAttr, false)
	}
}

func BenchmarkGRPCSetAttr(b *testing.B) {
	client, _, cleanup := setupGRPCBench()
	defer cleanup()

	client.Init(testGRPCBenchFormat(), false)

	ctx := Background()
	var inode Ino
	var attr Attr
	_ = client.Create(ctx, RootInode, "testfile", 0644, 0, 0, &inode, &attr)

	newAttr := &Attr{Mode: 0755, Mtime: time.Now().Unix()}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = client.SetAttr(ctx, inode, 0, 0, newAttr)
	}
}

func BenchmarkGRPCCreateReaddir(b *testing.B) {
	client, _, cleanup := setupGRPCBench()
	defer cleanup()

	client.Init(testGRPCBenchFormat(), false)

	ctx := Background()
	for i := 0; i < 10; i++ {
		var inode Ino
		var attr Attr
		_ = client.Create(ctx, RootInode, fmt.Sprintf("file%d", i), 0644, 0, 0, &inode, &attr)
	}

	b.ResetTimer()
	var entries []*Entry
	for i := 0; i < b.N; i++ {
		_ = client.Readdir(ctx, RootInode, 0, &entries)
	}
}

func BenchmarkGRPCMkdir(b *testing.B) {
	client, _, cleanup := setupGRPCBench()
	defer cleanup()

	client.Init(testGRPCBenchFormat(), false)

	ctx := Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name := fmt.Sprintf("dir%d", i)
		var inode Ino
		var attr Attr
		_ = client.Mkdir(ctx, RootInode, name, 0755, 0, 0, &inode, &attr)
		_ = client.Rmdir(ctx, RootInode, name)
	}
}

func BenchmarkGRPCLink(b *testing.B) {
	client, _, cleanup := setupGRPCBench()
	defer cleanup()

	client.Init(testGRPCBenchFormat(), false)

	ctx := Background()
	var sourceInode Ino
	var sourceAttr Attr
	_ = client.Create(ctx, RootInode, "source", 0644, 0, 0, &sourceInode, &sourceAttr)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var linkAttr Attr
		_ = client.Link(ctx, sourceInode, RootInode, fmt.Sprintf("link%d", i), &linkAttr)
	}
}

func BenchmarkGRPCLatency(b *testing.B) {
	client, _, cleanup := setupGRPCBench()
	defer cleanup()

	ctx := Background()

	b.Run("StatFS", func(b *testing.B) {
		var space, inodes uint64
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			start := time.Now()
			_ = client.StatFS(ctx, RootInode, &space, &inodes, nil, nil)
			latency := time.Since(start)
			if latency > 10*time.Millisecond {
				b.Logf("High latency: %v", latency)
			}
		}
	})

	b.Run("Create", func(b *testing.B) {
		var inode Ino
		var attr Attr
		for i := 0; i < b.N; i++ {
			name := fmt.Sprintf("file%d", i)
			start := time.Now()
			_ = client.Create(ctx, RootInode, name, 0644, 0, 0, &inode, &attr)
			latency := time.Since(start)
			_ = client.Unlink(ctx, RootInode, name)
			if latency > 10*time.Millisecond {
				b.Logf("High latency: %v", latency)
			}
		}
	})

	b.Run("Lookup", func(b *testing.B) {
		var inode Ino
		var attr Attr
		_ = client.Create(ctx, RootInode, "lookupfile", 0644, 0, 0, &inode, &attr)
		b.ResetTimer()
		var lookupIno Ino
		var lookupAttr Attr
		for i := 0; i < b.N; i++ {
			start := time.Now()
			_ = client.Lookup(ctx, RootInode, "lookupfile", &lookupIno, &lookupAttr, false)
			latency := time.Since(start)
			if latency > 10*time.Millisecond {
				b.Logf("High latency: %v", latency)
			}
		}
	})
}

func testGRPCBenchFormat() *Format {
	return &Format{
		Name:     fmt.Sprintf("grpc-bench-%d", atomic.AddUint64(&benchCounter, 1)),
		Storage:  "mem://test",
		DirStats: true,
	}
}
