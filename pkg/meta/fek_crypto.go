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
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// AGFK — wrapped FEK format (master plan §4.1). The layout and AAD encoding MUST stay
// byte-identical to agio-platform's DriveKeyManagerService implementation; the exact
// bytes are pinned by cross-repo known-answer test vectors (stage 9):
//
//	offset  size  field
//	0       4     magic "AGFK"
//	4       1     format version (currently 1)
//	5       4     kek_version (big-endian uint32, Company KEK version used to wrap)
//	9       12    nonce (AES-GCM)
//	21      32    ciphertext (the FEK)
//	53      16    GCM tag
//
// Total 69 bytes. AAD:
//
//	len(volume_uuid):4 || volume_uuid ||
//	len(company_id):4  || company_id ||
//	len(drive_file_id):4 || drive_file_id ||
//	inode:8 (big-endian) ||
//	fek_version:4 (big-endian)
const (
	fekMagic   = "AGFK"
	fekVersion = byte(1)

	fekNonceLen = 12
	fekTagLen   = 16
	fekSize     = 32 // FEK size in bytes (AES-256)
	kekSize     = 32 // Company KEK size in bytes (AES-256)

	// agfkLen is the fixed size of an AGFK blob.
	agfkLen = len(fekMagic) + 1 + 4 + fekNonceLen + fekSize + fekTagLen
)

var errAGFKMalformed = errors.New("agfk: malformed blob")

// FekAAD identifies the file a wrapped FEK belongs to; it is bound into the GCM AAD so
// a blob cannot be replayed against a different file, volume, or FEK version.
type FekAAD struct {
	VolumeUUID  string
	CompanyID   string
	DriveFileID string
	Inode       Ino
	FekVersion  uint32
}

func (a FekAAD) bytes() []byte {
	buf := make([]byte, 0, 4+len(a.VolumeUUID)+4+len(a.CompanyID)+4+len(a.DriveFileID)+8+4)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(a.VolumeUUID)))
	buf = append(buf, a.VolumeUUID...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(a.CompanyID)))
	buf = append(buf, a.CompanyID...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(a.DriveFileID)))
	buf = append(buf, a.DriveFileID...)
	buf = binary.BigEndian.AppendUint64(buf, uint64(a.Inode))
	buf = binary.BigEndian.AppendUint32(buf, a.FekVersion)
	return buf
}

// NewFEK returns a fresh 32-byte FEK from the CSPRNG (NFR-SEC-7).
func NewFEK() ([]byte, error) {
	fek := make([]byte, fekSize)
	if _, err := io.ReadFull(rand.Reader, fek); err != nil {
		return nil, fmt.Errorf("generate FEK: %w", err)
	}
	return fek, nil
}

// WrapFEK wraps plaintextFEK (32 bytes) under the Company KEK and returns an AGFK blob.
// A fresh random nonce is used for every wrap (NFR-SEC-9).
func WrapFEK(kek, plaintextFEK []byte, aad FekAAD, kekVersion uint32) ([]byte, error) {
	if len(kek) != kekSize {
		return nil, fmt.Errorf("agfk: kek must be %d bytes, got %d", kekSize, len(kek))
	}
	if len(plaintextFEK) != fekSize {
		return nil, fmt.Errorf("agfk: fek must be %d bytes, got %d", fekSize, len(plaintextFEK))
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("agfk: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("agfk: gcm: %w", err)
	}
	nonce := make([]byte, fekNonceLen)
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("agfk: nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, plaintextFEK, aad.bytes())
	blob := make([]byte, 0, agfkLen)
	blob = append(blob, fekMagic...)
	blob = append(blob, fekVersion)
	blob = binary.BigEndian.AppendUint32(blob, kekVersion)
	blob = append(blob, nonce...)
	return append(blob, sealed...), nil
}

// UnwrapFEK unwraps an AGFK blob under the Company KEK, returning the plaintext FEK and
// the embedded KEK version. Any tampering or AAD mismatch fails closed (NFR-SEC-11).
func UnwrapFEK(kek []byte, wrapped []byte, aad FekAAD) (fek []byte, kekVersion uint32, err error) {
	if len(kek) != kekSize {
		return nil, 0, fmt.Errorf("agfk: kek must be %d bytes, got %d", kekSize, len(kek))
	}
	if len(wrapped) != agfkLen {
		return nil, 0, errAGFKMalformed
	}
	if !bytes.HasPrefix(wrapped, []byte(fekMagic)) {
		return nil, 0, errAGFKMalformed
	}
	if wrapped[len(fekMagic)] != fekVersion {
		return nil, 0, fmt.Errorf("agfk: unsupported version %d", wrapped[len(fekMagic)])
	}
	kekVersion = binary.BigEndian.Uint32(wrapped[len(fekMagic)+1 : len(fekMagic)+5])
	nonce := wrapped[len(fekMagic)+5 : len(fekMagic)+5+fekNonceLen]
	sealed := wrapped[len(fekMagic)+5+fekNonceLen:]
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, 0, fmt.Errorf("agfk: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, 0, fmt.Errorf("agfk: gcm: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, sealed, aad.bytes())
	if err != nil {
		return nil, 0, fmt.Errorf("agfk: unwrap failed (tampered or AAD mismatch): %w", err)
	}
	if len(plain) != fekSize {
		return nil, 0, fmt.Errorf("agfk: unwrapped FEK must be %d bytes, got %d", fekSize, len(plain))
	}
	return plain, kekVersion, nil
}
