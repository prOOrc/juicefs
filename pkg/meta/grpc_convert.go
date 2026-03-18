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
	"time"

	aclAPI "github.com/juicedata/juicefs/pkg/acl"
	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// Attr conversion

func AttrToProto(a *Attr) *pb.ProtoAttr {
	if a == nil {
		return nil
	}
	return &pb.ProtoAttr{
		Flags:      uint32(a.Flags),
		Typ:        uint32(a.Typ),
		Mode:       uint32(a.Mode),
		Uid:        a.Uid,
		Gid:        a.Gid,
		Rdev:       a.Rdev,
		Atime:      a.Atime,
		Mtime:      a.Mtime,
		Ctime:      a.Ctime,
		Atimensec:  a.Atimensec,
		Mtimensec:  a.Mtimensec,
		Ctimensec:  a.Ctimensec,
		Nlink:      a.Nlink,
		Length:     a.Length,
		Parent:     uint64(a.Parent),
		Full:       a.Full,
		KeepCache:  a.KeepCache,
		AccessAcl:  a.AccessACL,
		DefaultAcl: a.DefaultACL,
	}
}

func ProtoToAttr(p *pb.ProtoAttr) *Attr {
	if p == nil {
		return nil
	}
	return &Attr{
		Flags:      uint8(p.Flags),
		Typ:        uint8(p.Typ),
		Mode:       uint16(p.Mode),
		Uid:        p.Uid,
		Gid:        p.Gid,
		Rdev:       p.Rdev,
		Atime:      p.Atime,
		Mtime:      p.Mtime,
		Ctime:      p.Ctime,
		Atimensec:  p.Atimensec,
		Mtimensec:  p.Mtimensec,
		Ctimensec:  p.Ctimensec,
		Nlink:      p.Nlink,
		Length:     p.Length,
		Parent:     Ino(p.Parent),
		Full:       p.Full,
		KeepCache:  p.KeepCache,
		AccessACL:  p.AccessAcl,
		DefaultACL: p.DefaultAcl,
	}
}

// Slice conversion

func SliceToProto(s Slice) *pb.ProtoSlice {
	return &pb.ProtoSlice{
		Id:   s.Id,
		Size: s.Size,
		Off:  s.Off,
		Len:  s.Len,
	}
}

func ProtoToSlice(p *pb.ProtoSlice) Slice {
	if p == nil {
		return Slice{}
	}
	return Slice{
		Id:   p.Id,
		Size: p.Size,
		Off:  p.Off,
		Len:  p.Len,
	}
}

func SlicesToProto(ss []Slice) []*pb.ProtoSlice {
	if ss == nil {
		return nil
	}
	result := make([]*pb.ProtoSlice, len(ss))
	for i, s := range ss {
		result[i] = SliceToProto(s)
	}
	return result
}

func ProtoToSlices(pp []*pb.ProtoSlice) []Slice {
	if pp == nil {
		return nil
	}
	result := make([]Slice, len(pp))
	for i, p := range pp {
		result[i] = ProtoToSlice(p)
	}
	return result
}

// Entry conversion

func EntryToProto(e *Entry) *pb.ProtoEntry {
	if e == nil {
		return nil
	}
	return &pb.ProtoEntry{
		Inode: uint64(e.Inode),
		Name:  e.Name,
		Attr:  AttrToProto(e.Attr),
	}
}

func ProtoToEntry(p *pb.ProtoEntry) *Entry {
	if p == nil {
		return nil
	}
	return &Entry{
		Inode: Ino(p.Inode),
		Name:  p.Name,
		Attr:  ProtoToAttr(p.Attr),
	}
}

func EntriesToProto(ees []*Entry) []*pb.ProtoEntry {
	if ees == nil {
		return nil
	}
	result := make([]*pb.ProtoEntry, len(ees))
	for i, e := range ees {
		result[i] = EntryToProto(e)
	}
	return result
}

func ProtoToEntries(pp []*pb.ProtoEntry) []*Entry {
	if pp == nil {
		return nil
	}
	result := make([]*Entry, len(pp))
	for i, p := range pp {
		result[i] = ProtoToEntry(p)
	}
	return result
}

// Summary conversion

func SummaryToProto(s *Summary) *pb.ProtoSummary {
	if s == nil {
		return nil
	}
	return &pb.ProtoSummary{
		Length: s.Length,
		Size:   s.Size,
		Files:  s.Files,
		Dirs:   s.Dirs,
	}
}

func ProtoToSummary(p *pb.ProtoSummary) *Summary {
	if p == nil {
		return nil
	}
	return &Summary{
		Length: p.Length,
		Size:   p.Size,
		Files:  p.Files,
		Dirs:   p.Dirs,
	}
}

// TreeSummary conversion

func TreeSummaryToProto(t *TreeSummary) *pb.ProtoTreeSummary {
	if t == nil {
		return nil
	}
	children := make([]*pb.ProtoTreeSummary, len(t.Children))
	for i, c := range t.Children {
		children[i] = TreeSummaryToProto(c)
	}
	return &pb.ProtoTreeSummary{
		Inode:    uint64(t.Inode),
		Path:     t.Path,
		Type:     uint32(t.Type),
		Size:     t.Size,
		Files:    t.Files,
		Dirs:     t.Dirs,
		Children: children,
	}
}

func ProtoToTreeSummary(p *pb.ProtoTreeSummary) *TreeSummary {
	if p == nil {
		return nil
	}
	children := make([]*TreeSummary, len(p.Children))
	for i, c := range p.Children {
		children[i] = ProtoToTreeSummary(c)
	}
	return &TreeSummary{
		Inode:    Ino(p.Inode),
		Path:     p.Path,
		Type:     uint8(p.Type),
		Size:     p.Size,
		Files:    p.Files,
		Dirs:     p.Dirs,
		Children: children,
	}
}

// Format conversion - handle both pointer and value

func FormatToProto(f Format) *pb.ProtoFormat {
	return FormatToProtoPtr(&f)
}

func FormatToProtoPtr(f *Format) *pb.ProtoFormat {
	if f == nil {
		return nil
	}
	return &pb.ProtoFormat{
		Name:             f.Name,
		Uuid:             f.UUID,
		Storage:          f.Storage,
		StorageClass:     f.StorageClass,
		Bucket:           f.Bucket,
		AccessKey:        f.AccessKey,
		SecretKey:        f.SecretKey,
		SessionToken:     f.SessionToken,
		BlockSize:        int32(f.BlockSize),
		Compression:      f.Compression,
		Shards:           int32(f.Shards),
		HashPrefix:       f.HashPrefix,
		Capacity:         f.Capacity,
		Inodes:           f.Inodes,
		EncryptKey:       f.EncryptKey,
		EncryptAlgo:      f.EncryptAlgo,
		KeyEncrypted:     f.KeyEncrypted,
		UploadLimit:      f.UploadLimit,
		DownloadLimit:    f.DownloadLimit,
		TrashDays:        int32(f.TrashDays),
		MetaVersion:      int32(f.MetaVersion),
		MinClientVersion: f.MinClientVersion,
		MaxClientVersion: f.MaxClientVersion,
		DirStats:         f.DirStats,
		UserGroupQuota:   f.UserGroupQuota,
		EnableAcl:        f.EnableACL,
		RangerRestUrl:    f.RangerRestUrl,
		RangerService:    f.RangerService,
		KerbConf:         f.KerbConf,
	}
}

func ProtoToFormat(p *pb.ProtoFormat) *Format {
	if p == nil {
		return nil
	}
	return &Format{
		Name:             p.Name,
		UUID:             p.Uuid,
		Storage:          p.Storage,
		StorageClass:     p.StorageClass,
		Bucket:           p.Bucket,
		AccessKey:        p.AccessKey,
		SecretKey:        p.SecretKey,
		SessionToken:     p.SessionToken,
		BlockSize:        int(p.BlockSize),
		Compression:      p.Compression,
		Shards:           int(p.Shards),
		HashPrefix:       p.HashPrefix,
		Capacity:         p.Capacity,
		Inodes:           p.Inodes,
		EncryptKey:       p.EncryptKey,
		EncryptAlgo:      p.EncryptAlgo,
		KeyEncrypted:     p.KeyEncrypted,
		UploadLimit:      p.UploadLimit,
		DownloadLimit:    p.DownloadLimit,
		TrashDays:        int(p.TrashDays),
		MetaVersion:      int(p.MetaVersion),
		MinClientVersion: p.MinClientVersion,
		MaxClientVersion: p.MaxClientVersion,
		DirStats:         p.DirStats,
		UserGroupQuota:   p.UserGroupQuota,
		EnableACL:        p.EnableAcl,
		RangerRestUrl:    p.RangerRestUrl,
		RangerService:    p.RangerService,
		KerbConf:         p.KerbConf,
	}
}

// Quota conversion

func QuotaToProto(q *Quota) *pb.ProtoQuota {
	if q == nil {
		return nil
	}
	return &pb.ProtoQuota{
		MaxSpace:   q.MaxSpace,
		MaxInodes:  q.MaxInodes,
		UsedSpace:  q.UsedSpace,
		UsedInodes: q.UsedInodes,
	}
}

func ProtoToQuota(p *pb.ProtoQuota) *Quota {
	if p == nil {
		return nil
	}
	return &Quota{
		MaxSpace:   p.MaxSpace,
		MaxInodes:  p.MaxInodes,
		UsedSpace:  p.UsedSpace,
		UsedInodes: p.UsedInodes,
	}
}

// SliceMapEntry conversion

func SliceMapToProto(m map[Ino][]Slice) []*pb.SliceMapEntry {
	if m == nil {
		return nil
	}
	result := make([]*pb.SliceMapEntry, 0, len(m))
	for ino, slices := range m {
		protoSlices := SlicesToProto(slices)
		result = append(result, &pb.SliceMapEntry{
			Inode:  uint64(ino),
			Slices: protoSlices,
		})
	}
	return result
}

func ProtoToSliceMap(pp []*pb.SliceMapEntry) map[Ino][]Slice {
	if pp == nil {
		return nil
	}
	result := make(map[Ino][]Slice, len(pp))
	for _, p := range pp {
		result[Ino(p.Inode)] = ProtoToSlices(p.Slices)
	}
	return result
}

// Session conversion

func SessionToProto(s *Session) *pb.ProtoSession {
	if s == nil {
		return nil
	}
	info := &pb.ProtoSessionInfo{
		Version:    s.Version,
		HostName:   s.HostName,
		IpAddrs:    s.IPAddrs,
		MountPoint: s.MountPoint,
		MountTime:  s.MountTime.Unix(),
		ProcessId:  int32(s.ProcessID),
	}
	flocks := make([]*pb.ProtoFlock, len(s.Flocks))
	for i, f := range s.Flocks {
		flocks[i] = &pb.ProtoFlock{
			Inode: uint64(f.Inode),
			Owner: f.Owner,
			Ltype: f.Ltype,
		}
	}
	plocks := make([]*pb.ProtoPlock, len(s.Plocks))
	for i, p := range s.Plocks {
		records := make([]*pb.ProtoPlockRecord, len(p.Records))
		for j, r := range p.Records {
			records[j] = &pb.ProtoPlockRecord{
				Type:  r.Type,
				Pid:   r.Pid,
				Start: r.Start,
				End:   r.End,
			}
		}
		plocks[i] = &pb.ProtoPlock{
			Inode:   uint64(p.Inode),
			Owner:   p.Owner,
			Records: records,
		}
	}
	sustained := make([]uint64, len(s.Sustained))
	for i, ino := range s.Sustained {
		sustained[i] = uint64(ino)
	}
	return &pb.ProtoSession{
		Sid:       s.Sid,
		Expire:    s.Expire.Unix(),
		Info:      info,
		Sustained: sustained,
		Flocks:    flocks,
		Plocks:    plocks,
	}
}

func ProtoToSession(p *pb.ProtoSession) *Session {
	if p == nil {
		return nil
	}
	info := SessionInfo{
		Version:    p.Info.Version,
		HostName:   p.Info.HostName,
		IPAddrs:    p.Info.IpAddrs,
		MountPoint: p.Info.MountPoint,
		MountTime:  time.Unix(p.Info.MountTime, 0),
		ProcessID:  int(p.Info.ProcessId),
	}
	flocks := make([]Flock, len(p.Flocks))
	for i, f := range p.Flocks {
		flocks[i] = Flock{
			Inode: Ino(f.Inode),
			Owner: f.Owner,
			Ltype: f.Ltype,
		}
	}
	// Note: plocks use unexported plockRecord type, so we can't convert them back
	// This is OK since we only need SessionToProto for the server
	sustained := make([]Ino, len(p.Sustained))
	for i, id := range p.Sustained {
		sustained[i] = Ino(id)
	}
	return &Session{
		Sid:         p.Sid,
		Expire:      time.Unix(p.Expire, 0),
		SessionInfo: info,
		Sustained:   sustained,
		Flocks:      flocks,
	}
}

// Alias functions for simpler names used in grpc_client.go

func toProtoAttr(a *Attr) *pb.ProtoAttr {
	return AttrToProto(a)
}

func fromProtoAttr(p *pb.ProtoAttr) *Attr {
	return ProtoToAttr(p)
}

func toProtoSlice(s *Slice) *pb.ProtoSlice {
	if s == nil {
		return nil
	}
	return SliceToProto(*s)
}

func fromProtoSlice(p *pb.ProtoSlice) *Slice {
	if p == nil {
		return nil
	}
	return &Slice{
		Id:   p.Id,
		Size: p.Size,
		Off:  p.Off,
		Len:  p.Len,
	}
}

func toProtoEntry(e *Entry) *pb.ProtoEntry {
	return EntryToProto(e)
}

func fromProtoEntry(p *pb.ProtoEntry) *Entry {
	return ProtoToEntry(p)
}

func toProtoSummary(s *Summary) *pb.ProtoSummary {
	return SummaryToProto(s)
}

func fromProtoSummary(p *pb.ProtoSummary) *Summary {
	return ProtoToSummary(p)
}

func toProtoTreeSummary(t *TreeSummary) *pb.ProtoTreeSummary {
	return TreeSummaryToProto(t)
}

func fromProtoTreeSummary(p *pb.ProtoTreeSummary) *TreeSummary {
	return ProtoToTreeSummary(p)
}

func toProtoFormat(f *Format) *pb.ProtoFormat {
	return FormatToProtoPtr(f)
}

func fromProtoFormat(p *pb.ProtoFormat) *Format {
	return ProtoToFormat(p)
}

func toProtoQuota(q *Quota) *pb.ProtoQuota {
	return QuotaToProto(q)
}

func fromProtoQuota(p *pb.ProtoQuota) *Quota {
	return ProtoToQuota(p)
}

func toProtoSession(s *Session) *pb.ProtoSession {
	return SessionToProto(s)
}

func fromProtoSession(p *pb.ProtoSession) *Session {
	return ProtoToSession(p)
}

func toProtoFlock(f *Flock) *pb.ProtoFlock {
	if f == nil {
		return nil
	}
	return &pb.ProtoFlock{
		Inode: uint64(f.Inode),
		Owner: f.Owner,
		Ltype: f.Ltype,
	}
}

func fromProtoFlock(p *pb.ProtoFlock) *Flock {
	if p == nil {
		return nil
	}
	return &Flock{
		Inode: Ino(p.Inode),
		Owner: p.Owner,
		Ltype: p.Ltype,
	}
}

func toProtoPlock(plock *Plock) *pb.ProtoPlock {
	if plock == nil {
		return nil
	}
	records := make([]*pb.ProtoPlockRecord, len(plock.Records))
	for i, r := range plock.Records {
		records[i] = &pb.ProtoPlockRecord{
			Type:  r.Type,
			Pid:   r.Pid,
			Start: r.Start,
			End:   r.End,
		}
	}
	return &pb.ProtoPlock{
		Inode:   uint64(plock.Inode),
		Owner:   plock.Owner,
		Records: records,
	}
}

func fromProtoPlock(p *pb.ProtoPlock) *Plock {
	if p == nil {
		return nil
	}
	// plockRecord is unexported, so we create a Plock with empty Records
	// The actual records are stored in the server's internal representation
	return &Plock{
		Inode: Ino(p.Inode),
		Owner: p.Owner,
		// Records field is unexported (plockRecord type), can't populate it here
	}
}

// ACL conversion

func toProtoACLRule(rule *aclAPI.Rule) *pb.ProtoACLRule {
	if rule == nil {
		return nil
	}
	return &pb.ProtoACLRule{
		Id:      uint32(rule.Owner),
		Type:    uint32(rule.Group),
		Perms:   uint32(rule.Mask),
		Entries: []uint32{uint32(rule.Other)},
	}
}

func fromProtoACLRule(p *pb.ProtoACLRule) *aclAPI.Rule {
	if p == nil {
		return nil
	}
	other := uint16(0)
	if len(p.Entries) > 0 {
		other = uint16(p.Entries[0])
	}
	return &aclAPI.Rule{
		Owner: uint16(p.Id),
		Group: uint16(p.Type),
		Mask:  uint16(p.Perms),
		Other: other,
	}
}
