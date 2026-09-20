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

package meta

import (
	"encoding/binary"

	"github.com/juicedata/juicefs/pkg/utils"
)

type slice struct {
	id    uint64
	size  uint32
	off   uint32
	len   uint32
	pos   uint32
	left  *slice
	right *slice

	// wrappedCEK is the AGCK blob (CEK wrapped under the file's FEK); nil for
	// legacy plaintext slices. It travels with the slice record (D2).
	wrappedCEK []byte
}

func newSlice(pos uint32, id uint64, cleng, off, len uint32) *slice {
	if len == 0 {
		return nil
	}
	s := &slice{}
	s.pos = pos
	s.id = id
	s.size = cleng
	s.off = off
	s.len = len
	s.left = nil
	s.right = nil
	return s
}

func (s *slice) read(buf []byte) {
	rb := utils.ReadBuffer(buf)
	s.pos = rb.Get32()
	s.id = rb.Get64()
	s.size = rb.Get32()
	s.off = rb.Get32()
	s.len = rb.Get32()
	// Optional tail: uint32 blobLen + AGCK blob (wrapped CEK). Absent for legacy.
	// Callers (readSlices) validate the tail length before calling read.
	if rb.Left() > 0 {
		blobLen := rb.Get32()
		s.wrappedCEK = append([]byte(nil), rb.Get(int(blobLen))...)
	}
}

func (s *slice) cut(pos uint32) (left, right *slice) {
	if s == nil {
		return nil, nil
	}
	if pos <= s.pos {
		if s.left == nil {
			s.left = newSlice(pos, 0, 0, 0, s.pos-pos)
		}
		left, s.left = s.left.cut(pos)
		return left, s
	} else if pos < s.pos+s.len {
		l := pos - s.pos
		right = newSlice(pos, s.id, s.size, s.off+l, s.len-l)
		right.wrappedCEK = s.wrappedCEK // both halves reference the same slice data
		right.right = s.right
		s.len = l
		s.right = nil
		return s, right
	} else {
		if s.right == nil {
			s.right = newSlice(s.pos+s.len, 0, 0, 0, pos-s.pos-s.len)
		}
		s.right, right = s.right.cut(pos)
		return s, right
	}
}

func (s *slice) visit(f func(*slice)) {
	if s == nil {
		return
	}
	s.left.visit(f)
	right := s.right
	f(s) // s could be freed
	right.visit(f)
}

const sliceBytes = 24

func marshalSlice(pos uint32, id uint64, size, off, len uint32) []byte {
	w := utils.NewBuffer(sliceBytes)
	w.Put32(pos)
	w.Put64(id)
	w.Put32(size)
	w.Put32(off)
	w.Put32(len)
	return w.Bytes()
}

// marshalSliceCEK marshals a slice record with an optional wrapped-CEK tail:
// [24B fixed][uint32 blobLen + AGCK blob]. An empty wrappedCEK produces the
// legacy 24-byte record (D2).
func marshalSliceCEK(pos uint32, id uint64, size, off, length uint32, wrappedCEK []byte) []byte {
	if len(wrappedCEK) == 0 {
		return marshalSlice(pos, id, size, off, length)
	}
	w := utils.NewBuffer(uint32(sliceBytes + 4 + len(wrappedCEK)))
	w.Put32(pos)
	w.Put64(id)
	w.Put32(size)
	w.Put32(off)
	w.Put32(length)
	w.Put32(uint32(len(wrappedCEK)))
	w.Put(wrappedCEK)
	return w.Bytes()
}

func readSlices(vals []string) []*slice {
	slices := make([]slice, len(vals))
	ss := make([]*slice, len(vals))
	for i, val := range vals {
		if len(val) < sliceBytes {
			logger.Errorf("corrupt slice: len=%d, val=%v", len(val), []byte(val))
			return nil
		}
		if tail := len(val) - sliceBytes; tail > 0 {
			if tail < 4 {
				logger.Errorf("corrupt slice: tail len=%d, val=%v", tail, []byte(val))
				return nil
			}
			blobLen := binary.BigEndian.Uint32([]byte(val[sliceBytes : sliceBytes+4]))
			if tail != 4+int(blobLen) {
				logger.Errorf("corrupt slice: tail len=%d, blobLen=%d", tail, blobLen)
				return nil
			}
		}
		s := &slices[i]
		s.read([]byte(val))
		ss[i] = s
	}
	return ss
}

func readSliceBuf(buf []byte) []*slice {
	if len(buf)%sliceBytes != 0 {
		logger.Errorf("corrupt slices: len=%d", len(buf))
		return nil
	}
	nSlices := len(buf) / sliceBytes
	slices := make([]slice, nSlices)
	ss := make([]*slice, nSlices)
	for i := 0; i < len(buf); i += sliceBytes {
		s := &slices[i/sliceBytes]
		// Fixed 24-byte records only (sql/tkv backends never store the CEK tail).
		s.read(buf[i : i+sliceBytes])
		ss[i/sliceBytes] = s
	}
	return ss
}

func buildSlice(ss []*slice) []Slice {
	var root *slice
	for i := range ss {
		s := new(slice)
		*s = *ss[i]
		var right *slice
		s.left, right = root.cut(s.pos)
		_, s.right = right.cut(s.pos + s.len)
		root = s
	}
	var pos uint32
	var chunk []Slice
	root.visit(func(s *slice) {
		if s.pos > pos {
			chunk = append(chunk, Slice{Size: s.pos - pos, Len: s.pos - pos})
			pos = s.pos
		}
		chunk = append(chunk, Slice{Id: s.id, Size: s.size, Off: s.off, Len: s.len, WrappedCEK: s.wrappedCEK})
		pos += s.len
	})
	return chunk
}

func compactChunk(ss []*slice) (uint32, uint32, []Slice) {
	var chunk = buildSlice(ss)
	var pos uint32
	n := len(chunk)
	for n > 1 {
		if chunk[0].Id == 0 {
			pos += chunk[0].Len
			chunk = chunk[1:]
			n--
		} else if chunk[n-1].Id == 0 {
			chunk = chunk[:n-1]
			n--
		} else {
			break
		}
	}
	if n == 1 && chunk[0].Id == 0 {
		chunk[0].Len = 1
	}
	var size uint32
	for _, c := range chunk {
		size += c.Len
	}
	return pos, size, chunk
}

func skipSome(chunk []*slice) int {
	var skipped int
	var total = len(chunk)
OUT:
	for skipped < total {
		ss := chunk[skipped:]
		pos, size, c := compactChunk(ss)
		first := ss[0]
		if first.len < (1<<20) || first.len*5 < size || size == 0 {
			// it's too small
			break
		}
		isFirst := func(pos uint32, s Slice) bool {
			return pos == first.pos && s.Id == first.id && s.Off == first.off && s.Len == first.len
		}
		if !isFirst(pos, c[0]) {
			// it's not the first slice, compact it
			break
		}
		for _, s := range ss[1:] {
			if s.equals(first) {
				break OUT
			}
		}
		skipped++
	}
	return skipped
}

// equals compares the data fields of two slice records (the struct carries a
// []byte field and cannot be compared directly).
func (s *slice) equals(o *slice) bool {
	return s.pos == o.pos && s.id == o.id && s.size == o.size && s.off == o.off && s.len == o.len
}
