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
	"syscall"
	"time"

	"github.com/google/uuid"
	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *MetaProxyServer) GetFormat(ctx context.Context, req *pb.GetFormatRequest) (*pb.GetFormatResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	format := eng.GetFormat()
	return &pb.GetFormatResponse{
		Errno:  0,
		Format: FormatToProto(format),
	}, nil
}

func (s *MetaProxyServer) Remove(ctx context.Context, req *pb.RemoveRequest) (*pb.RemoveResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	var count uint64
	errno := eng.Remove(mctx, Ino(req.Parent), req.Name, req.SkipTrash, int(req.NumThreads), &count)
	return &pb.RemoveResponse{
		Errno: uint32(errno),
		Count: count,
	}, nil
}

func (s *MetaProxyServer) BatchUnlink(ctx context.Context, req *pb.BatchUnlinkRequest) (*pb.BatchUnlinkResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	entries := ProtoToEntries(req.Entries)
	var count uint64
	errno := eng.BatchUnlink(mctx, Ino(req.Parent), entries, &count, req.SkipCheckTrash)
	return &pb.BatchUnlinkResponse{
		Errno: uint32(errno),
		Count: count,
	}, nil
}

func (s *MetaProxyServer) GetSummary(ctx context.Context, req *pb.GetSummaryRequest) (*pb.GetSummaryResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	var summary Summary
	errno := eng.GetSummary(mctx, Ino(req.Inode), &summary, req.Recursive, req.Strict)
	return &pb.GetSummaryResponse{
		Errno:   uint32(errno),
		Summary: SummaryToProto(&summary),
	}, nil
}

func (s *MetaProxyServer) GetTreeSummary(ctx context.Context, req *pb.GetTreeSummaryRequest) (*pb.GetTreeSummaryResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	var tree TreeSummary
	// SortBySize (zero value) keeps the historical behavior; the sort key is
	// not plumbed through the proxy protocol yet.
	errno := eng.GetTreeSummary(mctx, &tree, uint8(req.Depth), uint8(req.TopN), req.Strict, SortBySize, func(uint64, uint64) {})
	return &pb.GetTreeSummaryResponse{
		Errno: uint32(errno),
		Tree:  TreeSummaryToProto(&tree),
	}, nil
}

func (s *MetaProxyServer) Clone(ctx context.Context, req *pb.CloneRequest) (*pb.CloneResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	var count, total uint64
	var dstIno Ino
	errno := eng.Clone(mctx, Ino(req.SrcParentIno), Ino(req.SrcIno),
		Ino(req.DstParentIno), req.DstName, uint8(req.Cmode), uint16(req.Cumask),
		uint8(req.Concurrency), &count, &total, &dstIno)
	if errno != 0 {
		return &pb.CloneResponse{Errno: uint32(errno), Count: count, Total: total}, nil
	}
	resp := &pb.CloneResponse{Errno: 0, Count: count, Total: total, DstIno: uint64(dstIno)}

	// FR-OP-1..3 (D7): the target must get its own FEK; shared slice CEKs are
	// re-wrapped under it (zero-copy). Any failure rolls the whole clone back —
	// a target with foreign key material is unusable.
	if st := s.cloneEncryption(ctx, eng, mctx, Ino(req.SrcIno), dstIno, Ino(req.DstParentIno), req.DstName); st != 0 {
		var removed uint64
		_ = eng.Remove(mctx, Ino(req.DstParentIno), req.DstName, true, 1, &removed)
		return &pb.CloneResponse{Errno: uint32(st)}, nil
	}

	// Register the target in the path cache (as Create/Mkdir do): cloned entries
	// bypass those handlers, and without this Open/ResolveFileKey on the target
	// would fail closed with EACCES (unresolved path).
	var dstAttr Attr
	if st := eng.GetAttr(mctx, dstIno, &dstAttr); st == 0 {
		dstPath := s.inodePathCache.BuildChildPath(ctx, Ino(req.DstParentIno), req.DstName)
		s.inodePathCache.Set(dstIno, withDirSlash(dstPath, &dstAttr))
	}
	return resp, nil
}

// cloneEncryption orchestrates the per-file FEK sequence for a clone target
// (design 5.2): GetFileFEK(src) → CreateFileKey(dst) → SetFileCrypto(dst) →
// RewrapSlices. Legacy sources need no re-wrap and return 0 immediately.
func (s *MetaProxyServer) cloneEncryption(ctx context.Context, eng Engine, mctx Context, srcIno, dstIno, dstParent Ino, dstName string) syscall.Errno {
	var srcAttr Attr
	if st := eng.GetAttr(mctx, srcIno, &srcAttr); st != 0 {
		return st
	}
	if !srcAttr.Encrypted || len(srcAttr.WrappedFek) == 0 {
		return 0 // legacy clone — no re-wrap
	}
	srcPath := s.inodePathCache.Get(ctx, srcIno)
	dstPath := s.inodePathCache.BuildChildPath(ctx, dstParent, dstName)
	if srcPath == "" || dstPath == "" {
		return syscall.EACCES // fail-closed: cannot prove file identity to KeyManager
	}
	userID, err := extractUserIDFromOIDC(ctx)
	if err != nil {
		return syscall.EACCES // fail-closed
	}
	volUUID := eng.GetFormat().UUID
	ops := &cloneKeyOps{
		srcFek: func(i Ino, p string, a *Attr) ([]byte, error) {
			r, err := s.keyManager.GetFileFEK(ctx, &kmpb.GetFileFEKRequest{
				UserId: userID, VolumeUuid: volUUID, DriveFileId: a.DriveFileID,
				Inode: int64(i), Path: p, WrappedFek: a.WrappedFek,
				WriteAccess: false, FekVersion: a.FekVersion, VolumeName: s.volumeName,
			})
			if err != nil {
				return nil, err
			}
			return r.Fek, nil
		},
		dstFek: func(i Ino, p string) ([]byte, *FileCrypto, error) {
			driveFileID := uuid.New().String()
			created, err := s.keyManager.CreateFileKey(ctx, &kmpb.CreateFileKeyRequest{
				UserId: userID, VolumeUuid: volUUID, DriveFileId: driveFileID,
				Inode: int64(i), Path: p, VolumeName: s.volumeName,
			})
			if err != nil {
				return nil, nil, err
			}
			// CreateFileKey returns only the AGFK wrap; fetch the plaintext for
			// the re-wrap (existing RPCs only — no KeyManager contract change).
			r, err := s.keyManager.GetFileFEK(ctx, &kmpb.GetFileFEKRequest{
				UserId: userID, VolumeUuid: volUUID, DriveFileId: driveFileID,
				Inode: int64(i), Path: p, WrappedFek: created.WrappedFek,
				WriteAccess: true, FekVersion: created.FekVersion, VolumeName: s.volumeName,
			})
			if err != nil {
				return nil, nil, err
			}
			cryptoAlg := created.CryptoAlg
			if cryptoAlg == "" {
				cryptoAlg = "AES-256-GCM"
			}
			return r.Fek, &FileCrypto{
				WrappedFek:  created.WrappedFek,
				DriveFileID: driveFileID,
				FekVersion:  uint32(created.FekVersion),
				CryptoAlg:   cryptoAlg,
			}, nil
		},
	}
	return cloneRewrap(mctx, eng, srcIno, dstIno, srcPath, dstPath, ops)
}

func (s *MetaProxyServer) GetPaths(ctx context.Context, req *pb.GetPathsRequest) (*pb.GetPathsResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	paths := eng.GetPaths(mctx, Ino(req.Inode))
	return &pb.GetPathsResponse{
		Errno: 0,
		Paths: paths,
	}, nil
}

func (s *MetaProxyServer) Check(ctx context.Context, req *pb.CheckRequest) (*pb.CheckResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	opt := &CheckOpt{
		Repair:        req.Repair,
		Recursive:     req.Recursive,
		SyncDirStat:   req.SyncDirStat,
		RepairDirMode: uint16(req.RepairDirMode),
	}
	err = eng.Check(mctx, req.Fpath, opt)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &pb.CheckResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) CompactAll(ctx context.Context, req *pb.CompactAllRequest) (*pb.CompactAllResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := eng.CompactAll(mctx, int(req.Threads), nil)
	return &pb.CompactAllResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Compact(ctx context.Context, req *pb.CompactRequest) (*pb.CompactResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := eng.Compact(mctx, Ino(req.Inode), int(req.Concurrency), nil, nil)
	return &pb.CompactResponse{Errno: uint32(errno)}, nil
}

// ListSlices is deprecated: the Meta interface replaced it with CleanupSlices
// (upstream gc rework). The RPC stays registered for wire compatibility with
// older clients and always reports ENOSYS.
func (s *MetaProxyServer) ListSlices(ctx context.Context, req *pb.ListSlicesRequest) (*pb.ListSlicesResponse, error) {
	return &pb.ListSlicesResponse{Errno: uint32(syscall.ENOSYS)}, nil
}

func (s *MetaProxyServer) CleanupSlices(ctx context.Context, req *pb.CleanupSlicesRequest) (*pb.CleanupSlicesResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := eng.CleanupSlices(mctx)
	return &pb.CleanupSlicesResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ScanSlices(req *pb.ScanSlicesRequest, stream pb.MetaService_ScanSlicesServer) error {
	ctx := stream.Context()
	eng, err := s.engineFor(ctx)
	if err != nil {
		return err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	const batchSize = 1024
	batch := make([]*pb.ScanSlicesEntry, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := stream.Send(&pb.ScanSlicesResponse{Entries: batch})
		batch = make([]*pb.ScanSlicesEntry, 0, batchSize)
		return err
	}
	errno := eng.ScanSlices(mctx, &ScanSlicesOption{ScanPending: req.ScanPending}, func(ino Ino, s Slice) error {
		batch = append(batch, &pb.ScanSlicesEntry{
			Inode: uint64(ino),
			Slice: &pb.ProtoSlice{Id: s.Id, Size: s.Size, Off: s.Off, Len: s.Len},
		})
		if len(batch) >= batchSize {
			return flush()
		}
		return nil
	})
	if err := flush(); err != nil {
		return err
	}
	return stream.Send(&pb.ScanSlicesResponse{Errno: uint32(errno)})
}

func (s *MetaProxyServer) HandleQuota(ctx context.Context, req *pb.HandleQuotaRequest) (*pb.HandleQuotaResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	quotas := make(map[string]*Quota)
	for k, v := range req.Quotas {
		quotas[k] = ProtoToQuota(v)
	}
	err = eng.HandleQuota(mctx, uint8(req.Cmd), req.Qkey, uint32(req.Qtype), quotas,
		req.Strict, req.Repair, req.Create)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &pb.HandleQuotaResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ScanUserGroupUsage(ctx context.Context, req *pb.ScanUserGroupUsageRequest) (*pb.ScanUserGroupUsageResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	err = eng.ScanUserGroupUsage(mctx)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &pb.ScanUserGroupUsageResponse{Errno: uint32(errno)}, nil
}

// Chroot binds the session to the pooled engine of the requested subdir. The
// engine root is written exactly once, inside pool creation (I2) — no RPC ever
// chroots a published engine. The outcome of the session's first Chroot is
// final (I10): a replay of the same subdir returns the recorded errno without
// touching any engine; a different subdir fails with FailedPrecondition. In
// authz mode a user identity may only chroot to an existing directory: the
// target is pre-checked by a lookup pass on the session's root engine (I7),
// and pool creation re-checks it on the fresh engine right before Chroot.
// Service identities and the non-authz mode keep the create-as-today
// semantics (D9).
func (s *MetaProxyServer) Chroot(ctx context.Context, req *pb.ChrootRequest) (*pb.ChrootResponse, error) {
	sess, err := s.sessionFor(ctx)
	if err != nil {
		return nil, err
	}
	key := normalizeSubdir(req.Subdir)
	user := chrootLogUser(ctx)

	sess.chrootMu.Lock()
	defer sess.chrootMu.Unlock()

	sess.mu.Lock()
	done, prevKey, prevErrno := sess.chroot != "", sess.chroot, sess.chrootErrno
	sess.mu.Unlock()
	if done {
		if prevKey == key {
			// Idempotent replay: the recorded outcome, engines untouched.
			return &pb.ChrootResponse{Errno: prevErrno}, nil
		}
		logger.Warnf("meta-proxy chroot rejected id=%d user=%s subdir=%s reason=session_already_chrooted",
			sess.id, user, displaySubdir(key))
		return nil, status.Error(codes.FailedPrecondition, "meta-proxy: session is already chrooted")
	}

	// Authz mode with a user identity: the target must exist — chrooting must
	// not create directories on the user's behalf (I7). noCreate is forwarded
	// to the pool, whose create re-runs the lookup on the fresh engine.
	noCreate := s.authzEnabled && !oidc.IsServiceIdentity(ctx)
	if noCreate && key != "" {
		errno, lerr := s.checkChrootTarget(ctx, s.metaCtx(ctx, req.Ctx), sess, key)
		if lerr != nil {
			logger.Warnf("meta-proxy chroot rejected id=%d user=%s subdir=%s reason=engine_error",
				sess.id, user, displaySubdir(key))
			return nil, lerr
		}
		if errno != 0 {
			s.finishChroot(sess, key, uint32(errno), nil)
			logger.Warnf("meta-proxy chroot rejected id=%d user=%s subdir=%s reason=%s",
				sess.id, user, displaySubdir(key), chrootRejectReason(uint32(errno)))
			return &pb.ChrootResponse{Errno: uint32(errno)}, nil
		}
	}

	e, errno, err := s.pool.acquire(ctx, key, noCreate)
	if err != nil {
		logger.Warnf("meta-proxy chroot rejected id=%d user=%s subdir=%s reason=engine_error",
			sess.id, user, displaySubdir(key))
		return nil, err
	}
	if errno != 0 {
		// acquire already settled the failed reference away (settle); the
		// outcome is recorded — replays of this subdir return the same errno.
		s.finishChroot(sess, key, errno, nil)
		logger.Warnf("meta-proxy chroot rejected id=%d user=%s subdir=%s reason=%s",
			sess.id, user, displaySubdir(key), chrootRejectReason(errno))
		return &pb.ChrootResponse{Errno: errno}, nil
	}
	s.finishChroot(sess, key, 0, e)
	logger.Infof("meta-proxy chroot applied id=%d user=%s subdir=%s", sess.id, user, displaySubdir(key))
	return &pb.ChrootResponse{Errno: 0}, nil
}

// finishChroot records the final outcome of the session's first Chroot (I10).
// A non-nil entry makes key the session root and binds the session to the
// acquired engine; bind releases the previously held reference, if any (I4).
func (s *MetaProxyServer) finishChroot(sess *proxySession, key string, errno uint32, e *poolEntry) {
	sess.mu.Lock()
	sess.chroot = key
	sess.chrootErrno = errno
	if e != nil {
		sess.root = key
	}
	sess.mu.Unlock()
	if e != nil {
		s.bind(sess, e)
	}
}

// checkChrootTarget walks the components of key on the session's root engine
// (""), creating nothing (I7). The errno carries ENOENT/ENOTDIR; error is any
// other engine failure — the caller answers Unavailable and the session state
// stays untouched.
func (s *MetaProxyServer) checkChrootTarget(ctx context.Context, mctx Context, sess *proxySession, key string) (syscall.Errno, error) {
	e, err := s.sessionEntry(sess, ctx)
	if err != nil {
		return 0, err
	}
	return lookupSubdir(mctx, e.engine, key)
}

// chrootLogUser renders the caller's identity for chroot audit logs: "service"
// for a service identity, the OIDC sub for a user, "-" without any identity
// (D10).
func chrootLogUser(ctx context.Context) string {
	if oidc.IsServiceIdentity(ctx) {
		return "service"
	}
	if sub, err := extractUserIDFromOIDC(ctx); err == nil && sub != "" {
		return sub
	}
	return "-"
}

func (s *MetaProxyServer) CleanupTrashBefore(ctx context.Context, req *pb.CleanupTrashBeforeRequest) (*pb.CleanupTrashBeforeResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	var stats CleanupTrashStats
	errno := eng.CleanupTrashBefore(mctx, time.Unix(req.Edge, 0), func(int) {}, &stats)
	return &pb.CleanupTrashBeforeResponse{
		Errno:        uint32(errno),
		DeletedFiles: stats.DeletedFiles,
	}, nil
}

func (s *MetaProxyServer) CleanupDetachedNodesBefore(ctx context.Context, req *pb.CleanupDetachedNodesBeforeRequest) (*pb.CleanupDetachedNodesBeforeResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	eng.CleanupDetachedNodesBefore(mctx, time.Unix(req.Edge, 0), func() {})
	return &pb.CleanupDetachedNodesBeforeResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) ScanDeletedObject(req *pb.ScanDeletedObjectRequest, stream pb.MetaService_ScanDeletedObjectServer) error {
	ctx := stream.Context()
	eng, err := s.engineFor(ctx)
	if err != nil {
		return err
	}
	mctx := s.metaCtx(ctx, req.Ctx)

	sendErrno := func(errno syscall.Errno) error {
		return stream.Send(&pb.ScanDeletedObjectResponse{Errno: uint32(errno)})
	}

	if req.ScanTrashSlices {
		err := eng.ScanDeletedObject(mctx,
			func(ss []Slice, ts int64) (bool, error) {
				protoSlices := make([]*pb.ProtoSlice, 0, len(ss))
				for _, sl := range ss {
					protoSlices = append(protoSlices, &pb.ProtoSlice{
						Id:   sl.Id,
						Size: sl.Size,
						Off:  sl.Off,
						Len:  sl.Len,
					})
				}
				return true, stream.Send(&pb.ScanDeletedObjectResponse{
					Type: 1,
					Data: &pb.ScanDeletedObjectResponse_TrashSlices{
						TrashSlices: &pb.TrashSlicesData{
							Slices:    protoSlices,
							Timestamp: ts,
						},
					},
				})
			},
			nil, nil, nil)
		if err != nil {
			if errno, ok := err.(syscall.Errno); ok {
				return sendErrno(errno)
			}
			return err
		}
	}

	if req.ScanPendingSlices {
		err := eng.ScanDeletedObject(mctx,
			nil,
			func(id uint64, size uint32) (bool, error) {
				return true, stream.Send(&pb.ScanDeletedObjectResponse{
					Type: 2,
					Data: &pb.ScanDeletedObjectResponse_PendingSlice{
						PendingSlice: &pb.PendingSliceData{
							Id:   id,
							Size: size,
						},
					},
				})
			},
			nil, nil)
		if err != nil {
			if errno, ok := err.(syscall.Errno); ok {
				return sendErrno(errno)
			}
			return err
		}
	}

	if req.ScanTrashFiles {
		err := eng.ScanDeletedObject(mctx,
			nil, nil,
			func(inode Ino, size uint64, ts time.Time, count int64) (bool, error) {
				return true, stream.Send(&pb.ScanDeletedObjectResponse{
					Type: 3,
					Data: &pb.ScanDeletedObjectResponse_TrashFile{
						TrashFile: &pb.TrashFileData{
							Inode:     uint64(inode),
							Size:      size,
							Timestamp: ts.Unix(),
							Count:     count,
						},
					},
				})
			},
			nil)
		if err != nil {
			if errno, ok := err.(syscall.Errno); ok {
				return sendErrno(errno)
			}
			return err
		}
	}

	if req.ScanPendingFiles {
		err := eng.ScanDeletedObject(mctx,
			nil, nil, nil,
			func(ino Ino, size uint64, ts int64) (bool, error) {
				return true, stream.Send(&pb.ScanDeletedObjectResponse{
					Type: 4,
					Data: &pb.ScanDeletedObjectResponse_PendingFile{
						PendingFile: &pb.PendingFileData{
							Inode:     uint64(ino),
							Size:      size,
							Timestamp: ts,
						},
					},
				})
			})
		if err != nil {
			if errno, ok := err.(syscall.Errno); ok {
				return sendErrno(errno)
			}
			return err
		}
	}

	return nil
}

func (s *MetaProxyServer) ScanChangelog(req *pb.ScanChangelogRequest, stream pb.MetaService_ScanChangelogServer) error {
	ctx := stream.Context()
	eng, err := s.engineFor(ctx)
	if err != nil {
		return err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	return eng.ScanChangelog(mctx, req.Last, func(ver int64, entry string) error {
		return stream.Send(&pb.ScanChangelogResponse{
			Entries: []*pb.ChangelogEntry{{Ver: ver, Data: entry}},
		})
	})
}
