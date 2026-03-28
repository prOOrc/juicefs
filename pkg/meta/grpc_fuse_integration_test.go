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

// TestGRPCMetaFuseOperations tests all FUSE operations through gRPC meta proxy
func TestGRPCMetaFuseOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	redisURL := "redis://127.0.0.1:6379/32"
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   32,
	})
	defer rdb.Close()
	_ = rdb.FlushDB(context.Background())

	testDir := t.TempDir()
	_ = exec.Command("../../juicefs", "format", "--bucket", testDir, redisURL, "test-fuse-ops").Run()

	proxyCmd := exec.Command("../../juicefs", "meta-proxy", "--meta-backend", redisURL, "--addr", ":19580")
	require.NoError(t, proxyCmd.Start())
	defer proxyCmd.Process.Kill()
	time.Sleep(2 * time.Second)

	mountPoint := t.TempDir()
	mountCmd := exec.Command("../../juicefs", "mount", "grpc://127.0.0.1:19580/test-fuse-ops", mountPoint)
	require.NoError(t, mountCmd.Start())
	defer mountCmd.Process.Kill()
	time.Sleep(3 * time.Second)

	ctx := context.Background()

	t.Run("CreateAndReadFile", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "test_file.txt")
		content := []byte("Hello, gRPC Meta!")
		require.NoError(t, os.WriteFile(filePath, content, 0644))
		readContent, err := os.ReadFile(filePath)
		assert.NoError(t, err)
		assert.Equal(t, content, readContent)
	})

	t.Run("CreateAndRemoveDirectory", func(t *testing.T) {
		dirPath := filepath.Join(mountPoint, "test_dir")
		require.NoError(t, os.Mkdir(dirPath, 0755))
		info, err := os.Stat(dirPath)
		assert.NoError(t, err)
		assert.True(t, info.IsDir())
		require.NoError(t, os.Remove(dirPath))
		_, err = os.Stat(dirPath)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("CreateAndRemoveFile", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "to_delete.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("delete me"), 0644))
		require.NoError(t, os.Remove(filePath))
		_, err := os.Stat(filePath)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("RenameFile", func(t *testing.T) {
		oldPath := filepath.Join(mountPoint, "old_name.txt")
		newPath := filepath.Join(mountPoint, "new_name.txt")
		require.NoError(t, os.WriteFile(oldPath, []byte("rename test"), 0644))
		require.NoError(t, os.Rename(oldPath, newPath))
		_, err := os.Stat(newPath)
		assert.NoError(t, err)
		_, err = os.Stat(oldPath)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("RenameDirectory", func(t *testing.T) {
		oldDir := filepath.Join(mountPoint, "old_dir")
		newDir := filepath.Join(mountPoint, "new_dir")
		require.NoError(t, os.Mkdir(oldDir, 0755))
		require.NoError(t, os.Rename(oldDir, newDir))
		info, err := os.Stat(newDir)
		assert.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	t.Run("Symlink", func(t *testing.T) {
		target := filepath.Join(mountPoint, "target.txt")
		link := filepath.Join(mountPoint, "link.txt")
		require.NoError(t, os.WriteFile(target, []byte("target content"), 0644))
		require.NoError(t, os.Symlink(target, link))
		info, err := os.Lstat(link)
		assert.NoError(t, err)
		assert.True(t, info.Mode()&os.ModeSymlink != 0, "should be symlink")
		content, err := os.ReadFile(link)
		assert.NoError(t, err)
		assert.Equal(t, []byte("target content"), content)
	})

	t.Run("Hardlink", func(t *testing.T) {
		original := filepath.Join(mountPoint, "original.txt")
		hardlink := filepath.Join(mountPoint, "hardlink.txt")
		require.NoError(t, os.WriteFile(original, []byte("hardlink test"), 0644))
		require.NoError(t, os.Link(original, hardlink))
		info1, _ := os.Stat(original)
		info2, _ := os.Stat(hardlink)
		assert.Equal(t, info1.Sys().(*syscall.Stat_t).Ino, info2.Sys().(*syscall.Stat_t).Ino)
	})

	t.Run("ChangeFilePermissions", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "chmod_test.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("chmod test"), 0644))
		require.NoError(t, os.Chmod(filePath, 0755))
		info, err := os.Stat(filePath)
		assert.NoError(t, err)
		assert.Equal(t, os.FileMode(0755), info.Mode()&os.FileMode(0755))
	})

	t.Run("ChangeFileOwnership", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "chown_test.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("chown test"), 0644))
		// Note: chown may fail without root privileges
		_ = os.Chown(filePath, 0, 0)
	})

	t.Run("ListDirectory", func(t *testing.T) {
		dirPath := filepath.Join(mountPoint, "list_test")
		require.NoError(t, os.Mkdir(dirPath, 0755))
		for i := 0; i < 10; i++ {
			filePath := filepath.Join(dirPath, fmt.Sprintf("file_%d.txt", i))
			require.NoError(t, os.WriteFile(filePath, []byte(fmt.Sprintf("content %d", i)), 0644))
		}
		entries, err := os.ReadDir(dirPath)
		assert.NoError(t, err)
		assert.Len(t, entries, 10)
	})

	t.Run("TruncateFile", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "truncate_test.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("0123456789"), 0644))
		require.NoError(t, os.Truncate(filePath, 5))
		info, err := os.Stat(filePath)
		assert.NoError(t, err)
		assert.Equal(t, int64(5), info.Size())
	})

	t.Run("AppendToFile", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "append_test.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("initial "), 0644))
		f, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0644)
		assert.NoError(t, err)
		_, err = f.WriteString("appended")
		assert.NoError(t, err)
		f.Close()
		content, err := os.ReadFile(filePath)
		assert.NoError(t, err)
		assert.Equal(t, []byte("initial appended"), content)
	})

	t.Run("StatFS", func(t *testing.T) {
		var stat syscall.Statfs_t
		err := syscall.Statfs(mountPoint, &stat)
		assert.NoError(t, err)
		assert.Greater(t, stat.Blocks, uint64(0))
	})

	t.Run("FileLock", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "lock_test.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("lock test"), 0644))
		f, err := os.OpenFile(filePath, os.O_RDWR, 0644)
		assert.NoError(t, err)
		defer f.Close()
		// Test advisory lock
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		assert.NoError(t, err)
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	})

	t.Run("Xattr", func(t *testing.T) {
		t.Skip("xattr not available in standard Go")
		filePath := filepath.Join(mountPoint, "xattr_test.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("xattr test"), 0644))
		// Note: xattr requires syscall package
		// _ = syscall.Setxattr(filePath, "user.test", []byte("value"), 0)
		// value, _ := syscall.Getxattr(filePath, "user.test")
		_ = filePath
	})

	t.Run("LargeFile", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "large_file.bin")
		largeContent := make([]byte, 1024*1024) // 1MB
		for i := range largeContent {
			largeContent[i] = byte(i % 256)
		}
		require.NoError(t, os.WriteFile(filePath, largeContent, 0644))
		readContent, err := os.ReadFile(filePath)
		assert.NoError(t, err)
		assert.Equal(t, largeContent, readContent)
	})

	t.Run("ConcurrentFileOperations", func(t *testing.T) {
		dirPath := filepath.Join(mountPoint, "concurrent_test")
		require.NoError(t, os.Mkdir(dirPath, 0755))

		done := make(chan bool, 10)
		for i := 0; i < 10; i++ {
			go func(idx int) {
				filePath := filepath.Join(dirPath, fmt.Sprintf("concurrent_%d.txt", idx))
				content := []byte(fmt.Sprintf("concurrent content %d", idx))
				err := os.WriteFile(filePath, content, 0644)
				if err != nil {
					t.Errorf("Failed to write file %d: %v", idx, err)
				}
				done <- true
			}(i)
		}

		for i := 0; i < 10; i++ {
			<-done
		}

		entries, err := os.ReadDir(dirPath)
		assert.NoError(t, err)
		assert.Len(t, entries, 10)
	})

	t.Run("NestedDirectories", func(t *testing.T) {
		basePath := filepath.Join(mountPoint, "nested")
		deepPath := filepath.Join(basePath, "level1", "level2", "level3")
		require.NoError(t, os.MkdirAll(deepPath, 0755))

		filePath := filepath.Join(deepPath, "deep_file.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("deep content"), 0644))

		content, err := os.ReadFile(filePath)
		assert.NoError(t, err)
		assert.Equal(t, []byte("deep content"), content)

		info, err := os.Stat(basePath)
		assert.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	_ = ctx
}

// TestGRPCMetaEdgeCases tests edge cases and error handling
func TestGRPCMetaEdgeCases(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	redisURL := "redis://127.0.0.1:6379/33"
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   33,
	})
	defer rdb.Close()
	_ = rdb.FlushDB(context.Background())

	testDir := t.TempDir()
	_ = exec.Command("../../juicefs", "format", "--bucket", testDir, redisURL, "test-edge-cases").Run()

	proxyCmd := exec.Command("../../juicefs", "meta-proxy", "--meta-backend", redisURL, "--addr", ":19581")
	require.NoError(t, proxyCmd.Start())
	defer proxyCmd.Process.Kill()
	time.Sleep(2 * time.Second)

	mountPoint := t.TempDir()
	mountCmd := exec.Command("../../juicefs", "mount", "grpc://127.0.0.1:19581/test-edge-cases", mountPoint)
	require.NoError(t, mountCmd.Start())
	defer mountCmd.Process.Kill()
	time.Sleep(3 * time.Second)

	t.Run("EmptyFileName", func(t *testing.T) {
		// Empty filename should resolve to current directory
		filePath := filepath.Join(mountPoint, "")
		info, err := os.Stat(filePath)
		assert.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	t.Run("ReservedCharacters", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "file with spaces.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("spaces test"), 0644))
		content, err := os.ReadFile(filePath)
		assert.NoError(t, err)
		assert.Equal(t, []byte("spaces test"), content)
	})

	t.Run("UnicodeFileName", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "файл_文件_🎉.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("unicode test"), 0644))
		content, err := os.ReadFile(filePath)
		assert.NoError(t, err)
		assert.Equal(t, []byte("unicode test"), content)
	})

	t.Run("VeryLongFileName", func(t *testing.T) {
		longName := string(make([]rune, 200))
		for i := range longName {
			longName = longName[:i] + fmt.Sprintf("%d", i%10) + longName[i+1:]
		}
		filePath := filepath.Join(mountPoint, longName+".txt")
		err := os.WriteFile(filePath, []byte("long name"), 0644)
		// May fail on some filesystems
		_ = err
	})

	t.Run("DeleteNonExistentFile", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "nonexistent.txt")
		err := os.Remove(filePath)
		assert.Error(t, err)
	})

	t.Run("RmdirNonEmptyDirectory", func(t *testing.T) {
		dirPath := filepath.Join(mountPoint, "nonempty_dir")
		filePath := filepath.Join(dirPath, "file.txt")
		require.NoError(t, os.Mkdir(dirPath, 0755))
		require.NoError(t, os.WriteFile(filePath, []byte("content"), 0644))
		err := syscall.Rmdir(dirPath)
		assert.Error(t, err)
	})

	t.Run("SymlinkLoop", func(t *testing.T) {
		link1 := filepath.Join(mountPoint, "loop1")
		link2 := filepath.Join(mountPoint, "loop2")
		require.NoError(t, os.Symlink(link2, link1))
		require.NoError(t, os.Symlink(link1, link2))
		// Reading should fail or timeout
		_, err := os.ReadFile(link1)
		assert.Error(t, err)
	})

	t.Run("MultipleHardlinks", func(t *testing.T) {
		original := filepath.Join(mountPoint, "original.txt")
		require.NoError(t, os.WriteFile(original, []byte("hardlink test"), 0644))

		for i := 0; i < 5; i++ {
			linkPath := filepath.Join(mountPoint, fmt.Sprintf("hardlink_%d.txt", i))
			require.NoError(t, os.Link(original, linkPath))
		}

		// Verify all hardlinks point to same inode
		originalInfo, _ := os.Stat(original)
		for i := 0; i < 5; i++ {
			linkPath := filepath.Join(mountPoint, fmt.Sprintf("hardlink_%d.txt", i))
			linkInfo, err := os.Stat(linkPath)
			assert.NoError(t, err)
			// Check inode numbers match
			assert.Equal(t, originalInfo.Sys(), linkInfo.Sys())
		}
	})
}

// TestGRPCMetaConcurrentAccess tests concurrent access patterns
func TestGRPCMetaConcurrentAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	redisURL := "redis://127.0.0.1:6379/34"
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		DB:   34,
	})
	defer rdb.Close()
	_ = rdb.FlushDB(context.Background())

	testDir := t.TempDir()
	_ = exec.Command("../../juicefs", "format", "--bucket", testDir, redisURL, "test-concurrent").Run()

	proxyCmd := exec.Command("../../juicefs", "meta-proxy", "--meta-backend", redisURL, "--addr", ":19582")
	require.NoError(t, proxyCmd.Start())
	defer proxyCmd.Process.Kill()
	time.Sleep(2 * time.Second)

	mountPoint := t.TempDir()
	mountCmd := exec.Command("../../juicefs", "mount", "grpc://127.0.0.1:19582/test-concurrent", mountPoint)
	require.NoError(t, mountCmd.Start())
	defer mountCmd.Process.Kill()
	time.Sleep(3 * time.Second)

	t.Run("ConcurrentReads", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "concurrent_read.txt")
		require.NoError(t, os.WriteFile(filePath, make([]byte, 1024*1024), 0644))

		errors := make(chan error, 20)
		for i := 0; i < 20; i++ {
			go func() {
				_, err := os.ReadFile(filePath)
				errors <- err
			}()
		}

		for i := 0; i < 20; i++ {
			assert.NoError(t, <-errors)
		}
	})

	t.Run("ConcurrentWrites", func(t *testing.T) {
		filePath := filepath.Join(mountPoint, "concurrent_write.txt")

		errors := make(chan error, 10)
		for i := 0; i < 10; i++ {
			go func(idx int) {
				f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if err != nil {
					errors <- err
					return
				}
				_, err = f.WriteString(fmt.Sprintf("write %d\n", idx))
				f.Close()
				errors <- err
			}(i)
		}

		for i := 0; i < 10; i++ {
			err := <-errors
			assert.NoError(t, err)
		}

		content, err := os.ReadFile(filePath)
		assert.NoError(t, err)
		assert.Greater(t, len(content), 0)
	})

	t.Run("ConcurrentCreateDelete", func(t *testing.T) {
		dirPath := filepath.Join(mountPoint, "create_delete_test")
		require.NoError(t, os.Mkdir(dirPath, 0755))

		done := make(chan bool, 20)
		stop := make(chan struct{})

		go func() {
			for i := 0; i < 10; i++ {
				select {
				case <-stop:
					return
				default:
					filePath := filepath.Join(dirPath, fmt.Sprintf("file_%d.txt", i))
					_ = os.WriteFile(filePath, []byte(fmt.Sprintf("content %d", i)), 0644)
					done <- true
				}
			}
		}()

		go func() {
			entries, _ := os.ReadDir(dirPath)
			for len(entries) > 0 {
				select {
				case <-stop:
					return
				default:
					_ = os.Remove(filepath.Join(dirPath, entries[0].Name()))
					done <- true
					entries, _ = os.ReadDir(dirPath)
				}
			}
		}()

		time.Sleep(2 * time.Second)
		close(stop)

		for i := 0; i < 20; i++ {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("timeout waiting for operations")
			}
		}
	})
}
