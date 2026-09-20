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
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func randomKey32(t *testing.T, seed byte) []byte {
	t.Helper()
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

var testFekAAD = FekAAD{
	VolumeUUID:  "volume-uuid",
	CompanyID:   "company-id",
	DriveFileID: "drive-file-id",
	Inode:       42,
	FekVersion:  1,
}

func TestWrapFEK_RoundTrip(t *testing.T) {
	kek := randomKey32(t, 0x10)
	fek, err := NewFEK()
	require.NoError(t, err)
	assert.Len(t, fek, 32)

	blob, err := WrapFEK(kek, fek, testFekAAD, 5)
	require.NoError(t, err)
	assert.Len(t, blob, agfkLen) // 69 bytes
	assert.True(t, bytes.HasPrefix(blob, []byte(fekMagic)))

	out, kekVersion, err := UnwrapFEK(kek, blob, testFekAAD)
	require.NoError(t, err)
	assert.Equal(t, fek, out)
	assert.Equal(t, uint32(5), kekVersion)
}

func TestUnwrapFEK_TamperedFailsClosed(t *testing.T) {
	kek := randomKey32(t, 0x10)
	fek := randomKey32(t, 0x20)
	blob, err := WrapFEK(kek, fek, testFekAAD, 1)
	require.NoError(t, err)

	tampered := append([]byte{}, blob...)
	tampered[len(tampered)-1] ^= 0xFF // tag bit
	_, _, err = UnwrapFEK(kek, tampered, testFekAAD)
	assert.Error(t, err)

	tampered = append([]byte{}, blob...)
	tampered[25] ^= 0x01 // ciphertext bit
	_, _, err = UnwrapFEK(kek, tampered, testFekAAD)
	assert.Error(t, err)
}

func TestUnwrapFEK_AADMismatchFailsClosed(t *testing.T) {
	kek := randomKey32(t, 0x10)
	fek := randomKey32(t, 0x20)
	blob, err := WrapFEK(kek, fek, testFekAAD, 1)
	require.NoError(t, err)

	cases := []FekAAD{
		{VolumeUUID: "other", CompanyID: "company-id", DriveFileID: "drive-file-id", Inode: 42, FekVersion: 1},
		{VolumeUUID: "volume-uuid", CompanyID: "other", DriveFileID: "drive-file-id", Inode: 42, FekVersion: 1},
		{VolumeUUID: "volume-uuid", CompanyID: "company-id", DriveFileID: "other", Inode: 42, FekVersion: 1},
		{VolumeUUID: "volume-uuid", CompanyID: "company-id", DriveFileID: "drive-file-id", Inode: 43, FekVersion: 1},
		{VolumeUUID: "volume-uuid", CompanyID: "company-id", DriveFileID: "drive-file-id", Inode: 42, FekVersion: 2},
	}
	for i, aad := range cases {
		_, _, err := UnwrapFEK(kek, blob, aad)
		assert.Error(t, err, "case %d", i)
	}
}

func TestUnwrapFEK_WrongKEKFailsClosed(t *testing.T) {
	kek := randomKey32(t, 0x10)
	fek := randomKey32(t, 0x20)
	blob, err := WrapFEK(kek, fek, testFekAAD, 1)
	require.NoError(t, err)
	other := randomKey32(t, 0x30)
	_, _, err = UnwrapFEK(other, blob, testFekAAD)
	assert.Error(t, err)
}

func TestUnwrapFEK_MalformedBlob(t *testing.T) {
	kek := randomKey32(t, 0x10)
	_, _, err := UnwrapFEK(kek, []byte("short"), testFekAAD)
	assert.ErrorIs(t, err, errAGFKMalformed)

	bad := make([]byte, agfkLen) // right length, wrong magic
	_, _, err = UnwrapFEK(kek, bad, testFekAAD)
	assert.ErrorIs(t, err, errAGFKMalformed)

	bad = append([]byte{}, "XGFK"...)
	bad = append(bad, make([]byte, agfkLen-4)...)
	_, _, err = UnwrapFEK(kek, bad, testFekAAD)
	assert.ErrorIs(t, err, errAGFKMalformed)
}

func TestWrapFEK_KeySizes(t *testing.T) {
	fek := randomKey32(t, 0x20)
	_, err := WrapFEK(make([]byte, 16), fek, testFekAAD, 1)
	assert.Error(t, err)
	_, err = WrapFEK(randomKey32(t, 0x10), make([]byte, 16), testFekAAD, 1)
	assert.Error(t, err)
}

// TestUnwrapFEK_LayoutPinned builds an AGFK blob byte-by-byte with an independently
// constructed AAD (not via FekAAD.bytes()) and checks that UnwrapFEK accepts it. This
// pins the on-wire layout: magic(4)|version(1)|kek_version u32 BE(4)|nonce(12)|
// ciphertext(32)|tag(16), AAD = len-prefixed volume_uuid/company_id/drive_file_id +
// inode u64 BE + fek_version u32 BE. It must stay in sync with agio-platform.
func TestUnwrapFEK_LayoutPinned(t *testing.T) {
	kek := randomKey32(t, 0x10)
	fek := randomKey32(t, 0x20)
	nonce := bytes.Repeat([]byte{0x03}, 12)

	aad := make([]byte, 0, 64)
	aad = binary.BigEndian.AppendUint32(aad, 11)
	aad = append(aad, "volume-uuid"...)
	aad = binary.BigEndian.AppendUint32(aad, 10)
	aad = append(aad, "company-id"...)
	aad = binary.BigEndian.AppendUint32(aad, 13)
	aad = append(aad, "drive-file-id"...)
	aad = binary.BigEndian.AppendUint64(aad, 42)
	aad = binary.BigEndian.AppendUint32(aad, 1)

	block, err := aes.NewCipher(kek)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	sealed := gcm.Seal(nil, nonce, fek, aad)

	blob := make([]byte, 0, agfkLen)
	blob = append(blob, "AGFK"...)
	blob = append(blob, 1)                        // version
	blob = binary.BigEndian.AppendUint32(blob, 7) // kek_version
	blob = append(blob, nonce...)
	blob = append(blob, sealed...)

	out, kekVersion, err := UnwrapFEK(kek, blob, testFekAAD)
	require.NoError(t, err)
	assert.Equal(t, fek, out)
	assert.Equal(t, uint32(7), kekVersion)
}
