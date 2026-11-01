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

// Spec scenarios of implement-grpc-tls ("Plaintext FEK delivery requires TLS")
// exercised over a real gRPC transport (bufconn): plaintext Open on an
// encrypted volume is refused with Unauthenticated, TLS Open delivers the FEK,
// unencrypted volumes behave exactly as before.

import (
	"context"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestOpenFEKPlaintextRefused (scenario: Open on encrypted volume over
// plaintext connection): the server answers Unauthenticated and the response
// carries no plaintext FEK.
func TestOpenFEKPlaintextRefused(t *testing.T) {
	ts := newFekTransportServer(t, true, false)

	_, err := ts.Open(ts.userSubCtx(context.Background()), &pb.OpenRequest{
		Inode: testFekInode, Flags: uint32(syscall.O_RDONLY), CachedFekVersion: 1,
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err),
		"plaintext Open on an encrypted volume must be refused, cache hits included")

	// The same holds for ResolveFileKey (GetFileFEK-derived fields).
	_, err = ts.ResolveFileKey(ts.userSubCtx(context.Background()), &pb.ResolveFileKeyRequest{
		Inode: testFekInode,
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// TestOpenFEKOverTLS (scenario: Open on encrypted volume over TLS connection):
// the plaintext FEK is delivered, subject to the existing checks.
func TestOpenFEKOverTLS(t *testing.T) {
	ts := newFekTransportServer(t, true, true)

	resp, err := ts.Open(ts.userSubCtx(context.Background()), &pb.OpenRequest{
		Inode: testFekInode, Flags: uint32(syscall.O_RDONLY),
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)
	require.True(t, resp.Encrypted)
	require.Equal(t, ts.fek, resp.Fek, "TLS Open must deliver the plaintext FEK")
}

// TestResolveFileKeyOverTLS: the read-only FEK path (cache refill/compaction)
// delivers the plaintext FEK over TLS.
func TestResolveFileKeyOverTLS(t *testing.T) {
	ts := newFekTransportServer(t, true, true)

	resp, err := ts.ResolveFileKey(ts.userSubCtx(context.Background()), &pb.ResolveFileKeyRequest{
		Inode: testFekInode,
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)
	require.True(t, resp.Encrypted)
	require.Equal(t, ts.fek, resp.Fek)
}

// TestOpenUnencryptedVolumeUnaffected (scenario: Unencrypted volumes
// unaffected): a plaintext Open on a volume without volume-level encryption
// behaves exactly as before — including FEK delivery for legacy per-file
// encrypted attrs.
func TestOpenUnencryptedVolumeUnaffected(t *testing.T) {
	ts := newFekTransportServer(t, false, false)

	resp, err := ts.Open(ts.userSubCtx(context.Background()), &pb.OpenRequest{
		Inode: testFekInode, Flags: uint32(syscall.O_RDONLY),
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)
	require.True(t, resp.Encrypted)
	require.Equal(t, ts.fek, resp.Fek, "unencrypted volume: no TLS requirement")
}

// createFakeMeta answers Create/Mknod with a configurable attr and records
// Unlink rollbacks and SetFileCrypto calls (the fileCryptoSetter the proxy
// type-asserts). Any other call panics on the embedded nil Meta.
type createFakeMeta struct {
	Meta

	format     *Format
	createAttr Attr // attr the engine answers for Create/Mknod
	ino        Ino  // inode the engine answers

	unlinks    int
	setCrypto  int
	lastCrypto FileCrypto
}

func (f *createFakeMeta) Create(_ Context, _ Ino, _ string, _, _ uint16, _ uint32, inode *Ino, attr *Attr) syscall.Errno {
	*inode = f.ino
	*attr = f.createAttr
	return 0
}

func (f *createFakeMeta) Mknod(_ Context, _ Ino, _ string, _type uint8, _, _ uint16, _ uint32, _ string, inode *Ino, attr *Attr) syscall.Errno {
	*inode = f.ino
	*attr = f.createAttr
	attr.Typ = _type
	return 0
}

func (f *createFakeMeta) Unlink(_ Context, _ Ino, _ string, _ ...bool) syscall.Errno {
	f.unlinks++
	return 0
}

func (f *createFakeMeta) SetFileCrypto(_ Context, _ Ino, c *FileCrypto) syscall.Errno {
	f.setCrypto++
	f.lastCrypto = *c
	return 0
}

func (f *createFakeMeta) GetFormat() Format { return *f.format }

// testFekCreateInode is the inode the createFakeMeta engine answers with.
const testFekCreateInode = 100

// newFekCreateTransportServer serves a Create/Mknod-focused proxy over bufconn
// (TLS per tlsOn): the engine answers with createAttr at testFekCreateInode,
// root is cached so child paths resolve, and the volume is encryption-enabled.
func newFekCreateTransportServer(t *testing.T, tlsOn bool, km *fakeKeyManager, format *Format, createAttr Attr) (*fekTransportServer, *createFakeMeta) {
	t.Helper()
	m := &createFakeMeta{format: format, createAttr: createAttr, ino: testFekCreateInode}
	srv := NewMetaProxyServer(m, 100)
	srv.SetKeyManager(km)
	srv.InodePathCache().Set(RootInode, "/")
	srv.SetVolumeName(format.Name)
	ts := serveFekTransport(t, srv, tlsOn, nil)
	return ts, m
}

// TestCreateReturnsFekOverTLS (scenario "First write session is encrypted"):
// over TLS the CreateResponse carries the plaintext FEK of the freshly
// provisioned binding, so the creating client encrypts its very first write
// session without a close/re-open.
func TestCreateReturnsFekOverTLS(t *testing.T) {
	km := newFakeKeyManager()
	format := &Format{Name: "create-tls-test", UUID: uuid.New().String(), EncryptionEnabled: true}
	ts, m := newFekCreateTransportServer(t, true, km, format, Attr{Typ: TypeFile})

	resp, err := ts.Create(ts.userSubCtx(context.Background()), &pb.CreateRequest{
		Parent: uint64(RootInode), Name: "new.exr", Mode: 0o644, Cumask: 0o22,
		Flags:       uint32(syscall.O_CREAT | syscall.O_EXCL | syscall.O_WRONLY),
		DriveFileId: "dfid-create-1",
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)
	require.Len(t, resp.Fek, fekSize, "TLS Create must deliver the plaintext FEK for the first write session")
	require.EqualValues(t, 1, resp.FekVersion)

	fc := resp.Attr.GetFileCrypto()
	require.NotNil(t, fc)
	require.True(t, fc.GetEncrypted())
	require.Equal(t, "dfid-create-1", fc.GetDriveFileId(), "the binding must use the client-generated id")
	require.Len(t, fc.GetWrappedFek(), agfkLen)

	km.mu.Lock()
	require.Equal(t, 1, km.createCalls)
	require.Equal(t, 1, km.getFekCalls, "the response FEK is fetched via GetFileFEK")
	km.mu.Unlock()
	require.Equal(t, 1, m.setCrypto)
	require.Equal(t, "dfid-create-1", m.lastCrypto.DriveFileID)
	require.Equal(t, 0, m.unlinks, "a successful create must not roll back")
}

// TestCreatePlaintextConnNoFek: over a plaintext connection the Create
// provisioning still happens (wrapped fields only), but the response carries no
// plaintext FEK — the Open TLS contract (implement-grpc-tls) applied to the
// create path; the KeyManager is not even asked.
func TestCreatePlaintextConnNoFek(t *testing.T) {
	km := newFakeKeyManager()
	format := &Format{Name: "create-plain-test", UUID: uuid.New().String(), EncryptionEnabled: true}
	ts, m := newFekCreateTransportServer(t, false, km, format, Attr{Typ: TypeFile})

	resp, err := ts.Create(ts.userSubCtx(context.Background()), &pb.CreateRequest{
		Parent: uint64(RootInode), Name: "new.exr", Mode: 0o644, Cumask: 0o22,
		Flags: uint32(syscall.O_CREAT | syscall.O_WRONLY), DriveFileId: "dfid-plain-1",
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)
	require.Empty(t, resp.Fek, "plaintext connection must not carry the FEK")
	require.Zero(t, resp.FekVersion)

	fc := resp.Attr.GetFileCrypto()
	require.NotNil(t, fc)
	require.True(t, fc.GetEncrypted(), "the wrapped binding is provisioned regardless of TLS")

	km.mu.Lock()
	require.Equal(t, 1, km.createCalls)
	require.Equal(t, 0, km.getFekCalls, "the plaintext FEK is never fetched over a plaintext channel")
	km.mu.Unlock()
	require.Equal(t, 1, m.setCrypto)
	require.Equal(t, 0, m.unlinks)
}

// TestCreateOverwriteKeepsExistingBinding (scenario "Overwrite does not rebind
// the crypto identity"): O_CREAT without O_EXCL over an already-bound file must
// not call CreateFileKey/SetFileCrypto — a new drive_file_id would break the
// AAD of the existing slices' CEKs — and serves the existing binding's FEK.
func TestCreateOverwriteKeepsExistingBinding(t *testing.T) {
	km := newFakeKeyManager()
	format := &Format{Name: "create-overwrite-test", UUID: uuid.New().String(), EncryptionEnabled: true}
	fek := randomBytes(t, fekSize)
	wrapped, err := WrapFEK(km.kek, fek, FekAAD{
		VolumeUUID: format.UUID, CompanyID: testCompanyID,
		DriveFileID: "dfid-existing", Inode: testFekCreateInode, FekVersion: 1,
	}, 1)
	require.NoError(t, err)
	ts, m := newFekCreateTransportServer(t, true, km, format, Attr{
		Typ: TypeFile, Encrypted: true, DriveFileID: "dfid-existing", FekVersion: 1, WrappedFek: wrapped,
	})

	resp, err := ts.Create(ts.userSubCtx(context.Background()), &pb.CreateRequest{
		Parent: uint64(RootInode), Name: "existing.exr", Mode: 0o644, Cumask: 0o22,
		Flags: uint32(syscall.O_CREAT | syscall.O_WRONLY), DriveFileId: "dfid-fresh",
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)

	km.mu.Lock()
	require.Equal(t, 0, km.createCalls, "an existing binding must not be re-issued")
	require.Equal(t, 1, km.getFekCalls, "the existing binding's FEK is served via GetFileFEK")
	km.mu.Unlock()
	require.Equal(t, 0, m.setCrypto, "SetFileCrypto must not rebind the file")
	require.Equal(t, 0, m.unlinks)

	fc := resp.Attr.GetFileCrypto()
	require.NotNil(t, fc)
	require.Equal(t, "dfid-existing", fc.GetDriveFileId(), "drive_file_id must stay unchanged")
	require.Equal(t, wrapped, fc.GetWrappedFek())
	require.Len(t, resp.Fek, fekSize)
	require.Equal(t, fek, resp.Fek, "the delivered FEK is the existing binding's")
	require.EqualValues(t, 1, resp.FekVersion)
}

// TestCreateLegacyFileGetsBinding: a legacy unencrypted regular file resolved
// by Create on an encrypted volume receives its first binding (decision B2) —
// the file survives, and the response carries the new binding's plaintext FEK.
func TestCreateLegacyFileGetsBinding(t *testing.T) {
	km := newFakeKeyManager()
	format := &Format{Name: "create-legacy-test", UUID: uuid.New().String(), EncryptionEnabled: true}
	ts, m := newFekCreateTransportServer(t, true, km, format, Attr{
		Typ: TypeFile, Length: 4096, Mode: 0o644, Mtime: 12345, Uid: 1, Gid: 1,
	})

	resp, err := ts.Create(ts.userSubCtx(context.Background()), &pb.CreateRequest{
		Parent: uint64(RootInode), Name: "legacy.txt", Mode: 0o644, Cumask: 0o22,
		Flags: uint32(syscall.O_CREAT | syscall.O_WRONLY), DriveFileId: "dfid-legacy-1",
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)

	fc := resp.Attr.GetFileCrypto()
	require.NotNil(t, fc)
	require.True(t, fc.GetEncrypted(), "the legacy file must receive its first binding")
	require.Equal(t, "dfid-legacy-1", fc.GetDriveFileId())
	require.Len(t, resp.Fek, fekSize, "the new binding's FEK is delivered for the first write session")

	km.mu.Lock()
	require.Equal(t, 1, km.createCalls)
	require.Equal(t, 1, km.getFekCalls)
	km.mu.Unlock()
	require.Equal(t, 1, m.setCrypto)
	require.Equal(t, 0, m.unlinks, "a legacy file must not be rolled back")
}

// TestCreateDeliveryFailureRollbackScope (fix-review P1): a FEK-delivery
// failure after successful provisioning rolls back only an inode this call
// created — O_EXCL makes that certain (an existing file would have answered
// EEXIST). A may-be-existing file (legacy overwrite) survives: the writer
// fails closed (B4) instead of the call destroying older data.
func TestCreateDeliveryFailureRollbackScope(t *testing.T) {
	cases := []struct {
		name       string
		excl       bool
		createAttr Attr // engine attr: fresh file vs pre-existing legacy file
		wantErrno  syscall.Errno
		wantUnlink int
	}{
		{
			name:       "new file (O_EXCL) is rolled back",
			excl:       true,
			createAttr: Attr{Typ: TypeFile},
			wantErrno:  syscall.EACCES,
			wantUnlink: 1,
		},
		{
			name:       "legacy overwrite survives delivery failure",
			excl:       false,
			createAttr: Attr{Typ: TypeFile, Length: 4096, Mode: 0o644, Uid: 1, Gid: 1},
			wantErrno:  0,
			wantUnlink: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			km := newFakeKeyManager()
			format := &Format{Name: "delivery-fail-test", UUID: uuid.New().String(), EncryptionEnabled: true}
			ts, m := newFekCreateTransportServer(t, true, km, format, c.createAttr)
			km.mu.Lock()
			km.denyGetFek = true // CreateFileKey succeeds, the delivery (GetFileFEK) fails
			km.mu.Unlock()

			flags := uint32(syscall.O_CREAT | syscall.O_WRONLY)
			if c.excl {
				flags |= syscall.O_EXCL
			}
			resp, err := ts.Create(ts.userSubCtx(context.Background()), &pb.CreateRequest{
				Parent: uint64(RootInode), Name: "f.exr", Mode: 0o644, Cumask: 0o22,
				Flags: flags, DriveFileId: "dfid-delivery-1",
			})
			require.NoError(t, err)
			require.EqualValues(t, c.wantErrno, resp.Errno)
			require.Equal(t, c.wantUnlink, m.unlinks)
			require.Equal(t, 1, m.setCrypto, "the binding was provisioned in both cases")
			km.mu.Lock()
			require.Equal(t, 1, km.createCalls)
			km.mu.Unlock()
			if c.wantErrno == 0 {
				require.Empty(t, resp.Fek, "no FEK could be delivered")
				fc := resp.Attr.GetFileCrypto()
				require.NotNil(t, fc)
				require.True(t, fc.GetEncrypted(), "the fresh binding stays on the surviving file")
			}
		})
	}
}

// TestMknodRegularFileProvisionsBinding (scenario "Mknod regular file gets a
// binding"): Mknod of a regular file on an encrypted volume provisions the
// crypto binding (server-generated drive_file_id); fifo/device nodes are
// unaffected.
func TestMknodRegularFileProvisionsBinding(t *testing.T) {
	cases := []struct {
		name      string
		typ       uint32
		wantBound bool
	}{
		{"regular file", uint32(TypeFile), true},
		{"fifo unaffected", uint32(TypeFIFO), false},
		{"char device unaffected", uint32(TypeCharDev), false},
		{"block device unaffected", uint32(TypeBlockDev), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			km := newFakeKeyManager()
			format := &Format{Name: "mknod-test", UUID: uuid.New().String(), EncryptionEnabled: true}
			ts, m := newFekCreateTransportServer(t, true, km, format, Attr{Typ: TypeFile})

			resp, err := ts.Mknod(ts.userSubCtx(context.Background()), &pb.MknodRequest{
				Parent: uint64(RootInode), Name: "node", Type: c.typ, Mode: 0o644, Cumask: 0o22,
			})
			require.NoError(t, err)
			require.EqualValues(t, 0, resp.Errno)

			fc := resp.Attr.GetFileCrypto()
			if c.wantBound {
				require.NotNil(t, fc)
				require.True(t, fc.GetEncrypted())
				require.NotEmpty(t, fc.GetDriveFileId(), "the proxy generates the drive_file_id (no client field on Mknod)")
				require.Len(t, fc.GetWrappedFek(), agfkLen)
				km.mu.Lock()
				require.Equal(t, 1, km.createCalls)
				require.Equal(t, 0, km.getFekCalls, "no FEK delivery on the mknod path")
				km.mu.Unlock()
				require.Equal(t, 1, m.setCrypto)
				require.Equal(t, 0, m.unlinks)
			} else {
				require.Nil(t, fc, "non-regular nodes must stay unbound")
				km.mu.Lock()
				require.Equal(t, 0, km.createCalls)
				km.mu.Unlock()
				require.Equal(t, 0, m.setCrypto)
			}
		})
	}
}

// writeCaptureMeta records the mtime the Write handler forwards to the engine.
type writeCaptureMeta struct {
	Meta

	mu    sync.Mutex
	inode Ino
	slice Slice
	mtime time.Time
}

func (w *writeCaptureMeta) Write(_ Context, inode Ino, _ uint32, _ uint32, slice Slice, mtime time.Time) syscall.Errno {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inode, w.slice, w.mtime = inode, slice, mtime
	return 0
}

// TestWriteMtimeMicroseconds pins the WriteRequest.Mtime unit contract: the
// client sends microseconds since the epoch (grpc_client_data.Write:
// mtime.UnixNano()/1000), so the handler must reconstruct the instant with
// time.UnixMicro. Interpreting the value as seconds scaled the file mtime by
// 1e6 — the "year 56835320" stat corruption seen on stage acceptance.
func TestWriteMtimeMicroseconds(t *testing.T) {
	capture := &writeCaptureMeta{}
	s := NewMetaProxyServer(capture, 0)

	now := time.Now()
	sent := now.UnixNano() / 1000 // exactly what the client puts on the wire
	resp, err := s.Write(context.Background(), &pb.WriteRequest{
		Inode: 7,
		Slice: SliceToProto(Slice{Id: 9, Size: 4, Off: 0, Len: 4}),
		Mtime: sent,
	})
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.Errno)

	capture.mu.Lock()
	defer capture.mu.Unlock()
	got := capture.mtime
	require.Equal(t, sent/1e6, got.Unix(), "mtime seconds must be the microsecond value scaled down, not echoed")
	require.Equal(t, int((sent%1e6)*1e3), got.Nanosecond(), "sub-second part must carry the microsecond remainder")
	require.Less(t, got.Year(), 2300, "mtime must stay in a sane range")
}
