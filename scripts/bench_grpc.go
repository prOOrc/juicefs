package main

import (
	"context"
	"fmt"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
)

func main() {
	fmt.Println("=== gRPC Meta Benchmarks (Corrected) ===")
	fmt.Println()

	conf := meta.DefaultConf()
	m := meta.NewClient("grpc://127.0.0.1:9561/test-vol-grpc", conf)
	defer m.Shutdown()

	_, _ = m.Load(false)
	_ = m.NewSession(false)

	ctx := meta.WrapContext(context.Background())

	// GetAttr - разные inode чтобы избежать кэша
	fmt.Println("GetAttr (different inodes - bypass cache):")
	start := time.Now()
	var attr meta.Attr
	for i := 0; i < 100; i++ {
		// Используем разные inode чтобы избежать кэша
		inode := meta.Ino(1 + i) // inode 1, 2, 3, ...
		_ = m.GetAttr(ctx, inode, &attr)
	}
	elapsed := time.Since(start)
	fmt.Printf("  100 calls: %v (%v per call)\n", elapsed, elapsed/100)

	// GetAttr - тот же inode (использует кэш)
	fmt.Println("\nGetAttr (same inode - uses cache):")
	start = time.Now()
	for i := 0; i < 100; i++ {
		_ = m.GetAttr(ctx, 1, &attr) // Всегда inode 1
	}
	elapsed = time.Since(start)
	fmt.Printf("  100 calls: %v (%v per call)\n", elapsed, elapsed/100)

	// Wait for cache to expire
	fmt.Println("\nWaiting 1.1s for cache to expire...")
	time.Sleep(1100 * time.Millisecond)

	// GetAttr - после истечения кэша
	fmt.Println("\nGetAttr (after cache expiry):")
	start = time.Now()
	for i := 0; i < 100; i++ {
		_ = m.GetAttr(ctx, 1, &attr)
	}
	elapsed = time.Since(start)
	fmt.Printf("  100 calls: %v (%v per call)\n", elapsed, elapsed/100)

	// Lookup - разные файлы
	fmt.Println("\nLookup (different files):")
	var inode meta.Ino
	var lookupAttr meta.Attr
	start = time.Now()
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf(".config_%d", i) // Разные имена
		_ = m.Lookup(ctx, 1, name, &inode, &lookupAttr, false)
	}
	elapsed = time.Since(start)
	fmt.Printf("  100 calls: %v (%v per call)\n", elapsed, elapsed/100)

	// Lookup - тот же файл
	fmt.Println("\nLookup (same file - .config):")
	start = time.Now()
	for i := 0; i < 100; i++ {
		_ = m.Lookup(ctx, 1, ".config", &inode, &lookupAttr, false)
	}
	elapsed = time.Since(start)
	fmt.Printf("  100 calls: %v (%v per call)\n", elapsed, elapsed/100)
}
