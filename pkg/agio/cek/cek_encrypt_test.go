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

package cek

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

func TestWrapCEK_RoundTrip(t *testing.T) {
	fek := randomCEK(t)
	cek := randomCEK(t)
	wrapped, err := WrapCEK(fek, cek, "drive-file-uuid", 99, 2)
	require.NoError(t, err)
	assert.Len(t, wrapped, len(cekMagic)+1+cekNonceLen+cekSize+cekTagLen) // 65 bytes
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
	bad := make([]byte, len(cekMagic)+1+cekNonceLen+cekSize+cekTagLen)
	_, err = UnwrapCEK(fek, bad, "dfid", 1, 1)
	assert.ErrorIs(t, err, errAGCKMalformed)
}

func TestWrapCEK_WrongCEKSize(t *testing.T) {
	fek := randomCEK(t)
	_, err := WrapCEK(fek, make([]byte, 16), "dfid", 1, 1)
	assert.Error(t, err)
}
