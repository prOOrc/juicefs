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
	"syscall"
	"testing"

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
