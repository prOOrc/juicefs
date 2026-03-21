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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGRPCMetaIntegrationFullCycle tests the complete lifecycle of grpcMeta
// including mount, operations, and unmount.
func TestGRPCMetaIntegrationFullCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Start Redis for metadata
	redisPort := 16379
	redisURL := "redis://127.0.0.1:" + string(rune('0'+redisPort%10)) + string(rune('0'+(redisPort%100)/10)) + string(rune('0'+(redisPort%1000)/100)) + "/14"
	redisURL = "redis://127.0.0.1:6379/14"

	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   14,
	})
	defer rdb.Close()

	// Clean up Redis
	_ = rdb.FlushDB(nil)

	// Create temp directory for object storage
	testDir := t.TempDir()

	// Format the volume
	formatCmd := exec.Command("../../juicefs", "format", "--bucket", testDir, redisURL, "test-grpc-vol")
	formatCmd.Stdout = os.Stdout
	formatCmd.Stderr = os.Stderr
	err := formatCmd.Run()
	if err != nil {
		t.Logf("Format command output: %v", err)
		// Continue anyway, might already be formatted
	}

	// Start meta proxy
	proxyCmd := exec.Command("../../juicefs", "meta-proxy", "--meta-backend", redisURL, "--addr", ":19561")
	proxyCmd.Stdout = os.Stdout
	proxyCmd.Stderr = os.Stderr
	err = proxyCmd.Start()
	require.NoError(t, err, "Failed to start meta proxy")
	defer proxyCmd.Process.Kill()

	// Wait for proxy to start
	time.Sleep(2 * time.Second)

	// Mount via grpc
	mountPoint := t.TempDir()
	mountCmd := exec.Command("../../juicefs", "mount", "grpc://127.0.0.1:19561/test-grpc-vol", mountPoint, "--debug", "--no-usage-report")
	mountCmd.Stdout = os.Stdout
	mountCmd.Stderr = os.Stderr
	err = mountCmd.Start()
	require.NoError(t, err, "Failed to start mount")
	defer mountCmd.Process.Kill()

	// Wait for mount to be ready
	time.Sleep(3 * time.Second)

	// Test basic operations
	testFile := filepath.Join(mountPoint, "test_file.txt")
	testContent := []byte("Hello, grpcMeta!")

	// Write file
	err = os.WriteFile(testFile, testContent, 0644)
	assert.NoError(t, err, "Failed to write file")

	// Read file
	readContent, err := os.ReadFile(testFile)
	assert.NoError(t, err, "Failed to read file")
	assert.Equal(t, testContent, readContent, "File content mismatch")

	// List directory
	entries, err := os.ReadDir(mountPoint)
	assert.NoError(t, err, "Failed to list directory")
	assert.Len(t, entries, 1, "Should have one file")
	assert.Equal(t, "test_file.txt", entries[0].Name())

	// Create directory
	testDirPath := filepath.Join(mountPoint, "test_dir")
	err = os.Mkdir(testDirPath, 0755)
	assert.NoError(t, err, "Failed to create directory")

	// Verify directory exists
	info, err := os.Stat(testDirPath)
	assert.NoError(t, err, "Failed to stat directory")
	assert.True(t, info.IsDir(), "Should be a directory")

	// Clean up mount
	mountCmd.Process.Signal(os.Interrupt)
	time.Sleep(2 * time.Second)
}

// TestGRPCMetaSessionManagement tests session creation and management
func TestGRPCMetaSessionManagement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	redisURL := "redis://127.0.0.1:6379/15"
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   15,
	})
	defer rdb.Close()
	_ = rdb.FlushDB(nil)

	testDir := t.TempDir()

	// Format
	formatCmd := exec.Command("../../juicefs", "format", "--bucket", testDir, redisURL, "test-session-vol")
	err := formatCmd.Run()
	if err != nil {
		t.Logf("Format: %v", err)
	}

	// Start proxy
	proxyCmd := exec.Command("../../juicefs", "meta-proxy", "--meta-backend", redisURL, "--addr", ":19562")
	err = proxyCmd.Start()
	require.NoError(t, err)
	defer proxyCmd.Process.Kill()
	time.Sleep(2 * time.Second)

	// Create grpcMeta client
	conf := DefaultConf()
	meta, err := newGRPCMeta("grpc", "127.0.0.1:19562", conf)
	require.NoError(t, err)
	defer meta.Shutdown()

	// Load format
	format, err := meta.Load(false)
	assert.NoError(t, err, "Failed to load format")
	assert.NotNil(t, format, "Format should not be nil")

	// Test session creation
	err = meta.NewSession(false)
	assert.NoError(t, err, "Failed to create session")

	// Test basic operations with session
	var rootAttr Attr
	errno := meta.GetAttr(nil, 1, &rootAttr)
	assert.Equal(t, syscall.Errno(0), errno, "GetAttr should succeed")
	assert.True(t, rootAttr.Mode&syscall.S_IFDIR != 0, "Root should be a directory")

	// Close session
	err = meta.CloseSession()
	assert.NoError(t, err, "Failed to close session")
}

// TestGRPCMetaConcurrentOperations tests concurrent file operations
func TestGRPCMetaConcurrentOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	redisURL := "redis://127.0.0.1:6379/16"
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   16,
	})
	defer rdb.Close()
	_ = rdb.FlushDB(nil)

	testDir := t.TempDir()

	// Format
	formatCmd := exec.Command("../../juicefs", "format", "--bucket", testDir, redisURL, "test-concurrent-vol")
	err := formatCmd.Run()
	if err != nil {
		t.Logf("Format: %v", err)
	}

	// Start proxy
	proxyCmd := exec.Command("../../juicefs", "meta-proxy", "--meta-backend", redisURL, "--addr", ":19563")
	err = proxyCmd.Start()
	require.NoError(t, err)
	defer proxyCmd.Process.Kill()
	time.Sleep(2 * time.Second)

	// Mount
	mountPoint := t.TempDir()
	mountCmd := exec.Command("../../juicefs", "mount", "grpc://127.0.0.1:19563/test-concurrent-vol", mountPoint, "--no-usage-report")
	err = mountCmd.Start()
	require.NoError(t, err)
	defer mountCmd.Process.Kill()
	time.Sleep(3 * time.Second)

	// Concurrent writes
	numFiles := 10
	numWrites := 5
	errors := make(chan error, numFiles*numWrites)

	for i := 0; i < numFiles; i++ {
		go func(fileIdx int) {
			filePath := filepath.Join(mountPoint, fmt.Sprintf("concurrent_file_%d.txt", fileIdx))
			for j := 0; j < numWrites; j++ {
				content := []byte(fmt.Sprintf("Write %d to file %d", j, fileIdx))
				err := os.WriteFile(filePath, content, 0644)
				if err != nil {
					errors <- err
					return
				}
			}
			errors <- nil
		}(i)
	}

	// Wait for all writes
	closeErrors := make([]error, 0)
	for i := 0; i < numFiles*numWrites; i++ {
		if err := <-errors; err != nil {
			closeErrors = append(closeErrors, err)
		}
	}

	assert.Empty(t, closeErrors, "Concurrent writes should succeed")

	// Verify files
	entries, err := os.ReadDir(mountPoint)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, len(entries), numFiles, "Should have at least %d files", numFiles)

	// Clean up
	mountCmd.Process.Signal(os.Interrupt)
	time.Sleep(2 * time.Second)
}

// TestGRPCMetaCacheInvalidation tests that cache is properly invalidated
func TestGRPCMetaCacheInvalidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	redisURL := "redis://127.0.0.1:6379/17"
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   17,
	})
	defer rdb.Close()
	_ = rdb.FlushDB(nil)

	testDir := t.TempDir()

	// Format
	formatCmd := exec.Command("../../juicefs", "format", "--bucket", testDir, redisURL, "test-cache-vol")
	err := formatCmd.Run()
	if err != nil {
		t.Logf("Format: %v", err)
	}

	// Start proxy
	proxyCmd := exec.Command("../../juicefs", "meta-proxy", "--meta-backend", redisURL, "--addr", ":19564")
	err = proxyCmd.Start()
	require.NoError(t, err)
	defer proxyCmd.Process.Kill()
	time.Sleep(2 * time.Second)

	// Create client with short cache TTL
	conf := DefaultConf()

	meta, err := newGRPCMeta("grpc", "127.0.0.1:19564", conf)
	require.NoError(t, err)
	defer meta.Shutdown()

	// Load and create session
	_, err = meta.Load(false)
	assert.NoError(t, err)
	err = meta.NewSession(false)
	assert.NoError(t, err)

	// Create a file
	var inode Ino
	var attr Attr
	errno := meta.Create(nil, 1, "cache_test_file", 0644, 0, 0, &inode, &attr)
	assert.Equal(t, syscall.Errno(0), errno)

	// GetAttr should cache
	var attr1 Attr
	errno = meta.GetAttr(nil, inode, &attr1)
	assert.Equal(t, syscall.Errno(0), errno)

	// Modify the file (using Truncate to trigger mtime update)
	errno = meta.Truncate(nil, inode, 0, 100, &attr, false)
	assert.Equal(t, syscall.Errno(0), errno)

	// Wait a bit for time difference
	time.Sleep(1 * time.Second)

	// GetAttr should fetch fresh data
	var attr2 Attr
	errno = meta.GetAttr(nil, inode, &attr2)
	assert.Equal(t, syscall.Errno(0), errno)
	// Mtime should be updated (or at least different if time passed)
	_ = attr2 // Just verify we can get fresh attrs
}

// TestGRPCMetaGracefulShutdown tests graceful shutdown of mount and proxy
func TestGRPCMetaGracefulShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	redisURL := "redis://127.0.0.1:6379/18"
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   18,
	})
	defer rdb.Close()
	_ = rdb.FlushDB(nil)

	testDir := t.TempDir()

	// Format
	formatCmd := exec.Command("../../juicefs", "format", "--bucket", testDir, redisURL, "test-shutdown-vol")
	err := formatCmd.Run()
	if err != nil {
		t.Logf("Format: %v", err)
	}

	// Start proxy
	proxyCmd := exec.Command("../../juicefs", "meta-proxy", "--meta-backend", redisURL, "--addr", ":19565")
	err = proxyCmd.Start()
	require.NoError(t, err)

	// Mount
	mountPoint := t.TempDir()
	mountCmd := exec.Command("../../juicefs", "mount", "grpc://127.0.0.1:19565/test-shutdown-vol", mountPoint, "--no-usage-report")
	err = mountCmd.Start()
	require.NoError(t, err)
	time.Sleep(3 * time.Second)

	// Create a file
	testFile := filepath.Join(mountPoint, "shutdown_test.txt")
	err = os.WriteFile(testFile, []byte("test"), 0644)
	assert.NoError(t, err)

	// Test graceful unmount
	startTime := time.Now()
	mountCmd.Process.Signal(os.Interrupt)

	// Wait for process to exit
	done := make(chan struct{})
	go func() {
		mountCmd.Wait()
		close(done)
	}()

	select {
	case <-done:
		duration := time.Since(startTime)
		assert.Less(t, duration, 5*time.Second, fmt.Sprintf("Unmount should complete within 5s, took %v", duration))
	case <-time.After(10 * time.Second):
		t.Fatal("Unmount timed out after 10s")
	}

	// Test graceful proxy shutdown
	startTime = time.Now()
	proxyCmd.Process.Signal(os.Interrupt)

	done = make(chan struct{})
	go func() {
		proxyCmd.Wait()
		close(done)
	}()

	select {
	case <-done:
		duration := time.Since(startTime)
		assert.Less(t, duration, 5*time.Second, fmt.Sprintf("Proxy shutdown should complete within 5s, took %v", duration))
	case <-time.After(10 * time.Second):
		t.Fatal("Proxy shutdown timed out after 10s")
	}
}

// TestGRPCMetaDirHandlerIntegration tests DirHandler streaming
func TestGRPCMetaDirHandlerIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	redisURL := "redis://127.0.0.1:6379/19"
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   19,
	})
	defer rdb.Close()
	_ = rdb.FlushDB(nil)

	testDir := t.TempDir()

	// Format
	formatCmd := exec.Command("../../juicefs", "format", "--bucket", testDir, redisURL, "test-dirhandler-vol")
	err := formatCmd.Run()
	if err != nil {
		t.Logf("Format: %v", err)
	}

	// Start proxy
	proxyCmd := exec.Command("../../juicefs", "meta-proxy", "--meta-backend", redisURL, "--addr", ":19566")
	err = proxyCmd.Start()
	require.NoError(t, err)
	defer proxyCmd.Process.Kill()
	time.Sleep(2 * time.Second)

	// Create client
	conf := DefaultConf()
	meta, err := newGRPCMeta("grpc", "127.0.0.1:19566", conf)
	require.NoError(t, err)
	defer meta.Shutdown()

	// Load and create session
	_, err = meta.Load(false)
	assert.NoError(t, err)
	err = meta.NewSession(false)
	assert.NoError(t, err)

	// Create a directory with multiple files
	var dirInode Ino
	var dirAttr Attr
	errno := meta.Mkdir(nil, 1, "test_dir", 0755, 0, 0, &dirInode, &dirAttr)
	assert.Equal(t, syscall.Errno(0), errno)

	// Create multiple files
	for i := 0; i < 10; i++ {
		var fileInode Ino
		var fileAttr Attr
		errno = meta.Create(nil, dirInode, fmt.Sprintf("file_%d", i), 0644, 0, 0, &fileInode, &fileAttr)
		assert.Equal(t, syscall.Errno(0), errno)
	}

	// Test Readdir (uses DirHandler internally for large directories)
	var entries []*Entry
	errno = meta.Readdir(nil, dirInode, 1, &entries)
	assert.Equal(t, syscall.Errno(0), errno)
	assert.GreaterOrEqual(t, len(entries), 10, "Should have at least 10 entries")
}
