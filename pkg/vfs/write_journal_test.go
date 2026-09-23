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
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteJournal(t *testing.T) {
	t.Run("RoundTrip", func(t *testing.T) {
		path := t.TempDir() + "/journal.bin"
		j, err := NewWriteJournal(path)
		require.NoError(t, err)
		defer j.Close()

		require.True(t, j.Empty())
		require.NoError(t, j.Append(1, 0, []byte("hello")))
		require.NoError(t, j.Append(2, 5, []byte("world!")))
		require.NoError(t, j.Append(1, 100, nil)) // zero-length write is a valid record
		require.False(t, j.Empty())

		var got []journalRecord
		err = j.Replay(func(seq uint64, inode Ino, off int64, data []byte) error {
			got = append(got, journalRecord{seq: seq, inode: inode, off: off, data: data})
			return nil
		})
		require.NoError(t, err)
		require.Len(t, got, 3)
		require.EqualValues(t, 1, got[0].seq)
		require.Equal(t, Ino(1), got[0].inode)
		require.EqualValues(t, 0, got[0].off)
		require.Equal(t, []byte("hello"), got[0].data)
		require.EqualValues(t, 2, got[1].seq)
		require.Equal(t, Ino(2), got[1].inode)
		require.EqualValues(t, 5, got[1].off)
		require.Equal(t, []byte("world!"), got[1].data)
		require.EqualValues(t, 3, got[2].seq)
		require.Equal(t, Ino(1), got[2].inode)
		require.EqualValues(t, 100, got[2].off)
		require.Empty(t, got[2].data)
	})

	t.Run("CRCMismatch", func(t *testing.T) {
		path := t.TempDir() + "/journal.bin"
		j, err := NewWriteJournal(path)
		require.NoError(t, err)
		defer j.Close()
		require.NoError(t, j.Append(1, 0, []byte("hello")))
		require.NoError(t, j.Append(2, 5, []byte("world!")))

		// Corrupt one byte of the second record's data: record 1 is
		// 28 (header) + 5 (data) + 4 (crc) = 37 bytes, so record 2's data
		// starts at 37+28.
		buf, err := os.ReadFile(path)
		require.NoError(t, err)
		buf[37+journalHeaderLen+2] ^= 0xFF
		require.NoError(t, os.WriteFile(path, buf, 0o600))

		err = j.Replay(func(seq uint64, inode Ino, off int64, data []byte) error {
			t.Fatalf("record %d must not be delivered after corruption", seq)
			return nil
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "crc mismatch")
	})

	t.Run("SeqRecovery", func(t *testing.T) {
		path := t.TempDir() + "/journal.bin"
		j, err := NewWriteJournal(path)
		require.NoError(t, err)
		require.NoError(t, j.Append(1, 0, []byte("a")))
		require.NoError(t, j.Append(1, 1, []byte("b")))
		require.NoError(t, j.Append(1, 2, []byte("c")))
		require.NoError(t, j.Close())

		// Reopen: the sequence must continue after the recovered records.
		j, err = NewWriteJournal(path)
		require.NoError(t, err)
		defer j.Close()
		require.NoError(t, j.Append(1, 3, []byte("d")))

		var seqs []uint64
		err = j.Replay(func(seq uint64, inode Ino, off int64, data []byte) error {
			seqs = append(seqs, seq)
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, []uint64{1, 2, 3, 4}, seqs)
	})

	t.Run("Truncate", func(t *testing.T) {
		path := t.TempDir() + "/journal.bin"
		j, err := NewWriteJournal(path)
		require.NoError(t, err)
		defer j.Close()
		require.NoError(t, j.Append(1, 0, []byte("abc")))
		require.NoError(t, j.Truncate())

		require.True(t, j.Empty())
		n := 0
		require.NoError(t, j.Replay(func(seq uint64, inode Ino, off int64, data []byte) error {
			n++
			return nil
		}))
		require.Zero(t, n)

		// The sequence restarts after a truncate.
		require.NoError(t, j.Append(2, 7, []byte("x")))
		var seqs []uint64
		require.NoError(t, j.Replay(func(seq uint64, inode Ino, off int64, data []byte) error {
			seqs = append(seqs, seq)
			return nil
		}))
		require.Equal(t, []uint64{1}, seqs)
	})

	t.Run("TornTail", func(t *testing.T) {
		path := t.TempDir() + "/journal.bin"
		j, err := NewWriteJournal(path)
		require.NoError(t, err)
		require.NoError(t, j.Append(1, 0, []byte("hello")))
		require.NoError(t, j.Append(2, 5, []byte("world!")))
		sizeAfterTwo, err := os.Stat(path)
		require.NoError(t, err)
		require.NoError(t, j.Close())

		// Simulate a crash mid-write: append a partial record.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		require.NoError(t, err)
		_, err = f.Write([]byte{1, 2, 3, 4, 5})
		require.NoError(t, err)
		require.NoError(t, f.Close())

		j, err = NewWriteJournal(path)
		require.NoError(t, err)
		defer j.Close()
		st, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, sizeAfterTwo.Size(), st.Size(), "torn tail must be truncated away")

		var got []journalRecord
		err = j.Replay(func(seq uint64, inode Ino, off int64, data []byte) error {
			got = append(got, journalRecord{seq: seq, data: data})
			return nil
		})
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.True(t, bytes.Equal([]byte("hello"), got[0].data))
		require.True(t, bytes.Equal([]byte("world!"), got[1].data))
	})
}
