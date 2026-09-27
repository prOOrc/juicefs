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
	"fmt"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
)

// Render FEK cache parameters (FR-RND-7, NFR-PERF-5). The render node unwraps
// FEKs locally with the Company KEK, so the cache is larger and longer-lived
// than the user client's (100k/1h vs 100k/15min): render workloads keep the
// same files open for hours and re-opening must not pay an unwrap.
const (
	defaultRenderFekCacheSize = 100_000
	defaultRenderFekCacheTTL  = time.Hour
)

// RenderMeta is a meta.Meta decorator for render nodes (SRS §7): direct backend
// access with local FEK unwrap/generate under the Company KEK. No authz, no OIDC,
// no KeyManager RPC per file — the KEK fetched once at mount is the node's only
// trust anchor (FR-RND-2). Company isolation is cryptographic: a file wrapped
// under another company's KEK fails to unwrap (GCM tag mismatch) and Open fails
// closed with EIO (FR-RND-13, FR-TEST-12); the mount chroot to companies/{code}
// is defense in depth on top (decision 4.2).
type RenderMeta struct {
	Meta // inner backend (redisMeta), already chrooted to the company prefix

	kek        []byte // Company KEK, mlock'ed by the caller (FR-RND-3)
	kekVersion uint32 // version returned by FetchCompanyKEK; embedded in AGFK blobs
	volumeUUID string // AAD component
	companyID  string // company ID (UUID) — AAD component, must match the platform's
	prefix     string // "companies/{code}" — recorded for diagnostics

	fekCache *expirable.LRU[uint64, *fekEntry]
}

var _ Meta = (*RenderMeta)(nil)

// fekUnwrapLatency measures the duration of the local FEK unwrap under the
// Company KEK (task 10.4).
var fekUnwrapLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "fek_unwrap_latency_seconds",
	Help:    "duration of the local FEK unwrap under the Company KEK",
	Buckets: prometheus.ExponentialBuckets(0.001, 10, 4), // 1ms .. 1s
})

// NewRenderMeta wraps inner with local FEK handling. kek must be the 32-byte
// Company KEK from FetchCompanyKEK; kekVersion is the version that RPC returned
// (it is embedded into every AGFK blob this node produces).
func NewRenderMeta(inner Meta, kek []byte, kekVersion uint32, volumeUUID, companyID, prefix string) *RenderMeta {
	return newRenderMetaWithCache(inner, kek, kekVersion, volumeUUID, companyID, prefix,
		defaultRenderFekCacheSize, defaultRenderFekCacheTTL)
}

func newRenderMetaWithCache(inner Meta, kek []byte, kekVersion uint32, volumeUUID, companyID, prefix string, cacheSize int, cacheTTL time.Duration) *RenderMeta {
	return &RenderMeta{
		Meta:       inner,
		kek:        kek,
		kekVersion: kekVersion,
		volumeUUID: volumeUUID,
		companyID:  companyID,
		prefix:     prefix,
		fekCache: expirable.NewLRU[uint64, *fekEntry](cacheSize, func(_ uint64, e *fekEntry) {
			utils.MemClear(e.fek) // secure zeroing on eviction (NFR-SEC-3)
		}, cacheTTL),
	}
}

// cacheFek stores a plaintext FEK in the LRU, pinning it in RAM first
// (best-effort, NFR-SEC-3).
func (m *RenderMeta) cacheFek(inode Ino, fek []byte, version uint32) {
	mlockBestEffort("FEK", fek)
	m.fekCache.Add(uint64(inode), &fekEntry{fek: fek, version: version})
}

// resolveFEK returns the plaintext FEK of an encrypted file: from the LRU on a
// version-matching hit, otherwise by unwrapping the AGFK blob under the Company
// KEK. Any failure (foreign company KEK, tampered metadata) is returned as-is —
// the caller fails closed (FR-RND-13).
func (m *RenderMeta) resolveFEK(inode Ino, attr *Attr) (fek []byte, version uint32, err error) {
	if e, ok := m.fekCache.Get(uint64(inode)); ok && e.version == attr.FekVersion {
		fekCacheHits.Inc()
		return e.fek, e.version, nil
	}
	fekCacheMisses.Inc() // resolved by a local unwrap, not from the cache
	aad := FekAAD{
		VolumeUUID:  m.volumeUUID,
		CompanyID:   m.companyID,
		DriveFileID: attr.DriveFileID,
		Inode:       inode,
		FekVersion:  attr.FekVersion,
	}
	start := time.Now()
	fek, ver, err := UnwrapFEK(m.kek, attr.WrappedFek, aad)
	fekUnwrapLatency.Observe(time.Since(start).Seconds())
	if err != nil {
		return nil, 0, err
	}
	m.cacheFek(inode, fek, ver)
	return fek, ver, nil
}

// Open delegates to the backend and, for encrypted files, resolves the plaintext
// FEK: from the LRU on a version-matching hit, otherwise by unwrapping the AGFK
// blob locally. Any unwrap failure (foreign company KEK, tampered metadata) fails
// closed with EIO — never a partial or wrong key (FR-TEST-12).
func (m *RenderMeta) Open(ctx Context, inode Ino, flags uint32, attr *Attr) syscall.Errno {
	st := m.Meta.Open(ctx, inode, flags, attr)
	if st != 0 || attr == nil || !attr.Encrypted {
		return st
	}
	fek, ver, err := m.resolveFEK(inode, attr)
	if err != nil {
		return syscall.EIO // fail-closed (FR-RND-13)
	}
	attr.Fek = fek
	attr.FekVersion = ver
	return 0
}

// ResolveFileKey resolves the plaintext FEK of an encrypted file locally under
// the Company KEK (D8): LRU hit on a version-matching entry, otherwise unwrap
// the AGFK blob from the attr. It is installed as baseMeta.fileKeyResolver so
// compaction of encrypted chunks can re-wrap slice CEKs; any failure fails
// closed and the chunk is skipped (design 5.3).
func (m *RenderMeta) ResolveFileKey(ctx Context, inode Ino) ([]byte, string, uint32, error) {
	var attr Attr
	if st := m.Meta.GetAttr(ctx, inode, &attr); st != 0 {
		return nil, "", 0, fmt.Errorf("get attr %d: %s", inode, st)
	}
	if !attr.Encrypted {
		return nil, "", 0, nil
	}
	fek, ver, err := m.resolveFEK(inode, &attr)
	if err != nil {
		return nil, "", 0, fmt.Errorf("unwrap FEK of %d: %w", inode, err)
	}
	return fek, attr.DriveFileID, ver, nil
}

// Clone delegates to the backend and, for encrypted sources, gives the target
// its own FEK locally (design 5.2, render path): unwrap the source FEK under the
// Company KEK, generate a fresh target FEK, persist it and re-wrap the shared
// slice CEKs (zero-copy). Any failure rolls the whole clone back (fail-closed).
func (m *RenderMeta) Clone(ctx Context, srcParentIno, srcIno, dstParentIno Ino, dstName string, cmode uint8, cumask uint16, concurrency uint8, count, total *uint64, dstIno *Ino) syscall.Errno {
	st := m.Meta.Clone(ctx, srcParentIno, srcIno, dstParentIno, dstName, cmode, cumask, concurrency, count, total, dstIno)
	if st != 0 || dstIno == nil {
		return st
	}
	var srcAttr Attr
	if st := m.Meta.GetAttr(ctx, srcIno, &srcAttr); st != 0 {
		return st
	}
	if !srcAttr.Encrypted || len(srcAttr.WrappedFek) == 0 {
		return 0 // legacy clone — no re-wrap
	}
	ops := &cloneKeyOps{
		srcFek: func(i Ino, _ string, a *Attr) ([]byte, error) {
			fek, _, err := UnwrapFEK(m.kek, a.WrappedFek, FekAAD{
				VolumeUUID:  m.volumeUUID,
				CompanyID:   m.companyID,
				DriveFileID: a.DriveFileID,
				Inode:       i,
				FekVersion:  a.FekVersion,
			})
			return fek, err
		},
		dstFek: func(i Ino, _ string) ([]byte, *FileCrypto, error) {
			fek, err := NewFEK()
			if err != nil {
				return nil, nil, err
			}
			driveFileID := uuid.New().String()
			wrapped, err := WrapFEK(m.kek, fek, FekAAD{
				VolumeUUID:  m.volumeUUID,
				CompanyID:   m.companyID,
				DriveFileID: driveFileID,
				Inode:       i,
				FekVersion:  1,
			}, m.kekVersion)
			if err != nil {
				return nil, nil, err
			}
			return fek, &FileCrypto{
				WrappedFek:  wrapped,
				DriveFileID: driveFileID,
				FekVersion:  1,
				CryptoAlg:   "AES-256-GCM",
			}, nil
		},
	}
	if st := cloneRewrap(ctx, m.Meta, srcIno, *dstIno, "", "", ops); st != 0 {
		var removed uint64
		_ = m.Meta.Remove(ctx, dstParentIno, dstName, true, 1, &removed) // rollback (fail-closed)
		return st
	}
	return 0
}

// CopyFileRange delegates to the backend and, for an encrypted source, re-wraps
// the copied range under the target's own FEK locally (design 5.5): both FEKs are
// resolved under the Company KEK (LRU first). A legacy target cannot hold
// encrypted slices — EOPNOTSUPP before the metadata operation (fail-closed).
func (m *RenderMeta) CopyFileRange(ctx Context, fin Ino, offIn uint64, fout Ino, offOut uint64, size uint64, flags uint32, copied, outLength *uint64) syscall.Errno {
	var srcAttr, dstAttr Attr
	if st := m.Meta.GetAttr(ctx, fin, &srcAttr); st != 0 {
		return st
	}
	srcEncrypted := srcAttr.Encrypted && len(srcAttr.WrappedFek) > 0
	if srcEncrypted {
		if st := m.Meta.GetAttr(ctx, fout, &dstAttr); st != 0 {
			return st
		}
		if !dstAttr.Encrypted || len(dstAttr.WrappedFek) == 0 {
			return syscall.EOPNOTSUPP // fail-closed: a legacy target cannot hold encrypted slices
		}
	}
	st := m.Meta.CopyFileRange(ctx, fin, offIn, fout, offOut, size, flags, copied, outLength)
	if st != 0 || !srcEncrypted || *copied == 0 {
		return st
	}
	srcFek, _, err := m.resolveFEK(fin, &srcAttr)
	if err != nil {
		return syscall.EIO // fail-closed (FR-RND-13)
	}
	dstFek, _, err := m.resolveFEK(fout, &dstAttr)
	if err != nil {
		return syscall.EIO // fail-closed
	}
	rewrapper, ok := m.Meta.(sliceRangeRewrapper)
	if !ok {
		return syscall.EOPNOTSUPP
	}
	start := uint32(offOut / ChunkSize)
	end := uint32((offOut + *copied - 1) / ChunkSize)
	return rewrapper.RewrapSlicesRange(ctx, fout, srcFek, dstFek,
		SliceCryptoAAD{DriveFileID: srcAttr.DriveFileID, FekVersion: srcAttr.FekVersion},
		SliceCryptoAAD{DriveFileID: dstAttr.DriveFileID, FekVersion: dstAttr.FekVersion},
		start, end)
}

// Create delegates to the backend and, for regular files, generates a per-file
// FEK locally, wraps it under the Company KEK and persists it via SetFileCrypto.
// Any failure after the file exists rolls it back (FR-USR-2, same as the proxy).
func (m *RenderMeta) Create(ctx Context, parent Ino, name string, mode uint16, cumask uint16, flags uint32, inode *Ino, attr *Attr) syscall.Errno {
	st := m.Meta.Create(ctx, parent, name, mode, cumask, flags, inode, attr)
	if st != 0 || attr == nil || attr.Typ != TypeFile {
		return st
	}
	fek, err := NewFEK()
	if err != nil {
		return syscall.EIO
	}
	driveFileID := uuid.New().String()
	aad := FekAAD{
		VolumeUUID:  m.volumeUUID,
		CompanyID:   m.companyID,
		DriveFileID: driveFileID,
		Inode:       *inode,
		FekVersion:  1,
	}
	wrapped, err := WrapFEK(m.kek, fek, aad, m.kekVersion)
	if err != nil {
		return syscall.EIO
	}
	setter, ok := m.Meta.(fileCryptoSetter)
	if !ok {
		return syscall.EIO
	}
	if st := setter.SetFileCrypto(ctx, *inode, &FileCrypto{
		WrappedFek:  wrapped,
		DriveFileID: driveFileID,
		FekVersion:  1,
		CryptoAlg:   "AES-256-GCM",
	}); st != 0 {
		_ = m.Meta.Unlink(ctx, parent, name) // rollback (FR-USR-2)
		return st
	}
	attr.Encrypted = true
	attr.DriveFileID = driveFileID
	attr.FekVersion = 1
	attr.WrappedFek = wrapped
	attr.CryptoAlg = "AES-256-GCM"
	m.cacheFek(*inode, fek, 1)
	attr.Fek = fek // so the VFS can open the new file without a second unwrap
	return 0
}

// WipeKeys zeroes the Company KEK and all cached FEKs. Called on unmount
// (NFR-SEC-5); after it returns the node can no longer read encrypted data.
func (m *RenderMeta) WipeKeys() {
	utils.MemClear(m.kek)
	for _, e := range m.fekCache.Values() {
		utils.MemClear(e.fek)
	}
	m.fekCache.Purge() // onEvict zeroes the entries again — idempotent
}
