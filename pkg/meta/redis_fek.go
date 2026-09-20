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
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

// FileCrypto — crypto metadata of a file to be written into the inode attr.
type FileCrypto struct {
	WrappedFek  []byte
	DriveFileID string
	FekVersion  uint32
	CryptoAlg   string // "AES-256-GCM"
}

// SetFileCrypto writes the crypto fields into the inode attr in a single Redis
// transaction (GET attr -> mutate crypto fields -> SET). The plaintext FEK is
// never stored — only the AGFK blob wrapped under the company KEK.
func (m *redisMeta) SetFileCrypto(ctx Context, inode Ino, c *FileCrypto) syscall.Errno {
	var attr Attr
	err := m.txn(ctx, func(tx *redis.Tx) error {
		a, err := tx.Get(ctx, m.inodeKey(inode)).Bytes()
		if err != nil {
			return err
		}
		m.parseAttr(a, &attr)
		attr.Encrypted = true
		attr.WrappedFek = c.WrappedFek
		attr.DriveFileID = c.DriveFileID
		attr.FekVersion = c.FekVersion
		attr.CryptoAlg = c.CryptoAlg
		now := time.Now()
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, m.inodeKey(inode), m.marshal(&attr), 0)
			m.genLog(ctx, pipe, now, "SETFILECRYPTO(%d,%s,%d)", inode, c.DriveFileID, c.FekVersion)
			return nil
		})
		return err
	}, m.inodeKey(inode))
	if err != nil {
		return errno(err)
	}
	return 0
}
