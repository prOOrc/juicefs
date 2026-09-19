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
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func randomCEK(t *testing.T) []byte {
	t.Helper()
	cek := make([]byte, 32)
	_, err := rand.Read(cek)
	require.NoError(t, err)
	return cek
}

func TestEncryptBlock_RoundTrip(t *testing.T) {
	cek := randomCEK(t)
	plain := bytes.Repeat([]byte{0xAB}, 4<<20) // full BlockSize
	ct, err := EncryptBlock(cek, plain, 42, 3)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(ct, []byte(chunkMagic)))
	assert.Len(t, ct, len(plain)+agdfOverhead)

	out, err := DecryptBlock(cek, ct, 42, 3)
	require.NoError(t, err)
	assert.Equal(t, plain, out)
}

func TestEncryptBlock_SmallBlock(t *testing.T) {
	cek := randomCEK(t)
	plain := []byte("partial block data")
	ct, err := EncryptBlock(cek, plain, 7, 0)
	require.NoError(t, err)
	out, err := DecryptBlock(cek, ct, 7, 0)
	require.NoError(t, err)
	assert.Equal(t, plain, out)
}

func TestDecryptBlock_TamperTag(t *testing.T) {
	cek := randomCEK(t)
	ct, err := EncryptBlock(cek, []byte("data"), 1, 0)
	require.NoError(t, err)
	ct[len(ct)-1] ^= 0xFF // flip a bit in the GCM tag
	_, err = DecryptBlock(cek, ct, 1, 0)
	assert.Error(t, err) // fail-closed (NFR-SEC-11)
}

func TestDecryptBlock_TamperCiphertext(t *testing.T) {
	cek := randomCEK(t)
	ct, err := EncryptBlock(cek, []byte("data"), 1, 0)
	require.NoError(t, err)
	ct[20] ^= 0x01 // flip a bit in the ciphertext
	_, err = DecryptBlock(cek, ct, 1, 0)
	assert.Error(t, err)
}

func TestDecryptBlock_WrongKey(t *testing.T) {
	cek := randomCEK(t)
	ct, err := EncryptBlock(cek, []byte("data"), 1, 0)
	require.NoError(t, err)
	other := randomCEK(t)
	_, err = DecryptBlock(other, ct, 1, 0)
	assert.Error(t, err)
}

func TestDecryptBlock_AADMismatch(t *testing.T) {
	cek := randomCEK(t)
	ct, err := EncryptBlock(cek, []byte("data"), 1, 0)
	require.NoError(t, err)
	// different slice ID
	_, err = DecryptBlock(cek, ct, 2, 0)
	assert.Error(t, err)
	// different block index
	_, err = DecryptBlock(cek, ct, 1, 1)
	assert.Error(t, err)
}

func TestEncryptBlock_NonceUniqueness(t *testing.T) {
	cek := randomCEK(t)
	plain := []byte("same plaintext")
	ct1, err := EncryptBlock(cek, plain, 1, 0)
	require.NoError(t, err)
	ct2, err := EncryptBlock(cek, plain, 1, 0)
	require.NoError(t, err)
	assert.NotEqual(t, ct1, ct2) // fresh nonce per call (NFR-SEC-9)
}

func TestIsLegacyBlock(t *testing.T) {
	cek := randomCEK(t)
	ct, err := EncryptBlock(cek, []byte("data"), 1, 0)
	require.NoError(t, err)
	assert.False(t, IsLegacyBlock(ct))
	assert.True(t, IsLegacyBlock([]byte("plain data")))
	assert.True(t, IsLegacyBlock(nil))
}

func TestDecryptBlock_LegacyInputFails(t *testing.T) {
	cek := randomCEK(t)
	_, err := DecryptBlock(cek, []byte("plain data"), 1, 0)
	assert.ErrorIs(t, err, errAGDFMalformed)
}

func TestWrapCEK_RoundTrip(t *testing.T) {
	fek := randomCEK(t)
	cek := randomCEK(t)
	wrapped, err := WrapCEK(fek, cek, "drive-file-uuid", 99, 2)
	require.NoError(t, err)
	assert.Len(t, wrapped, len(cekMagic)+1+agdfNonceLen+cekSize+agdfTagLen) // 65 bytes
	assert.True(t, bytes.HasPrefix(wrapped, []byte(cekMagic)))

	out, err := UnwrapCEK(fek, wrapped, "drive-file-uuid", 99, 2)
	require.NoError(t, err)
	assert.Equal(t, cek, out)
}

func TestUnwrapCEK_Tamper(t *testing.T) {
	fek := randomCEK(t)
	cek := randomCEK(t)
	wrapped, err := WrapCEK(fek, cek, "dfid", 1, 1)
	require.NoError(t, err)
	wrapped[len(wrapped)-1] ^= 0xFF
	_, err = UnwrapCEK(fek, wrapped, "dfid", 1, 1)
	assert.Error(t, err)
}

func TestUnwrapCEK_AADMismatch(t *testing.T) {
	fek := randomCEK(t)
	cek := randomCEK(t)
	wrapped, err := WrapCEK(fek, cek, "dfid", 1, 1)
	require.NoError(t, err)
	// different drive file ID
	_, err = UnwrapCEK(fek, wrapped, "other-dfid", 1, 1)
	assert.Error(t, err)
	// different slice ID
	_, err = UnwrapCEK(fek, wrapped, "dfid", 2, 1)
	assert.Error(t, err)
	// different FEK version
	_, err = UnwrapCEK(fek, wrapped, "dfid", 1, 2)
	assert.Error(t, err)
}

func TestUnwrapCEK_WrongFEK(t *testing.T) {
	fek := randomCEK(t)
	cek := randomCEK(t)
	wrapped, err := WrapCEK(fek, cek, "dfid", 1, 1)
	require.NoError(t, err)
	other := randomCEK(t)
	_, err = UnwrapCEK(other, wrapped, "dfid", 1, 1)
	assert.Error(t, err)
}

func TestUnwrapCEK_Malformed(t *testing.T) {
	fek := randomCEK(t)
	_, err := UnwrapCEK(fek, []byte("short"), "dfid", 1, 1)
	assert.ErrorIs(t, err, errAGCKMalformed)
	// right length but wrong magic
	bad := make([]byte, len(cekMagic)+1+agdfNonceLen+cekSize+agdfTagLen)
	_, err = UnwrapCEK(fek, bad, "dfid", 1, 1)
	assert.ErrorIs(t, err, errAGCKMalformed)
}

func TestWrapCEK_WrongCEKSize(t *testing.T) {
	fek := randomCEK(t)
	_, err := WrapCEK(fek, make([]byte, 16), "dfid", 1, 1)
	assert.Error(t, err)
}
