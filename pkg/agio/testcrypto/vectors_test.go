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

// Package testcrypto pins the AGFK/AGCK/AGDF byte formats with cross-repo
// known-answer vectors (stage 9, task 9.1). vectors.json is an identical copy
// in agio-platform (src/application/authz/service/testcrypto/vectors.json);
// any format change on either side breaks both tests.
package testcrypto

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
)

type agfkAAD struct {
	VolumeUUID  string `json:"volume_uuid"`
	CompanyID   string `json:"company_id"`
	DriveFileID string `json:"drive_file_id"`
	Inode       uint64 `json:"inode"`
	FekVersion  uint32 `json:"fek_version"`
}

type agfkVector struct {
	KEK        string  `json:"kek"`
	FEK        string  `json:"fek"`
	AAD        agfkAAD `json:"aad"`
	KekVersion uint32  `json:"kek_version"`
	Nonce      string  `json:"nonce"`
	Blob       string  `json:"blob"`
}

type agckVector struct {
	FEK         string `json:"fek"`
	CEK         string `json:"cek"`
	DriveFileID string `json:"drive_file_id"`
	SliceID     uint64 `json:"slice_id"`
	FekVersion  uint32 `json:"fek_version"`
	Nonce       string `json:"nonce"`
	Blob        string `json:"blob"`
}

type agdfVector struct {
	CEK        string `json:"cek"`
	Plaintext  string `json:"plaintext"`
	SliceID    uint64 `json:"slice_id"`
	BlockIndex uint32 `json:"block_index"`
	Nonce      string `json:"nonce"`
	Blob       string `json:"blob"`
}

type vectors struct {
	AGFK agfkVector `json:"AGFK"`
	AGCK agckVector `json:"AGCK"`
	AGDF agdfVector `json:"AGDF"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("vectors.json")
	if err != nil {
		t.Fatalf("read vectors.json: %s", err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vectors.json: %s", err)
	}
	return v
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %s", s, err)
	}
	return b
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64: %s", err)
	}
	return b
}

// TestKnownAnswerVectors verifies that the fork's AGFK/AGCK/AGDF implementations
// reproduce the pinned blobs byte-for-byte and unwrap/decrypt them back.
func TestKnownAnswerVectors(t *testing.T) {
	v := loadVectors(t)

	t.Run("AGFK", func(t *testing.T) {
		kek, fek := mustHex(t, v.AGFK.KEK), mustHex(t, v.AGFK.FEK)
		nonce := mustHex(t, v.AGFK.Nonce)
		aad := meta.FekAAD{
			VolumeUUID:  v.AGFK.AAD.VolumeUUID,
			CompanyID:   v.AGFK.AAD.CompanyID,
			DriveFileID: v.AGFK.AAD.DriveFileID,
			Inode:       meta.Ino(v.AGFK.AAD.Inode),
			FekVersion:  v.AGFK.AAD.FekVersion,
		}
		want := mustB64(t, v.AGFK.Blob)
		if len(want) != 69 {
			t.Fatalf("AGFK vector blob = %d bytes, want 69", len(want))
		}
		got, err := meta.WrapFEKWithNonce(kek, fek, aad, v.AGFK.KekVersion, nonce)
		if err != nil {
			t.Fatalf("WrapFEKWithNonce: %s", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("AGFK wrap mismatch:\n got %x\nwant %x", got, want)
		}
		unwrapped, kekVersion, err := meta.UnwrapFEK(kek, want, aad)
		if err != nil {
			t.Fatalf("UnwrapFEK: %s", err)
		}
		if !bytes.Equal(unwrapped, fek) || kekVersion != v.AGFK.KekVersion {
			t.Fatalf("UnwrapFEK = (%x, %d), want (%x, %d)", unwrapped, kekVersion, fek, v.AGFK.KekVersion)
		}
	})

	t.Run("AGCK", func(t *testing.T) {
		fek, cek := mustHex(t, v.AGCK.FEK), mustHex(t, v.AGCK.CEK)
		nonce := mustHex(t, v.AGCK.Nonce)
		want := mustB64(t, v.AGCK.Blob)
		if len(want) != 65 {
			t.Fatalf("AGCK vector blob = %d bytes, want 65", len(want))
		}
		got, err := chunk.WrapCEKWithNonce(fek, cek, v.AGCK.DriveFileID, v.AGCK.SliceID, v.AGCK.FekVersion, nonce)
		if err != nil {
			t.Fatalf("WrapCEKWithNonce: %s", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("AGCK wrap mismatch:\n got %x\nwant %x", got, want)
		}
		unwrapped, err := chunk.UnwrapCEK(fek, want, v.AGCK.DriveFileID, v.AGCK.SliceID, v.AGCK.FekVersion)
		if err != nil {
			t.Fatalf("UnwrapCEK: %s", err)
		}
		if !bytes.Equal(unwrapped, cek) {
			t.Fatalf("UnwrapCEK = %x, want %x", unwrapped, cek)
		}
	})

	t.Run("AGDF", func(t *testing.T) {
		cek := mustHex(t, v.AGDF.CEK)
		plain := mustB64(t, v.AGDF.Plaintext)
		nonce := mustHex(t, v.AGDF.Nonce)
		want := mustB64(t, v.AGDF.Blob)
		got, err := chunk.EncryptBlockWithNonce(cek, plain, v.AGDF.SliceID, v.AGDF.BlockIndex, nonce)
		if err != nil {
			t.Fatalf("EncryptBlockWithNonce: %s", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("AGDF encrypt mismatch: got %d bytes, want %d bytes (first diff at %d)",
				len(got), len(want), firstDiff(got, want))
		}
		decrypted, err := chunk.DecryptBlock(cek, want, v.AGDF.SliceID, v.AGDF.BlockIndex)
		if err != nil {
			t.Fatalf("DecryptBlock: %s", err)
		}
		if !bytes.Equal(decrypted, plain) {
			t.Fatalf("DecryptBlock mismatch at %d", firstDiff(decrypted, plain))
		}
	})
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
