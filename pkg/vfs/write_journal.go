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
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

// WriteJournal is an append-only durability log for user-level writes made
// while the hub is unreachable (NFR-OFF-1/3). Each record carries (inode,
// offset, data) so it can be re-applied through the normal write path after
// the hub recovers. The journal lives in the local cache dir, is created
// 0600 and holds plaintext data (not keys), so WipeKeys does not touch it;
// it is truncated on logout revocation.
type WriteJournal struct {
	path string
	mu   sync.Mutex
	f    *os.File
	seq  uint64
	off  int64 // write offset: end of the last valid record
}

const journalHeaderLen = 8 + 8 + 8 + 4 // seq, inode, off, len (big endian)

// NewWriteJournal opens (creating if needed) the journal file and recovers
// the sequence number from existing records. A torn or corrupt tail left by a
// crash is truncated away so it cannot be mistaken for data on replay.
func NewWriteJournal(path string) (*WriteJournal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	j := &WriteJournal{path: path, f: f}
	var end int64
	for {
		start := end
		var hdr [journalHeaderLen]byte
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			break // EOF or torn tail
		}
		length := binary.BigEndian.Uint32(hdr[24:28])
		if length > 1<<31 {
			break // corrupt record
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(f, data); err != nil {
			break // torn tail
		}
		var crcBuf [4]byte
		if _, err := io.ReadFull(f, crcBuf[:]); err != nil {
			break // torn tail
		}
		want := binary.BigEndian.Uint32(crcBuf[:])
		got := crc32.Update(crc32.ChecksumIEEE(hdr[:]), crc32.IEEETable, data)
		if got != want {
			break // corrupt record: drop it and everything after
		}
		end = start + journalHeaderLen + int64(length) + 4
		if seq := binary.BigEndian.Uint64(hdr[:8]); seq > j.seq {
			j.seq = seq
		}
	}
	if size, err := f.Seek(0, io.SeekEnd); err == nil && size != end {
		_ = f.Truncate(end) // drop the torn/corrupt tail
	}
	j.off = end
	return j, nil
}

// Append records a user-level write. The record is fsynced before returning
// so it survives a crash (NFR-OFF-1).
func (j *WriteJournal) Append(inode Ino, off int64, data []byte) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return os.ErrClosed
	}
	j.seq++
	var hdr [journalHeaderLen]byte
	binary.BigEndian.PutUint64(hdr[0:8], j.seq)
	binary.BigEndian.PutUint64(hdr[8:16], uint64(inode))
	binary.BigEndian.PutUint64(hdr[16:24], uint64(off))
	binary.BigEndian.PutUint32(hdr[24:28], uint32(len(data)))
	rec := make([]byte, 0, journalHeaderLen+len(data)+4)
	rec = append(rec, hdr[:]...)
	rec = append(rec, data...)
	crc := crc32.Update(crc32.ChecksumIEEE(hdr[:]), crc32.IEEETable, data)
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], crc)
	rec = append(rec, tail[:]...)
	if _, err := j.f.WriteAt(rec, j.off); err != nil {
		return err
	}
	j.off += int64(len(rec))
	return j.f.Sync()
}

// Replay walks the records in order and passes each to fn. The snapshot is
// taken under the journal lock (so it is consistent with Append), but fn runs
// without it — fn re-enters the writer, which may Append again if the hub
// drops mid-replay. Replay stops at the first CRC failure or fn error and
// returns it; the journal is kept so a later retry sees the same records
// (re-applying an identical record is idempotent for reads — buildSlice
// resolves overlaps last-write-wins).
func (j *WriteJournal) Replay(fn func(seq uint64, inode Ino, off int64, data []byte) error) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	recs, err := j.readAll()
	j.mu.Unlock()
	if err != nil {
		return err
	}
	for _, r := range recs {
		if err := fn(r.seq, r.inode, r.off, r.data); err != nil {
			return err
		}
	}
	return nil
}

// Inodes returns the distinct inodes present in the journal records, in
// first-seen order. The snapshot is taken under the journal lock, mirroring
// Replay; the replay re-Open phase (design A2) uses it to refresh the FEKs of
// journaled files before their records are applied.
func (j *WriteJournal) Inodes() []Ino {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	recs, err := j.readAll()
	j.mu.Unlock()
	if err != nil {
		return nil
	}
	var inos []Ino
	seen := make(map[Ino]struct{}, len(recs))
	for _, r := range recs {
		if _, ok := seen[r.inode]; !ok {
			seen[r.inode] = struct{}{}
			inos = append(inos, r.inode)
		}
	}
	return inos
}

type journalRecord struct {
	seq   uint64
	inode Ino
	off   int64
	data  []byte
}

// readAll scans the file from the start; must be called with j.mu held.
func (j *WriteJournal) readAll() ([]journalRecord, error) {
	r, err := os.Open(j.path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var recs []journalRecord
	for {
		var hdr [journalHeaderLen]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				return recs, nil
			}
			return nil, fmt.Errorf("read journal: %s", err)
		}
		length := binary.BigEndian.Uint32(hdr[24:28])
		if length > 1<<31 {
			return nil, fmt.Errorf("corrupt journal record: len=%d", length)
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, fmt.Errorf("torn journal record: %s", err)
		}
		var crcBuf [4]byte
		if _, err := io.ReadFull(r, crcBuf[:]); err != nil {
			return nil, fmt.Errorf("torn journal record: %s", err)
		}
		want := binary.BigEndian.Uint32(crcBuf[:])
		got := crc32.Update(crc32.ChecksumIEEE(hdr[:]), crc32.IEEETable, data)
		if got != want {
			return nil, fmt.Errorf("corrupt journal record seq=%d: crc mismatch", binary.BigEndian.Uint64(hdr[:8]))
		}
		recs = append(recs, journalRecord{
			seq:   binary.BigEndian.Uint64(hdr[:8]),
			inode: Ino(binary.BigEndian.Uint64(hdr[8:16])),
			off:   int64(binary.BigEndian.Uint64(hdr[16:24])),
			data:  data,
		})
	}
}

// Empty reports whether the journal holds no records.
func (j *WriteJournal) Empty() bool {
	if j == nil {
		return true
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.off == 0
}

// Truncate removes all records after a fully successful replay.
func (j *WriteJournal) Truncate() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return os.ErrClosed
	}
	if err := j.f.Truncate(0); err != nil {
		return err
	}
	j.off = 0
	j.seq = 0
	return j.f.Sync()
}

// Close closes the journal file.
func (j *WriteJournal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}
