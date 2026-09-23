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

	// AGCK — CEK wrapped under the file's FEK (slice metadata):
	// magic(4)|version(1)|nonce(12)|ciphertext(32)|tag(16) = 65 bytes.
	cekMagic   = "AGCK"
	cekVersion = byte(1)

	agdfNonceLen = 12
	agdfTagLen   = 16
	cekSize      = 32 // CEK size in bytes (AES-256)

	// agdfOverhead is the number of extra bytes an encrypted block adds over the
	// plaintext: magic(4) + version(1) + nonce(12) + tag(16).
	agdfOverhead = len(chunkMagic) + 1 + agdfNonceLen + agdfTagLen
)

var (
	errAGDFMalformed = errors.New("agdf: malformed block")
	errAGCKMalformed = errors.New("agck: malformed blob")
)

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

// buildCEKAAD encodes the AAD binding a wrapped CEK to its file identity:
//
//	len(driveFileID):4 || driveFileID || sliceID:8 (big-endian) || fekVersion:4 (big-endian).
//
// Length-prefixed string fields keep the encoding unambiguous.
func buildCEKAAD(driveFileID string, sliceID uint64, fekVersion uint32) []byte {
	buf := make([]byte, 0, 4+len(driveFileID)+12)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(driveFileID)))
	buf = append(buf, driveFileID...)
	buf = binary.BigEndian.AppendUint64(buf, sliceID)
	buf = binary.BigEndian.AppendUint32(buf, fekVersion)
	return buf
}

// CEKSize is the size in bytes of a slice content-encryption key (AES-256).
const CEKSize = cekSize

// NewCEK generates a fresh random slice CEK (NFR-SEC-9: unique per slice).
func NewCEK() ([]byte, error) {
	cek := make([]byte, cekSize)
	if _, err := io.ReadFull(rand.Reader, cek); err != nil {
		return nil, fmt.Errorf("agck: generate CEK: %w", err)
	}
	return cek, nil
}

// WrapCEK wraps cek (32 bytes) under the file's FEK and returns an AGCK blob (65 bytes).
func WrapCEK(fek, cek []byte, driveFileID string, sliceID uint64, fekVersion uint32) ([]byte, error) {
	nonce := make([]byte, agdfNonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("agck: nonce: %w", err)
	}
	return WrapCEKWithNonce(fek, cek, driveFileID, sliceID, fekVersion, nonce)
}

// WrapCEKWithNonce is the deterministic variant of WrapCEK pinned by the cross-repo
// known-answer vectors (stage 9). Production code must use WrapCEK.
func WrapCEKWithNonce(fek, cek []byte, driveFileID string, sliceID uint64, fekVersion uint32, nonce []byte) ([]byte, error) {
	if len(nonce) != agdfNonceLen {
		return nil, fmt.Errorf("agck: nonce must be %d bytes, got %d", agdfNonceLen, len(nonce))
	}
	if len(cek) != cekSize {
		return nil, fmt.Errorf("agck: cek must be %d bytes, got %d", cekSize, len(cek))
	}
	block, err := aes.NewCipher(fek)
	if err != nil {
		return nil, fmt.Errorf("agck: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("agck: gcm: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, cek, buildCEKAAD(driveFileID, sliceID, fekVersion))
	out := make([]byte, 0, len(cekMagic)+1+agdfNonceLen+len(sealed))
	out = append(out, cekMagic...)
	out = append(out, cekVersion)
	out = append(out, nonce...)
	return append(out, sealed...), nil
}

// UnwrapCEK unwraps an AGCK blob under the file's FEK. Any tampering or AAD mismatch
// fails closed (NFR-SEC-11).
func UnwrapCEK(fek []byte, wrapped []byte, driveFileID string, sliceID uint64, fekVersion uint32) ([]byte, error) {
	if len(wrapped) != len(cekMagic)+1+agdfNonceLen+cekSize+agdfTagLen {
		return nil, errAGCKMalformed
	}
	if !bytes.HasPrefix(wrapped, []byte(cekMagic)) {
		return nil, errAGCKMalformed
	}
	if wrapped[len(cekMagic)] != cekVersion {
		return nil, fmt.Errorf("agck: unsupported version %d", wrapped[len(cekMagic)])
	}
	nonce := wrapped[len(cekMagic)+1 : len(cekMagic)+1+agdfNonceLen]
	sealed := wrapped[len(cekMagic)+1+agdfNonceLen:]
	block, err := aes.NewCipher(fek)
	if err != nil {
		return nil, fmt.Errorf("agck: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("agck: gcm: %w", err)
	}
	plain, err := gcm.Open(nil, nonce, sealed, buildCEKAAD(driveFileID, sliceID, fekVersion))
	if err != nil {
		return nil, fmt.Errorf("agck: unwrap failed (tampered or AAD mismatch): %w", err)
	}
	if len(plain) != cekSize {
		return nil, fmt.Errorf("agck: unwrapped CEK must be %d bytes, got %d", cekSize, len(plain))
	}
	return plain, nil
}
