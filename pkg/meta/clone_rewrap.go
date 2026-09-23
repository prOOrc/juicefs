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
	"syscall"
)

// sliceRewrapper is implemented by metadata engines that can re-wrap the CEKs of
// shared slices under a new file FEK without touching S3 objects (redisMeta, task 5.1).
type sliceRewrapper interface {
	RewrapSlices(ctx Context, dstIno Ino, srcFek, dstFek []byte, src, dst SliceCryptoAAD) syscall.Errno
}

// sliceRangeRewrapper is the CopyFileRange variant: only the chunk lists covering
// the copied range are re-wrapped (task 5.5).
type sliceRangeRewrapper interface {
	RewrapSlicesRange(ctx Context, dstIno Ino, srcFek, dstFek []byte, src, dst SliceCryptoAAD, startIndx, endIndx uint32) syscall.Errno
}

// cloneKeyOps resolves the key material for one cloned file. Proxy: KeyManager RPCs
// (authz enforced by the platform). Render: local unwrap/generate under the Company KEK.
type cloneKeyOps struct {
	// srcFek returns the plaintext FEK of the source file.
	srcFek func(srcIno Ino, srcPath string, srcAttr *Attr) ([]byte, error)
	// dstFek creates a fresh FEK for the target and returns the plaintext key
	// plus the crypto metadata to persist via SetFileCrypto.
	dstFek func(dstIno Ino, dstPath string) (fek []byte, crypto *FileCrypto, err error)
}

// cloneRewrap gives a clone target its own FEK and re-wraps every shared slice's
// CEK under it (FR-OP-1..3, D7): zero-copy, S3 objects are never rewritten. For a
// directory it walks the source tree and applies the same sequence to every
// encrypted file; the destination tree mirrors it (names are identical after
// Clone). Legacy files pass through untouched. Any failure is returned as-is; the
// caller must roll back the whole clone — a target with foreign key material is
// unusable (fail-closed).
func cloneRewrap(ctx Context, m Meta, srcIno, dstIno Ino, srcPath, dstPath string, ops *cloneKeyOps) syscall.Errno {
	var srcAttr Attr
	if st := m.GetAttr(ctx, srcIno, &srcAttr); st != 0 {
		return st
	}
	if !srcAttr.Encrypted || len(srcAttr.WrappedFek) == 0 {
		return 0 // legacy — the verbatim clone is fine
	}
	if srcAttr.Typ == TypeDirectory {
		var entries []*Entry
		if st := m.Readdir(ctx, srcIno, 1, &entries); st != 0 {
			return st
		}
		for _, e := range entries {
			var childDstIno Ino
			var _a Attr
			if st := m.Lookup(ctx, dstIno, string(e.Name), &childDstIno, &_a, false); st != 0 {
				return st
			}
			if st := cloneRewrap(ctx, m, e.Inode, childDstIno, srcPath+"/"+string(e.Name), dstPath+"/"+string(e.Name), ops); st != 0 {
				return st
			}
		}
		return 0
	}
	if srcAttr.Typ != TypeFile {
		return 0 // symlinks etc. carry no data
	}

	srcFek, err := ops.srcFek(srcIno, srcPath, &srcAttr)
	if err != nil {
		return syscall.EACCES // fail-closed (NFR-AVAIL-3)
	}
	dstFek, crypto, err := ops.dstFek(dstIno, dstPath)
	if err != nil {
		return syscall.EACCES // fail-closed
	}
	setter, ok := m.(fileCryptoSetter)
	if !ok {
		return syscall.EIO
	}
	if st := setter.SetFileCrypto(ctx, dstIno, crypto); st != 0 {
		return st
	}
	rewrapper, ok := m.(sliceRewrapper)
	if !ok {
		return syscall.EOPNOTSUPP
	}
	return rewrapper.RewrapSlices(ctx, dstIno, srcFek, dstFek,
		SliceCryptoAAD{DriveFileID: srcAttr.DriveFileID, FekVersion: srcAttr.FekVersion},
		SliceCryptoAAD{DriveFileID: crypto.DriveFileID, FekVersion: crypto.FekVersion})
}
