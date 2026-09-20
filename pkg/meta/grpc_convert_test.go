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
	"testing"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
)

func TestAttrToProto_Crypto(t *testing.T) {
	t.Run("plain attr has no FileCrypto", func(t *testing.T) {
		a := &Attr{Mode: 0o644, Length: 10}
		p := AttrToProto(a)
		assert.Nil(t, p.FileCrypto)
	})

	t.Run("encrypted attr round-trips", func(t *testing.T) {
		a := &Attr{

			Mode:        0o644,
			Length:      1024,
			Encrypted:   true,
			DriveFileID: "d1",
			FekVersion:  3,
			CryptoAlg:   "AES-256-GCM",
			WrappedFek:  []byte("agfk-blob"),
		}
		p := AttrToProto(a)
		assert.NotNil(t, p.FileCrypto)
		assert.Equal(t, []byte("agfk-blob"), p.FileCrypto.WrappedFek)
		assert.Equal(t, "d1", p.FileCrypto.DriveFileId)
		assert.True(t, p.FileCrypto.Encrypted)
		assert.EqualValues(t, 3, p.FileCrypto.FekVersion)
		assert.Equal(t, "AES-256-GCM", p.FileCrypto.CryptoAlg)

		back := ProtoToAttr(p)
		assert.True(t, back.Encrypted)
		assert.Equal(t, "d1", back.DriveFileID)
		assert.EqualValues(t, 3, back.FekVersion)
		assert.Equal(t, "AES-256-GCM", back.CryptoAlg)
		assert.Equal(t, []byte("agfk-blob"), back.WrappedFek)
	})

	t.Run("wrapped FEK without Encrypted flag still serializes", func(t *testing.T) {
		a := &Attr{WrappedFek: []byte("blob")}
		p := AttrToProto(a)
		assert.NotNil(t, p.FileCrypto)
		assert.Equal(t, []byte("blob"), p.FileCrypto.WrappedFek)
	})

	t.Run("plaintext FEK never comes back from proto", func(t *testing.T) {
		a := &Attr{Encrypted: true, Fek: []byte("plaintext-fek")}
		p := AttrToProto(a)
		assert.Nil(t, p.FileCrypto.WrappedFek)
		back := ProtoToAttr(p)
		assert.Nil(t, back.Fek)
	})
}

func TestSliceToProto_WrappedCEK(t *testing.T) {
	s := Slice{Id: 9, Size: 4096, Off: 0, Len: 100, WrappedCEK: []byte("agck-blob")}
	p := SliceToProto(s)
	assert.Equal(t, []byte("agck-blob"), p.WrappedCek)

	back := ProtoToSlice(p)
	assert.Equal(t, []byte("agck-blob"), back.WrappedCEK)
	assert.EqualValues(t, 9, back.Id)
	assert.EqualValues(t, 4096, back.Size)

	// plain slice: no CEK
	p2 := SliceToProto(Slice{Id: 1})
	assert.Nil(t, p2.WrappedCek)
}

func TestFormatToProto_Encryption(t *testing.T) {
	f := Format{
		Name:              "vol",
		UUID:              "uuid-1",
		EncryptionEnabled: true,
		KEKVersion:        5,
	}
	p := FormatToProto(f)
	assert.True(t, p.EncryptionEnabled)
	assert.EqualValues(t, 5, p.KekVersion)

	back := ProtoToFormat(p)
	assert.True(t, back.EncryptionEnabled)
	assert.Equal(t, 5, back.KEKVersion)
	assert.Equal(t, "vol", back.Name)

	// legacy volume: fields stay zero
	p2 := FormatToProto(Format{Name: "legacy"})
	assert.False(t, p2.EncryptionEnabled)
	assert.EqualValues(t, 0, p2.KekVersion)
}

func TestProtoFileCrypto_NilSafe(t *testing.T) {
	var nilAttr *pb.ProtoAttr
	assert.Nil(t, ProtoToAttr(nilAttr))
	assert.Nil(t, AttrToProto(nil))
	assert.Empty(t, ProtoToSlice(nil))
}
