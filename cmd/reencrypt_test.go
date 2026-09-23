/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
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

package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
)

// encTestRedisAddr returns the Redis host:port for the encryption integration tests.
// It defaults to the local 127.0.0.1:6379; the compose suite (make
// test.enc.integration, stage 9) overrides it with REDIS_ADDR.
func encTestRedisAddr() string {
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		return addr
	}
	return "127.0.0.1:6379"
}

// reencryptTestMeta uses Redis DB 4 (DBs 11, 12, 14, 15 are taken by other cmd tests).
func reencryptTestMeta() string { return "redis://" + encTestRedisAddr() + "/4" }

type reencryptEnv struct {
	m         meta.Meta
	store     chunk.ChunkStore
	chunkConf chunk.Config
	kek       []byte
	uuid      string
	company   string
}

// newReencryptEnv prepares an encrypted volume (Redis DB 4, in-memory object
// storage) with the DeleteSlice handler registered the same way as the
// reencrypt command. Trash is enabled so old slices are deleted lazily during
// the migration and concurrent readers of stale chunk lists keep working
// (FR-MIG-6).
func newReencryptEnv(t *testing.T) *reencryptEnv {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: encTestRedisAddr(), DB: 4})
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flush db: %s", err)
	}
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	m := meta.NewClient(reencryptTestMeta(), conf)
	format := &meta.Format{
		Name: "reenc-test", UUID: "11111111-2222-3333-4444-555555555555",
		Storage: "mem", Bucket: "test", BlockSize: 4096, Compression: "none",
		TrashDays: 1, MetaVersion: meta.MaxVersion,
		EncryptionEnabled: true, KEKVersion: 1,
	}
	if err := m.Init(format, true); err != nil {
		t.Fatalf("init: %s", err)
	}
	blob, err := object.CreateStorage("mem", "test", "", "", "")
	if err != nil {
		t.Fatalf("storage: %s", err)
	}
	chunkConf := *getDefaultChunkConf(format)
	chunkConf.CacheDir = "memory"
	store := chunk.NewCachedStore(blob, chunkConf, nil)
	m.OnMsg(meta.DeleteSlice, func(args ...interface{}) error {
		return store.Remove(args[0].(uint64), int(args[1].(uint32)))
	})
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i*7 + 1)
	}
	return &reencryptEnv{m: m, store: store, chunkConf: chunkConf, kek: kek, uuid: format.UUID, company: "test-company"}
}

// pattern returns n deterministic bytes distinguishable per seed.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) ^ seed
	}
	return b
}

// putSlice uploads data as a slice and appends it to the chunk list at pos.
func putSlice(t *testing.T, env *reencryptEnv, inode meta.Ino, indx uint32, pos uint32, data []byte) {
	t.Helper()
	var id uint64
	if st := env.m.NewSlice(meta.Background(), &id); st != 0 {
		t.Fatalf("newslice: %v", st)
	}
	w := env.store.NewWriter(id, 0)
	if _, err := w.WriteAt(data, 0); err != nil {
		t.Fatalf("write slice: %s", err)
	}
	if err := w.Finish(len(data)); err != nil {
		t.Fatalf("finish slice: %s", err)
	}
	st := env.m.Write(meta.Background(), inode, indx, pos, meta.Slice{Id: id, Size: uint32(len(data)), Off: 0, Len: uint32(len(data))}, time.Now())
	if st != 0 {
		t.Fatalf("write: %v", st)
	}
}

// readFileData reads the whole file through meta + store, decrypting slices as
// needed (legacy files are read as plaintext passthrough).
func readFileData(t *testing.T, env *reencryptEnv, inode meta.Ino) []byte {
	t.Helper()
	ctx := meta.Background()
	var attr meta.Attr
	if st := env.m.GetAttr(ctx, inode, &attr); st != 0 {
		t.Fatalf("getattr: %v", st)
	}
	var fek []byte
	if attr.Encrypted {
		aad := meta.FekAAD{VolumeUUID: env.uuid, CompanyID: env.company, DriveFileID: attr.DriveFileID, Inode: inode, FekVersion: attr.FekVersion}
		f, _, err := meta.UnwrapFEK(env.kek, attr.WrappedFek, aad)
		if err != nil {
			t.Fatalf("unwrap FEK: %s", err)
		}
		fek = f
	}
	out := make([]byte, attr.Length)
	nChunks := uint32((attr.Length + meta.ChunkSize - 1) / meta.ChunkSize)
	for indx := uint32(0); indx < nChunks; indx++ {
		var list []meta.Slice
		if st := env.m.Read(ctx, inode, indx, &list); st != 0 {
			t.Fatalf("read chunk %d: %v", indx, st)
		}
		pos := uint32(0)
		for _, s := range list {
			var key []byte
			if len(s.WrappedCEK) > 0 {
				k, err := chunk.UnwrapCEK(fek, s.WrappedCEK, attr.DriveFileID, s.Id, attr.FekVersion)
				if err != nil {
					t.Fatalf("unwrap CEK: %s", err)
				}
				key = k
			}
			r := env.store.NewReaderWithKey(s.Id, int(s.Size), key)
			page := chunk.NewPage(make([]byte, s.Len))
			n, err := r.ReadAt(ctx, page, int(s.Off))
			if err != nil && err != io.EOF {
				t.Fatalf("read slice %d: %s", s.Id, err)
			}
			off := int64(indx)*int64(meta.ChunkSize) + int64(pos) + int64(s.Off)
			copy(out[off:], page.Data[:n])
			pos += s.Len
		}
	}
	return out
}

// TestReencrypt_RoundTrip (task 8.2): a legacy file with two chunks is
// migrated; every chunk becomes a single encrypted slice and the data reads
// back byte-identical.
func TestReencrypt_RoundTrip(t *testing.T) {
	env := newReencryptEnv(t)
	var inode meta.Ino
	var attr meta.Attr
	if st := env.m.Mknod(meta.Background(), meta.RootInode, "legacy", meta.TypeFile, 0644, 022, 0, "", &inode, &attr); st != 0 {
		t.Fatalf("mknod: %v", st)
	}

	d0a := pattern(1<<20, 5)
	d0b := pattern(512<<10, 9)
	d1 := pattern(256<<10, 13)
	putSlice(t, env, inode, 0, 0, d0a)
	putSlice(t, env, inode, 0, uint32(len(d0a)), d0b)
	putSlice(t, env, inode, 1, 0, d1)

	w := newReencryptWorker(env.m, env.store, env.chunkConf, env.kek, 1, env.uuid, env.company, false, 2, 0, 0)
	if err := w.run(context.Background(), ""); err != nil {
		t.Fatalf("run: %s", err)
	}

	var after meta.Attr
	if st := env.m.GetAttr(meta.Background(), inode, &after); st != 0 {
		t.Fatalf("getattr: %v", st)
	}
	if !after.Encrypted || len(after.WrappedFek) == 0 || after.DriveFileID == "" {
		t.Fatalf("file is not marked encrypted: %+v", after)
	}
	for indx := uint32(0); indx < 2; indx++ {
		var list []meta.Slice
		if st := env.m.Read(meta.Background(), inode, indx, &list); st != 0 {
			t.Fatalf("read: %v", st)
		}
		if len(list) != 1 || len(list[0].WrappedCEK) == 0 {
			t.Fatalf("chunk %d is not a single encrypted slice: %+v", indx, list)
		}
	}

	want := make([]byte, after.Length)
	copy(want, d0a)
	copy(want[len(d0a):], d0b)
	copy(want[meta.ChunkSize:], d1)
	if got := readFileData(t, env, inode); !bytes.Equal(got, want) {
		t.Fatalf("data mismatch after reencrypt")
	}
}

// TestReencrypt_Idempotent (task 8.2): a second run over a fully migrated file
// swaps nothing — every chunk is recognized as already encrypted.
func TestReencrypt_Idempotent(t *testing.T) {
	env := newReencryptEnv(t)
	var inode meta.Ino
	var attr meta.Attr
	if st := env.m.Mknod(meta.Background(), meta.RootInode, "legacy", meta.TypeFile, 0644, 022, 0, "", &inode, &attr); st != 0 {
		t.Fatalf("mknod: %v", st)
	}
	d0 := pattern(1<<20, 5)
	d1 := pattern(256<<10, 13)
	putSlice(t, env, inode, 0, 0, d0)
	putSlice(t, env, inode, 1, 0, d1)

	mkWorker := func() *reencryptWorker {
		return newReencryptWorker(env.m, env.store, env.chunkConf, env.kek, 1, env.uuid, env.company, false, 2, 0, 0)
	}

	w1 := mkWorker()
	if err := w1.run(context.Background(), ""); err != nil {
		t.Fatalf("first run: %s", err)
	}
	if w1.chunksSwapped != 2 || w1.chunksSkipped != 0 {
		t.Fatalf("first run: swapped=%d skipped=%d, want 2/0", w1.chunksSwapped, w1.chunksSkipped)
	}

	w2 := mkWorker()
	if err := w2.run(context.Background(), ""); err != nil {
		t.Fatalf("second run: %s", err)
	}
	if w2.chunksSwapped != 0 || w2.chunksSkipped != 2 {
		t.Fatalf("second run: swapped=%d skipped=%d, want 0/2", w2.chunksSwapped, w2.chunksSkipped)
	}

	want := make([]byte, meta.ChunkSize+uint64(len(d1)))
	copy(want, d0)
	copy(want[meta.ChunkSize:], d1)
	if got := readFileData(t, env, inode); !bytes.Equal(got, want) {
		t.Fatalf("data mismatch after idempotent re-run")
	}
}

// TestReencrypt_Resume (task 8.2): a partially migrated file (FEK set, chunk 0
// already swapped) is finished by a fresh run — only the remaining chunk is
// swapped and the data stays intact.
func TestReencrypt_Resume(t *testing.T) {
	env := newReencryptEnv(t)
	var inode meta.Ino
	var attr meta.Attr
	if st := env.m.Mknod(meta.Background(), meta.RootInode, "legacy", meta.TypeFile, 0644, 022, 0, "", &inode, &attr); st != 0 {
		t.Fatalf("mknod: %v", st)
	}
	d0 := pattern(1<<20, 5)
	d1 := pattern(256<<10, 13)
	putSlice(t, env, inode, 0, 0, d0)
	putSlice(t, env, inode, 1, 0, d1)

	// simulate an interrupted run: FEK persisted, chunk 0 already migrated
	fek, err := meta.NewFEK()
	if err != nil {
		t.Fatalf("new FEK: %s", err)
	}
	const dfid = "dfid-resume"
	aad := meta.FekAAD{VolumeUUID: env.uuid, CompanyID: env.company, DriveFileID: dfid, Inode: inode, FekVersion: 1}
	wrapped, err := meta.WrapFEK(env.kek, fek, aad, 1)
	if err != nil {
		t.Fatalf("wrap FEK: %s", err)
	}
	setter, ok := env.m.(fileCryptoSetter)
	if !ok {
		t.Fatal("meta engine does not support SetFileCrypto")
	}
	if st := setter.SetFileCrypto(meta.Background(), inode, &meta.FileCrypto{WrappedFek: wrapped, DriveFileID: dfid, FekVersion: 1, CryptoAlg: reencryptCryptoAlg}); st != 0 {
		t.Fatalf("setfilecrypto: %v", st)
	}
	w1 := newReencryptWorker(env.m, env.store, env.chunkConf, env.kek, 1, env.uuid, env.company, false, 1, 0, 0)
	if err := w1.migrateChunk(meta.WrapContext(context.Background()), inode, 0, 0, fek, dfid, 1); err != nil {
		t.Fatalf("manual chunk 0 migration: %s", err)
	}

	// a fresh run must skip chunk 0 and migrate only chunk 1
	w2 := newReencryptWorker(env.m, env.store, env.chunkConf, env.kek, 1, env.uuid, env.company, false, 1, 0, 0)
	if err := w2.run(context.Background(), ""); err != nil {
		t.Fatalf("run: %s", err)
	}
	if w2.chunksSwapped != 1 || w2.chunksSkipped != 1 {
		t.Fatalf("resume: swapped=%d skipped=%d, want 1/1", w2.chunksSwapped, w2.chunksSkipped)
	}

	want := make([]byte, meta.ChunkSize+uint64(len(d1)))
	copy(want, d0)
	copy(want[meta.ChunkSize:], d1)
	if got := readFileData(t, env, inode); !bytes.Equal(got, want) {
		t.Fatalf("data mismatch after resume")
	}
}

// TestReencrypt_RotateCEK (task 8.4): rotation re-wraps every chunk under a
// fresh CEK — the slice record changes, the data stays intact, and the old
// object becomes a GC candidate via the swap's refcount handling.
func TestReencrypt_RotateCEK(t *testing.T) {
	env := newReencryptEnv(t)
	var inode meta.Ino
	var attr meta.Attr
	if st := env.m.Mknod(meta.Background(), meta.RootInode, "legacy", meta.TypeFile, 0644, 022, 0, "", &inode, &attr); st != 0 {
		t.Fatalf("mknod: %v", st)
	}
	d0 := pattern(1<<20, 5)
	putSlice(t, env, inode, 0, 0, d0)

	// migrate first
	w1 := newReencryptWorker(env.m, env.store, env.chunkConf, env.kek, 1, env.uuid, env.company, false, 1, 0, 0)
	if err := w1.run(context.Background(), ""); err != nil {
		t.Fatalf("migrate: %s", err)
	}
	var before []meta.Slice
	if st := env.m.Read(meta.Background(), inode, 0, &before); st != 0 {
		t.Fatalf("read: %v", st)
	}
	if len(before) != 1 || len(before[0].WrappedCEK) == 0 {
		t.Fatalf("not migrated: %+v", before)
	}

	// rotate the CEK
	w2 := newReencryptWorker(env.m, env.store, env.chunkConf, env.kek, 1, env.uuid, env.company, true, 1, 0, 0)
	if err := w2.run(context.Background(), ""); err != nil {
		t.Fatalf("rotate: %s", err)
	}
	if w2.chunksSwapped != 1 || w2.chunksSkipped != 0 {
		t.Fatalf("rotation: swapped=%d skipped=%d, want 1/0", w2.chunksSwapped, w2.chunksSkipped)
	}
	var after []meta.Slice
	if st := env.m.Read(meta.Background(), inode, 0, &after); st != 0 {
		t.Fatalf("read: %v", st)
	}
	if len(after) != 1 || bytes.Equal(after[0].WrappedCEK, before[0].WrappedCEK) {
		t.Fatalf("CEK was not rotated: %+v", after)
	}
	if got := readFileData(t, env, inode); !bytes.Equal(got, d0) {
		t.Fatal("data mismatch after rotation")
	}
}

// fakeKeyManagerServer is an in-process DriveKeyManagerService for the
// enable-encryption test.
type fakeKeyManagerServer struct {
	kmpb.UnimplementedDriveKeyManagerServiceServer
	kek []byte
}

func (f *fakeKeyManagerServer) FetchCompanyKEK(ctx context.Context, req *kmpb.FetchCompanyKEKRequest) (*kmpb.FetchCompanyKEKResponse, error) {
	return &kmpb.FetchCompanyKEKResponse{Kek: f.kek, KekVersion: 1}, nil
}

func (f *fakeKeyManagerServer) ProvisionCompanyKEK(ctx context.Context, req *kmpb.ProvisionCompanyKEKRequest) (*kmpb.ProvisionCompanyKEKResponse, error) {
	return &kmpb.ProvisionCompanyKEKResponse{KekVersion: 1, Created: true}, nil
}

func startFakeKeyManager(t *testing.T) (addr string, kek []byte) {
	t.Helper()
	kek = make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 100)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}
	srv := grpc.NewServer()
	kmpb.RegisterDriveKeyManagerServiceServer(srv, &fakeKeyManagerServer{kek: kek})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), kek
}

// TestLegacyReadable_AfterEnable (task 8.3, FR-MIG-1): enable-encryption
// provisions the KEK and marks the volume encrypted; legacy files stay
// readable as plaintext passthrough; a second run is idempotent.
func TestLegacyReadable_AfterEnable(t *testing.T) {
	kmAddr, _ := startFakeKeyManager(t)
	metaURL := "redis://" + encTestRedisAddr() + "/5"
	rdb := redis.NewClient(&redis.Options{Addr: encTestRedisAddr(), DB: 5})
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flush db: %s", err)
	}

	// legacy volume: encryption disabled
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	m := meta.NewClient(metaURL, conf)
	format := &meta.Format{
		Name: "reenc-legacy", UUID: "22222222-3333-4444-5555-666666666666",
		Storage: "mem", Bucket: "test", BlockSize: 4096, Compression: "none",
		TrashDays: 1, MetaVersion: meta.MaxVersion,
	}
	if err := m.Init(format, true); err != nil {
		t.Fatalf("init: %s", err)
	}
	blob, err := object.CreateStorage("mem", "test", "", "", "")
	if err != nil {
		t.Fatalf("storage: %s", err)
	}
	chunkConf := *getDefaultChunkConf(format)
	chunkConf.CacheDir = "memory"
	store := chunk.NewCachedStore(blob, chunkConf, nil)
	m.OnMsg(meta.DeleteSlice, func(args ...interface{}) error {
		return store.Remove(args[0].(uint64), int(args[1].(uint32)))
	})

	// a legacy file with plaintext data
	var inode meta.Ino
	var attr meta.Attr
	if st := m.Mknod(meta.Background(), meta.RootInode, "legacy", meta.TypeFile, 0644, 022, 0, "", &inode, &attr); st != 0 {
		t.Fatalf("mknod: %v", st)
	}
	data := pattern(1<<20, 7)
	var id uint64
	if st := m.NewSlice(meta.Background(), &id); st != 0 {
		t.Fatalf("newslice: %v", st)
	}
	w := store.NewWriter(id, 0)
	if _, err := w.WriteAt(data, 0); err != nil {
		t.Fatalf("write slice: %s", err)
	}
	if err := w.Finish(len(data)); err != nil {
		t.Fatalf("finish slice: %s", err)
	}
	if st := m.Write(meta.Background(), inode, 0, 0, meta.Slice{Id: id, Size: uint32(len(data)), Off: 0, Len: uint32(len(data))}, time.Now()); st != 0 {
		t.Fatalf("write: %v", st)
	}

	const company = "company-1"
	enableArgs := []string{"", "enable-encryption", metaURL, "--company-id", company, "--keymanager-service", kmAddr}
	if err := Main(enableArgs); err != nil {
		t.Fatalf("enable-encryption: %s", err)
	}

	format2, err := m.Load(true)
	if err != nil {
		t.Fatalf("load: %s", err)
	}
	if !format2.EncryptionEnabled || format2.KEKVersion != 1 {
		t.Fatalf("format not updated: %+v", format2)
	}

	// the legacy file is untouched and still readable (plaintext passthrough)
	var a meta.Attr
	if st := m.GetAttr(meta.Background(), inode, &a); st != 0 {
		t.Fatalf("getattr: %v", st)
	}
	if a.Encrypted {
		t.Fatal("legacy file must stay unencrypted")
	}
	r := store.NewReader(id, len(data))
	page := chunk.NewPage(make([]byte, len(data)))
	n, err := r.ReadAt(context.Background(), page, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("legacy read: %s", err)
	}
	if n != len(data) || !bytes.Equal(page.Data[:n], data) {
		t.Fatal("legacy data mismatch")
	}

	// idempotent second run
	if err := Main(enableArgs); err != nil {
		t.Fatalf("second enable-encryption: %s", err)
	}
	format3, err := m.Load(true)
	if err != nil {
		t.Fatalf("load: %s", err)
	}
	if !format3.EncryptionEnabled || format3.KEKVersion != 1 {
		t.Fatalf("format changed by idempotent run: %+v", format3)
	}
}

// TestRateLimit (task 8.5): with --iops 50 a file of 100 chunks takes at least
// ~2 s to migrate and the measured swap rate stays under the limit.
func TestRateLimit(t *testing.T) {
	env := newReencryptEnv(t)
	var inode meta.Ino
	var attr meta.Attr
	if st := env.m.Mknod(meta.Background(), meta.RootInode, "big", meta.TypeFile, 0644, 022, 0, "", &inode, &attr); st != 0 {
		t.Fatalf("mknod: %v", st)
	}
	const nChunks = 100
	for indx := uint32(0); indx < nChunks; indx++ {
		putSlice(t, env, inode, indx, 0, pattern(4096, byte(indx)))
	}

	w := newReencryptWorker(env.m, env.store, env.chunkConf, env.kek, 1, env.uuid, env.company, false, 1, 50, 0)
	start := time.Now()
	if err := w.run(context.Background(), ""); err != nil {
		t.Fatalf("run: %s", err)
	}
	elapsed := time.Since(start)
	if w.chunksSwapped != nChunks {
		t.Fatalf("swapped=%d, want %d", w.chunksSwapped, nChunks)
	}
	if elapsed < 1900*time.Millisecond {
		t.Fatalf("%d swaps at 50 IOPS took %v, want >= ~1.9s", nChunks, elapsed)
	}
	if iops := float64(nChunks) / elapsed.Seconds(); iops > 60 {
		t.Fatalf("measured IOPS %.1f exceeds the 50 limit (with margin)", iops)
	}
}

// readOnce reads the whole file once; any unreadable slice is an error. It is
// safe to call concurrently: it never touches test state.
func readOnce(env *reencryptEnv, inode meta.Ino) error {
	ctx := meta.Background()
	var attr meta.Attr
	if st := env.m.GetAttr(ctx, inode, &attr); st != 0 {
		return st
	}
	var fek []byte
	if attr.Encrypted {
		aad := meta.FekAAD{VolumeUUID: env.uuid, CompanyID: env.company, DriveFileID: attr.DriveFileID, Inode: inode, FekVersion: attr.FekVersion}
		f, _, err := meta.UnwrapFEK(env.kek, attr.WrappedFek, aad)
		if err != nil {
			return err
		}
		fek = f
	}
	nChunks := uint32((attr.Length + meta.ChunkSize - 1) / meta.ChunkSize)
	for indx := uint32(0); indx < nChunks; indx++ {
		var list []meta.Slice
		if st := env.m.Read(ctx, inode, indx, &list); st != 0 {
			return st
		}
		for _, s := range list {
			var key []byte
			if len(s.WrappedCEK) > 0 {
				k, err := chunk.UnwrapCEK(fek, s.WrappedCEK, attr.DriveFileID, s.Id, attr.FekVersion)
				if err != nil {
					return err
				}
				key = k
			}
			r := env.store.NewReaderWithKey(s.Id, int(s.Size), key)
			page := chunk.NewPage(make([]byte, s.Len))
			if _, err := r.ReadAt(ctx, page, int(s.Off)); err != nil && err != io.EOF {
				return err
			}
		}
	}
	return nil
}

// TestReencrypt_ConcurrentReadWrite (task 8.5, FR-MIG-6): while the worker
// migrates a file, a reader keeps reading it (stale chunk lists included) and
// a writer keeps appending — new writes go encrypted. No read may fail and the
// final data must be intact.
func TestReencrypt_ConcurrentReadWrite(t *testing.T) {
	env := newReencryptEnv(t)
	var inode meta.Ino
	var attr meta.Attr
	if st := env.m.Mknod(meta.Background(), meta.RootInode, "rw", meta.TypeFile, 0644, 022, 0, "", &inode, &attr); st != 0 {
		t.Fatalf("mknod: %v", st)
	}
	dA := pattern(1<<20, 11)
	putSlice(t, env, inode, 0, 0, dA)

	// reader: continuously reads the file — must never fail
	var readErrs int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := readOnce(env, inode); err != nil {
				atomic.AddInt64(&readErrs, 1)
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	// writer: once the file is marked encrypted (the worker does this before
	// any chunk swap), appends encrypted slices to chunk 0. Meta writes are
	// retried on conflict with the worker's swap transaction, like a real VFS.
	var writeErrs int64
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		var a meta.Attr
		for {
			if st := env.m.GetAttr(meta.Background(), inode, &a); st == 0 && a.Encrypted {
				break
			}
			time.Sleep(time.Millisecond)
		}
		aad := meta.FekAAD{VolumeUUID: env.uuid, CompanyID: env.company, DriveFileID: a.DriveFileID, Inode: inode, FekVersion: a.FekVersion}
		fek, _, err := meta.UnwrapFEK(env.kek, a.WrappedFek, aad)
		if err != nil {
			atomic.AddInt64(&writeErrs, 1)
			return
		}
		const blockSize = 256 << 10
		for i := 1; i <= 8; i++ {
			var id uint64
			if st := env.m.NewSlice(meta.Background(), &id); st != 0 {
				atomic.AddInt64(&writeErrs, 1)
				return
			}
			cek := make([]byte, 32)
			for j := range cek {
				cek[j] = byte(i*31 + j)
			}
			wrappedCEK, err := chunk.WrapCEK(fek, cek, a.DriveFileID, id, a.FekVersion)
			if err != nil {
				atomic.AddInt64(&writeErrs, 1)
				return
			}
			data := pattern(blockSize, byte(i))
			w := env.store.NewWriterWithKey(id, 0, cek)
			if _, err := w.WriteAt(data, 0); err != nil {
				atomic.AddInt64(&writeErrs, 1)
				return
			}
			if err := w.Finish(len(data)); err != nil {
				atomic.AddInt64(&writeErrs, 1)
				return
			}
			pos := uint32(i * blockSize)
			var st syscall.Errno
			for attempt := 0; attempt < 100; attempt++ {
				st = env.m.Write(meta.Background(), inode, 0, pos, meta.Slice{Id: id, Size: blockSize, Off: 0, Len: blockSize, WrappedCEK: wrappedCEK}, time.Now())
				if st == 0 {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			if st != 0 {
				atomic.AddInt64(&writeErrs, 1)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	w := newReencryptWorker(env.m, env.store, env.chunkConf, env.kek, 1, env.uuid, env.company, false, 1, 0, 0)
	if err := w.run(context.Background(), ""); err != nil {
		t.Fatalf("run: %s", err)
	}

	<-writerDone
	close(stop)

	if n := atomic.LoadInt64(&readErrs); n != 0 {
		t.Fatalf("%d read errors during migration (FR-MIG-6)", n)
	}
	if n := atomic.LoadInt64(&writeErrs); n != 0 {
		t.Fatalf("%d write errors during migration (FR-MIG-6)", n)
	}
	var after meta.Attr
	if st := env.m.GetAttr(meta.Background(), inode, &after); st != 0 {
		t.Fatalf("getattr: %v", st)
	}
	// blocks i occupy [i*blockSize, (i+1)*blockSize): the first three overlap
	// dA and overwrite it, so the file ends at 9*blockSize
	const blockSize = 256 << 10
	wantLen := uint64(len(dA))
	if end := uint64(9 * blockSize); end > wantLen {
		wantLen = end
	}
	if after.Length != wantLen {
		t.Fatalf("length %d, want %d", after.Length, wantLen)
	}
	want := make([]byte, wantLen)
	copy(want, dA)
	for i := 1; i <= 8; i++ {
		copy(want[i*blockSize:(i+1)*blockSize], pattern(blockSize, byte(i)))
	}
	if got := readFileData(t, env, inode); !bytes.Equal(got, want) {
		t.Fatal("data mismatch after concurrent migration")
	}
}
