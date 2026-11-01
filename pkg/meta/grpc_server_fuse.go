/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
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
	"context"
	"fmt"
	"path"
	"syscall"
	"time"

	"github.com/google/uuid"
	authzpb "github.com/juicedata/juicefs/pkg/meta/authz_pb"
	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// connectionIsTLS reports whether the RPC arrived over a TLS connection
// (implement-grpc-tls: fail-closed plaintext FEK delivery).
func connectionIsTLS(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return false
	}
	_, ok = p.AuthInfo.(credentials.TLSInfo)
	return ok
}

func (s *MetaProxyServer) StatFS(ctx context.Context, req *pb.StatFSRequest) (*pb.StatFSResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var total, avail, iused, iavail uint64
	errno := s.meta.StatFS(mctx, Ino(req.Ino), &total, &avail, &iused, &iavail)
	return &pb.StatFSResponse{
		Errno:      uint32(errno),
		TotalSpace: total,
		AvailSpace: avail,
		Iused:      iused,
		Iavail:     iavail,
	}, nil
}

func (s *MetaProxyServer) Lookup(ctx context.Context, req *pb.LookupRequest) (*pb.LookupResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Lookup(mctx, Ino(req.Parent), req.Name, &inode, &attr, req.CheckPermission)

	if errno == 0 {
		childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if childPath != "" {
			s.inodePathCache.Set(inode, withDirSlash(childPath, &attr))
		}
	}

	return &pb.LookupResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Resolve(ctx context.Context, req *pb.ResolveRequest) (*pb.ResolveResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Resolve(mctx, Ino(req.Parent), req.Path, &inode, &attr, req.Force)
	return &pb.ResolveResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Access(ctx context.Context, req *pb.AccessRequest) (*pb.AccessResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.Access(mctx, Ino(req.Inode), uint8(req.Modemask), &attr)
	return &pb.AccessResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) GetAttr(ctx context.Context, req *pb.GetAttrRequest) (*pb.GetAttrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.GetAttr(mctx, Ino(req.Inode), &attr)
	return &pb.GetAttrResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) SetAttr(ctx context.Context, req *pb.SetAttrRequest) (*pb.SetAttrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	attr := ProtoToAttr(req.Attr)
	errno := s.meta.SetAttr(mctx, Ino(req.Inode), uint16(req.Set), uint8(req.Sggidclearmode), attr)
	return &pb.SetAttrResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(attr),
	}, nil
}

func (s *MetaProxyServer) CheckSetAttr(ctx context.Context, req *pb.CheckSetAttrRequest) (*pb.CheckSetAttrResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	attr := *ProtoToAttr(req.Attr)
	errno := s.meta.CheckSetAttr(mctx, Ino(req.Inode), uint16(req.Set), attr)
	return &pb.CheckSetAttrResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Mknod(ctx context.Context, req *pb.MknodRequest) (*pb.MknodResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Mknod(mctx, Ino(req.Parent), req.Name, uint8(req.Type),
		uint16(req.Mode), uint16(req.Cumask), req.Rdev, req.Path, &inode, &attr)

	if errno == 0 {
		childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if childPath != "" {
			s.inodePathCache.Set(inode, withDirSlash(childPath, &attr))
		}

		// Encryption (decision B5): a regular file created via Mknod on an
		// encrypted volume gets its crypto binding exactly like a Create;
		// fifo/device/socket nodes are unaffected. No plaintext FEK rides in
		// MknodResponse — the first write goes through Open, which delivers it.
		if s.encryptionEnabled() && req.Type == uint32(TypeFile) && s.keyManager != nil && len(attr.WrappedFek) == 0 {
			driveFileID := uuid.New().String() // MknodRequest carries no client-side id (FR-USR-3 covers Create)
			st := s.provisionFileCrypto(ctx, mctx, Ino(req.Parent), req.Name, inode, childPath, driveFileID, &attr)
			if st != 0 && st != syscall.EEXIST {
				return &pb.MknodResponse{Errno: uint32(st)}, nil
			}
			// The EEXIST sentinel (lost provisioning race) is success here: attr
			// was refreshed with the standing binding and Mknod delivers no FEK.
		}
	}

	return &pb.MknodResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Mkdir(ctx context.Context, req *pb.MkdirRequest) (*pb.MkdirResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Mkdir(mctx, Ino(req.Parent), req.Name, uint16(req.Mode),
		uint16(req.Cumask), uint8(req.Copysgid), &inode, &attr)

	if errno == 0 {
		childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if childPath != "" {
			s.inodePathCache.Set(inode, withDirSlash(childPath, &attr))
		}
	}

	return &pb.MkdirResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Create(ctx context.Context, req *pb.CreateRequest) (*pb.CreateResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Create(mctx, Ino(req.Parent), req.Name, uint16(req.Mode),
		uint16(req.Cumask), uint32(req.Flags), &inode, &attr)

	if errno != 0 {
		return &pb.CreateResponse{Errno: uint32(errno)}, nil
	}

	childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
	if childPath != "" {
		s.inodePathCache.Set(inode, withDirSlash(childPath, &attr))
	}

	// Encryption: issue the per-file FEK via KeyManager (FR-USR-1). The company
	// is derived server-side from the path (decision 3.8) — the client never
	// sends it.
	var fek []byte
	var fekVer uint32
	if s.encryptionEnabled() && attr.Typ == TypeFile && s.keyManager != nil {
		if len(attr.WrappedFek) > 0 {
			// O_CREAT overwrite of a file that already carries a binding
			// (decision B2): the binding must stay as-is — re-issuing it with a
			// new drive_file_id would break the AAD of the existing slices'
			// CEKs. Serve the existing binding's FEK best-effort: POSIX requires
			// O_CREAT-without-O_EXCL to succeed, and the client falls back to
			// ResolveFileKey when no FEK is delivered.
			var err error
			fek, fekVer, err = s.fetchFEKForCreate(ctx, inode, &attr, childPath, req.Flags)
			if err != nil {
				logger.Warnf("create inode %d: deliver existing binding FEK: %v — client falls back to ResolveFileKey", inode, err)
			}
		} else {
			// New file, or a legacy file without a binding: provision the first
			// binding (decision B2). A provisioning failure rolls the file back
			// (FR-USR-2) — a file whose key could not even be created is
			// unusable. The provisioning write is a compare-and-swap: a loser of
			// the create race (EEXIST sentinel) must not re-bind the inode — it
			// joins the idempotent overwrite path above with the standing
			// binding (fix-create-crypto-binding-race, design D2).
			st := s.provisionFileCrypto(ctx, mctx, Ino(req.Parent), req.Name, inode, childPath, req.DriveFileId, &attr)
			if st != 0 && st != syscall.EEXIST {
				return &pb.CreateResponse{Errno: uint32(st)}, nil
			}
			// The creating client must encrypt its very first write session
			// (design Addendum 2026-11-01, B1): deliver the plaintext FEK here.
			// A delivery failure rolls the file back only when this call created
			// the inode: with O_EXCL set, success proves the inode is fresh (an
			// existing file would have answered EEXIST above) and the CAS was
			// won (the EEXIST sentinel proves the inode existed). For a may-be-
			// existing file (legacy overwrite or lost race) deleting would
			// destroy data older than this call — delivery stays best-effort:
			// the client falls back to ResolveFileKey and the writer fails
			// closed without a FEK (B4).
			var err error
			fek, fekVer, err = s.fetchFEKForCreate(ctx, inode, &attr, childPath, req.Flags)
			if err != nil {
				if req.Flags&syscall.O_EXCL != 0 && st != syscall.EEXIST {
					_ = s.meta.Unlink(mctx, Ino(req.Parent), req.Name) // rollback (FR-USR-2)
					return &pb.CreateResponse{Errno: uint32(syscall.EACCES)}, nil
				}
				logger.Warnf("create inode %d: deliver fresh binding FEK: %v — file kept, writer fails closed without a FEK", inode, err)
			}
		}
	}

	resp := &pb.CreateResponse{Errno: 0, Inode: uint64(inode), Attr: AttrToProto(&attr)}
	if len(fek) > 0 {
		resp.Fek = fek
		resp.FekVersion = int32(fekVer)
	}
	return resp, nil
}

// provisionFileCrypto issues the first crypto binding for a regular file created
// on an encrypted volume (Create/Mknod): KeyManager CreateFileKey followed by a
// compare-and-swap write (SetFileCryptoIfUnbound), updating attr in place. Any
// failure removes the file (Unlink) and returns the errno to answer — only valid
// for files this very call created (FR-USR-2). The EEXIST sentinel (raised by
// the CAS write only) means a concurrent creator won the race: the inode
// existed, so nothing is rolled back and attr is refreshed with the standing
// binding — the caller switches to the idempotent overwrite path
// (fix-create-crypto-binding-race, design D2).
func (s *MetaProxyServer) provisionFileCrypto(ctx context.Context, mctx Context, parent Ino, name string, inode Ino, childPath string, driveFileID string, attr *Attr) syscall.Errno {
	userID, err := extractUserIDFromOIDC(ctx) // FR-ID-3: UUID validation (decision 3.7)
	if err != nil {
		_ = s.meta.Unlink(mctx, parent, name)
		return syscall.EACCES // fail-closed
	}
	resp, err := s.keyManager.CreateFileKey(ctx, &kmpb.CreateFileKeyRequest{
		UserId:      userID,
		VolumeUuid:  s.meta.GetFormat().UUID,
		DriveFileId: driveFileID,
		Inode:       int64(inode),
		Path:        childPath,
		VolumeName:  s.volumeName,
	})
	if err != nil {
		_ = s.meta.Unlink(mctx, parent, name) // rollback (FR-USR-2)
		return syscall.EIO
	}
	setter, ok := s.meta.(fileCryptoSetter)
	if !ok {
		_ = s.meta.Unlink(mctx, parent, name)
		return syscall.EIO
	}
	cryptoAlg := resp.CryptoAlg
	if cryptoAlg == "" {
		cryptoAlg = "AES-256-GCM"
	}
	c := &FileCrypto{
		WrappedFek:  resp.WrappedFek,
		DriveFileID: driveFileID,
		FekVersion:  uint32(resp.FekVersion),
		CryptoAlg:   cryptoAlg,
	}
	// The EEXIST sentinel is only meaningful from the CAS write — its contract
	// (unlike the unconditional SetFileCrypto) defines it as "already bound".
	if cas, ok := s.meta.(fileCryptoSetterCAS); ok {
		if st := cas.SetFileCryptoIfUnbound(mctx, inode, c); st != 0 {
			if st != syscall.EEXIST {
				_ = s.meta.Unlink(mctx, parent, name)
				return st
			}
			// A concurrent creator provisioned the binding between the attr
			// read and the CAS. The inode existed, so no rollback ever applies
			// here — serve the standing binding instead.
			if gerr := s.meta.GetAttr(mctx, inode, attr); gerr != 0 {
				logger.Warnf("create inode %d: fetch standing binding after lost race: %s", inode, gerr)
			}
			return syscall.EEXIST
		}
	} else {
		// Engines without the CAS keep today's unconditional write: no
		// regression, the binding race stays the known limitation (design B2).
		// Its errors never read as the lost-race sentinel.
		logger.Warnf("engine %T does not support SetFileCryptoIfUnbound — provisioning unconditionally (create-time binding race possible)", s.meta)
		if st := setter.SetFileCrypto(mctx, inode, c); st != 0 {
			_ = s.meta.Unlink(mctx, parent, name)
			return st
		}
	}
	attr.Encrypted = true
	attr.DriveFileID = driveFileID
	attr.FekVersion = uint32(resp.FekVersion)
	attr.WrappedFek = resp.WrappedFek
	attr.CryptoAlg = cryptoAlg
	return 0
}

// fetchFEKForCreate delivers the plaintext FEK for a CreateResponse (decision
// B1): the file is either freshly provisioned or already bound (O_CREAT
// overwrite), and the creating client needs the key to encrypt its first write
// session. TLS only — over a plaintext connection nothing is fetched and the
// fields stay empty (the Open TLS contract, nil/nil/nil error).
func (s *MetaProxyServer) fetchFEKForCreate(ctx context.Context, inode Ino, attr *Attr, path string, flags uint32) ([]byte, uint32, error) {
	if !connectionIsTLS(ctx) {
		return nil, 0, nil
	}
	userID, err := extractUserIDFromOIDC(ctx)
	if err != nil {
		return nil, 0, err
	}
	fekResp, err := s.keyManager.GetFileFEK(ctx, &kmpb.GetFileFEKRequest{
		UserId:      userID,
		VolumeUuid:  s.meta.GetFormat().UUID,
		DriveFileId: attr.DriveFileID,
		Inode:       int64(inode),
		Path:        path,
		WrappedFek:  attr.WrappedFek,
		WriteAccess: flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0,
		FekVersion:  attr.FekVersion,
		VolumeName:  s.volumeName,
	})
	if err != nil {
		return nil, 0, err
	}
	return fekResp.Fek, uint32(fekResp.FekVersion), nil
}

func (s *MetaProxyServer) Open(ctx context.Context, req *pb.OpenRequest) (*pb.OpenResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.Open(mctx, Ino(req.Inode), uint32(req.Flags), &attr)
	if errno != 0 {
		return &pb.OpenResponse{Errno: uint32(errno)}, nil
	}
	resp := &pb.OpenResponse{Errno: 0, Attr: AttrToProto(&attr)}

	if attr.Encrypted {
		// Fail-closed FEK (implement-grpc-tls): on an encrypted volume the
		// plaintext FEK never leaves the server over a plaintext connection,
		// regardless of the listener mode. Volumes without volume-level
		// encryption enabled are unaffected.
		if s.encryptionEnabled() && !connectionIsTLS(ctx) {
			return nil, status.Error(codes.Unauthenticated, "plaintext FEK delivery requires a TLS connection")
		}
		resp.Encrypted = true
		resp.FekVersion = int32(attr.FekVersion)
		// Client cache hit: authz was already checked by the interceptor and the
		// client holds this FEK version, so KeyManager is not called (decision 3.1).
		if s.keyManager != nil && req.CachedFekVersion != attr.FekVersion {
			forWrite := req.Flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0
			path, gone := s.resolvePath(ctx, Ino(req.Inode))
			if gone {
				return &pb.OpenResponse{Errno: uint32(syscall.ENOENT)}, nil
			}
			if path == "" {
				return &pb.OpenResponse{Errno: uint32(syscall.EACCES)}, nil // fail-closed
			}
			userID, err := extractUserIDFromOIDC(ctx) // FR-ID-3 (decision 3.7)
			if err != nil {
				return &pb.OpenResponse{Errno: uint32(syscall.EACCES)}, nil // fail-closed
			}
			fekResp, err := s.keyManager.GetFileFEK(ctx, &kmpb.GetFileFEKRequest{
				UserId:      userID,
				VolumeUuid:  s.meta.GetFormat().UUID,
				DriveFileId: attr.DriveFileID,
				Inode:       int64(req.Inode),
				Path:        path,
				WrappedFek:  attr.WrappedFek,
				WriteAccess: forWrite,
				FekVersion:  attr.FekVersion,
				VolumeName:  s.volumeName,
			})
			if err != nil {
				return &pb.OpenResponse{Errno: uint32(syscall.EACCES)}, nil // fail-closed (NFR-AVAIL-3)
			}
			resp.Fek = fekResp.Fek
			resp.FekVersion = int32(fekResp.FekVersion)
		}
	}
	return resp, nil
}

// ResolveFileKey returns the plaintext FEK of an encrypted file (task 5.2).
// Authz is Read on the inode (interceptor). Non-encrypted files answer
// encrypted=false without a KeyManager round-trip; any failure fails closed
// with EACCES (NFR-AVAIL-3) — the client must never fall back to a wrong key.
func (s *MetaProxyServer) ResolveFileKey(ctx context.Context, req *pb.ResolveFileKeyRequest) (*pb.ResolveFileKeyResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	if errno := s.meta.GetAttr(mctx, Ino(req.Inode), &attr); errno != 0 {
		return &pb.ResolveFileKeyResponse{Errno: uint32(errno)}, nil
	}
	resp := &pb.ResolveFileKeyResponse{Errno: 0}
	if !attr.Encrypted || s.keyManager == nil {
		return resp, nil
	}
	resp.Encrypted = true
	resp.FekVersion = int32(attr.FekVersion)
	resp.DriveFileId = attr.DriveFileID
	// Fail-closed FEK (implement-grpc-tls): same TLS rule as Open — the
	// GetFileFEK-derived plaintext never leaves the server over a plaintext
	// connection on an encrypted volume.
	if s.encryptionEnabled() && !connectionIsTLS(ctx) {
		return nil, status.Error(codes.Unauthenticated, "plaintext FEK delivery requires a TLS connection")
	}
	path, gone := s.resolvePath(ctx, Ino(req.Inode))
	if gone {
		return &pb.ResolveFileKeyResponse{Errno: uint32(syscall.ENOENT)}, nil
	}
	if path == "" {
		return &pb.ResolveFileKeyResponse{Errno: uint32(syscall.EACCES)}, nil // fail-closed
	}
	userID, err := extractUserIDFromOIDC(ctx)
	if err != nil {
		return &pb.ResolveFileKeyResponse{Errno: uint32(syscall.EACCES)}, nil // fail-closed
	}
	fekResp, err := s.keyManager.GetFileFEK(ctx, &kmpb.GetFileFEKRequest{
		UserId:      userID,
		VolumeUuid:  s.meta.GetFormat().UUID,
		DriveFileId: attr.DriveFileID,
		Inode:       int64(req.Inode),
		Path:        path,
		WrappedFek:  attr.WrappedFek,
		WriteAccess: false, // read-only FEK (client cache refill / compaction)
		FekVersion:  attr.FekVersion,
		VolumeName:  s.volumeName,
	})
	if err != nil {
		return &pb.ResolveFileKeyResponse{Errno: uint32(syscall.EACCES)}, nil // fail-closed (NFR-AVAIL-3)
	}
	resp.Fek = fekResp.Fek
	resp.FekVersion = int32(fekResp.FekVersion)
	return resp, nil
}

// RotateFileKey rotates the FEK of one encrypted file (offboarding, task 7.5).
// The caller's OIDC identity is used for the KeyManager calls; admin_user_id
// must match it (the field exists for audit, a mismatch is rejected). Any
// failure is fail-closed (see rotateFileFEK).
func (s *MetaProxyServer) RotateFileKey(ctx context.Context, req *pb.RotateFileKeyRequest) (*pb.RotateFileKeyResponse, error) {
	fail := func(e syscall.Errno) *pb.RotateFileKeyResponse {
		return &pb.RotateFileKeyResponse{Errno: uint32(e)}
	}
	if s.keyManager == nil {
		return fail(syscall.EOPNOTSUPP), nil
	}
	userID, err := extractUserIDFromOIDC(ctx)
	if err != nil || userID != req.GetAdminUserId() {
		return fail(syscall.EACCES), nil // fail-closed: identity must match the admin claim
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	if st := s.meta.GetAttr(mctx, Ino(req.Inode), &attr); st != 0 {
		return fail(st), nil
	}
	if !attr.Encrypted || len(attr.WrappedFek) == 0 {
		return fail(syscall.EINVAL), nil // legacy file — nothing to rotate
	}
	path := s.inodePathCache.Get(Ino(req.Inode))
	if path == "" {
		return fail(syscall.EACCES), nil // fail-closed: cannot prove file identity to KeyManager
	}
	st := s.rotateFileFEK(ctx, mctx, userID, Ino(req.Inode), &attr, path)
	if st != 0 {
		return fail(st), nil
	}
	var newAttr Attr
	if st := s.meta.GetAttr(mctx, Ino(req.Inode), &newAttr); st != 0 {
		return fail(st), nil
	}
	return &pb.RotateFileKeyResponse{Errno: 0, NewFekVersion: int32(newAttr.FekVersion)}, nil
}

// rotateFileFEK performs the FEK rotation sequence for one file (task 7.5):
// GetFileFEK(old) -> platform RotateFileFEK (org-admin check + audit
// server-side) -> SetFileCrypto(v+1) -> RewrapSlices. Fail-closed at every
// step; no rollback is needed — the old key stays valid until SetFileCrypto,
// and RewrapSlices is atomic per chunk.
func (s *MetaProxyServer) rotateFileFEK(ctx context.Context, mctx Context, userID string, inode Ino, attr *Attr, path string) syscall.Errno {
	volUUID := s.meta.GetFormat().UUID
	// 1. Current FEK (Read on the path, enforced by the platform).
	oldResp, err := s.keyManager.GetFileFEK(ctx, &kmpb.GetFileFEKRequest{
		UserId: userID, VolumeUuid: volUUID, DriveFileId: attr.DriveFileID,
		Inode: int64(inode), Path: path, WrappedFek: attr.WrappedFek,
		WriteAccess: false, FekVersion: attr.FekVersion, VolumeName: s.volumeName,
	})
	if err != nil {
		return syscall.EACCES // fail-closed (NFR-AVAIL-3)
	}
	oldFek := oldResp.Fek
	// 2. New FEK from the platform (org-admin check + audit server-side).
	rot, err := s.keyManager.RotateFileFEK(ctx, &kmpb.RotateFileFEKRequest{
		UserId: userID, VolumeUuid: volUUID, DriveFileId: attr.DriveFileID,
		Inode: int64(inode), Path: path, CurrentFekVersion: attr.FekVersion, VolumeName: s.volumeName,
	})
	if err != nil {
		utils.MemClear(oldFek)
		return syscall.EACCES // fail-closed
	}
	newFek := rot.PlaintextFek
	cryptoAlg := rot.CryptoAlg
	if cryptoAlg == "" {
		cryptoAlg = "AES-256-GCM"
	}
	newVersion := uint32(rot.FekVersion)
	// 3. Persist the new wrapped FEK (from here on the old key is not authoritative).
	setter, ok := s.meta.(fileCryptoSetter)
	if !ok {
		utils.MemClear(oldFek)
		utils.MemClear(newFek)
		return syscall.EIO
	}
	if st := setter.SetFileCrypto(mctx, inode, &FileCrypto{
		WrappedFek:  rot.WrappedFek,
		DriveFileID: attr.DriveFileID,
		FekVersion:  newVersion,
		CryptoAlg:   cryptoAlg,
	}); st != 0 {
		utils.MemClear(oldFek)
		utils.MemClear(newFek)
		return st
	}
	// 4. Re-wrap every slice CEK under the new FEK (zero-copy; atomic per chunk).
	rewrapper, ok := s.meta.(sliceRewrapper)
	if !ok {
		utils.MemClear(oldFek)
		utils.MemClear(newFek)
		return syscall.EOPNOTSUPP
	}
	st := rewrapper.RewrapSlices(mctx, inode, oldFek, newFek,
		SliceCryptoAAD{DriveFileID: attr.DriveFileID, FekVersion: attr.FekVersion},
		SliceCryptoAAD{DriveFileID: attr.DriveFileID, FekVersion: newVersion})
	utils.MemClear(oldFek)
	utils.MemClear(newFek)
	return st
}

// defaultRotateRate is the per-call rate limit of RotateFileKeysByPaths in
// files/second (task 7.6).
const defaultRotateRate = 10

// maxRotateBatchPaths caps one RotateFileKeysByPaths call (task 7.6).
const maxRotateBatchPaths = 100

// RotateFileKeysByPaths rotates the FEKs of a batch of files by full client
// path (offboarding, task 7.6): rate-limited, resumable via checkpoint (the
// index into the caller's path list), and every skipped path is reported with
// its reason while still advancing the checkpoint. The caller's OIDC identity
// must match admin_user_id; each file goes through the same fail-closed
// sequence as RotateFileKey.
func (s *MetaProxyServer) RotateFileKeysByPaths(ctx context.Context, req *pb.RotateFileKeysByPathsRequest) (*pb.RotateFileKeysByPathsResponse, error) {
	resp := &pb.RotateFileKeysByPathsResponse{}
	if s.keyManager == nil {
		resp.Errno = uint32(syscall.EOPNOTSUPP)
		return resp, nil
	}
	userID, err := extractUserIDFromOIDC(ctx)
	if err != nil || userID != req.GetAdminUserId() {
		resp.Errno = uint32(syscall.EACCES) // fail-closed: identity must match the admin claim
		return resp, nil
	}
	paths := req.GetPaths()
	if len(paths) > maxRotateBatchPaths {
		resp.Errno = uint32(syscall.EINVAL)
		return resp, nil
	}
	start := int(req.GetCheckpoint())
	if start < 0 || start > len(paths) {
		resp.Errno = uint32(syscall.EINVAL)
		return resp, nil
	}
	// No MetaContext in the request: the caller is an org admin (interceptor +
	// platform check), and per-file authz is enforced by KeyManager on each path.
	mctx := s.metaCtx(ctx, nil)
	resp.NextCheckpoint = int64(start) // a fully-consumed batch reports its own end
	var interval time.Duration
	if s.rotateRate > 0 {
		interval = time.Second / time.Duration(s.rotateRate)
	}
	for i := start; i < len(paths); i++ {
		if i > start && interval > 0 {
			select {
			case <-ctx.Done():
				resp.NextCheckpoint = int64(i) // resume where we stopped
				return resp, nil
			case <-time.After(interval):
			}
		}
		p := paths[i]
		var inode Ino
		var attr Attr
		st := s.meta.Resolve(mctx, RootInode, p, &inode, &attr, true)
		if st == 0 && (!attr.Encrypted || len(attr.WrappedFek) == 0) {
			st = syscall.EINVAL // legacy file — nothing to rotate
		}
		if st == 0 {
			st = s.rotateFileFEK(ctx, mctx, userID, inode, &attr, p)
		}
		if st == 0 {
			resp.RotatedPaths = append(resp.RotatedPaths, p)
		} else {
			resp.SkippedPaths = append(resp.SkippedPaths, fmt.Sprintf("%s: %s", p, st))
		}
		resp.NextCheckpoint = int64(i + 1) // skipped paths advance the checkpoint too
	}
	return resp, nil
}

// resolvePath returns the cached path of ino, attempting server-side recovery
// through the engine on a miss (fix-meta-proxy-session-churn D1). gone=true
// means the engine reports the inode as nonexistent (design I7) — answer
// ENOENT; an unresolved path without gone stays fail-closed at the caller.
func (s *MetaProxyServer) resolvePath(ctx context.Context, ino Ino) (p string, gone bool) {
	if p = s.inodePathCache.Get(ino); p != "" {
		return p, false
	}
	if s.pathResolver == nil {
		return "", false
	}
	p, resolved, gone := s.pathResolver.Resolve(ctx, ino)
	if gone {
		return "", true
	}
	if resolved {
		return p, false
	}
	return "", false
}

// resolvedParentPath returns the parent's path (cache, then recovery), or ""
// when even recovery cannot reconstruct it.
func (s *MetaProxyServer) resolvedParentPath(ctx context.Context, parent Ino) string {
	p, _ := s.resolvePath(ctx, parent)
	return p
}

func (s *MetaProxyServer) Close(ctx context.Context, req *pb.CloseRequest) (*pb.CloseResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Close(mctx, Ino(req.Inode))
	return &pb.CloseResponse{Errno: uint32(errno)}, nil
}

// invalidateChild drops the cached path of a removed child (design D5). The
// parent is recovered through the engine when it is not cached, so delete
// hygiene keeps working on a cold proxy (restart / eviction).
func (s *MetaProxyServer) invalidateChild(ctx context.Context, parent Ino, name string, dir bool) {
	childPath := s.inodePathCache.BuildChildPath(parent, name)
	if childPath == "" {
		parentPath := s.resolvedParentPath(ctx, parent)
		if parentPath == "" {
			return // unresolvable parent — stale descendants heal via recovery
		}
		childPath = path.Join(parentPath, name)
	}
	if childIno := s.inodePathCache.GetInodeByPath(childPath); childIno != 0 {
		s.pathResolver.Invalidate(childIno)
	}
	if dir {
		s.inodePathCache.RemoveSubtree(childPath)
		return
	}
	s.inodePathCache.UnsetByPath(childPath)
}

func (s *MetaProxyServer) Unlink(ctx context.Context, req *pb.UnlinkRequest) (*pb.UnlinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Unlink(mctx, Ino(req.Parent), req.Name, req.SkipCheckTrash)

	if errno == 0 {
		s.invalidateChild(ctx, Ino(req.Parent), string(req.Name), false)
	}

	return &pb.UnlinkResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Rmdir(ctx context.Context, req *pb.RmdirRequest) (*pb.RmdirResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Rmdir(mctx, Ino(req.Parent), req.Name, req.SkipCheckTrash)

	if errno == 0 {
		s.invalidateChild(ctx, Ino(req.Parent), string(req.Name), true)
	}

	return &pb.RmdirResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Rename(ctx context.Context, req *pb.RenameRequest) (*pb.RenameResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Rename(mctx, Ino(req.ParentSrc), req.NameSrc,
		Ino(req.ParentDst), req.NameDst, uint32(req.Flags), &inode, &attr)

	if errno == 0 && inode > 0 {
		oldPath := s.inodePathCache.BuildChildPath(Ino(req.ParentSrc), string(req.NameSrc))
		if oldPath == "" {
			// Parent not cached (restart / eviction): recover it so the rename
			// can rewrite the subtree mapping (design D5).
			if p := s.resolvedParentPath(ctx, Ino(req.ParentSrc)); p != "" {
				oldPath = path.Join(p, string(req.NameSrc))
			}
		}
		newPath := s.inodePathCache.BuildChildPath(Ino(req.ParentDst), string(req.NameDst))
		if newPath == "" {
			if p := s.resolvedParentPath(ctx, Ino(req.ParentDst)); p != "" {
				newPath = path.Join(p, string(req.NameDst))
			}
		}
		// Directories are stored with a trailing slash (authz path convention).
		oldPath = withDirSlash(oldPath, &attr)
		newPath = withDirSlash(newPath, &attr)

		if newPath != "" {
			// If destination already exists in cache and is a different inode,
			// remove its mapping first (rename overwrites the destination).
			if dstInode := s.inodePathCache.GetInodeByPath(newPath); dstInode != 0 && dstInode != inode {
				s.inodePathCache.RemoveSubtree(newPath)
				s.pathResolver.Invalidate(dstInode)
			}

			// If old path is known, update subtree (covers both the entry and children).
			if oldPath != "" {
				s.inodePathCache.RenameSubtree(oldPath, newPath)
			}

			// Guarantee that the moved inode has a mapping to the new path.
			if !s.inodePathCache.Move(inode, newPath) {
				s.inodePathCache.Set(inode, newPath)
			}
		}
	}

	return &pb.RenameResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Link(ctx context.Context, req *pb.LinkRequest) (*pb.LinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.Link(mctx, Ino(req.InodeSrc), Ino(req.Parent), req.Name, &attr)

	// Link creates a new directory entry for an existing inode.
	// The inode already has a path; the new name is an additional reference.
	// We don't update the cache because an inode can have multiple paths (hardlinks).
	// Authz checks use the first-cached path, which is sufficient for company-scoped permissions.
	return &pb.LinkResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Symlink(ctx context.Context, req *pb.SymlinkRequest) (*pb.SymlinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var inode Ino
	var attr Attr
	errno := s.meta.Symlink(mctx, Ino(req.Parent), req.Name, req.Path, &inode, &attr)

	if errno == 0 {
		childPath := s.inodePathCache.BuildChildPath(Ino(req.Parent), string(req.Name))
		if childPath != "" {
			s.inodePathCache.Set(inode, withDirSlash(childPath, &attr))
		}
	}

	return &pb.SymlinkResponse{
		Errno: uint32(errno),
		Inode: uint64(inode),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) ReadLink(ctx context.Context, req *pb.ReadLinkRequest) (*pb.ReadLinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var path []byte
	errno := s.meta.ReadLink(mctx, Ino(req.Inode), &path)
	return &pb.ReadLinkResponse{
		Errno: uint32(errno),
		Path:  path,
	}, nil
}

func (s *MetaProxyServer) Truncate(ctx context.Context, req *pb.TruncateRequest) (*pb.TruncateResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var attr Attr
	errno := s.meta.Truncate(mctx, Ino(req.Inode), uint8(req.Flags), req.Length, &attr, req.SkipPermCheck)
	return &pb.TruncateResponse{
		Errno: uint32(errno),
		Attr:  AttrToProto(&attr),
	}, nil
}

func (s *MetaProxyServer) Fallocate(ctx context.Context, req *pb.FallocateRequest) (*pb.FallocateResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var length uint64
	errno := s.meta.Fallocate(mctx, Ino(req.Inode), uint8(req.Mode), req.Off, req.Size, &length)
	return &pb.FallocateResponse{
		Errno:  uint32(errno),
		Length: length,
	}, nil
}

func (s *MetaProxyServer) Readdir(ctx context.Context, req *pb.ReaddirRequest) (*pb.ReaddirResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var entries []*Entry
	errno := s.meta.Readdir(mctx, Ino(req.Inode), uint8(req.Wantattr), &entries)

	if errno != 0 {
		return &pb.ReaddirResponse{Errno: uint32(errno)}, nil
	}

	// Post-filter first: filter entries by authorization (like MinIO's filterWalkResultCh)
	filtered, errno := s.filterEntriesByAuthz(ctx, Ino(req.Inode), entries)
	if errno != 0 {
		return &pb.ReaddirResponse{Errno: uint32(errno)}, nil
	}

	// Cache only the filtered (allowed) entries — prevents cache pollution from
	// unauthorized paths and avoids hardlink first-seen being set from forbidden listings.
	if len(filtered) > 0 {
		mappings := make(map[Ino]string)
		for _, e := range filtered {
			childPath := s.inodePathCache.BuildChildPath(Ino(req.Inode), string(e.Name))
			if childPath != "" {
				mappings[e.Inode] = withDirSlash(childPath, e.Attr)
			}
		}
		if len(mappings) > 0 {
			s.inodePathCache.SetMany(mappings)
		}
	}

	protoEntries := make([]*pb.ProtoEntry, len(filtered))
	for i, e := range filtered {
		protoEntries[i] = EntryToProto(e)
	}
	return &pb.ReaddirResponse{Errno: 0, Entries: protoEntries}, nil
}

// filterEntriesByAuthz filters directory entries by user permissions.
// Follows MinIO's pattern: get all results from backend, then filter by batch authz check.
// Entries the user cannot see are simply not returned to the client.
// A nonzero errno fails the whole listing (design D2): a parent whose path is
// unrecoverable even after engine-side reconstruction answers EACCES instead
// of a silent empty listing — "empty dir, errno 0" reads like data loss.
// An empty result with errno 0 stays legitimate for a known path with zero
// authorized children.
func (s *MetaProxyServer) filterEntriesByAuthz(ctx context.Context, parentInode Ino, entries []*Entry) ([]*Entry, syscall.Errno) {
	if s.authzInterceptor == nil || !s.authzInterceptor.enabled {
		return entries, 0 // no authz configured — return all
	}

	userID, err := s.authzInterceptor.extractUserID(ctx)
	if err != nil || userID == "" {
		authzLogger.Warnf("Readdir: missing or invalid userID — returning empty list (deny): %v", err)
		return nil, 0 // fail-closed: no user = no access
	}

	// Pre-check: user must have View on the parent directory to list it at all.
	parentPath, gone := s.resolvePath(ctx, parentInode)
	if gone {
		return nil, syscall.ENOENT // the directory was deleted — honest ENOENT
	}
	if parentPath == "" {
		authzLogger.Debugf("Readdir: parent inode %d unrecoverable — EACCES (fail-closed)", parentInode)
		return nil, syscall.EACCES // can't resolve parent path even after recovery
	}

	// Root is always viewable (same as isAlwaysAllowed in the interceptor).
	if parentPath != "/" {
		allowedParent, err := s.authzInterceptor.client.CheckPermission(ctx, userID, parentPath, AuthzPermissionView, authzpb.Operation_OP_OTHER)
		if err != nil || !allowedParent {
			authzLogger.Debugf("Readdir: parent check denied for user=%s path=%s err=%v", userID, parentPath, err)
			return nil, 0 // fail-closed: can't list this directory
		}
	}

	// Build paths for all entries (already cached by SetMany above)
	paths := make([]string, 0, len(entries))
	validIndices := make([]int, 0, len(entries))
	for i, e := range entries {
		name := string(e.Name)
		if name == "." || name == ".." {
			// Synthetic entries (baseMeta.Readdir prepends them); the kernel
			// synthesizes its own, and authzListing strips them before the
			// filter — never bulk-check or forward them.
			continue
		}
		p := s.inodePathCache.Get(e.Inode)
		if p == "" {
			// Fallback: build from the actual parent inode (not RootInode).
			// Directories get a trailing slash (authz path convention).
			p = withDirSlash(s.inodePathCache.BuildChildPath(parentInode, string(e.Name)), e.Attr)
		}
		if p == "" || p == "/" {
			continue // empty or root path — skip this entry
		}
		paths = append(paths, p)
		validIndices = append(validIndices, i)
	}

	if len(paths) == 0 {
		return nil, 0
	}

	// Batch check permissions (View — can see the entry in directory listing)
	allowed, err := s.authzInterceptor.client.CheckBulkPermissions(ctx, userID, paths, AuthzPermissionView)
	if err != nil {
		authzLogger.Warnf("Readdir batch authz error: %v — returning empty list (fail-closed)", err)
		return nil, 0 // fail-closed on authz error
	}

	if len(allowed) != len(paths) {
		authzLogger.Warnf("Readdir authz bulk result length mismatch: %d != %d — returning empty list",
			len(allowed), len(paths))
		return nil, 0
	}

	// Filter: keep only allowed entries
	filtered := make([]*Entry, 0, len(entries))
	for i, idx := range validIndices {
		if allowed[i] {
			filtered = append(filtered, entries[idx])
		}
	}

	if len(filtered) != len(entries) {
		authzLogger.Debugf("Readdir filtered %d/%d entries for user %s", len(filtered), len(entries), userID)
	}

	return filtered, 0
}

func (s *MetaProxyServer) Read(ctx context.Context, req *pb.ReadRequest) (*pb.ReadResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var slices []Slice
	errno := s.meta.Read(mctx, Ino(req.Inode), req.Indx, &slices)
	return &pb.ReadResponse{
		Errno:  uint32(errno),
		Slices: SlicesToProto(slices),
	}, nil
}

func (s *MetaProxyServer) Write(ctx context.Context, req *pb.WriteRequest) (*pb.WriteResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	slice := ProtoToSlice(req.Slice)
	// WriteRequest.Mtime is microseconds since the epoch: the client sends
	// mtime.UnixNano()/1000 (grpc_client_data.Write). Interpreting it as
	// seconds stored a time ~1e6 years in the future (the "year 56835320"
	// stat corruption).
	errno := s.meta.Write(mctx, Ino(req.Inode), req.Indx, req.Off, slice, time.UnixMicro(req.GetMtime()))
	return &pb.WriteResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) NewSlice(ctx context.Context, req *pb.NewSliceRequest) (*pb.NewSliceResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var id uint64
	errno := s.meta.NewSlice(mctx, &id)
	return &pb.NewSliceResponse{
		Errno: uint32(errno),
		Id:    id,
	}, nil
}

func (s *MetaProxyServer) InvalidateChunkCache(ctx context.Context, req *pb.InvalidateChunkCacheRequest) (*pb.InvalidateChunkCacheResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	s.meta.InvalidateChunkCache(mctx, Ino(req.Inode), req.Indx)
	return &pb.InvalidateChunkCacheResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) CopyFileRange(ctx context.Context, req *pb.CopyFileRangeRequest) (*pb.CopyFileRangeResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var copied, outLength uint64

	// FR-OP-4: the slice-sharing path copies records verbatim, so an encrypted
	// source shares slices whose CEKs are wrapped under the SOURCE FEK. The target
	// must re-wrap them under its own FEK (post-op). A legacy target cannot hold
	// encrypted slices at all — fail closed before touching metadata.
	var srcAttr, dstAttr Attr
	if st := s.meta.GetAttr(mctx, Ino(req.Fin), &srcAttr); st != 0 {
		return &pb.CopyFileRangeResponse{Errno: uint32(st)}, nil
	}
	srcEncrypted := srcAttr.Encrypted && len(srcAttr.WrappedFek) > 0
	if srcEncrypted {
		if st := s.meta.GetAttr(mctx, Ino(req.Fout), &dstAttr); st != 0 {
			return &pb.CopyFileRangeResponse{Errno: uint32(st)}, nil
		}
		if !dstAttr.Encrypted || len(dstAttr.WrappedFek) == 0 {
			return &pb.CopyFileRangeResponse{Errno: uint32(syscall.EOPNOTSUPP)}, nil
		}
	}

	errno := s.meta.CopyFileRange(mctx, Ino(req.Fin), req.OffIn, Ino(req.Fout),
		req.OffOut, req.Size, uint32(req.Flags), &copied, &outLength)
	if errno != 0 {
		return &pb.CopyFileRangeResponse{Errno: uint32(errno), Copied: copied, OutLength: outLength}, nil
	}

	if srcEncrypted && copied > 0 {
		if st := s.copyFileRangeRewrap(ctx, mctx, Ino(req.Fin), Ino(req.Fout), req.OffOut, copied, &srcAttr, &dstAttr); st != 0 {
			return &pb.CopyFileRangeResponse{Errno: uint32(st)}, nil
		}
	}
	return &pb.CopyFileRangeResponse{
		Errno:     0,
		Copied:    copied,
		OutLength: outLength,
	}, nil
}

// copyFileRangeRewrap re-wraps the CEKs of the copied range in the target's chunk
// lists from under the source FEK to under the target FEK (design 5.5): both FEKs
// are resolved via KeyManager (Read on the source, Write on the target — authz is
// enforced by the platform), then RewrapSlicesRange touches only the chunk lists
// covering [offOut, offOut+copied). S3 data is not touched. Any failure fails
// closed — the caller must treat the target range as unreadable.
func (s *MetaProxyServer) copyFileRangeRewrap(ctx context.Context, mctx Context, fin, fout Ino, offOut, copied uint64, srcAttr, dstAttr *Attr) syscall.Errno {
	srcPath := s.inodePathCache.Get(fin)
	dstPath := s.inodePathCache.Get(fout)
	if srcPath == "" || dstPath == "" {
		return syscall.EACCES // fail-closed: cannot prove file identity to KeyManager
	}
	userID, err := extractUserIDFromOIDC(ctx)
	if err != nil {
		return syscall.EACCES // fail-closed
	}
	volUUID := s.meta.GetFormat().UUID
	fekOf := func(attr *Attr, path string, ino Ino, writeAccess bool) ([]byte, error) {
		r, err := s.keyManager.GetFileFEK(ctx, &kmpb.GetFileFEKRequest{
			UserId: userID, VolumeUuid: volUUID, DriveFileId: attr.DriveFileID,
			Inode: int64(ino), Path: path, WrappedFek: attr.WrappedFek,
			WriteAccess: writeAccess, FekVersion: attr.FekVersion, VolumeName: s.volumeName,
		})
		if err != nil {
			return nil, err
		}
		return r.Fek, nil
	}
	srcFek, err := fekOf(srcAttr, srcPath, fin, false)
	if err != nil {
		return syscall.EACCES // fail-closed (NFR-AVAIL-3)
	}
	dstFek, err := fekOf(dstAttr, dstPath, fout, true)
	if err != nil {
		return syscall.EACCES // fail-closed
	}
	rewrapper, ok := s.meta.(sliceRangeRewrapper)
	if !ok {
		return syscall.EOPNOTSUPP
	}
	start := uint32(offOut / ChunkSize)
	end := uint32((offOut + copied - 1) / ChunkSize)
	return rewrapper.RewrapSlicesRange(mctx, fout, srcFek, dstFek,
		SliceCryptoAAD{DriveFileID: srcAttr.DriveFileID, FekVersion: srcAttr.FekVersion},
		SliceCryptoAAD{DriveFileID: dstAttr.DriveFileID, FekVersion: dstAttr.FekVersion},
		start, end)
}
