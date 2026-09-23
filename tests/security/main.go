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

// Command security runs the stage-9 leak scenarios (FR-TEST-27/28/30) as pure
// crypto checks: an attacker who stole S3 objects and/or Redis metadata but does
// NOT have the Company KEK must not be able to recover any plaintext. It prints
// machine-readable STATUS lines plus a markdown details section to stdout;
// run-all.sh assembles tests/security/report.md together with the Go-test
// scenarios (FR-TEST-26/29). No external services are required.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"

	chunkenc "github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
)

type scenario struct {
	fr     string
	name   string
	pass   bool
	detail []string
}

func randomKey(n int) []byte {
	k := make([]byte, n)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}

// attackerCannotOpen reports whether every raw AES-256-GCM open attempt of
// (nonce, sealed) under key fails for all AAD guesses — i.e. an attacker who
// knows the blob format but not the key gets no plaintext.
func attackerCannotOpen(key, nonce, sealed []byte, aadGuesses [][]byte) bool {
	block, err := aes.NewCipher(key)
	if err != nil {
		return false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return false
	}
	for _, aad := range aadGuesses {
		if plain, err := gcm.Open(nil, nonce, sealed, aad); err == nil && len(plain) > 0 {
			return false
		}
	}
	return true
}

// s27S3Leak (FR-TEST-27): an attacker who downloaded the S3 objects (AGDF blobs)
// but does not have the slice CEK cannot read the chunks.
func s27S3Leak() scenario {
	s := scenario{fr: "FR-TEST-27", name: "S3 leak: chunks unreadable without CEK"}
	cek := randomKey(32)
	plaintext := make([]byte, 4096)
	for i := range plaintext {
		plaintext[i] = byte(i % 251)
	}
	const sliceID, blockIndex = uint64(77), uint32(0)

	blob, err := chunkenc.EncryptBlock(cek, plaintext, sliceID, blockIndex)
	if err != nil {
		s.detail = append(s.detail, fmt.Sprintf("setup failed: %v", err))
		return s
	}

	if _, err := chunkenc.DecryptBlock(randomKey(32), blob, sliceID, blockIndex); err == nil {
		s.detail = append(s.detail, "FAIL: the AGDF blob decrypted under a random CEK")
		return s
	}
	s.detail = append(s.detail, "DecryptBlock with a random CEK → error (GCM tag mismatch)")

	// Raw open of the AGDF payload (magic 4 + ver 1 + nonce 12 + sealed) with a
	// zero key — even guessing the exact AAD layout does not help without the CEK.
	aadGuess := make([]byte, 12)
	binary.BigEndian.PutUint64(aadGuess[:8], sliceID)
	binary.BigEndian.PutUint32(aadGuess[8:], blockIndex)
	if !attackerCannotOpen(make([]byte, 32), blob[5:17], blob[17:], [][]byte{nil, aadGuess}) {
		s.detail = append(s.detail, "FAIL: raw AES-256-GCM open of the AGDF payload succeeded with a zero key")
		return s
	}
	s.detail = append(s.detail, "raw AES-256-GCM open of the AGDF payload with a zero key (empty and exact AAD guesses) → error")

	if bytes.Contains(blob, plaintext[:256]) {
		s.detail = append(s.detail, "FAIL: the AGDF blob contains plaintext bytes")
		return s
	}
	s.detail = append(s.detail, "the AGDF blob contains no plaintext bytes (first 256 B checked)")

	s.pass = true
	return s
}

// s28RedisLeak (FR-TEST-28): an attacker who read the Redis metadata (wrapped_fek
// in the file attr) but does not have the Company KEK cannot unwrap the FEK.
func s28RedisLeak() scenario {
	s := scenario{fr: "FR-TEST-28", name: "Redis leak: wrapped_fek unreadable without Company KEK"}
	kekA := randomKey(32) // the Company KEK — the attacker does NOT have it
	fek := randomKey(32)
	aad := meta.FekAAD{VolumeUUID: "vol-1", CompanyID: "comp-1", DriveFileID: "df-1", Inode: 42, FekVersion: 1}

	wrapped, err := meta.WrapFEK(kekA, fek, aad, 1)
	if err != nil {
		s.detail = append(s.detail, fmt.Sprintf("setup failed: %v", err))
		return s
	}

	if _, _, err := meta.UnwrapFEK(randomKey(32), wrapped, aad); err == nil {
		s.detail = append(s.detail, "FAIL: the AGFK blob unwrapped under a random KEK")
		return s
	}
	s.detail = append(s.detail, "UnwrapFEK with a random KEK → error (GCM tag mismatch)")

	// Raw open of the AGFK payload (magic 4 + ver 1 + kek_version 4 + nonce 12 +
	// sealed) with a zero key.
	if !attackerCannotOpen(make([]byte, 32), wrapped[9:21], wrapped[21:], [][]byte{nil}) {
		s.detail = append(s.detail, "FAIL: raw AES-256-GCM open of the AGFK payload succeeded with a zero key")
		return s
	}
	s.detail = append(s.detail, "raw AES-256-GCM open of the AGFK payload with a zero key → error")

	if bytes.Contains(wrapped, fek) {
		s.detail = append(s.detail, "FAIL: the AGFK blob contains the plaintext FEK")
		return s
	}
	s.detail = append(s.detail, "the AGFK blob contains no plaintext FEK bytes")

	s.pass = true
	return s
}

// s30CombinedLeak (FR-TEST-30): an attacker who stole Redis AND S3 at the same
// time holds wrapped_fek + AGCK + AGDF — but without the Company KEK the key
// chain is broken at its first link and no plaintext is recoverable.
func s30CombinedLeak() scenario {
	s := scenario{fr: "FR-TEST-30", name: "Redis+S3 leak: data unreadable without Company KEK"}
	kekA := randomKey(32) // the Company KEK — the attacker does NOT have it
	fek := randomKey(32)
	aad := meta.FekAAD{VolumeUUID: "vol-1", CompanyID: "comp-1", DriveFileID: "df-1", Inode: 42, FekVersion: 1}
	const dfID = "df-1"
	const sliceID, blockIndex = uint64(77), uint32(0)
	cek := randomKey(32)
	plaintext := make([]byte, 4096)
	for i := range plaintext {
		plaintext[i] = byte(i % 197)
	}

	wrappedFek, err := meta.WrapFEK(kekA, fek, aad, 1) // from the Redis attr
	if err != nil {
		s.detail = append(s.detail, fmt.Sprintf("setup failed: %v", err))
		return s
	}
	agck, err := chunkenc.WrapCEK(fek, cek, dfID, sliceID, 1) // from S3 slice metadata
	if err != nil {
		s.detail = append(s.detail, fmt.Sprintf("setup failed: %v", err))
		return s
	}
	agdf, err := chunkenc.EncryptBlock(cek, plaintext, sliceID, blockIndex) // from the S3 object
	if err != nil {
		s.detail = append(s.detail, fmt.Sprintf("setup failed: %v", err))
		return s
	}

	// Step 1 (Redis): without the Company KEK the FEK cannot be unwrapped — the
	// chain is broken here.
	if _, _, err := meta.UnwrapFEK(randomKey(32), wrappedFek, aad); err == nil {
		s.detail = append(s.detail, "FAIL: the AGFK blob unwrapped under a random KEK")
		return s
	}
	s.detail = append(s.detail, "step 1 (Redis): UnwrapFEK with any non-Company KEK → error — the key chain is broken at its first link")

	// Step 2 (S3): even a guessed FEK does not unwrap the CEK.
	if _, err := chunkenc.UnwrapCEK(randomKey(32), agck, dfID, sliceID, 1); err == nil {
		s.detail = append(s.detail, "FAIL: the AGCK blob unwrapped under a random FEK")
		return s
	}
	s.detail = append(s.detail, "step 2 (S3): UnwrapCEK with a random FEK → error (GCM tag mismatch)")

	// Step 3 (S3): even a guessed CEK does not decrypt the block.
	if _, err := chunkenc.DecryptBlock(randomKey(32), agdf, sliceID, blockIndex); err == nil {
		s.detail = append(s.detail, "FAIL: the AGDF blob decrypted under a random CEK")
		return s
	}
	s.detail = append(s.detail, "step 3 (S3): DecryptBlock with a random CEK → error (GCM tag mismatch)")

	// Sanity: with the real FEK the AGCK→AGDF chain decrypts — the Company KEK is
	// the only missing piece.
	cekOut, err := chunkenc.UnwrapCEK(fek, agck, dfID, sliceID, 1)
	if err != nil {
		s.detail = append(s.detail, fmt.Sprintf("sanity check failed: UnwrapCEK with the real FEK: %v", err))
		return s
	}
	plainOut, err := chunkenc.DecryptBlock(cekOut, agdf, sliceID, blockIndex)
	if err != nil || !bytes.Equal(plainOut, plaintext) {
		s.detail = append(s.detail, "sanity check failed: the AGCK→AGDF chain did not decrypt with the real keys")
		return s
	}
	s.detail = append(s.detail, "sanity: with the real FEK the AGCK→AGDF chain decrypts — the Company KEK is the only missing piece")

	s.pass = true
	return s
}

func main() {
	scenarios := []scenario{s27S3Leak(), s28RedisLeak(), s30CombinedLeak()}

	allPass := true
	for _, sc := range scenarios {
		status := "PASS"
		if !sc.pass {
			status = "FAIL"
			allPass = false
		}
		fmt.Printf("STATUS %s %s %s\n", sc.fr, status, sc.name)
	}

	fmt.Println()
	fmt.Println("## Details")
	for _, sc := range scenarios {
		fmt.Printf("\n### %s — %s\n\n", sc.fr, sc.name)
		for _, d := range sc.detail {
			fmt.Printf("- %s\n", d)
		}
	}

	if !allPass {
		os.Exit(1)
	}
}
