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
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/golang-lru/v2/expirable"
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
			for i := range e.fek { // secure zeroing on eviction (NFR-SEC-3)
				e.fek[i] = 0
			}
		}, cacheTTL),
	}
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
	if e, ok := m.fekCache.Get(uint64(inode)); ok && e.version == attr.FekVersion {
		attr.Fek = e.fek
		return 0
	}
	aad := FekAAD{
		VolumeUUID:  m.volumeUUID,
		CompanyID:   m.companyID,
		DriveFileID: attr.DriveFileID,
		Inode:       inode,
		FekVersion:  attr.FekVersion,
	}
	fek, ver, err := UnwrapFEK(m.kek, attr.WrappedFek, aad)
	if err != nil {
		return syscall.EIO // fail-closed (FR-RND-13)
	}
	m.fekCache.Add(uint64(inode), &fekEntry{fek: fek, version: ver})
	attr.Fek = fek
	attr.FekVersion = ver
	return 0
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
	m.fekCache.Add(uint64(*inode), &fekEntry{fek: fek, version: 1})
	attr.Fek = fek // so the VFS can open the new file without a second unwrap
	return 0
}

// WipeKeys zeroes the Company KEK and all cached FEKs. Called on unmount
// (NFR-SEC-5); after it returns the node can no longer read encrypted data.
func (m *RenderMeta) WipeKeys() {
	for i := range m.kek {
		m.kek[i] = 0
	}
	m.fekCache.Purge()
}
