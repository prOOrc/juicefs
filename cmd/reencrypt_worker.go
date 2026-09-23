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
	"context"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/uuid"
	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/vfs"
	"golang.org/x/time/rate"
)

const (
	reencryptMaxAttempts = 5
	reencryptCryptoAlg   = "AES-256-GCM"
)

// fileCryptoSetter is implemented by metadata engines that can persist the
// per-file crypto fields (redisMeta; see pkg/meta/redis_fek.go).
type fileCryptoSetter interface {
	SetFileCrypto(ctx meta.Context, inode meta.Ino, c *meta.FileCrypto) syscall.Errno
}

// contiguousChunker is implemented by metadata engines that can resolve a
// chunk's true slice positions (redisMeta; the public Slice type carries no pos).
type contiguousChunker interface {
	ContiguousChunk(ctx meta.Context, inode meta.Ino, indx uint32) (pos uint32, size uint32, merged []meta.Slice, origin []meta.Slice, st syscall.Errno)
}

// reencryptWorker migrates legacy plaintext files to the encrypted format
// (stage 8). Per file: the FEK is generated and stored in the attr BEFORE any
// chunk swap (decision 8.6), so new writes during the migration go encrypted;
// then each chunk is merged into one slice under a fresh CEK and swapped
// atomically via ReencryptChunk (conflict → re-read and retry). Per chunk: a
// list where every slice already carries an AGCK tail is skipped, which makes
// the walk idempotent and resumable.
type reencryptWorker struct {
	m          meta.Meta
	store      chunk.ChunkStore
	chunkConf  chunk.Config
	kek        []byte
	kekVersion uint32
	volumeUUID string
	companyID  string
	rotateCEK  bool

	sem  chan struct{} // file-level concurrency
	iops *rate.Limiter // one token per chunk swap
	bw   *rate.Limiter // tokens = merged bytes

	mu            sync.Mutex
	filesMigrated int
	filesSkipped  int
	chunksSwapped int
	chunksSkipped int
	bytesMerged   int64
	errors        int
}

func newReencryptWorker(m meta.Meta, store chunk.ChunkStore, chunkConf chunk.Config,
	kek []byte, kekVersion uint32, volumeUUID, companyID string, rotateCEK bool,
	concurrency int, iops, bandwidth int64) *reencryptWorker {
	w := &reencryptWorker{
		m:          m,
		store:      store,
		chunkConf:  chunkConf,
		kek:        kek,
		kekVersion: kekVersion,
		volumeUUID: volumeUUID,
		companyID:  companyID,
		rotateCEK:  rotateCEK,
		sem:        make(chan struct{}, concurrency),
	}
	if iops > 0 {
		w.iops = rate.NewLimiter(rate.Limit(iops), 1)
	}
	if bandwidth > 0 {
		bps := rate.Limit(bandwidth) * 1024 * 1024
		w.bw = rate.NewLimiter(bps, int(bandwidth)*1024*1024) // 1s burst
	}
	return w
}

func (w *reencryptWorker) run(ctx context.Context, path string) error {
	mctx := meta.WrapContext(ctx)
	dir, err := w.resolvePath(mctx, path)
	if err != nil {
		return err
	}
	logger.Infof("reencrypt: walking from %q (inode %d), rotate-cek=%v", path, dir, w.rotateCEK)
	return w.walk(mctx, dir)
}

// resolvePath maps a volume-relative path to an inode ("" or "/" = root).
func (w *reencryptWorker) resolvePath(ctx meta.Context, path string) (meta.Ino, error) {
	inode := meta.RootInode
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part == "" {
			continue
		}
		var attr meta.Attr
		if st := w.m.Lookup(ctx, inode, part, &inode, &attr, false); st != 0 {
			return 0, fmt.Errorf("lookup %q: %s", part, st)
		}
	}
	return inode, nil
}

func (w *reencryptWorker) walk(ctx meta.Context, dir meta.Ino) error {
	var entries []*meta.Entry
	if st := w.m.Readdir(ctx, dir, 1, &entries); st != 0 {
		return fmt.Errorf("readdir %d: %s", dir, st)
	}
	for _, e := range entries {
		if ctx.Canceled() {
			return ctx.Err()
		}
		switch string(e.Name) {
		case ".", "..":
			continue // Readdir prepends dot entries
		}
		switch e.Attr.Typ {
		case meta.TypeDirectory:
			if e.Inode == meta.TrashInode {
				continue // deleted files are not migrated
			}
			if err := w.walk(ctx, e.Inode); err != nil {
				return err
			}
		case meta.TypeFile:
			w.processFile(ctx, e.Inode)
		}
	}
	return nil
}

func (w *reencryptWorker) processFile(ctx meta.Context, inode meta.Ino) {
	w.sem <- struct{}{}
	defer func() { <-w.sem }()
	if err := w.migrateFile(ctx, inode); err != nil {
		w.mu.Lock()
		w.errors++
		w.mu.Unlock()
		logger.Warnf("reencrypt: file %d: %s", inode, err)
	}
}

func (w *reencryptWorker) report() {
	w.mu.Lock()
	defer w.mu.Unlock()
	logger.Infof("reencrypt: done — %d files migrated, %d skipped (already encrypted), "+
		"%d chunks swapped (%s), %d chunks skipped, %d errors",
		w.filesMigrated, w.filesSkipped, w.chunksSwapped, humanize.IBytes(uint64(w.bytesMerged)), w.chunksSkipped, w.errors)
}

// migrateFile migrates one file (legacy → encrypted) or rotates its CEK. It is
// idempotent: a fully migrated file results in zero chunk swaps.
func (w *reencryptWorker) migrateFile(ctx meta.Context, inode meta.Ino) error {
	var attr meta.Attr
	if st := w.m.GetAttr(ctx, inode, &attr); st != 0 {
		return fmt.Errorf("getattr: %s", st)
	}
	if attr.Length == 0 {
		w.mu.Lock()
		w.filesSkipped++
		w.mu.Unlock()
		return nil
	}

	var fek []byte
	var driveFileID string
	var fekVersion uint32
	switch {
	case !attr.Encrypted:
		// Legacy file: generate the FEK and mark the file encrypted BEFORE any
		// chunk swap (decision 8.6) — new writes during the migration go
		// encrypted, legacy chunks are read as-is until their swap.
		f, err := meta.NewFEK()
		if err != nil {
			return err
		}
		fek = f
		driveFileID = uuid.New().String()
		fekVersion = 1
		aad := meta.FekAAD{VolumeUUID: w.volumeUUID, CompanyID: w.companyID, DriveFileID: driveFileID, Inode: inode, FekVersion: fekVersion}
		wrapped, err := meta.WrapFEK(w.kek, fek, aad, w.kekVersion)
		if err != nil {
			return fmt.Errorf("wrap FEK: %w", err)
		}
		setter, ok := w.m.(fileCryptoSetter)
		if !ok {
			return fmt.Errorf("meta engine does not support SetFileCrypto (Redis required)")
		}
		if st := setter.SetFileCrypto(ctx, inode, &meta.FileCrypto{
			WrappedFek: wrapped, DriveFileID: driveFileID, FekVersion: fekVersion, CryptoAlg: reencryptCryptoAlg,
		}); st != 0 {
			return fmt.Errorf("setfilecrypto: %s", st)
		}
	default:
		// Encrypted file: resume (partially migrated) or CEK rotation — unwrap
		// the FEK from the attr under the Company KEK. A KEK rotated in between
		// fails closed here and the file is skipped (design, stage 8 risks).
		aad := meta.FekAAD{VolumeUUID: w.volumeUUID, CompanyID: w.companyID, DriveFileID: attr.DriveFileID, Inode: inode, FekVersion: attr.FekVersion}
		f, kv, err := meta.UnwrapFEK(w.kek, attr.WrappedFek, aad)
		if err != nil {
			return fmt.Errorf("unwrap FEK: %w (KEK mismatch? file skipped)", err)
		}
		if kv != w.kekVersion {
			return fmt.Errorf("KEK version mismatch: attr v%d, current v%d (file skipped)", kv, w.kekVersion)
		}
		fek, driveFileID, fekVersion = f, attr.DriveFileID, attr.FekVersion
	}

	nChunks := uint32((attr.Length + meta.ChunkSize - 1) / meta.ChunkSize)
	for indx := uint32(0); indx < nChunks; indx++ {
		if ctx.Canceled() {
			return ctx.Err()
		}
		if err := w.migrateChunk(ctx, inode, indx, attr.Tier, fek, driveFileID, fekVersion); err != nil {
			return fmt.Errorf("chunk %d: %w", indx, err)
		}
	}
	w.mu.Lock()
	w.filesMigrated++
	w.mu.Unlock()
	return nil
}

// migrateChunk merges one chunk into a single encrypted slice and swaps it in
// atomically. Returns nil without a swap when the chunk is empty or already
// fully encrypted (idempotency). On EAGAIN (the list changed since the Read)
// the chunk is re-read and retried.
func (w *reencryptWorker) migrateChunk(ctx meta.Context, inode meta.Ino, indx uint32, tier uint8,
	fek []byte, driveFileID string, fekVersion uint32) error {
	cc, ok := w.m.(contiguousChunker)
	if !ok {
		return fmt.Errorf("meta engine does not support ContiguousChunk (Redis required)")
	}
	for attempt := 0; attempt < reencryptMaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 10 * time.Millisecond)
		}
		// One snapshot gives both the conflict-check origin and the merge plan
		// with true positions (the public Slice type carries no pos).
		pos, size, merged, list, st := cc.ContiguousChunk(ctx, inode, indx)
		if st != 0 {
			return fmt.Errorf("contiguous: %s", st)
		}
		if len(list) == 0 {
			return nil
		}
		allEncrypted := true
		for _, s := range list {
			if len(s.WrappedCEK) == 0 {
				allEncrypted = false
				break
			}
		}
		// A fully encrypted chunk is done — except in CEK rotation mode, where
		// it is exactly what must be re-wrapped under a fresh CEK.
		if allEncrypted && !w.rotateCEK {
			w.mu.Lock()
			w.chunksSkipped++
			w.mu.Unlock()
			return nil
		}
		if size == 0 {
			return nil
		}
		var id uint64
		if st := w.m.NewSlice(ctx, &id); st != 0 {
			return fmt.Errorf("newslice: %s", st)
		}

		// Rate limits (8.5): bandwidth over the merged bytes, IOPS per swap.
		if w.bw != nil {
			if err := w.bw.WaitN(ctx, int(size)); err != nil {
				return err
			}
		}
		wrappedCEK, err := vfs.ReencryptChunkData(w.chunkConf, w.store, merged, id, tier, fek, driveFileID, fekVersion)
		if err != nil {
			_ = w.store.Remove(id, int(size)) // drop the orphaned object
			return fmt.Errorf("merge: %w", err)
		}

		if w.iops != nil {
			if err := w.iops.Wait(ctx); err != nil {
				_ = w.store.Remove(id, int(size))
				return err
			}
		}
		newSlice := meta.Slice{Id: id, Size: size, Off: 0, Len: size, WrappedCEK: wrappedCEK}
		st = w.m.ReencryptChunk(ctx, inode, indx, list, newSlice, pos)
		switch st {
		case 0:
			w.mu.Lock()
			w.chunksSwapped++
			w.bytesMerged += int64(size)
			w.mu.Unlock()
			return nil
		case syscall.EAGAIN:
			// the list changed since the Read — drop the orphaned object and retry
			_ = w.store.Remove(id, int(size))
			continue
		case syscall.ENOSYS:
			return fmt.Errorf("meta engine does not support ReencryptChunk (Redis required): %w", syscall.ENOSYS)
		default:
			_ = w.store.Remove(id, int(size))
			return fmt.Errorf("reencryptchunk: %s", st)
		}
	}
	return fmt.Errorf("too many conflicts (list keeps changing)")
}
