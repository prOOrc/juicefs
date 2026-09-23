/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
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
	"context"
	"fmt"
	"time"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	compactSizeHistogram = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "compact_size_histogram_bytes",
		Help:    "Distribution of size of compacted data in bytes.",
		Buckets: prometheus.ExponentialBuckets(1024, 2, 16),
	})
)

func readSlice(store chunk.ChunkStore, s *meta.Slice, key []byte, page *chunk.Page, off int) error {
	buf := page.Data
	read := 0
	reader := store.NewReaderWithKey(s.Id, int(s.Size), key)
	for read < len(buf) {
		p := page.Slice(read, len(buf)-read)
		n, err := reader.ReadAt(context.Background(), p, off+int(s.Off))
		p.Release()
		if n == 0 && err != nil {
			return err
		}
		read += n
		off += n
	}
	return nil
}

// Compact merges slices into a new slice id. For encrypted sources (slices with
// wrapped CEKs) it unwraps each CEK under the file's FEK, reads with it, writes
// the merged data under a fresh CEK and returns that CEK wrapped under the same
// FEK (FR-OP-7); the caller persists it in the new slice record. fek must be
// non-nil when any source slice is encrypted — otherwise compaction fails closed.
func Compact(conf chunk.Config, store chunk.ChunkStore, slices []meta.Slice, id uint64, tierID uint8,
	fek []byte, driveFileID string, fekVersion uint32) (wrappedCEK []byte, err error) {
	for utils.AllocMemory()-store.UsedMemory() > int64(conf.BufferSize)*3/2 {
		time.Sleep(time.Millisecond * 100)
	}
	var size uint32
	encrypted := false
	for _, s := range slices {
		size += s.Len
		if len(s.WrappedCEK) > 0 {
			encrypted = true
		}
	}
	if encrypted && len(fek) == 0 {
		return nil, fmt.Errorf("compact %d: encrypted slices require the file FEK (fail-closed)", id)
	}
	compactSizeHistogram.Observe(float64(size))
	logger.Debugf("compact %d slices (%d bytes) to new slice %d", len(slices), size, id)

	var newCEK []byte
	var writer chunk.Writer
	if encrypted {
		newCEK, err = chunk.NewCEK()
		if err != nil {
			return nil, err
		}
		writer = store.NewWriterWithKey(id, tierID, newCEK)
	} else {
		writer = store.NewWriter(id, tierID)
	}
	writer.SetWriteback(false)

	var pos int
	for i, s := range slices {
		if s.Id == 0 {
			_, err := writer.WriteAt(make([]byte, int(s.Len)), int64(pos))
			if err != nil {
				writer.Abort()
				return nil, err
			}
			pos += int(s.Len)
			continue
		}
		var key []byte
		if len(s.WrappedCEK) > 0 {
			key, err = chunk.UnwrapCEK(fek, s.WrappedCEK, driveFileID, s.Id, fekVersion)
			if err != nil {
				logger.Errorf("can't compact to slice %d: unwrap CEK of slice %d: %s (fail-closed)", id, s.Id, err)
				writer.Abort()
				return nil, err
			}
		}
		var read int
		for read < int(s.Len) {
			l := min(conf.BlockSize, int(s.Len)-read)
			p := chunk.NewOffPage(l)
			if err := readSlice(store, &slices[i], key, p, read); err != nil {
				logger.Debugf("can't compact to slice %d, retry later, read %d: %s", id, i, err)
				p.Release()
				writer.Abort()
				return nil, err
			}
			_, err := writer.WriteAt(p.Data, int64(pos+read))
			p.Release()
			if err != nil {
				logger.Errorf("can't compact to slice %d, retry later, write: %s", id, err)
				writer.Abort()
				return nil, err
			}
			read += l
			if pos+read >= conf.BlockSize {
				if err = writer.FlushTo(pos + read); err != nil {
					panic(err)
				}
			}
		}
		pos += int(s.Len)
	}
	err = writer.Finish(pos)
	if err != nil {
		writer.Abort()
		return nil, err
	}
	if encrypted {
		wrappedCEK, err = chunk.WrapCEK(fek, newCEK, driveFileID, id, fekVersion)
		if err != nil {
			return nil, fmt.Errorf("compact %d: wrap CEK: %w", id, err)
		}
	}
	return wrappedCEK, nil
}
