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

package chunk

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	cekenc "github.com/juicedata/juicefs/pkg/agio/cek"
)

// AGDF — encrypted block stored in object storage (master plan §4.1):
//
//	offset  size  field
//	0       4     magic "AGDF"
//	4       1     format version (currently 1)
//	5       12    nonce (AES-GCM)
//	17      N     ciphertext
//	17+N    16    GCM tag
//
// AAD = sliceID(8B big-endian) || blockIndex(4B big-endian). The slice ID is part of
// the object key, so a blob cannot be replayed against another slice or block.
const (
	chunkMagic   = "AGDF"
	chunkVersion = byte(1)

	agdfNonceLen = 12
	agdfTagLen   = 16

	// agdfOverhead is the number of extra bytes an encrypted block adds over the
	// plaintext: magic(4) + version(1) + nonce(12) + tag(16).
	agdfOverhead = len(chunkMagic) + 1 + agdfNonceLen + agdfTagLen
)

var errAGDFMalformed = errors.New("agdf: malformed block")

func buildBlockAAD(sliceID uint64, blockIndex uint32) []byte {
	aad := make([]byte, 12)
	binary.BigEndian.PutUint64(aad[:8], sliceID)
	binary.BigEndian.PutUint32(aad[8:], blockIndex)
	return aad
}

// EncryptBlock encrypts a block with AES-256-GCM under cek (32 bytes). A fresh random
// nonce is used for every call (NFR-SEC-9). The result is an AGDF blob.
func EncryptBlock(cek, plaintext []byte, sliceID uint64, blockIndex uint32) ([]byte, error) {
	nonce := make([]byte, agdfNonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("agdf: nonce: %w", err)
	}
	return EncryptBlockWithNonce(cek, plaintext, sliceID, blockIndex, nonce)
}

// EncryptBlockWithNonce is the deterministic variant of EncryptBlock pinned by the
// cross-repo known-answer vectors (stage 9). Production code must use EncryptBlock.
func EncryptBlockWithNonce(cek, plaintext []byte, sliceID uint64, blockIndex uint32, nonce []byte) ([]byte, error) {
	if len(nonce) != agdfNonceLen {
		return nil, fmt.Errorf("agdf: nonce must be %d bytes, got %d", agdfNonceLen, len(nonce))
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("agdf: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("agdf: gcm: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, plaintext, buildBlockAAD(sliceID, blockIndex))
	out := make([]byte, 0, len(chunkMagic)+1+agdfNonceLen+len(sealed))
	out = append(out, chunkMagic...)
	out = append(out, chunkVersion)
	out = append(out, nonce...)
	return append(out, sealed...), nil
}

// IsLegacyBlock reports whether data is not an AGDF blob, i.e. a plaintext legacy block
// (NFR-COMPAT-1).
func IsLegacyBlock(data []byte) bool {
	return !bytes.HasPrefix(data, []byte(chunkMagic))
}

// DecryptBlock decrypts an AGDF blob under cek. Any tampering or AAD mismatch fails
// closed (NFR-SEC-11): no plaintext is returned.
func DecryptBlock(cek, data []byte, sliceID uint64, blockIndex uint32) ([]byte, error) {
	if IsLegacyBlock(data) {
		return nil, errAGDFMalformed
	}
	if len(data) < len(chunkMagic)+1+agdfNonceLen+agdfTagLen {
		return nil, errAGDFMalformed
	}
	if data[len(chunkMagic)] != chunkVersion {
		return nil, fmt.Errorf("agdf: unsupported version %d", data[len(chunkMagic)])
	}
	nonce := data[len(chunkMagic)+1 : len(chunkMagic)+1+agdfNonceLen]
	sealed := data[len(chunkMagic)+1+agdfNonceLen:]
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("agdf: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("agdf: gcm: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, sealed, buildBlockAAD(sliceID, blockIndex))
	if err != nil {
		return nil, fmt.Errorf("agdf: decrypt failed (tampered or AAD mismatch): %w", err)
	}
	return plain, nil
}

// The AGCK CEK wrap/unwrap primitive lives in the cgo-free package
// github.com/juicedata/juicefs/pkg/agio/cek so that consumers building the
// pkg/meta graph with CGO_ENABLED=0 do not pull in this package's cgo-only
// compression dependencies. The identifiers below are thin re-exports kept for
// existing callers of this package; the behavior is identical.

// CEKSize is the size in bytes of a slice content-encryption key (AES-256).
const CEKSize = cekenc.CEKSize

// NewCEK generates a fresh random slice CEK (NFR-SEC-9: unique per slice).
func NewCEK() ([]byte, error) {
	return cekenc.NewCEK()
}

// WrapCEK wraps cek (32 bytes) under the file's FEK and returns an AGCK blob (65 bytes).
func WrapCEK(fek, cek []byte, driveFileID string, sliceID uint64, fekVersion uint32) ([]byte, error) {
	return cekenc.WrapCEK(fek, cek, driveFileID, sliceID, fekVersion)
}

// WrapCEKWithNonce is the deterministic variant of WrapCEK pinned by the cross-repo
// known-answer vectors (stage 9). Production code must use WrapCEK.
func WrapCEKWithNonce(fek, cek []byte, driveFileID string, sliceID uint64, fekVersion uint32, nonce []byte) ([]byte, error) {
	return cekenc.WrapCEKWithNonce(fek, cek, driveFileID, sliceID, fekVersion, nonce)
}

// UnwrapCEK unwraps an AGCK blob under the file's FEK. Any tampering or AAD mismatch
// fails closed (NFR-SEC-11).
func UnwrapCEK(fek []byte, wrapped []byte, driveFileID string, sliceID uint64, fekVersion uint32) ([]byte, error) {
	return cekenc.UnwrapCEK(fek, wrapped, driveFileID, sliceID, fekVersion)
}
