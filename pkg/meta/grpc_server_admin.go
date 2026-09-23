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
)

func (s *MetaProxyServer) GetFormat(ctx context.Context, req *pb.GetFormatRequest) (*pb.GetFormatResponse, error) {
	format := s.meta.GetFormat()
	return &pb.GetFormatResponse{
		Errno:  0,
		Format: FormatToProto(format),
	}, nil
}

func (s *MetaProxyServer) Remove(ctx context.Context, req *pb.RemoveRequest) (*pb.RemoveResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var count uint64
	errno := s.meta.Remove(mctx, Ino(req.Parent), req.Name, req.SkipTrash, int(req.NumThreads), &count)
	return &pb.RemoveResponse{
		Errno: uint32(errno),
		Count: count,
	}, nil
}

func (s *MetaProxyServer) BatchUnlink(ctx context.Context, req *pb.BatchUnlinkRequest) (*pb.BatchUnlinkResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	entries := ProtoToEntries(req.Entries)
	var count uint64
	errno := s.meta.BatchUnlink(mctx, Ino(req.Parent), entries, &count, req.SkipCheckTrash)
	return &pb.BatchUnlinkResponse{
		Errno: uint32(errno),
		Count: count,
	}, nil
}

func (s *MetaProxyServer) GetSummary(ctx context.Context, req *pb.GetSummaryRequest) (*pb.GetSummaryResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var summary Summary
	errno := s.meta.GetSummary(mctx, Ino(req.Inode), &summary, req.Recursive, req.Strict)
	return &pb.GetSummaryResponse{
		Errno:   uint32(errno),
		Summary: SummaryToProto(&summary),
	}, nil
}

func (s *MetaProxyServer) GetTreeSummary(ctx context.Context, req *pb.GetTreeSummaryRequest) (*pb.GetTreeSummaryResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var tree TreeSummary
	errno := s.meta.GetTreeSummary(mctx, &tree, uint8(req.Depth), uint8(req.TopN), req.Strict, func(uint64, uint64) {})
	return &pb.GetTreeSummaryResponse{
		Errno: uint32(errno),
		Tree:  TreeSummaryToProto(&tree),
	}, nil
}

func (s *MetaProxyServer) Clone(ctx context.Context, req *pb.CloneRequest) (*pb.CloneResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var count, total uint64
	var dstIno Ino
	errno := s.meta.Clone(mctx, Ino(req.SrcParentIno), Ino(req.SrcIno),
		Ino(req.DstParentIno), req.DstName, uint8(req.Cmode), uint16(req.Cumask),
		uint8(req.Concurrency), &count, &total, &dstIno)
	if errno != 0 {
		return &pb.CloneResponse{Errno: uint32(errno), Count: count, Total: total}, nil
	}
	resp := &pb.CloneResponse{Errno: 0, Count: count, Total: total, DstIno: uint64(dstIno)}

	// FR-OP-1..3 (D7): the target must get its own FEK; shared slice CEKs are
	// re-wrapped under it (zero-copy). Any failure rolls the whole clone back —
	// a target with foreign key material is unusable.
	if st := s.cloneEncryption(ctx, mctx, Ino(req.SrcIno), dstIno, Ino(req.DstParentIno), req.DstName); st != 0 {
		var removed uint64
		_ = s.meta.Remove(mctx, Ino(req.DstParentIno), req.DstName, true, 1, &removed)
		return &pb.CloneResponse{Errno: uint32(st)}, nil
	}

	// Register the target in the path cache (as Create/Mkdir do): cloned entries
	// bypass those handlers, and without this Open/ResolveFileKey on the target
	// would fail closed with EACCES (unresolved path).
	var dstAttr Attr
	if st := s.meta.GetAttr(mctx, dstIno, &dstAttr); st == 0 {
		dstPath := s.inodePathCache.BuildChildPath(Ino(req.DstParentIno), req.DstName)
		s.inodePathCache.Set(dstIno, withDirSlash(dstPath, &dstAttr))
	}
	return resp, nil
}

// cloneEncryption orchestrates the per-file FEK sequence for a clone target
// (design 5.2): GetFileFEK(src) → CreateFileKey(dst) → SetFileCrypto(dst) →
// RewrapSlices. Legacy sources need no re-wrap and return 0 immediately.
func (s *MetaProxyServer) cloneEncryption(ctx context.Context, mctx Context, srcIno, dstIno, dstParent Ino, dstName string) syscall.Errno {
	var srcAttr Attr
	if st := s.meta.GetAttr(mctx, srcIno, &srcAttr); st != 0 {
		return st
	}
	if !srcAttr.Encrypted || len(srcAttr.WrappedFek) == 0 {
		return 0 // legacy clone — no re-wrap
	}
	srcPath := s.inodePathCache.Get(srcIno)
	dstPath := s.inodePathCache.BuildChildPath(dstParent, dstName)
	if srcPath == "" || dstPath == "" {
		return syscall.EACCES // fail-closed: cannot prove file identity to KeyManager
	}
	userID, err := extractUserIDFromOIDC(ctx)
	if err != nil {
		return syscall.EACCES // fail-closed
	}
	volUUID := s.meta.GetFormat().UUID
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
	return cloneRewrap(mctx, s.meta, srcIno, dstIno, srcPath, dstPath, ops)
}

func (s *MetaProxyServer) GetPaths(ctx context.Context, req *pb.GetPathsRequest) (*pb.GetPathsResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	paths := s.meta.GetPaths(mctx, Ino(req.Inode))
	return &pb.GetPathsResponse{
		Errno: 0,
		Paths: paths,
	}, nil
}

func (s *MetaProxyServer) Check(ctx context.Context, req *pb.CheckRequest) (*pb.CheckResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	opt := &CheckOpt{
		Repair:        req.Repair,
		Recursive:     req.Recursive,
		SyncDirStat:   req.SyncDirStat,
		RepairDirMode: uint16(req.RepairDirMode),
	}
	err := s.meta.Check(mctx, req.Fpath, opt)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &pb.CheckResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) CompactAll(ctx context.Context, req *pb.CompactAllRequest) (*pb.CompactAllResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.CompactAll(mctx, int(req.Threads), nil)
	return &pb.CompactAllResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Compact(ctx context.Context, req *pb.CompactRequest) (*pb.CompactResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Compact(mctx, Ino(req.Inode), int(req.Concurrency), nil, nil)
	return &pb.CompactResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ListSlices(ctx context.Context, req *pb.ListSlicesRequest) (*pb.ListSlicesResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	slices := ProtoToSliceMap(req.Slices)
	errno := s.meta.ListSlices(mctx, slices, req.ScanPending, req.Delete, func() {})
	return &pb.ListSlicesResponse{
		Errno:  uint32(errno),
		Slices: SliceMapToProto(slices),
	}, nil
}

func (s *MetaProxyServer) HandleQuota(ctx context.Context, req *pb.HandleQuotaRequest) (*pb.HandleQuotaResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	quotas := make(map[string]*Quota)
	for k, v := range req.Quotas {
		quotas[k] = ProtoToQuota(v)
	}
	err := s.meta.HandleQuota(mctx, uint8(req.Cmd), req.Qkey, uint32(req.Qtype), quotas,
		req.Strict, req.Repair, req.Create)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &pb.HandleQuotaResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) ScanUserGroupUsage(ctx context.Context, req *pb.ScanUserGroupUsageRequest) (*pb.ScanUserGroupUsageResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	err := s.meta.ScanUserGroupUsage(mctx)
	var errno syscall.Errno
	if err != nil {
		errno = syscall.EIO
	}
	return &pb.ScanUserGroupUsageResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) Chroot(ctx context.Context, req *pb.ChrootRequest) (*pb.ChrootResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	errno := s.meta.Chroot(mctx, req.Subdir)
	return &pb.ChrootResponse{Errno: uint32(errno)}, nil
}

func (s *MetaProxyServer) CleanupTrashBefore(ctx context.Context, req *pb.CleanupTrashBeforeRequest) (*pb.CleanupTrashBeforeResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	var stats CleanupTrashStats
	errno := s.meta.CleanupTrashBefore(mctx, time.Unix(req.Edge, 0), func(int) {}, &stats)
	return &pb.CleanupTrashBeforeResponse{
		Errno:        uint32(errno),
		DeletedFiles: stats.DeletedFiles,
	}, nil
}

func (s *MetaProxyServer) CleanupDetachedNodesBefore(ctx context.Context, req *pb.CleanupDetachedNodesBeforeRequest) (*pb.CleanupDetachedNodesBeforeResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	s.meta.CleanupDetachedNodesBefore(mctx, time.Unix(req.Edge, 0), func() {})
	return &pb.CleanupDetachedNodesBeforeResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) ScanDeletedObject(req *pb.ScanDeletedObjectRequest, stream pb.MetaService_ScanDeletedObjectServer) error {
	ctx := stream.Context()
	mctx := s.metaCtx(ctx, req.Ctx)

	sendErrno := func(errno syscall.Errno) error {
		return stream.Send(&pb.ScanDeletedObjectResponse{Errno: uint32(errno)})
	}

	if req.ScanTrashSlices {
		err := s.meta.ScanDeletedObject(mctx,
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
		err := s.meta.ScanDeletedObject(mctx,
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
		err := s.meta.ScanDeletedObject(mctx,
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
		err := s.meta.ScanDeletedObject(mctx,
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
	mctx := s.metaCtx(ctx, req.Ctx)
	return s.meta.ScanChangelog(mctx, req.Last, func(ver int64, entry string) error {
		return stream.Send(&pb.ScanChangelogResponse{
			Entries: []*pb.ChangelogEntry{{Ver: ver, Data: entry}},
		})
	})
}
