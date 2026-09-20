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
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/juicedata/juicefs/pkg/utils"
)

// legacyAttrBytes replicates the pre-encryption Attr.Marshal layout to pin
// the legacy format byte-for-byte (NFR-COMPAT-1).
func legacyAttrBytes(a Attr, size uint32) []byte {
	w := utils.NewBuffer(size)
	w.Put8(a.Flags)
	w.Put16((uint16(a.Typ) << 12) | (a.Mode & 0xfff))
	w.Put32(a.Uid)
	w.Put32(a.Gid)
	w.Put64(uint64(a.Atime))
	w.Put32(a.Atimensec)
	w.Put64(uint64(a.Mtime))
	w.Put32(a.Mtimensec)
	w.Put64(uint64(a.Ctime))
	w.Put32(a.Ctimensec)
	w.Put32(a.Nlink)
	w.Put64(a.Length)
	w.Put32(a.Rdev)
	w.Put64(uint64(a.Parent))
	if a.AccessACL+a.DefaultACL > 0 {
		w.Put32(a.AccessACL)
		w.Put32(a.DefaultACL)
	}
	if a.Tier != 0 {
		w.Put8(a.Tier)
	}
	return w.Bytes()
}

func TestAttrMarshalLegacyByteIdentical(t *testing.T) {
	base := Attr{Typ: TypeFile, Mode: 0o644, Uid: 1, Gid: 2, Atime: 100, Mtime: 200, Ctime: 300, Nlink: 1, Length: 4096, Parent: 7}

	cases := []struct {
		name string
		attr Attr
		size uint32
	}{
		{"plain", base, 72},
		{"tier", func() Attr { a := base; a.Tier = 3; return a }(), 73},
		{"acl", func() Attr { a := base; a.AccessACL = 11; a.DefaultACL = 12; return a }(), 80},
		{"acl+tier", func() Attr { a := base; a.AccessACL = 11; a.Tier = 3; return a }(), 81},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.attr.Marshal()
			if len(got) != int(c.size) {
				t.Fatalf("marshal len = %d, want %d", len(got), c.size)
			}
			want := legacyAttrBytes(c.attr, c.size)
			if !bytes.Equal(got, want) {
				t.Fatalf("legacy marshal changed:\n got %x\nwant %x", got, want)
			}
			var out Attr
			out.Unmarshal(got)
			if out.Encrypted || out.WrappedFek != nil || out.DriveFileID != "" || out.FekVersion != 0 || out.CryptoAlg != "" {
				t.Fatalf("legacy attr must not parse crypto fields: %+v", out)
			}
			if out.Typ != c.attr.Typ || out.Mode != c.attr.Mode || out.Uid != c.attr.Uid || out.Length != c.attr.Length ||
				out.Parent != c.attr.Parent || out.AccessACL != c.attr.AccessACL || out.DefaultACL != c.attr.DefaultACL || out.Tier != c.attr.Tier {
				t.Fatalf("round-trip mismatch: in=%+v out=%+v", c.attr, out)
			}
		})
	}
}

func TestAttrRoundTripEncrypted(t *testing.T) {
	wrappedFek := bytes.Repeat([]byte{0x5A}, 69) // AGFK blob size
	in := Attr{
		Typ: TypeFile, Mode: 0o600, Uid: 42, Gid: 43, Atime: 1, Mtime: 2, Ctime: 3, Nlink: 1, Length: 8192, Parent: 9,
		AccessACL: 5, DefaultACL: 6, Tier: 2,
		Encrypted: true, DriveFileID: "08a0b1c2-d3e4-5f60-7182-93a4b5c6d7e8", FekVersion: 3, CryptoAlg: "AES-256-GCM", WrappedFek: wrappedFek,
	}
	buf := in.Marshal()
	suffixLen := 1 + 4 + len(wrappedFek) + 4 + len(in.DriveFileID) + 4 + 1 + len(in.CryptoAlg)
	if want := 72 + 8 + 1 + suffixLen; len(buf) != want {
		t.Fatalf("marshal len = %d, want %d", len(buf), want)
	}

	var out Attr
	out.Unmarshal(buf)
	if !out.Encrypted {
		t.Fatal("Encrypted flag lost in round-trip")
	}
	if !bytes.Equal(out.WrappedFek, wrappedFek) {
		t.Fatalf("WrappedFek mismatch:\n got %x\nwant %x", out.WrappedFek, wrappedFek)
	}
	if out.DriveFileID != in.DriveFileID || out.FekVersion != in.FekVersion || out.CryptoAlg != in.CryptoAlg {
		t.Fatalf("crypto fields mismatch: %+v", out)
	}
	if out.Typ != in.Typ || out.Mode != in.Mode || out.Uid != in.Uid || out.Length != in.Length ||
		out.Parent != in.Parent || out.AccessACL != in.AccessACL || out.DefaultACL != in.DefaultACL || out.Tier != in.Tier {
		t.Fatalf("base fields mismatch: in=%+v out=%+v", in, out)
	}
}

func TestAttrFekNeverMarshaled(t *testing.T) {
	base := Attr{Typ: TypeFile, Mode: 0o644, Uid: 1, Length: 100, Parent: 2}
	withFek := base
	withFek.Fek = bytes.Repeat([]byte{0xEE}, 32)
	if !bytes.Equal(base.Marshal(), withFek.Marshal()) {
		t.Fatal("transient Fek must not affect the marshaled bytes")
	}
	// and it must not survive a round-trip
	var out Attr
	out.Unmarshal(withFek.Marshal())
	if out.Fek != nil {
		t.Fatalf("Fek must never be unmarshaled, got %x", out.Fek)
	}
}

func TestAttrTierZeroEncrypted(t *testing.T) {
	// Ambiguity case: Tier == 0 on an encrypted attr. The Tier byte must still
	// be written so the suffix marker is not misread as Tier.
	in := Attr{Typ: TypeFile, Mode: 0o644, Uid: 1, Length: 100, Parent: 2,
		Encrypted: true, DriveFileID: "id-1", FekVersion: 1, CryptoAlg: "AES-256-GCM", WrappedFek: bytes.Repeat([]byte{1}, 69)}
	var out Attr
	out.Unmarshal(in.Marshal())
	if !out.Encrypted {
		t.Fatal("encrypted attr with Tier=0 lost its crypto suffix")
	}
	if out.Tier != 0 {
		t.Fatalf("Tier = %d, want 0", out.Tier)
	}
	if out.DriveFileID != "id-1" || out.FekVersion != 1 || out.CryptoAlg != "AES-256-GCM" {
		t.Fatalf("crypto fields mismatch: %+v", out)
	}
}

func TestAttrUnmarshalCorruptSuffix(t *testing.T) {
	valid := Attr{Typ: TypeFile, Mode: 0o644, Uid: 7, Length: 100, Parent: 2,
		Encrypted: true, DriveFileID: "id-1", FekVersion: 1, CryptoAlg: "AES-256-GCM", WrappedFek: bytes.Repeat([]byte{9}, 69)}
	buf := valid.Marshal()
	// Layout offsets: base(71) + ACLs(8) + Tier(1) + marker(1) + wfekLen(4) ...
	const markerOff = 71 + 8 + 1
	const wfekLenOff = markerOff + 1

	cases := []struct {
		name string
		buf  []byte
	}{
		{"bad marker", func() []byte { b := append([]byte(nil), buf...); b[markerOff] = 2; return b }()},
		{"wrappedFek len too large", func() []byte {
			b := append([]byte(nil), buf...)
			binary.BigEndian.PutUint32(b[wfekLenOff:], 0xFFFF)
			return b
		}()},
		{"truncated at marker+1", buf[:markerOff+1]},
		{"truncated in wrappedFek", buf[:len(buf)-20]},
		{"truncated before cryptoAlg", buf[:len(buf)-5]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out Attr
			out.Unmarshal(c.buf) // must not panic
			if out.Encrypted || out.WrappedFek != nil || out.DriveFileID != "" || out.FekVersion != 0 || out.CryptoAlg != "" {
				t.Fatalf("corrupt suffix must fail closed, got %+v", out)
			}
			if out.Uid != 7 || out.Length != 100 || out.Parent != 2 {
				t.Fatalf("base fields must survive a corrupt suffix: %+v", out)
			}
		})
	}
}

func TestFormatEncryptionFieldsJSON(t *testing.T) {
	f := Format{Name: "vol", EncryptionEnabled: true, KEKVersion: 2}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var g Format
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if !g.EncryptionEnabled || g.KEKVersion != 2 {
		t.Fatalf("round-trip mismatch: %+v", g)
	}
	// omitempty: absent when zero (legacy volumes keep their JSON shape)
	b2, err := json.Marshal(Format{Name: "vol"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b2, []byte("EncryptionEnabled")) || bytes.Contains(b2, []byte("KEKVersion")) {
		t.Fatalf("zero encryption fields must be omitted from JSON: %s", b2)
	}
}
