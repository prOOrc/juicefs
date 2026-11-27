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

// Package cek holds the AGCK primitive: wrapping a slice content-encryption key
// (CEK) under a file's FEK. It is intentionally stdlib-only and cgo-free, so
// consumers that build the pkg/meta dependency graph with CGO_ENABLED=0 (static
// Linux binaries) do not pull in pkg/chunk and its cgo-only compression stack.
// The AGCK byte format is pinned cross-repo by the known-answer vectors in
// pkg/agio/testcrypto and must not change.
package cek

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

// AGCK — CEK wrapped under the file's FEK (slice metadata):
// magic(4)|version(1)|nonce(12)|ciphertext(32)|tag(16) = 65 bytes.
const (
	cekMagic   = "AGCK"
	cekVersion = byte(1)

	cekNonceLen = 12 // AES-GCM nonce length
	cekTagLen   = 16 // AES-GCM tag length
	cekSize     = 32 // CEK size in bytes (AES-256)
)

var errAGCKMalformed = errors.New("agck: malformed blob")

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
	nonce := make([]byte, cekNonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("agck: nonce: %w", err)
	}
	return WrapCEKWithNonce(fek, cek, driveFileID, sliceID, fekVersion, nonce)
}

// WrapCEKWithNonce is the deterministic variant of WrapCEK pinned by the cross-repo
// known-answer vectors (stage 9). Production code must use WrapCEK.
func WrapCEKWithNonce(fek, cek []byte, driveFileID string, sliceID uint64, fekVersion uint32, nonce []byte) ([]byte, error) {
	if len(nonce) != cekNonceLen {
		return nil, fmt.Errorf("agck: nonce must be %d bytes, got %d", cekNonceLen, len(nonce))
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
	out := make([]byte, 0, len(cekMagic)+1+cekNonceLen+len(sealed))
	out = append(out, cekMagic...)
	out = append(out, cekVersion)
	out = append(out, nonce...)
	return append(out, sealed...), nil
}

// UnwrapCEK unwraps an AGCK blob under the file's FEK. Any tampering or AAD mismatch
// fails closed (NFR-SEC-11).
func UnwrapCEK(fek []byte, wrapped []byte, driveFileID string, sliceID uint64, fekVersion uint32) ([]byte, error) {
	if len(wrapped) != len(cekMagic)+1+cekNonceLen+cekSize+cekTagLen {
		return nil, errAGCKMalformed
	}
	if !bytes.HasPrefix(wrapped, []byte(cekMagic)) {
		return nil, errAGCKMalformed
	}
	if wrapped[len(cekMagic)] != cekVersion {
		return nil, fmt.Errorf("agck: unsupported version %d", wrapped[len(cekMagic)])
	}
	nonce := wrapped[len(cekMagic)+1 : len(cekMagic)+1+cekNonceLen]
	sealed := wrapped[len(cekMagic)+1+cekNonceLen:]
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
