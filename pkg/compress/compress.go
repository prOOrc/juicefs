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

package compress

import (
	"fmt"
	"strings"

	"github.com/klauspost/compress/zstd"
	lz4 "github.com/pierrec/lz4/v4"
)

// Pure-Go реализации (cfapi-сборка движка без CGO, change fix-cfapi-
// followups A5): DataDog/zstd и hungys/go-lz4 — cgo-обёртки libzstd/liblz4,
// заменены на klauspost/compress и pierrec/lz4. Форматы данных совместимы:
// zstd — тот же фрейм-формат (данные, сжатые старым бинарем, читаются новым
// и наоборот), lz4 — тот же сырой block-формат. Рантайм-контракты фасада
// сохранены: запись в переданный буфер без аллокаций наружу, «buffer too
// short» только при реальном превышении len(dst).

// Один синглтон на процесс: klauspost-кодек потокобезопасен и пулит буферы
// сам; concurrency 1 — предсказуемая латентность на воркера чанка.
var (
	zstdEncoder, _ = zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest), // DataDog level 1 (ZSTD_LEVEL=1)
		zstd.WithEncoderConcurrency(1))
	zstdDecoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
)

// ZSTD_LEVEL compression level used by Zstd
const ZSTD_LEVEL = 1 // fastest

// Compressor interface to be implemented by a compression algo
type Compressor interface {
	Name() string
	CompressBound(int) int
	Compress(dst, src []byte) (int, error)
	Decompress(dst, src []byte) (int, error)
}

// NewCompressor returns a struct implementing Compressor interface
func NewCompressor(algr string) Compressor {
	algr = strings.ToLower(algr)
	if algr == "zstd" {
		return ZStandard{ZSTD_LEVEL}
	} else if algr == "lz4" {
		return LZ4{}
	} else if algr == "none" || algr == "" {
		return noOp{}
	}
	return nil
}

type noOp struct{}

func (n noOp) Name() string            { return "Noop" }
func (n noOp) CompressBound(l int) int { return l }
func (n noOp) Compress(dst, src []byte) (int, error) {
	if len(dst) < len(src) {
		return 0, fmt.Errorf("buffer too short: %d < %d", len(dst), len(src))
	}
	copy(dst, src)
	return len(src), nil
}
func (n noOp) Decompress(dst, src []byte) (int, error) {
	if len(dst) < len(src) {
		return 0, fmt.Errorf("buffer too short: %d < %d", len(dst), len(src))
	}
	copy(dst, src)
	return len(src), nil
}

// ZStandard implements Compressor interface using zstd library
type ZStandard struct {
	level int
}

// Name returns name of the algorithm Zstd
func (n ZStandard) Name() string { return "Zstd" }

// CompressBound max size of compressed data — зеркалирует ZSTD_COMPRESSBOUND
// libzstd с запасом +4KiB (урок f78e14cd: буферы STANDARD с запасом).
func (n ZStandard) CompressBound(l int) int {
	b := l + (l >> 8)
	if l < 128*1024 {
		b += (128*1024 - l) >> 11
	} else {
		b += 512
	}
	return b + 4096
}

// emptyZstdFrame — минимальный валидный zstd-фрейм пустого контента
// (magic + FHD single-segment с 1-байтовым FCS=0 + raw-блок size 0).
// klauspost EncodeAll на пустом входе возвращает ПУСТОЙ выход (без фрейма),
// а контракт фасада требует, чтобы compress(empty) давал декодируемый
// результат — как libzstd в старой сборке (DataDog).
var emptyZstdFrame = []byte{0x28, 0xB5, 0x2F, 0xFD, 0x20, 0x00, 0x01, 0x00, 0x00}

// Compress using Zstd
func (n ZStandard) Compress(dst, src []byte) (int, error) {
	if len(src) == 0 {
		if len(dst) < len(emptyZstdFrame) {
			return 0, fmt.Errorf("buffer too short: %d < %d", len(dst), len(emptyZstdFrame))
		}
		copy(dst, emptyZstdFrame)
		return len(emptyZstdFrame), nil
	}
	d := zstdEncoder.EncodeAll(src, nil)
	if len(d) > len(dst) {
		return 0, fmt.Errorf("buffer too short: %d < %d", len(dst), len(d))
	}
	copy(dst, d)
	return len(d), nil
}

// Decompress using Zstd
func (n ZStandard) Decompress(dst, src []byte) (int, error) {
	if len(src) == 0 {
		return 0, fmt.Errorf("decompress an empty input")
	}
	d, err := zstdDecoder.DecodeAll(src, nil)
	if err != nil {
		return 0, err
	}
	if len(d) > len(dst) {
		return 0, fmt.Errorf("buffer too short: %d < %d", len(dst), len(d))
	}
	copy(dst, d)
	return len(d), nil
}

// LZ4 implements Compressor using LZ4 library
type LZ4 struct{}

// Name returns name of the algorithm LZ4
func (l LZ4) Name() string { return "LZ4" }

// CompressBound max size of compressed data
func (l LZ4) CompressBound(size int) int { return lz4.CompressBlockBound(size) }

// Compress using LZ4 algorithm (raw block, как liblz4 в hungys/go-lz4).
// pierrec CompressBlock на слишком маленьком dst возвращает (0, nil) —
// контракт старой сборки требует ошибку (тест «expect short buffer error»).
func (l LZ4) Compress(dst, src []byte) (int, error) {
	n, err := lz4.CompressBlock(src, dst, nil)
	if err == nil && n == 0 && len(src) > 0 {
		return 0, fmt.Errorf("lz4: buffer too short: %d for %d bytes", len(dst), len(src))
	}
	return n, err
}

// Decompress using LZ4 algorithm (raw block)
func (l LZ4) Decompress(dst, src []byte) (int, error) {
	if len(src) == 0 {
		return 0, fmt.Errorf("decompress an empty input")
	}
	return lz4.UncompressBlock(src, dst)
}
