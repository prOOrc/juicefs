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
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetFileCrypto(t *testing.T) {
	if os.Getenv("SKIP_NON_CORE") == "true" {
		t.Skipf("skip non-core test")
	}
	m, err := newRedisMeta("redis", "127.0.0.1:6379/13", testConfig())
	require.NoError(t, err)
	rm := m.(*redisMeta)
	defer rm.Shutdown()
	require.NoError(t, rm.rdb.FlushAll(Background()).Err())

	if err := m.Init(testFormat(), true); err != nil {
		t.Fatalf("init: %s", err)
	}
	if _, err := m.Load(true); err != nil {
		t.Fatalf("load: %s", err)
	}

	ctx := Background()
	var inode Ino
	var attr Attr
	if st := m.Mknod(ctx, RootInode, "encfile", TypeFile, 0644, 022, 0, "", &inode, &attr); st != 0 {
		t.Fatalf("mknod: %s", st)
	}

	crypto := &FileCrypto{
		WrappedFek:  []byte("agfk-blob"),
		DriveFileID: "dfid-1",
		FekVersion:  1,
		CryptoAlg:   "AES-256-GCM",
	}
	if st := rm.SetFileCrypto(ctx, inode, crypto); st != 0 {
		t.Fatalf("setfilecrypto: %s", st)
	}

	var got Attr
	if st := m.GetAttr(ctx, inode, &got); st != 0 {
		t.Fatalf("getattr: %s", st)
	}
	require.True(t, got.Encrypted)
	require.Equal(t, []byte("agfk-blob"), got.WrappedFek)
	require.Equal(t, "dfid-1", got.DriveFileID)
	require.EqualValues(t, 1, got.FekVersion)
	require.Equal(t, "AES-256-GCM", got.CryptoAlg)
	// non-crypto fields are preserved
	require.EqualValues(t, 0644, got.Mode)
	require.EqualValues(t, TypeFile, got.Typ)

	t.Run("missing inode returns ENOENT", func(t *testing.T) {
		st := rm.SetFileCrypto(ctx, Ino(999999), crypto)
		require.Equal(t, syscall.ENOENT, st)
	})

	t.Run("update overwrites previous crypto fields", func(t *testing.T) {
		crypto2 := &FileCrypto{
			WrappedFek:  []byte("agfk-blob-v2"),
			DriveFileID: "dfid-1",
			FekVersion:  2,
			CryptoAlg:   "AES-256-GCM",
		}
		require.Equal(t, syscall.Errno(0), rm.SetFileCrypto(ctx, inode, crypto2))
		var got2 Attr
		require.Equal(t, syscall.Errno(0), m.GetAttr(ctx, inode, &got2))
		require.Equal(t, []byte("agfk-blob-v2"), got2.WrappedFek)
		require.EqualValues(t, 2, got2.FekVersion)
	})
}
