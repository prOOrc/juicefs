/*
 * JuiceFS, Copyright 2024 Juicedata, Inc.
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

package gateway

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/fs"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/vfs"
	minio "github.com/minio/minio/cmd"
	miniohash "github.com/minio/minio/pkg/hash"
)

type lockResult struct {
	lk  *jfsFLock
	err error
}

type createErrMeta struct {
	meta.Meta
	onCreate func()
}

func (m *createErrMeta) Create(ctx meta.Context, parent meta.Ino, name string, mode, cumask uint16, flags uint32, inode *meta.Ino, attr *meta.Attr) syscall.Errno {
	if m.onCreate != nil {
		onCreate := m.onCreate
		m.onCreate = nil
		onCreate()
		return syscall.ENOENT
	}
	return m.Meta.Create(ctx, parent, name, mode, cumask, flags, inode, attr)
}

func TestObjectLocks(t *testing.T) {
	var locks objectLocks
	var wg sync.WaitGroup
	var count int
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				unlock := locks.lock("bucket", "key")
				previous := count
				runtime.Gosched()
				count = previous + 1
				unlock()
			}
		}()
	}
	wg.Wait()
	if count != 1600 || len(locks.locks) != 0 {
		t.Fatalf("count=%d, remaining locks=%d", count, len(locks.locks))
	}

	unlock := locks.lock("bucket", "key")
	defer unlock()
	done := make(chan struct{})
	go func() {
		for _, key := range [][2]string{{"bucket", "other"}, {"other", "key"}} {
			release := locks.lock(key[0], key[1])
			release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated keys blocked")
	}
}

func TestObjectWriteSerialization(t *testing.T) {
	n, _, bucket := newTestGateway(t, Config{})
	ctx := context.Background()
	data := []byte("object data")
	reader := func() *minio.PutObjReader {
		return newTestPutObjReader(t, bytes.NewReader(data), data)
	}
	if _, err := n.PutObject(ctx, bucket, "source", reader(), minio.ObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	uploadID, err := n.NewMultipartUpload(ctx, bucket, "target", minio.ObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	part, err := n.PutObjectPart(ctx, bucket, "target", uploadID, 1, reader(), minio.ObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		wait  bool
		write func() error
	}{
		{"put", true, func() error {
			_, err := n.PutObject(ctx, bucket, "target", reader(), minio.ObjectOptions{})
			return err
		}},
		{"different key", false, func() error {
			_, err := n.PutObject(ctx, bucket, "unrelated", reader(), minio.ObjectOptions{})
			return err
		}},
		{"copy", true, func() error {
			_, err := n.CopyObject(ctx, bucket, "source", bucket, "target", minio.ObjectInfo{}, minio.ObjectOptions{}, minio.ObjectOptions{})
			return err
		}},
		{"complete", true, func() error {
			_, err := n.CompleteMultipartUpload(ctx, bucket, "target", uploadID, []minio.CompletePart{{PartNumber: 1, ETag: part.ETag}}, minio.ObjectOptions{})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gated := &gatedReader{reader: bytes.NewReader(data), started: make(chan struct{}), release: make(chan struct{})}
			putReader := newTestPutObjReader(t, gated, data)
			release := sync.OnceFunc(func() { close(gated.release) })
			var wg sync.WaitGroup
			defer func() { release(); wg.Wait() }()
			start := func(fn func() error) <-chan error {
				done := make(chan error, 1)
				wg.Add(1)
				go func() { defer wg.Done(); done <- fn() }()
				return done
			}
			first := start(func() error {
				_, err := n.PutObject(ctx, bucket, "target", putReader, minio.ObjectOptions{})
				return err
			})
			select {
			case <-gated.started:
			case <-time.After(2 * time.Second):
				t.Fatal("first PUT did not start")
			}
			second := start(tc.write)
			if tc.wait {
				select {
				case err := <-second:
					t.Fatalf("write did not wait for same-key PUT: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
				release()
			}
			waitObjectWrite(t, second)
			release()
			waitObjectWrite(t, first)
		})
	}
}

func waitObjectWrite(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("object write did not finish")
	}
}

func TestGatewayLock(t *testing.T) {
	m := meta.NewClient("memkv://", nil)
	format := &meta.Format{
		Name:      "test",
		BlockSize: 4096,
		Capacity:  1 << 30,
		DirStats:  true,
	}
	_ = m.Init(format, true)
	var conf = vfs.Config{
		Meta: meta.DefaultConf(),
		Chunk: &chunk.Config{
			BlockSize:   format.BlockSize << 10,
			MaxUpload:   1,
			MaxDownload: 200,
			BufferSize:  100 << 20,
		},
		DirEntryTimeout: time.Millisecond * 100,
		EntryTimeout:    time.Millisecond * 100,
		AttrTimeout:     time.Millisecond * 100,
	}
	objStore, _ := object.CreateStorage("mem", "", "", "", "")
	store := chunk.NewCachedStore(objStore, *conf.Chunk, nil)
	jfs, err := fs.NewFileSystem(&conf, m, store, nil)
	if err != nil {
		t.Fatalf("initialize  failed: %s", err)
	}
	jfsObj := &jfsObjects{fs: jfs, conf: &conf, listPool: minio.NewTreeWalkPool(time.Minute * 30), gConf: &Config{Umask: 022}, nsMutex: minio.NewNSLock(false)}
	mctx = meta.NewContext(uint32(os.Getpid()), uint32(os.Getuid()), []uint32{uint32(os.Getgid())})
	if err := jfs.Mkdir(mctx, minio.MinioMetaBucket, 0777, 022); err != 0 {
		t.Fatalf("mkdir failed: %s", err)
	}

	rwLocker := jfsObj.NewNSLock(minio.MinioMetaBucket, minio.MinioMetaLockFile)

	if _, err := rwLocker.GetLock(context.Background(), minio.NewDynamicTimeout(2*time.Second, 1*time.Second)); err != nil {
		t.Fatalf("get lock failed: %s", err)
	}
	if _, err := rwLocker.GetLock(context.Background(), minio.NewDynamicTimeout(2*time.Second, 1*time.Second)); !errors.As(err, &minio.OperationTimedOut{}) {
		t.Fatalf("GetLock should return timeout error: %s", err)
	}
	rwLocker.Unlock()

	if _, err := rwLocker.GetRLock(context.Background(), minio.NewDynamicTimeout(2*time.Second, 1*time.Second)); err != nil {
		t.Fatalf("get lock failed: %s", err)
	}
	if _, err := rwLocker.GetRLock(context.Background(), minio.NewDynamicTimeout(2*time.Second, 1*time.Second)); err != nil {
		t.Fatalf("GetRLock should return nil: %s", err)
	}
	rwLocker.RUnlock()
	rwLocker.RUnlock()

	if _, err := rwLocker.GetLock(context.Background(), minio.NewDynamicTimeout(2*time.Second, 1*time.Second)); err != nil {
		t.Fatalf("get lock failed: %s", err)
	}
	if _, err := rwLocker.GetRLock(context.Background(), minio.NewDynamicTimeout(2*time.Second, 1*time.Second)); !errors.As(err, &minio.OperationTimedOut{}) {
		t.Fatalf("GetRLock should return timeout error: %s", err)
	}
	rwLocker.Unlock()

	if _, err := rwLocker.GetRLock(context.Background(), minio.NewDynamicTimeout(2*time.Second, 1*time.Second)); err != nil {
		t.Fatalf("GetRLock failed: %s", err)
	}
	if _, err := rwLocker.GetLock(context.Background(), minio.NewDynamicTimeout(2*time.Second, 1*time.Second)); !errors.As(err, &minio.OperationTimedOut{}) {
		t.Fatalf("GetRLock should return timeout error: %s", err)
	}
	rwLocker.RUnlock()

}

func TestBucketLifecycleLockCancellation(t *testing.T) {
	jfsObj, _, _ := newTestGateway(t, Config{MultiBucket: true})
	if err := jfsObj.MakeBucketWithLocation(context.Background(), minio.MinioMetaBucket, minio.BucketOptions{}); err != nil {
		t.Fatalf("create metadata bucket: %s", err)
	}

	lk, err := jfsObj.lockBucket(context.Background())
	if err != nil {
		t.Fatalf("lock bucket: %s", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan lockResult, 1)
	go func() {
		lk, err := jfsObj.lockBucket(ctx)
		result <- lockResult{lk, err}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case r := <-result:
		if r.lk != nil {
			r.lk.Unlock()
		}
		if !errors.Is(r.err, context.Canceled) {
			lk.Unlock()
			t.Fatalf("lock should stop after context cancellation, got %v", r.err)
		}
	case <-time.After(2 * time.Second):
		lk.Unlock()
		t.Fatal("lock did not stop after context cancellation")
	}
	lk.Unlock()

	next, err := jfsObj.lockBucket(context.Background())
	if err != nil {
		t.Fatalf("lock after canceled waiter: %s", err)
	}
	next.Unlock()
}

func TestBucketLifecycleLockAcrossGateways(t *testing.T) {
	metaURL := "sqlite3://" + filepath.Join(t.TempDir(), "gateway-lock.db")
	format := &meta.Format{Name: "test", BlockSize: 4096, Capacity: 1 << 30, DirStats: true}
	m1 := meta.NewClient(metaURL, nil)
	if err := m1.Init(format, true); err != nil {
		t.Fatalf("init metadata: %s", err)
	}
	if err := m1.NewSession(true); err != nil {
		t.Fatalf("create first metadata session: %s", err)
	}
	defer m1.CloseSession()
	m2 := meta.NewClient(metaURL, nil)
	if _, err := m2.Load(true); err != nil {
		t.Fatalf("load metadata: %s", err)
	}
	if err := m2.NewSession(true); err != nil {
		t.Fatalf("create second metadata session: %s", err)
	}
	defer m2.CloseSession()

	g1, _ := newTestGatewayWithMeta(t, m1, format, Config{MultiBucket: true})
	g2, _ := newTestGatewayWithMeta(t, m2, format, Config{MultiBucket: true})
	if err := g1.MakeBucketWithLocation(context.Background(), minio.MinioMetaBucket, minio.BucketOptions{}); err != nil {
		t.Fatalf("create metadata bucket: %s", err)
	}

	lk, err := g1.lockBucket(context.Background())
	if err != nil {
		t.Fatalf("lock bucket from first gateway: %s", err)
	}
	result := make(chan lockResult, 1)
	go func() {
		lk, err := g2.lockBucket(context.Background())
		result <- lockResult{lk, err}
	}()
	select {
	case r := <-result:
		if r.err == nil {
			r.lk.Unlock()
		}
		lk.Unlock()
		t.Fatalf("lock from second gateway should block, got %v", r.err)
	case <-time.After(100 * time.Millisecond):
	}
	lk.Unlock()
	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("second gateway should acquire lock after unlock: %s", r.err)
		}
		r.lk.Unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("second gateway did not acquire lock after unlock")
	}
}

func TestObjectCommitAfterBucketDeleted(t *testing.T) {
	format := &meta.Format{Name: "test", BlockSize: 4096, Capacity: 1 << 30, DirStats: true}
	m := meta.NewClient("memkv://", nil)
	if err := m.Init(format, true); err != nil {
		t.Fatalf("init meta: %s", err)
	}
	g1, _ := newTestGatewayWithMeta(t, m, format, Config{MultiBucket: true})
	g2, _ := newTestGatewayWithMeta(t, m, format, Config{MultiBucket: true})
	ctx := context.Background()
	const bucket = "gone-bucket"
	if err := g1.MakeBucketWithLocation(ctx, minio.MinioMetaBucket, minio.BucketOptions{}); err != nil {
		t.Fatalf("create metadata bucket: %s", err)
	}
	if err := g1.MakeBucketWithLocation(ctx, "src-bucket", minio.BucketOptions{}); err != nil {
		t.Fatalf("create src bucket: %s", err)
	}
	if err := g1.MakeBucketWithLocation(ctx, bucket, minio.BucketOptions{}); err != nil {
		t.Fatalf("create bucket: %s", err)
	}
	srcData := []byte("src")
	if _, err := g1.PutObject(ctx, "src-bucket", "src-obj", newTestPutObjReader(t, bytes.NewReader(srcData), srcData), minio.ObjectOptions{}); err != nil {
		t.Fatalf("put src object: %s", err)
	}
	uploadID, err := g1.NewMultipartUpload(ctx, bucket, "obj", minio.ObjectOptions{})
	if err != nil {
		t.Fatalf("create multipart upload: %s", err)
	}
	partData := []byte("part")
	part, err := g1.PutObjectPart(ctx, bucket, "obj", uploadID, 1, newTestPutObjReader(t, bytes.NewReader(partData), partData), minio.ObjectOptions{})
	if err != nil {
		t.Fatalf("put part: %s", err)
	}
	// warm g1's dentry cache so checkBucket may still see the bucket
	if _, err := g1.GetBucketInfo(ctx, bucket); err != nil {
		t.Fatalf("get bucket info: %s", err)
	}
	// the second gateway deletes the bucket; g1's cache is not invalidated
	if err := g2.DeleteBucket(ctx, bucket, false); err != nil {
		t.Fatalf("delete bucket: %s", err)
	}

	for _, object := range []string{"obj", "dir/obj", "dir/"} {
		data := []byte("data")
		if _, err := g1.PutObject(ctx, bucket, object, newTestPutObjReader(t, bytes.NewReader(data), data), minio.ObjectOptions{}); !errors.As(err, &minio.BucketNotFound{}) {
			t.Fatalf("PutObject %s after bucket deleted should return BucketNotFound, got %v", object, err)
		}
	}
	if _, err := g1.CopyObject(ctx, "src-bucket", "src-obj", bucket, "copy-obj", minio.ObjectInfo{}, minio.ObjectOptions{}, minio.ObjectOptions{}); !errors.As(err, &minio.BucketNotFound{}) {
		t.Fatalf("CopyObject after bucket deleted should return BucketNotFound, got %v", err)
	}
	if _, err := g1.CompleteMultipartUpload(ctx, bucket, "obj", uploadID, []minio.CompletePart{{PartNumber: 1, ETag: part.ETag}}, minio.ObjectOptions{}); !errors.As(err, &minio.BucketNotFound{}) {
		t.Fatalf("CompleteMultipartUpload after bucket deleted should return BucketNotFound, got %v", err)
	}
	// exercise the commit path deterministically, bypassing checkBucket
	data := []byte("data")
	if _, err := g1.putObject(ctx, bucket, g1.path(bucket, "obj"),
		newTestPutObjReader(t, bytes.NewReader(data), data), minio.ObjectOptions{}, func(string) {}); !errors.As(err, &minio.BucketNotFound{}) {
		t.Fatalf("putObject after bucket deleted should return BucketNotFound, got %v", err)
	}
	if _, errno := g2.fs.Stat(mctx, g2.path(bucket)); !fs.IsNotExist(errno) {
		t.Fatalf("deleted bucket was recreated: %s", errno)
	}
}

func TestCopyObjectTempCreateAfterBucketDeleted(t *testing.T) {
	format := &meta.Format{Name: "test", BlockSize: 4096, Capacity: 1 << 30, DirStats: true}
	baseMeta := meta.NewClient("memkv://", nil)
	if err := baseMeta.Init(format, true); err != nil {
		t.Fatalf("init meta: %s", err)
	}
	createMeta := &createErrMeta{Meta: baseMeta}
	g1, _ := newTestGatewayWithMeta(t, createMeta, format, Config{MultiBucket: true})
	g2, _ := newTestGatewayWithMeta(t, baseMeta, format, Config{MultiBucket: true})
	ctx := context.Background()
	for _, bucket := range []string{minio.MinioMetaBucket, "src-bucket", "dst-bucket"} {
		if err := g1.MakeBucketWithLocation(ctx, bucket, minio.BucketOptions{}); err != nil {
			t.Fatalf("create bucket %s: %s", bucket, err)
		}
	}
	data := []byte("data")
	if _, err := g1.PutObject(ctx, "src-bucket", "src-obj", newTestPutObjReader(t, bytes.NewReader(data), data), minio.ObjectOptions{}); err != nil {
		t.Fatalf("put source object: %s", err)
	}

	var deleteErr error
	createMeta.onCreate = func() {
		deleteErr = g2.DeleteBucket(ctx, "dst-bucket", false)
	}
	if _, err := g1.CopyObject(ctx, "src-bucket", "src-obj", "dst-bucket", "dst-obj", minio.ObjectInfo{}, minio.ObjectOptions{}, minio.ObjectOptions{}); !errors.As(err, &minio.BucketNotFound{}) {
		t.Fatalf("CopyObject after destination bucket deletion should return BucketNotFound, got %v", err)
	}
	if deleteErr != nil {
		t.Fatalf("delete destination bucket: %s", deleteErr)
	}
}

func TestObjectCommitErrBucketAlive(t *testing.T) {
	jfsObj, _, _ := newTestGateway(t, Config{MultiBucket: true})
	ctx := context.Background()
	if err := jfsObj.MakeBucketWithLocation(ctx, minio.MinioMetaBucket, minio.BucketOptions{}); err != nil {
		t.Fatalf("create metadata bucket: %s", err)
	}
	if err := jfsObj.MakeBucketWithLocation(ctx, "alive-bucket", minio.BucketOptions{}); err != nil {
		t.Fatalf("create bucket: %s", err)
	}
	// a pruned parent or a removed temporary source surfaces as ENOENT while
	// the bucket still exists: the object/uploadID mapping must be kept
	if err := jfsObj.objectCommitErr(ctx, syscall.ENOENT, "alive-bucket", "obj"); !errors.As(err, &minio.ObjectNotFound{}) {
		t.Fatalf("commit ENOENT with alive bucket should return ObjectNotFound, got %v", err)
	}
	if err := jfsObj.objectCommitErr(ctx, syscall.ENOENT, "alive-bucket", "obj", "upload-id"); !errors.As(err, &minio.InvalidUploadID{}) {
		t.Fatalf("commit ENOENT with alive bucket and uploadID should return InvalidUploadID, got %v", err)
	}
	if err := jfsObj.DeleteBucket(ctx, "alive-bucket", false); err != nil {
		t.Fatalf("delete bucket: %s", err)
	}
	if err := jfsObj.objectCommitErr(ctx, syscall.ENOENT, "alive-bucket", "obj"); !errors.As(err, &minio.BucketNotFound{}) {
		t.Fatalf("commit ENOENT after bucket deleted should return BucketNotFound, got %v", err)
	}
}

type lookupErrMeta struct {
	meta.Meta
	name string
}

func (m *lookupErrMeta) Lookup(ctx meta.Context, parent meta.Ino, name string, inode *meta.Ino, attr *meta.Attr, checkPerm bool) syscall.Errno {
	if name == m.name {
		return syscall.EIO
	}
	return m.Meta.Lookup(ctx, parent, name, inode, attr, checkPerm)
}

func TestObjectCommitErrLookupFailure(t *testing.T) {
	format := &meta.Format{Name: "test", BlockSize: 4096, Capacity: 1 << 30, DirStats: true}
	m := meta.NewClient("memkv://", nil)
	if err := m.Init(format, true); err != nil {
		t.Fatalf("init meta: %s", err)
	}
	jfsObj, _ := newTestGatewayWithMeta(t, &lookupErrMeta{Meta: m, name: "err-bucket"}, format, Config{MultiBucket: true})
	// a metadata failure while verifying the bucket root must be surfaced,
	// not masked as ObjectNotFound
	if err := jfsObj.objectCommitErr(context.Background(), syscall.ENOENT, "err-bucket", "obj"); !errors.Is(err, syscall.EIO) {
		t.Fatalf("commit ENOENT with failing metadata lookup should return EIO, got %v", err)
	}
}

func TestBucketLifecycleConsistency(t *testing.T) {
	jfsObj, jfs, _ := newTestGateway(t, Config{MultiBucket: true})
	ctx := context.Background()
	if err := jfsObj.MakeBucketWithLocation(ctx, minio.MinioMetaBucket, minio.BucketOptions{}); err != nil {
		t.Fatalf("create metadata bucket: %s", err)
	}

	t.Run("make and delete use lifecycle lock", func(t *testing.T) {
		lk, err := jfsObj.lockBucket(ctx)
		if err != nil {
			t.Fatalf("lock bucket: %s", err)
		}
		made := make(chan error, 1)
		go func() {
			made <- jfsObj.MakeBucketWithLocation(ctx, "locked-bucket", minio.BucketOptions{})
		}()
		select {
		case err := <-made:
			lk.Unlock()
			t.Fatalf("MakeBucket should wait for lifecycle lock, got %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		lk.Unlock()
		select {
		case err := <-made:
			if err != nil {
				t.Fatalf("MakeBucket after unlock: %s", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("MakeBucket did not continue after unlock")
		}

		lk, err = jfsObj.lockBucket(ctx)
		if err != nil {
			t.Fatalf("lock bucket before delete: %s", err)
		}
		deleted := make(chan error, 1)
		go func() {
			deleted <- jfsObj.DeleteBucket(ctx, "locked-bucket", false)
		}()
		select {
		case err := <-deleted:
			lk.Unlock()
			t.Fatalf("DeleteBucket should wait for lifecycle lock, got %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		lk.Unlock()
		select {
		case err := <-deleted:
			if err != nil {
				t.Fatalf("DeleteBucket after unlock: %s", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("DeleteBucket did not continue after unlock")
		}
	})

	t.Run("failed delete keeps metadata", func(t *testing.T) {
		const bucket = "nonempty-bucket"
		if err := jfsObj.MakeBucketWithLocation(ctx, bucket, minio.BucketOptions{}); err != nil {
			t.Fatalf("create bucket: %s", err)
		}
		createTestFile(t, jfs, jfsObj.path(bucket, "object"))
		if err := jfsObj.DeleteBucket(ctx, bucket, false); !errors.As(err, &minio.BucketNotEmpty{}) {
			t.Fatalf("delete non-empty bucket should fail with BucketNotEmpty, got %v", err)
		}
		metadataPath := jfsObj.path(minio.MinioMetaBucket, minio.BucketMetaPrefix, bucket, minio.BucketMetadataFile)
		if _, errno := jfs.Stat(mctx, metadataPath); errno != 0 {
			t.Fatalf("bucket metadata should remain after failed delete: %s", errno)
		}
	})

	t.Run("delete moves temporary files to trash", func(t *testing.T) {
		const bucket = "temporary-files-bucket"
		if err := jfsObj.MakeBucketWithLocation(ctx, bucket, minio.BucketOptions{}); err != nil {
			t.Fatalf("create bucket: %s", err)
		}
		uploadID, err := jfsObj.NewMultipartUpload(ctx, bucket, "multipart", minio.ObjectOptions{})
		if err != nil {
			t.Fatalf("create multipart upload: %s", err)
		}
		partData := []byte("part")
		if _, err := jfsObj.PutObjectPart(ctx, bucket, "multipart", uploadID, 1, newTestPutObjReader(t, bytes.NewReader(partData), partData), minio.ObjectOptions{}); err != nil {
			t.Fatalf("put object part: %s", err)
		}
		if err := jfsObj.DeleteBucket(ctx, bucket, false); err != nil {
			t.Fatalf("delete bucket: %s", err)
		}
		if _, errno := jfs.Stat(mctx, jfsObj.tpath(bucket)); !fs.IsNotExist(errno) {
			t.Fatalf("bucket temporary files should be removed, got %s", errno)
		}
		trashDir := jfsObj.tpath(bucketTrashDir)
		f, errno := jfs.Open(mctx, trashDir, 0)
		if errno != 0 {
			t.Fatalf("open bucket trash directory: %s", errno)
		}
		entries, errno := f.Readdir(mctx, 0)
		_ = f.Close(mctx)
		if errno != 0 {
			t.Fatalf("read bucket trash directory: %s", errno)
		}
		found := false
		for _, entry := range entries {
			movedUpload := path.Join(trashDir, entry.Name(), "uploads", uploadID[:subDirPrefix], uploadID)
			if _, errno = jfs.Stat(mctx, movedUpload); errno == 0 {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("bucket temporary files were not moved to trash")
		}
	})

	t.Run("missing bucket cleans stale metadata", func(t *testing.T) {
		const bucket = "stale-metadata-bucket"
		if err := jfsObj.MakeBucketWithLocation(ctx, bucket, minio.BucketOptions{}); err != nil {
			t.Fatalf("create bucket: %s", err)
		}
		if errno := jfs.Delete(mctx, jfsObj.path(bucket)); errno != 0 {
			t.Fatalf("remove bucket directory: %s", errno)
		}
		if err := jfsObj.DeleteBucket(ctx, bucket, false); !errors.As(err, &minio.BucketNotFound{}) {
			t.Fatalf("delete missing bucket should return BucketNotFound, got %v", err)
		}
		metadataPath := jfsObj.path(minio.MinioMetaBucket, minio.BucketMetaPrefix, bucket, minio.BucketMetadataFile)
		if _, errno := jfs.Stat(mctx, metadataPath); !errors.Is(errno, os.ErrNotExist) {
			t.Fatalf("stale bucket metadata should be removed, got %s", errno)
		}
	})

	t.Run("metadata failure rolls back bucket", func(t *testing.T) {
		const bucket = "rollback-bucket"
		metadataDir := jfsObj.path(minio.MinioMetaBucket, minio.BucketMetaPrefix)
		if errno := jfs.MkdirAll(mctx, metadataDir, 0777, 022); errno != 0 {
			t.Fatalf("create metadata directory: %s", errno)
		}
		createTestFile(t, jfs, jfsObj.path(minio.MinioMetaBucket, minio.BucketMetaPrefix, bucket))
		if err := jfsObj.MakeBucketWithLocation(ctx, bucket, minio.BucketOptions{}); err == nil {
			t.Fatal("MakeBucket should fail when metadata cannot be saved")
		}
		if _, errno := jfs.Stat(mctx, jfsObj.path(bucket)); !errors.Is(errno, os.ErrNotExist) {
			t.Fatalf("bucket should be removed after metadata failure, got %s", errno)
		}
	})
}

// TestPutObjectInFlightPinsBucket (add-gateway-platform-mode, complete-at-
// destination trade-off, design decision 3): the object is created at the
// destination path before the payload streams, so a bucket delete concurrent
// with an in-flight PutObject observes the destination object and fails with
// BucketNotEmpty — the staging flow hid the object under `.sys/tmp` until the
// atomic rename and let the delete succeed. A commit racing a real deletion
// still never recreates the deleted bucket root and reports BucketNotFound
// (TestObjectCommitAfterBucketDeleted exercises that path for the
// direct-create flow).
func TestPutObjectInFlightPinsBucket(t *testing.T) {
	format := &meta.Format{Name: "test", BlockSize: 4096, Capacity: 1 << 30, DirStats: true}
	m := meta.NewClient("memkv://", nil)
	if err := m.Init(format, true); err != nil {
		t.Fatalf("init metadata: %s", err)
	}
	g1, _ := newTestGatewayWithMeta(t, m, format, Config{MultiBucket: true})
	g2, _ := newTestGatewayWithMeta(t, m, format, Config{MultiBucket: true})
	ctx := context.Background()
	const bucket = "deleted-bucket"
	if err := g1.MakeBucketWithLocation(ctx, minio.MinioMetaBucket, minio.BucketOptions{}); err != nil {
		t.Fatalf("create metadata bucket: %s", err)
	}
	if err := g1.MakeBucketWithLocation(ctx, bucket, minio.BucketOptions{}); err != nil {
		t.Fatalf("create bucket: %s", err)
	}

	data := []byte("object data")
	reader := &gatedReader{reader: bytes.NewReader(data), started: make(chan struct{}), release: make(chan struct{})}
	putReader := newTestPutObjReader(t, reader, data)
	putDone := make(chan error, 1)
	go func() {
		_, err := g1.PutObject(ctx, bucket, "dir/object", putReader, minio.ObjectOptions{})
		putDone <- err
	}()
	select {
	case <-reader.started:
	case <-time.After(2 * time.Second):
		t.Fatal("PutObject did not start reading")
	}

	// the destination object is already visible, so the concurrent delete must
	// refuse the bucket instead of removing it under the in-flight put
	if err := g2.DeleteBucket(ctx, bucket, false); !errors.As(err, &minio.BucketNotEmpty{}) {
		close(reader.release)
		t.Fatalf("DeleteBucket during in-flight put must see the destination object, got %v", err)
	}
	close(reader.release)
	select {
	case err := <-putDone:
		if err != nil {
			t.Fatalf("PutObject must succeed on a bucket that stayed alive: %s", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PutObject did not finish")
	}

	// the object written by the in-flight put is visible to the second gateway
	if _, err := g2.GetObjectInfo(ctx, bucket, "dir/object", minio.ObjectOptions{}); err != nil {
		t.Fatalf("object written by in-flight put must be visible: %s", err)
	}
}

type gatedReader struct {
	reader  io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *gatedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.reader.Read(p)
}

func newTestPutObjReader(t *testing.T, r io.Reader, data []byte) *minio.PutObjReader {
	t.Helper()
	sum := md5.Sum(data)
	hashReader, err := miniohash.NewReader(r, int64(len(data)), hex.EncodeToString(sum[:]), "", int64(len(data)))
	if err != nil {
		t.Fatalf("create put object reader: %s", err)
	}
	return minio.NewPutObjReader(hashReader)
}

func newTestGateway(t *testing.T, conf Config) (*jfsObjects, *fs.FileSystem, string) {
	t.Helper()

	m := meta.NewClient("memkv://", nil)
	format := &meta.Format{
		Name:      "test",
		BlockSize: 4096,
		Capacity:  1 << 30,
		DirStats:  true,
	}
	if err := m.Init(format, true); err != nil {
		t.Fatalf("init meta: %s", err)
	}
	jfsObj, jfs := newTestGatewayWithMeta(t, m, format, conf)
	return jfsObj, jfs, format.Name
}

func newTestGatewayWithMeta(t *testing.T, m meta.Meta, format *meta.Format, conf Config) (*jfsObjects, *fs.FileSystem) {
	t.Helper()
	vfsConf := &vfs.Config{
		Meta: meta.DefaultConf(),
		Chunk: &chunk.Config{
			BlockSize:   format.BlockSize << 10,
			MaxUpload:   1,
			MaxDownload: 200,
			BufferSize:  100 << 20,
		},
		DirEntryTimeout: time.Millisecond * 100,
		EntryTimeout:    time.Millisecond * 100,
		AttrTimeout:     time.Millisecond * 100,
	}
	objStore, _ := object.CreateStorage("mem", "", "", "", "")
	store := chunk.NewCachedStore(objStore, *vfsConf.Chunk, nil)
	jfs, err := fs.NewFileSystem(vfsConf, m, store, nil)
	if err != nil {
		t.Fatalf("initialize failed: %s", err)
	}
	conf.Bucket = format.Name
	if conf.Umask == 0 {
		conf.Umask = 022
	}
	jfsObj := &jfsObjects{
		fs:       jfs,
		conf:     vfsConf,
		listPool: minio.NewTreeWalkPool(time.Minute * 30),
		gConf:    &conf,
		nsMutex:  minio.NewNSLock(false),
	}
	mctx = meta.NewContext(uint32(os.Getpid()), uint32(os.Getuid()), []uint32{uint32(os.Getgid())})
	if conf.MultiBucket && !conf.ReadOnly {
		if errno := jfs.MkdirAll(mctx, jfsObj.tpath(bucketTrashDir), 0777, conf.Umask); errno != 0 {
			t.Fatalf("create bucket trash directory: %s", errno)
		}
	}
	return jfsObj, jfs
}

func TestMkdirAllInBucket(t *testing.T) {
	jfsObj, jfs, _ := newTestGateway(t, Config{MultiBucket: true})
	ctx := context.Background()
	const bucket = "bucket"

	if eno := jfs.Mkdir(mctx, jfsObj.path(bucket), 0777, 022); eno != 0 {
		t.Fatalf("mkdir bucket: %s", eno)
	}
	if err := jfsObj.mkdirAllInBucket(ctx, bucket, jfsObj.path(bucket, "dir", "subdir")); err != nil {
		t.Fatalf("mkdir within bucket: %s", err)
	}
	if _, eno := jfs.Stat(mctx, jfsObj.path(bucket, "dir", "subdir")); eno != 0 {
		t.Fatalf("stat directory: %s", eno)
	}

	const missingBucket = "missing-bucket"
	err := jfsObj.mkdirAllInBucket(ctx, missingBucket, jfsObj.path(missingBucket, "dir", "subdir"))
	if err == nil || !fs.IsNotExist(err) {
		t.Fatalf("mkdir under missing bucket should fail with ENOENT, got %v", err)
	}
	if _, eno := jfs.Stat(mctx, jfsObj.path(missingBucket)); !fs.IsNotExist(eno) {
		t.Fatalf("missing bucket was recreated: %s", eno)
	}
}

func createTestFile(t *testing.T, jfs *fs.FileSystem, name string) {
	t.Helper()
	f, eno := jfs.Create(mctx, name, 0666, 022)
	if eno != 0 {
		t.Fatalf("create %s: %s", name, eno)
	}
	if eno = f.Close(mctx); eno != 0 {
		t.Fatalf("close %s: %s", name, eno)
	}
}

func assertHeadObject(t *testing.T, jfsObj *jfsObjects, bucket, object string, wantFound bool) {
	t.Helper()
	_, err := jfsObj.GetObjectInfo(context.Background(), bucket, object, minio.ObjectOptions{})
	if wantFound {
		if err != nil {
			t.Fatalf("head %s should succeed: %s", object, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("head %s should fail", object)
	}
	if !errors.As(err, &minio.ObjectNotFound{}) {
		t.Fatalf("head %s should return ObjectNotFound, got %T: %s", object, err, err)
	}
}

func TestGetObjectInfo(t *testing.T) {
	t.Run("head file slash fails with head dir", func(t *testing.T) {
		jfsObj, jfs, bucket := newTestGateway(t, Config{HeadDir: true})
		createTestFile(t, jfs, "/file")

		assertHeadObject(t, jfsObj, bucket, "file", true)
		assertHeadObject(t, jfsObj, bucket, "file/", false)
		assertHeadObject(t, jfsObj, bucket, "file/child", false)
	})

	t.Run("put file under implicit directory", func(t *testing.T) {
		jfsObj, jfs, bucket := newTestGateway(t, Config{})
		if eno := jfs.Mkdir(mctx, "/dir1", 0777, 022); eno != 0 {
			t.Fatalf("mkdir dir1: %s", eno)
		}
		createTestFile(t, jfs, "/dir1/key1")

		assertHeadObject(t, jfsObj, bucket, "dir1", false)
		assertHeadObject(t, jfsObj, bucket, "dir1/", false)
		assertHeadObject(t, jfsObj, bucket, "dir1/key1", true)
		assertHeadObject(t, jfsObj, bucket, "dir1/key1/", false)
		assertHeadObject(t, jfsObj, bucket, "dir1/key1/child", false)
	})

	t.Run("put explicit directory object", func(t *testing.T) {
		jfsObj, jfs, bucket := newTestGateway(t, Config{})
		if eno := jfs.MkdirAll(mctx, "/dir1/key1", 0777, 022); eno != 0 {
			t.Fatalf("mkdir dir1/key1: %s", eno)
		}
		jfsObj.setFileAtime("/dir1/key1", 0)

		assertHeadObject(t, jfsObj, bucket, "dir1/key1", false)
		assertHeadObject(t, jfsObj, bucket, "dir1/key1/", true)
	})

	t.Run("head dir allows implicit directories but not file slash", func(t *testing.T) {
		jfsObj, jfs, bucket := newTestGateway(t, Config{HeadDir: true})
		if eno := jfs.Mkdir(mctx, "/dir1", 0777, 022); eno != 0 {
			t.Fatalf("mkdir dir1: %s", eno)
		}
		createTestFile(t, jfs, "/dir1/key1")

		assertHeadObject(t, jfsObj, bucket, "dir1", true)
		assertHeadObject(t, jfsObj, bucket, "dir1/", true)
		assertHeadObject(t, jfsObj, bucket, "dir1/key1", true)
		assertHeadObject(t, jfsObj, bucket, "dir1/key1/", false)
	})
}

func TestDeleteObjects(t *testing.T) {
	t.Run("keep sibling object named as bucket", func(t *testing.T) {
		jfsObj, jfs, _ := newTestGateway(t, Config{MultiBucket: true})
		bucket := "jmrq"
		if eno := jfs.Mkdir(mctx, "/"+bucket, 0777, 022); eno != 0 {
			t.Fatalf("mkdir bucket: %s", eno)
		}
		createTestFile(t, jfs, "/"+bucket+"/"+bucket) // key == bucket name -> /jmrq/jmrq
		createTestFile(t, jfs, "/"+bucket+"/rror")

		_, errs := jfsObj.DeleteObjects(context.Background(), bucket,
			[]minio.ObjectToDelete{{ObjectName: "rror"}}, minio.ObjectOptions{})
		for _, e := range errs {
			if e != nil {
				t.Fatalf("delete rror: %s", e)
			}
		}
		assertHeadObject(t, jfsObj, bucket, bucket, true)
		assertHeadObject(t, jfsObj, bucket, "rror", false)
	})

	t.Run("keep explicit directory object parent", func(t *testing.T) {
		jfsObj, jfs, _ := newTestGateway(t, Config{MultiBucket: true})
		bucket := "bkt1"
		if eno := jfs.Mkdir(mctx, "/"+bucket, 0777, 022); eno != 0 {
			t.Fatalf("mkdir bucket: %s", eno)
		}
		if eno := jfs.MkdirAll(mctx, "/"+bucket+"/a/b", 0777, 022); eno != 0 {
			t.Fatalf("mkdir a/b: %s", eno)
		}
		jfsObj.setFileAtime("/"+bucket+"/a/b", 0) // explicit directory object "a/b/"
		createTestFile(t, jfs, "/"+bucket+"/a/b/c")

		_, errs := jfsObj.DeleteObjects(context.Background(), bucket,
			[]minio.ObjectToDelete{{ObjectName: "a/b/c"}}, minio.ObjectOptions{})
		for _, e := range errs {
			if e != nil {
				t.Fatalf("delete a/b/c: %s", e)
			}
		}
		assertHeadObject(t, jfsObj, bucket, "a/b/", true)
		assertHeadObject(t, jfsObj, bucket, "a/b/c", false)
	})

	t.Run("prune empty implicit directories", func(t *testing.T) {
		jfsObj, jfs, _ := newTestGateway(t, Config{MultiBucket: true})
		bucket := "bkt2"
		if eno := jfs.Mkdir(mctx, "/"+bucket, 0777, 022); eno != 0 {
			t.Fatalf("mkdir bucket: %s", eno)
		}
		if eno := jfs.MkdirAll(mctx, "/"+bucket+"/x/y", 0777, 022); eno != 0 {
			t.Fatalf("mkdir x/y: %s", eno)
		}
		createTestFile(t, jfs, "/"+bucket+"/x/y/z")

		_, errs := jfsObj.DeleteObjects(context.Background(), bucket,
			[]minio.ObjectToDelete{{ObjectName: "x/y/z"}}, minio.ObjectOptions{})
		for _, e := range errs {
			if e != nil {
				t.Fatalf("delete x/y/z: %s", e)
			}
		}
		if fi, eno := jfs.Stat(mctx, "/"+bucket+"/x"); eno == 0 {
			t.Fatalf("implicit dir /x should be pruned, still exists: isDir=%v", fi.IsDir())
		}
	})

	t.Run("keep sibling bucket sharing name prefix", func(t *testing.T) {
		jfsObj, jfs, _ := newTestGateway(t, Config{MultiBucket: true})
		// two buckets whose names share a string prefix ("jmrq" vs "jmrqfoo")
		if eno := jfs.Mkdir(mctx, "/jmrq", 0777, 022); eno != 0 {
			t.Fatalf("mkdir jmrq: %s", eno)
		}
		if eno := jfs.Mkdir(mctx, "/jmrqfoo", 0777, 022); eno != 0 {
			t.Fatalf("mkdir jmrqfoo: %s", eno)
		}
		createTestFile(t, jfs, "/jmrqfoo/x")

		// a traversal key against bucket jmrq must never prune the sibling bucket dir
		_, _ = jfsObj.DeleteObjects(context.Background(), "jmrq",
			[]minio.ObjectToDelete{{ObjectName: "../jmrqfoo/x"}}, minio.ObjectOptions{})
		if _, eno := jfs.Stat(mctx, "/jmrqfoo"); eno != 0 {
			t.Fatalf("sibling bucket dir /jmrqfoo must not be pruned, got: %s", eno)
		}
	})
}

// readTestObject reads the whole object at the given path through the
// filesystem layer.
func readTestObject(t *testing.T, jfs *fs.FileSystem, p string) []byte {
	t.Helper()
	f, errno := jfs.Open(mctx, p, 0)
	if errno != 0 {
		t.Fatalf("open %s: %s", p, errno)
	}
	defer func() { _ = f.Close(mctx) }()
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(mctx, buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("read %s: %s", p, err)
		}
	}
	return out
}

// newCompleteTestGateway builds a test gateway with fs-level caches disabled:
// CompleteMultipartUpload writes data and attributes through meta-level
// CopyFileRange, which does not invalidate the fs attr cache (true for the
// previous rename-based flow as well), so assertions need the uncached view.
func newCompleteTestGateway(t *testing.T, conf Config) (*jfsObjects, *fs.FileSystem, string) {
	t.Helper()
	m := meta.NewClient("memkv://", nil)
	format := &meta.Format{
		Name:      "test",
		BlockSize: 4096,
		Capacity:  1 << 30,
		DirStats:  true,
	}
	if err := m.Init(format, true); err != nil {
		t.Fatalf("init meta: %s", err)
	}
	vfsConf := &vfs.Config{
		Meta: meta.DefaultConf(),
		Chunk: &chunk.Config{
			BlockSize:   format.BlockSize << 10,
			MaxUpload:   1,
			MaxDownload: 200,
			BufferSize:  100 << 20,
		},
	}
	objStore, _ := object.CreateStorage("mem", "", "", "", "")
	store := chunk.NewCachedStore(objStore, *vfsConf.Chunk, nil)
	jfs, err := fs.NewFileSystem(vfsConf, m, store, nil)
	if err != nil {
		t.Fatalf("initialize failed: %s", err)
	}
	conf.Bucket = format.Name
	if conf.Umask == 0 {
		conf.Umask = 022
	}
	jfsObj := &jfsObjects{
		fs:       jfs,
		conf:     vfsConf,
		listPool: minio.NewTreeWalkPool(time.Minute * 30),
		gConf:    &conf,
		nsMutex:  minio.NewNSLock(false),
	}
	mctx = meta.NewContext(uint32(os.Getpid()), uint32(os.Getuid()), []uint32{uint32(os.Getgid())})
	return jfsObj, jfs, format.Name
}

func multipartUploadForTest(t *testing.T, n *jfsObjects, ctx context.Context, bucket, object string, partDatas ...[]byte) (string, []minio.CompletePart) {
	t.Helper()
	uploadID, err := n.NewMultipartUpload(ctx, bucket, object, minio.ObjectOptions{})
	if err != nil {
		t.Fatalf("new multipart upload: %s", err)
	}
	var parts []minio.CompletePart
	for i, data := range partDatas {
		part, err := n.PutObjectPart(ctx, bucket, object, uploadID, i+1, newTestPutObjReader(t, bytes.NewReader(data), data), minio.ObjectOptions{})
		if err != nil {
			t.Fatalf("put part %d: %s", i+1, err)
		}
		parts = append(parts, minio.CompletePart{PartNumber: i + 1, ETag: part.ETag})
	}
	return uploadID, parts
}

// TestCompleteMultipartUploadAtDestination (add-gateway-platform-mode 2.1,
// spec "Complete keys the final object by its real path"): Complete creates
// the final object at the destination key, readable through the S3 API, and
// removes the staging upload directory; no complete file is staged under
// `.sys/uploads`.
func TestCompleteMultipartUploadAtDestination(t *testing.T) {
	n, jfs, bucket := newCompleteTestGateway(t, Config{})
	ctx := context.Background()
	data1 := []byte("hello ")
	data2 := []byte("world")
	object := "companies/acme/render.exr"

	uploadID, parts := multipartUploadForTest(t, n, ctx, bucket, object, data1, data2)
	objInfo, err := n.CompleteMultipartUpload(ctx, bucket, object, uploadID, parts, minio.ObjectOptions{})
	if err != nil {
		t.Fatalf("complete: %s", err)
	}
	if objInfo.Size != int64(len(data1)+len(data2)) {
		t.Fatalf("size %d, want %d", objInfo.Size, len(data1)+len(data2))
	}
	if want := minio.ComputeCompleteMultipartMD5(parts); objInfo.ETag != want {
		t.Fatalf("etag %q, want multipart md5 %q", objInfo.ETag, want)
	}

	// the object is readable at the destination key with the merged content
	got := readTestObject(t, jfs, n.path(bucket, object))
	if !bytes.Equal(got, append(append([]byte{}, data1...), data2...)) {
		t.Fatalf("content %q, want %q", got, "hello world")
	}
	if _, err := n.GetObjectInfo(ctx, bucket, object, minio.ObjectOptions{}); err != nil {
		t.Fatalf("GetObjectInfo by key: %s", err)
	}

	// staging is gone: no upload directory, hence no staged complete file
	if _, errno := n.fs.Stat(mctx, n.upath(bucket, uploadID)); !fs.IsNotExist(errno) {
		t.Fatalf("staging upload directory still exists: %s", errno)
	}

	// a nested second complete keeps working at deeper keys
	uploadID2, parts2 := multipartUploadForTest(t, n, ctx, bucket, "companies/acme/nested/other.bin", data2, data1)
	if _, err := n.CompleteMultipartUpload(ctx, bucket, "companies/acme/nested/other.bin", uploadID2, parts2, minio.ObjectOptions{}); err != nil {
		t.Fatalf("complete nested: %s", err)
	}
	if got := readTestObject(t, jfs, n.path(bucket, "companies/acme/nested/other.bin")); !bytes.Equal(got, append(append([]byte{}, data2...), data1...)) {
		t.Fatalf("nested content %q", got)
	}
}

// TestCompleteMultipartUploadReplacesExisting (unencrypted equivalence of the
// previous rename semantics): Complete onto an existing key replaces the
// object wholesale.
func TestCompleteMultipartUploadReplacesExisting(t *testing.T) {
	n, jfs, bucket := newCompleteTestGateway(t, Config{})
	ctx := context.Background()
	oldData := []byte("stale object payload")
	newData := []byte("fresh content")
	object := "companies/acme/replaced.bin"

	if _, err := n.PutObject(ctx, bucket, object, newTestPutObjReader(t, bytes.NewReader(oldData), oldData), minio.ObjectOptions{}); err != nil {
		t.Fatalf("put stale object: %s", err)
	}
	uploadID, parts := multipartUploadForTest(t, n, ctx, bucket, object, newData)
	if _, err := n.CompleteMultipartUpload(ctx, bucket, object, uploadID, parts, minio.ObjectOptions{}); err != nil {
		t.Fatalf("complete over existing object: %s", err)
	}
	if got := readTestObject(t, jfs, n.path(bucket, object)); !bytes.Equal(got, newData) {
		t.Fatalf("content %q, want replacement %q", got, newData)
	}
	if _, errno := n.fs.Stat(mctx, n.upath(bucket, uploadID)); !fs.IsNotExist(errno) {
		t.Fatalf("staging upload directory still exists: %s", errno)
	}
}

// TestCompleteMultipartUploadFailureRemovesPartial (spec "Failed complete
// leaves no partial object"): a part lost before Complete fails the commit,
// the partially written destination file is removed and the staging directory
// is left for the background cleanup.
func TestCompleteMultipartUploadFailureRemovesPartial(t *testing.T) {
	n, _, bucket := newCompleteTestGateway(t, Config{})
	ctx := context.Background()
	data1 := []byte("hello ")
	data2 := []byte("world")
	object := "companies/acme/broken.exr"

	uploadID, parts := multipartUploadForTest(t, n, ctx, bucket, object, data1, data2)
	// lose a part after the upload is registered
	if errno := n.fs.Delete(mctx, n.ppath(bucket, uploadID, "2")); errno != 0 {
		t.Fatalf("delete part: %s", errno)
	}
	if _, err := n.CompleteMultipartUpload(ctx, bucket, object, uploadID, parts, minio.ObjectOptions{}); err == nil {
		t.Fatal("complete with a missing part must fail")
	}
	// no partial object at the destination
	if _, errno := n.fs.Stat(mctx, n.path(bucket, object)); !fs.IsNotExist(errno) {
		t.Fatalf("partial object left at destination: %s", errno)
	}
	// the staging directory stays for the background cleanup (unchanged)
	if _, errno := n.fs.Stat(mctx, n.upath(bucket, uploadID)); errno != 0 {
		t.Fatalf("staging directory must remain for background cleanup: %s", errno)
	}
}

// TestCompleteMultipartUploadAbortUnchanged (2.2 regression): Abort removes
// the staging directory and never touches the destination.
func TestCompleteMultipartUploadAbortUnchanged(t *testing.T) {
	n, _, bucket := newCompleteTestGateway(t, Config{})
	ctx := context.Background()
	object := "companies/acme/aborted.bin"
	uploadID, _ := multipartUploadForTest(t, n, ctx, bucket, object, []byte("part"))
	if err := n.AbortMultipartUpload(ctx, bucket, object, uploadID, minio.ObjectOptions{}); err != nil {
		t.Fatalf("abort: %s", err)
	}
	if _, errno := n.fs.Stat(mctx, n.upath(bucket, uploadID)); !fs.IsNotExist(errno) {
		t.Fatalf("staging directory still exists after abort: %s", errno)
	}
	if _, errno := n.fs.Stat(mctx, n.path(bucket, object)); !fs.IsNotExist(errno) {
		t.Fatalf("abort must not create the destination object: %s", errno)
	}
}

// TestCompleteMultipartUploadDestinationIsDirectory: a directory occupying the
// destination key is a conflict, like it was for the rename-based flow.
func TestCompleteMultipartUploadDestinationIsDirectory(t *testing.T) {
	n, _, bucket := newCompleteTestGateway(t, Config{})
	ctx := context.Background()
	object := "companies/acme/dir-obj"
	if errno := n.fs.MkdirAll(mctx, n.path(bucket, object), 0777, 022); errno != 0 {
		t.Fatalf("mkdir destination: %s", errno)
	}
	uploadID, parts := multipartUploadForTest(t, n, ctx, bucket, object, []byte("data"))
	if _, err := n.CompleteMultipartUpload(ctx, bucket, object, uploadID, parts, minio.ObjectOptions{}); err == nil {
		t.Fatal("complete onto a directory must fail")
	}
	if _, errno := n.fs.Stat(mctx, n.path(bucket, object)); errno != 0 {
		t.Fatalf("destination directory must survive the failed complete: %s", errno)
	}
}

// errAfterDataReader serves only the first few bytes of the payload, then
// fails: it emulates a client connection breaking mid-upload after the
// destination file was created. The failure must hit before the declared
// payload length is reached (the hash reader wraps the source in a
// LimitReader that would turn an exhausted stream into a clean EOF).
type errAfterDataReader struct {
	head []byte // bytes served before the failure
	err  error
	off  int
}

func (r *errAfterDataReader) Read(p []byte) (int, error) {
	if r.off >= len(r.head) {
		return 0, r.err
	}
	n := copy(p, r.head[r.off:])
	r.off += n
	return n, nil
}

// TestPutObjectAtDestination (add-gateway-platform-mode, gap found on the
// hector-stage pilot, spec "Complete keys the final object by its real path"
// applied to single Put): PutObject creates the object directly at the
// destination key — readable through the S3 API — and never stages a temp file
// under `.sys/tmp` (a staged+renamed file was keyed under the synthetic `.sys`
// company on encrypted volumes and became unreadable at the destination).
func TestPutObjectAtDestination(t *testing.T) {
	n, jfs, bucket := newTestGateway(t, Config{})
	ctx := context.Background()
	data := []byte("single put payload")
	object := "companies/acme/render/shot01/frame.exr"

	objInfo, err := n.PutObject(ctx, bucket, object, newTestPutObjReader(t, bytes.NewReader(data), data), minio.ObjectOptions{})
	if err != nil {
		t.Fatalf("put: %s", err)
	}
	if objInfo.Size != int64(len(data)) {
		t.Fatalf("size %d, want %d", objInfo.Size, len(data))
	}
	sum := md5.Sum(data)
	if want := hex.EncodeToString(sum[:]); objInfo.ETag != want {
		t.Fatalf("etag %q, want md5 %q", objInfo.ETag, want)
	}

	// the object is readable at the destination key with the full content
	got := readTestObject(t, jfs, n.path(bucket, object))
	if !bytes.Equal(got, data) {
		t.Fatalf("content %q, want %q", got, data)
	}
	info, err := n.GetObjectInfo(ctx, bucket, object, minio.ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObjectInfo by key: %s", err)
	}
	if info.Size != int64(len(data)) {
		t.Fatalf("head size %d, want %d", info.Size, len(data))
	}

	// staging is untouched: no `.sys/tmp` tree is created at all
	if _, errno := n.fs.Stat(mctx, n.tpath(bucket, "tmp")); !fs.IsNotExist(errno) {
		t.Fatalf("staging tmp directory was created: %s", errno)
	}
}

// TestPutObjectReplacesExisting (unencrypted equivalence of the previous
// rename semantics): Put onto an existing key replaces the object wholesale.
func TestPutObjectReplacesExisting(t *testing.T) {
	n, jfs, bucket := newTestGateway(t, Config{})
	ctx := context.Background()
	oldData := []byte("stale object payload")
	newData := []byte("fresh content")
	object := "companies/acme/replaced.bin"

	if _, err := n.PutObject(ctx, bucket, object, newTestPutObjReader(t, bytes.NewReader(oldData), oldData), minio.ObjectOptions{}); err != nil {
		t.Fatalf("put stale object: %s", err)
	}
	if _, err := n.PutObject(ctx, bucket, object, newTestPutObjReader(t, bytes.NewReader(newData), newData), minio.ObjectOptions{}); err != nil {
		t.Fatalf("put over existing object: %s", err)
	}
	if got := readTestObject(t, jfs, n.path(bucket, object)); !bytes.Equal(got, newData) {
		t.Fatalf("content %q, want replacement %q", got, newData)
	}
}

// TestPutObjectDestinationIsDirectory: a directory occupying the destination
// key is a conflict, like it was for the rename-based flow.
func TestPutObjectDestinationIsDirectory(t *testing.T) {
	n, _, bucket := newTestGateway(t, Config{})
	ctx := context.Background()
	object := "companies/acme/dir-obj"
	if errno := n.fs.MkdirAll(mctx, n.path(bucket, object), 0777, 022); errno != 0 {
		t.Fatalf("mkdir destination: %s", errno)
	}
	data := []byte("data")
	if _, err := n.PutObject(ctx, bucket, object, newTestPutObjReader(t, bytes.NewReader(data), data), minio.ObjectOptions{}); err == nil {
		t.Fatal("put onto a directory must fail")
	}
	if _, errno := n.fs.Stat(mctx, n.path(bucket, object)); errno != 0 {
		t.Fatalf("destination directory must survive the failed put: %s", errno)
	}
}

// TestPutObjectFailureRemovesPartial (spec "Failed complete leaves no partial
// object", applied to single Put): a reader failing mid-upload fails the put,
// the partially written destination file is removed.
func TestPutObjectFailureRemovesPartial(t *testing.T) {
	n, _, bucket := newTestGateway(t, Config{})
	ctx := context.Background()
	data := []byte("partial payload")
	object := "companies/acme/broken.bin"

	reader := &errAfterDataReader{head: data[:7], err: errors.New("connection lost")}
	if _, err := n.PutObject(ctx, bucket, object, newTestPutObjReader(t, reader, data), minio.ObjectOptions{}); err == nil {
		t.Fatal("put with a failing reader must fail")
	}
	// no partial object at the destination
	if _, errno := n.fs.Stat(mctx, n.path(bucket, object)); !fs.IsNotExist(errno) {
		t.Fatalf("partial object left at destination: %s", errno)
	}
}
