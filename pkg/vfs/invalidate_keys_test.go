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

package vfs

import (
	"bytes"
	crand "crypto/rand"
	"syscall"
	"testing"

	"github.com/juicedata/juicefs/pkg/meta"
)

// TestInvalidateAllKeys verifies that InvalidateAllKeys wipes the plaintext
// FEKs of open handles, marks them stale (Read/Write -> EIO) and leaves legacy
// handles untouched (task 6.1, NFR-SEC-3).
func TestInvalidateAllKeys(t *testing.T) {
	v, _ := createTestVFS(nil, "")
	ctx := NewLogContext(meta.NewContext(10, 1, []uint32{2, 3}))

	fe, e := v.Mknod(ctx, 1, "enc", 0644|syscall.S_IFREG, 0, 0)
	if e != 0 {
		t.Fatalf("mknod enc: %s", e)
	}
	fek := make([]byte, 32)
	if _, err := crand.Read(fek); err != nil {
		t.Fatalf("generate FEK: %s", err)
	}
	attr := &meta.Attr{Encrypted: true, Fek: fek, FekVersion: 1}
	fh := v.newFileHandle(fe.Inode, 0, syscall.O_RDWR, 0, attr)

	fe2, e := v.Mknod(ctx, 1, "leg", 0644|syscall.S_IFREG, 0, 0)
	if e != 0 {
		t.Fatalf("mknod leg: %s", e)
	}
	fh2 := v.newFileHandle(fe2.Inode, 0, syscall.O_RDONLY, 0, nil)

	v.InvalidateAllKeys()

	h := v.findHandle(fe.Inode, fh)
	if h == nil {
		t.Fatal("encrypted handle not found")
	}
	if !h.stale {
		t.Fatal("encrypted handle not marked stale")
	}
	if h.fek != nil {
		t.Fatal("FEK not cleared from the handle")
	}
	if !bytes.Equal(fek, make([]byte, 32)) {
		t.Fatal("FEK backing array not zeroed")
	}

	buf := make([]byte, 4)
	if _, e := v.Read(ctx, fe.Inode, buf, 0, fh); e != syscall.EIO {
		t.Fatalf("read on stale handle = %s, want EIO", e)
	}
	if e := v.Write(ctx, fe.Inode, buf, 0, fh); e != syscall.EIO {
		t.Fatalf("write on stale handle = %s, want EIO", e)
	}

	// legacy handles must keep working (design 6.4: legacy files as now)
	if _, e := v.Read(ctx, fe2.Inode, buf, 0, fh2); e != 0 {
		t.Fatalf("read on legacy handle = %s, want OK", e)
	}
}
