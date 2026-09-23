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

// rewrapChunkList re-wraps the AGCK tails of one chunk list in a single transaction:
// LRange → unwrap under srcFek / wrap under dstFek → LSet. Records without an AGCK
// tail (legacy) are left as-is. Any unwrap failure aborts the transaction
// (fail-closed, EIO) — no partial rewrite of the list.
func (m *redisMeta) rewrapChunkList(ctx Context, ino Ino, indx uint32, srcFek, dstFek []byte, src, dst SliceCryptoAAD) syscall.Errno {
	key := m.chunkKey(ino, indx)
	err := m.txn(ctx, func(tx *redis.Tx) error {
		vals, err := tx.LRange(ctx, key, 0, -1).Result()
		if err != nil {
			return err
		}
		if len(vals) == 0 {
			return nil // hole or empty chunk list
		}
		newVals := make([]string, len(vals))
		changed := false
		for i, val := range vals {
			nv, err := rewrapSliceRecord(val, srcFek, dstFek, src, dst)
			if err != nil {
				return err // fail-closed: no partial rewrite
			}
			newVals[i] = nv
			if nv != val {
				changed = true
			}
		}
		if !changed {
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			for i, nv := range newVals {
				pipe.LSet(ctx, key, int64(i), nv)
			}
			m.genLog(ctx, pipe, time.Now(), "REWRAPSLICES(%d,%d)", ino, indx)
			return nil
		})
		return err
	}, key)
	if err != nil {
		return errno(err)
	}
	return 0
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
		return "", syscall.EIO // wrong FEK or tampered record — fail closed
	}
	newBlob, err := chunkenc.WrapCEK(dstFek, cek, dst.DriveFileID, s.id, dst.FekVersion)
	if err != nil {
		return "", fmt.Errorf("rewrap: wrap CEK: %w", err)
	}
	return string(marshalSliceCEK(s.pos, s.id, s.size, s.off, s.len, newBlob)), nil
}
