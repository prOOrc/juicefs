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

package meta

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"time"

	chunkenc "github.com/juicedata/juicefs/pkg/chunk"
	"github.com/redis/go-redis/v9"
)

// rewrapTestHook, if set, runs between the origin snapshot and the conflict
// txn of rewrapChunkList — a test seam for deterministically landing a
// concurrent write inside the checked window (TestRewrapSlices_Conflict).
var rewrapTestHook func()

// FileCrypto — crypto metadata of a file to be written into the inode attr.
type FileCrypto struct {
	WrappedFek  []byte
	DriveFileID string
	FekVersion  uint32
	CryptoAlg   string // "AES-256-GCM"
}

// SetFileCrypto writes the crypto fields into the inode attr in a single Redis
// transaction (GET attr -> mutate crypto fields -> SET). The plaintext FEK is
// never stored — only the AGFK blob wrapped under the company KEK.
func (m *redisMeta) SetFileCrypto(ctx Context, inode Ino, c *FileCrypto) syscall.Errno {
	var attr Attr
	err := m.txn(ctx, func(tx *redis.Tx) error {
		a, err := tx.Get(ctx, m.inodeKey(inode)).Bytes()
		if err != nil {
			return err
		}
		m.parseAttr(a, &attr)
		attr.Encrypted = true
		attr.WrappedFek = c.WrappedFek
		attr.DriveFileID = c.DriveFileID
		attr.FekVersion = c.FekVersion
		attr.CryptoAlg = c.CryptoAlg
		now := time.Now()
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, m.inodeKey(inode), m.marshal(&attr), 0)
			m.genLog(ctx, pipe, now, "SETFILECRYPTO(%d,%s,%d)", inode, c.DriveFileID, c.FekVersion)
			return nil
		})
		return err
	}, m.inodeKey(inode))
	if err != nil {
		return errno(err)
	}
	return 0
}

// SliceCryptoAAD — AAD for AGCK wraps (design.md, «Межэтапные контракты»): binds a
// wrapped CEK to the file identity it belongs to. The slice ID is shared between
// source and target (clone/copy share slice objects), so only the file-level
// components differ between src and dst.
type SliceCryptoAAD struct {
	DriveFileID string
	FekVersion  uint32
}

// RewrapSlices re-wraps every wrapped CEK (AGCK tail) in the chunk lists of inode
// dstIno from under srcFek to under dstFek. S3 data is not touched (FR-OP-2, AC-7):
// only the slice records in Redis change. Legacy records (no AGCK tail) pass through
// unchanged. One transaction per chunk list; an unwrap failure fails closed with EIO
// and the caller must treat dstIno as unusable (design 5.2: Unlink on error).
func (m *redisMeta) RewrapSlices(ctx Context, dstIno Ino, srcFek, dstFek []byte, src, dst SliceCryptoAAD) syscall.Errno {
	var attr Attr
	if st := m.GetAttr(ctx, dstIno, &attr); st != 0 {
		return st
	}
	for indx := uint32(0); indx <= uint32(attr.Length/ChunkSize); indx++ {
		if st := m.rewrapChunkList(ctx, dstIno, indx, srcFek, dstFek, src, dst); st != 0 {
			return st
		}
	}
	return 0
}

// RewrapSlicesRange is RewrapSlices restricted to chunk lists [startIndx..endIndx] —
// used by CopyFileRange, where only the copied range shares slices with the source
// (FR-OP-4); the rest of the target keeps its own CEKs.
func (m *redisMeta) RewrapSlicesRange(ctx Context, dstIno Ino, srcFek, dstFek []byte, src, dst SliceCryptoAAD, startIndx, endIndx uint32) syscall.Errno {
	for indx := startIndx; indx <= endIndx; indx++ {
		if st := m.rewrapChunkList(ctx, dstIno, indx, srcFek, dstFek, src, dst); st != 0 {
			return st
		}
	}
	return 0
}

// maxRewrapAttempts bounds the origin-check retry loop of rewrapChunkList: a
// concurrent writer keeps appending while the rotation iterates, and each
// conflict costs one re-snapshot + recompute round.
const maxRewrapAttempts = 50

// rewrapChunkList re-wraps the AGCK tails of one chunk list: snapshot → compute
// → txn{verify origin → LSet}. Records without an AGCK tail (legacy) are left
// as-is. Any unwrap failure aborts with EIO (fail-closed) — no partial rewrite
// of the list. If a concurrent writer changed the list between the snapshot and
// the transaction, the apply is rejected with EAGAIN and the whole round is
// repeated from a fresh snapshot (design Addendum A1): the new record is picked
// up and re-wrapped correctly, because the proxy still holds the old FEK.
func (m *redisMeta) rewrapChunkList(ctx Context, ino Ino, indx uint32, srcFek, dstFek []byte, src, dst SliceCryptoAAD) syscall.Errno {
	key := m.chunkKey(ino, indx)
	for attempt := 0; ; attempt++ {
		vals, err := m.rdb.LRange(ctx, key, 0, -1).Result()
		if err != nil {
			return errno(err)
		}
		if len(vals) == 0 {
			return 0 // hole or empty chunk list
		}
		newVals := make([]string, len(vals))
		changed := false
		for i, val := range vals {
			nv, err := rewrapSliceRecord(val, srcFek, dstFek, src, dst)
			if err != nil {
				return errno(err) // fail-closed: no partial rewrite
			}
			newVals[i] = nv
			if nv != val {
				changed = true
			}
		}
		if !changed {
			return 0
		}
		if rewrapTestHook != nil {
			rewrapTestHook()
		}
		st := errno(m.txn(ctx, func(tx *redis.Tx) error {
			cur, err := tx.LRange(ctx, key, 0, -1).Result()
			if err != nil {
				return err
			}
			if rewrapOriginChanged(vals, cur) {
				return syscall.EAGAIN // concurrent writer — retry from a fresh snapshot
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				for i, nv := range newVals {
					pipe.LSet(ctx, key, int64(i), nv)
				}
				m.genLog(ctx, pipe, time.Now(), "REWRAPSLICES(%d,%d)", ino, indx)
				return nil
			})
			return err
		}, key))
		if st != syscall.EAGAIN {
			return st
		}
		if attempt+1 >= maxRewrapAttempts {
			// EAGAIN is an internal signal of the origin check; callers (clone,
			// rotation, CopyFileRange) treat it as a transient conflict and must
			// not see it. Exhausting retries means the list kept changing under
			// sustained concurrent writes — fail closed like an unwrap failure.
			logger.Errorf("rewrap chunk %d/%d: list kept changing after %d attempts", ino, indx, attempt+1)
			return syscall.EIO
		}
		time.Sleep(time.Millisecond * 10)
	}
}

// rewrapOriginChanged reports whether the chunk list changed between the origin
// snapshot and the in-transaction re-read. Raw-record comparison is deliberately
// used instead of the buildSlice comparison that doReencryptChunk uses: re-wrap
// maps record i→i by index, so any raw change (including an appended duplicate
// of an identical record, which buildSlice would not see) invalidates the
// computed values; raw equality is strictly stronger and safe because the retry
// is idempotent.
func rewrapOriginChanged(origin, cur []string) bool {
	if len(origin) != len(cur) {
		return true
	}
	for i := range origin {
		if origin[i] != cur[i] {
			return true
		}
	}
	return false
}

// rewrapSliceRecord re-wraps the AGCK tail of one slice record from under srcFek to
// under dstFek. Records without an AGCK tail (legacy plaintext slices) are returned
// unchanged. An unwrap failure returns EIO (fail-closed, NFR-SEC-11).
func rewrapSliceRecord(val string, srcFek, dstFek []byte, src, dst SliceCryptoAAD) (string, error) {
	if len(val) <= sliceBytes {
		return val, nil // legacy record — no CEK to re-wrap
	}
	tail := len(val) - sliceBytes
	if tail < 4 {
		return "", fmt.Errorf("rewrap: corrupt slice record (tail %d)", tail)
	}
	blobLen := int(binary.BigEndian.Uint32([]byte(val[sliceBytes : sliceBytes+4])))
	if tail != 4+blobLen {
		return "", fmt.Errorf("rewrap: corrupt slice record (tail %d, blobLen %d)", tail, blobLen)
	}
	s := new(slice)
	s.read([]byte(val))
	cek, err := chunkenc.UnwrapCEK(srcFek, s.wrappedCEK, src.DriveFileID, s.id, src.FekVersion)
	if err != nil {
		// Mixed-version tolerance (design Addendum A1): a writer that re-resolved
		// its FEK mid-rotation commits under the NEW version while rotation is
		// still iterating. The fallback is safe because the GCM tag + AAD binding
		// make it fail-closed: only a genuine (dstFek, dstAAD) record passes.
		if _, err2 := chunkenc.UnwrapCEK(dstFek, s.wrappedCEK, dst.DriveFileID, s.id, dst.FekVersion); err2 == nil {
			return val, nil // already re-wrapped — leave byte-identical
		}
		return "", syscall.EIO // wrong FEK or tampered record — fail closed
	}
	newBlob, err := chunkenc.WrapCEK(dstFek, cek, dst.DriveFileID, s.id, dst.FekVersion)
	if err != nil {
		return "", fmt.Errorf("rewrap: wrap CEK: %w", err)
	}
	return string(marshalSliceCEK(s.pos, s.id, s.size, s.off, s.len, newBlob)), nil
}
