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

package grpc

import (
	"time"

	aclAPI "github.com/juicedata/juicefs/pkg/acl"
	"github.com/juicedata/juicefs/pkg/meta"
)

// Attr conversion

func AttrToProto(a *meta.Attr) *ProtoAttr {
	if a == nil {
		return nil
	}
	return &ProtoAttr{
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

func ProtoToAttr(p *ProtoAttr) *meta.Attr {
	if p == nil {
		return nil
	}
	return &meta.Attr{
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
		Parent:     meta.Ino(p.Parent),
		Full:       p.Full,
		KeepCache:  p.KeepCache,
		AccessACL:  p.AccessAcl,
		DefaultACL: p.DefaultAcl,
	}
}

// Slice conversion

func SliceToProto(s meta.Slice) *ProtoSlice {
	return &ProtoSlice{
		Id:   s.Id,
		Size: s.Size,
		Off:  s.Off,
		Len:  s.Len,
	}
}

func ProtoToSlice(p *ProtoSlice) meta.Slice {
	if p == nil {
		return meta.Slice{}
	}
	return meta.Slice{
		Id:   p.Id,
		Size: p.Size,
		Off:  p.Off,
		Len:  p.Len,
	}
}

func SlicesToProto(ss []meta.Slice) []*ProtoSlice {
	if ss == nil {
		return nil
	}
	result := make([]*ProtoSlice, len(ss))
	for i, s := range ss {
		result[i] = SliceToProto(s)
	}
	return result
}

func ProtoToSlices(pp []*ProtoSlice) []meta.Slice {
	if pp == nil {
		return nil
	}
	result := make([]meta.Slice, len(pp))
	for i, p := range pp {
		result[i] = ProtoToSlice(p)
	}
	return result
}

// Entry conversion

func EntryToProto(e *meta.Entry) *ProtoEntry {
	if e == nil {
		return nil
	}
	return &ProtoEntry{
		Inode: uint64(e.Inode),
		Name:  e.Name,
		Attr:  AttrToProto(e.Attr),
	}
}

func ProtoToEntry(p *ProtoEntry) *meta.Entry {
	if p == nil {
		return nil
	}
	return &meta.Entry{
		Inode: meta.Ino(p.Inode),
		Name:  p.Name,
		Attr:  ProtoToAttr(p.Attr),
	}
}

func EntriesToProto(ees []*meta.Entry) []*ProtoEntry {
	if ees == nil {
		return nil
	}
	result := make([]*ProtoEntry, len(ees))
	for i, e := range ees {
		result[i] = EntryToProto(e)
	}
	return result
}

func ProtoToEntries(pp []*ProtoEntry) []*meta.Entry {
	if pp == nil {
		return nil
	}
	result := make([]*meta.Entry, len(pp))
	for i, p := range pp {
		result[i] = ProtoToEntry(p)
	}
	return result
}

// Summary conversion

func SummaryToProto(s *meta.Summary) *ProtoSummary {
	if s == nil {
		return nil
	}
	return &ProtoSummary{
		Length: s.Length,
		Size:   s.Size,
		Files:  s.Files,
		Dirs:   s.Dirs,
	}
}

func ProtoToSummary(p *ProtoSummary) *meta.Summary {
	if p == nil {
		return nil
	}
	return &meta.Summary{
		Length: p.Length,
		Size:   p.Size,
		Files:  p.Files,
		Dirs:   p.Dirs,
	}
}

// TreeSummary conversion

func TreeSummaryToProto(t *meta.TreeSummary) *ProtoTreeSummary {
	if t == nil {
		return nil
	}
	children := make([]*ProtoTreeSummary, len(t.Children))
	for i, c := range t.Children {
		children[i] = TreeSummaryToProto(c)
	}
	return &ProtoTreeSummary{
		Inode:    uint64(t.Inode),
		Path:     t.Path,
		Type:     uint32(t.Type),
		Size:     t.Size,
		Files:    t.Files,
		Dirs:     t.Dirs,
		Children: children,
	}
}

func ProtoToTreeSummary(p *ProtoTreeSummary) *meta.TreeSummary {
	if p == nil {
		return nil
	}
	children := make([]*meta.TreeSummary, len(p.Children))
	for i, c := range p.Children {
		children[i] = ProtoToTreeSummary(c)
	}
	return &meta.TreeSummary{
		Inode:    meta.Ino(p.Inode),
		Path:     p.Path,
		Type:     uint8(p.Type),
		Size:     p.Size,
		Files:    p.Files,
		Dirs:     p.Dirs,
		Children: children,
	}
}

// Format conversion - handle both pointer and value

func FormatToProto(f meta.Format) *ProtoFormat {
	return FormatToProtoPtr(&f)
}

func FormatToProtoPtr(f *meta.Format) *ProtoFormat {
	if f == nil {
		return nil
	}
	return &ProtoFormat{
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

func ProtoToFormat(p *ProtoFormat) *meta.Format {
	if p == nil {
		return nil
	}
	return &meta.Format{
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

func QuotaToProto(q *meta.Quota) *ProtoQuota {
	if q == nil {
		return nil
	}
	return &ProtoQuota{
		MaxSpace:   q.MaxSpace,
		MaxInodes:  q.MaxInodes,
		UsedSpace:  q.UsedSpace,
		UsedInodes: q.UsedInodes,
	}
}

func ProtoToQuota(p *ProtoQuota) *meta.Quota {
	if p == nil {
		return nil
	}
	return &meta.Quota{
		MaxSpace:   p.MaxSpace,
		MaxInodes:  p.MaxInodes,
		UsedSpace:  p.UsedSpace,
		UsedInodes: p.UsedInodes,
	}
}

// SliceMapEntry conversion

func SliceMapToProto(m map[meta.Ino][]meta.Slice) []*SliceMapEntry {
	if m == nil {
		return nil
	}
	result := make([]*SliceMapEntry, 0, len(m))
	for ino, slices := range m {
		protoSlices := SlicesToProto(slices)
		result = append(result, &SliceMapEntry{
			Inode:  uint64(ino),
			Slices: protoSlices,
		})
	}
	return result
}

func ProtoToSliceMap(pp []*SliceMapEntry) map[meta.Ino][]meta.Slice {
	if pp == nil {
		return nil
	}
	result := make(map[meta.Ino][]meta.Slice, len(pp))
	for _, p := range pp {
		result[meta.Ino(p.Inode)] = ProtoToSlices(p.Slices)
	}
	return result
}

// Session conversion

func SessionToProto(s *meta.Session) *ProtoSession {
	if s == nil {
		return nil
	}
	info := &ProtoSessionInfo{
		Version:    s.Version,
		HostName:   s.HostName,
		IpAddrs:    s.IPAddrs,
		MountPoint: s.MountPoint,
		MountTime:  s.MountTime.Unix(),
		ProcessId:  int32(s.ProcessID),
	}
	flocks := make([]*ProtoFlock, len(s.Flocks))
	for i, f := range s.Flocks {
		flocks[i] = &ProtoFlock{
			Inode: uint64(f.Inode),
			Owner: f.Owner,
			Ltype: f.Ltype,
		}
	}
	plocks := make([]*ProtoPlock, len(s.Plocks))
	for i, p := range s.Plocks {
		records := make([]*ProtoPlockRecord, len(p.Records))
		for j, r := range p.Records {
			records[j] = &ProtoPlockRecord{
				Type:  r.Type,
				Pid:   r.Pid,
				Start: r.Start,
				End:   r.End,
			}
		}
		plocks[i] = &ProtoPlock{
			Inode:   uint64(p.Inode),
			Owner:   p.Owner,
			Records: records,
		}
	}
	sustained := make([]uint64, len(s.Sustained))
	for i, ino := range s.Sustained {
		sustained[i] = uint64(ino)
	}
	return &ProtoSession{
		Sid:       s.Sid,
		Expire:    s.Expire.Unix(),
		Info:      info,
		Sustained: sustained,
		Flocks:    flocks,
		Plocks:    plocks,
	}
}

func ProtoToSession(p *ProtoSession) *meta.Session {
	if p == nil {
		return nil
	}
	info := meta.SessionInfo{
		Version:    p.Info.Version,
		HostName:   p.Info.HostName,
		IPAddrs:    p.Info.IpAddrs,
		MountPoint: p.Info.MountPoint,
		MountTime:  time.Unix(p.Info.MountTime, 0),
		ProcessID:  int(p.Info.ProcessId),
	}
	flocks := make([]meta.Flock, len(p.Flocks))
	for i, f := range p.Flocks {
		flocks[i] = meta.Flock{
			Inode: meta.Ino(f.Inode),
			Owner: f.Owner,
			Ltype: f.Ltype,
		}
	}
	// Note: plocks use unexported plockRecord type, so we can't convert them back
	// This is OK since we only need SessionToProto for the server
	sustained := make([]meta.Ino, len(p.Sustained))
	for i, id := range p.Sustained {
		sustained[i] = meta.Ino(id)
	}
	return &meta.Session{
		Sid:         p.Sid,
		Expire:      time.Unix(p.Expire, 0),
		SessionInfo: info,
		Sustained:   sustained,
		Flocks:      flocks,
	}
}

// Alias functions for simpler names used in grpc_client.go

func toProtoAttr(a *meta.Attr) *ProtoAttr {
	return AttrToProto(a)
}

func fromProtoAttr(p *ProtoAttr) *meta.Attr {
	return ProtoToAttr(p)
}

func toProtoSlice(s *meta.Slice) *ProtoSlice {
	if s == nil {
		return nil
	}
	return SliceToProto(*s)
}

func fromProtoSlice(p *ProtoSlice) *meta.Slice {
	if p == nil {
		return nil
	}
	return &meta.Slice{
		Id:   p.Id,
		Size: p.Size,
		Off:  p.Off,
		Len:  p.Len,
	}
}

func toProtoEntry(e *meta.Entry) *ProtoEntry {
	return EntryToProto(e)
}

func fromProtoEntry(p *ProtoEntry) *meta.Entry {
	return ProtoToEntry(p)
}

func toProtoSummary(s *meta.Summary) *ProtoSummary {
	return SummaryToProto(s)
}

func fromProtoSummary(p *ProtoSummary) *meta.Summary {
	return ProtoToSummary(p)
}

func toProtoTreeSummary(t *meta.TreeSummary) *ProtoTreeSummary {
	return TreeSummaryToProto(t)
}

func fromProtoTreeSummary(p *ProtoTreeSummary) *meta.TreeSummary {
	return ProtoToTreeSummary(p)
}

func toProtoFormat(f *meta.Format) *ProtoFormat {
	return FormatToProtoPtr(f)
}

func fromProtoFormat(p *ProtoFormat) *meta.Format {
	return ProtoToFormat(p)
}

func toProtoQuota(q *meta.Quota) *ProtoQuota {
	return QuotaToProto(q)
}

func fromProtoQuota(p *ProtoQuota) *meta.Quota {
	return ProtoToQuota(p)
}

func toProtoSession(s *meta.Session) *ProtoSession {
	return SessionToProto(s)
}

func fromProtoSession(p *ProtoSession) *meta.Session {
	return ProtoToSession(p)
}

func toProtoFlock(f *meta.Flock) *ProtoFlock {
	if f == nil {
		return nil
	}
	return &ProtoFlock{
		Inode: uint64(f.Inode),
		Owner: f.Owner,
		Ltype: f.Ltype,
	}
}

func fromProtoFlock(p *ProtoFlock) *meta.Flock {
	if p == nil {
		return nil
	}
	return &meta.Flock{
		Inode: meta.Ino(p.Inode),
		Owner: p.Owner,
		Ltype: p.Ltype,
	}
}

func toProtoPlock(plock *meta.Plock) *ProtoPlock {
	if plock == nil {
		return nil
	}
	records := make([]*ProtoPlockRecord, len(plock.Records))
	for i, r := range plock.Records {
		records[i] = &ProtoPlockRecord{
			Type:  r.Type,
			Pid:   r.Pid,
			Start: r.Start,
			End:   r.End,
		}
	}
	return &ProtoPlock{
		Inode:   uint64(plock.Inode),
		Owner:   plock.Owner,
		Records: records,
	}
}

func fromProtoPlock(p *ProtoPlock) *meta.Plock {
	if p == nil {
		return nil
	}
	// plockRecord is unexported, so we create a Plock with empty Records
	// The actual records are stored in the server's internal representation
	return &meta.Plock{
		Inode: meta.Ino(p.Inode),
		Owner: p.Owner,
		// Records field is unexported (plockRecord type), can't populate it here
	}
}

// ACL conversion

func toProtoACLRule(rule *aclAPI.Rule) *ProtoACLRule {
	if rule == nil {
		return nil
	}
	return &ProtoACLRule{
		Id:      uint32(rule.Owner),
		Type:    uint32(rule.Group),
		Perms:   uint32(rule.Mask),
		Entries: []uint32{uint32(rule.Other)},
	}
}

func fromProtoACLRule(p *ProtoACLRule) *aclAPI.Rule {
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
