//go:build windows
// +build windows

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

package cfapi

import (
	"runtime"
	"testing"
	"unsafe"
)

// TestBufferPoolBasics: выравнивание, счётчики, переиспользование.
func TestBufferPoolBasics(t *testing.T) {
	pool := NewBufferPool(2)
	defer pool.Close()

	buf, free := pool.Get(1)
	if buf == nil {
		t.Fatal("Get вернул nil")
	}
	defer free()
	if len(buf) != allocationGranularity {
		t.Errorf("размер буфера = %d, ожидается одна гранула %d", len(buf), allocationGranularity)
	}
	base := uintptr(unsafe.Pointer(unsafe.SliceData(buf)))
	if base%allocationGranularity != 0 {
		t.Errorf("буфер не выровнен на %d: %#x", allocationGranularity, base)
	}
	if pool.Busy() != 1 {
		t.Errorf("Busy = %d, ожидается 1", pool.Busy())
	}

	free()
	if pool.Busy() != 0 {
		t.Errorf("Busy после free = %d, ожидается 0", pool.Busy())
	}
	if pool.Idle() != 1 {
		t.Errorf("Idle = %d, ожидается 1", pool.Idle())
	}

	allocsBefore, _ := pool.Stats()
	buf2, free2 := pool.Get(allocationGranularity)
	defer free2()
	allocsAfter, _ := pool.Stats()
	if allocsBefore != allocsAfter {
		t.Errorf("Get выделил новый буфер вместо переиспользования: allocs %d -> %d", allocsBefore, allocsAfter)
	}
	if uintptr(unsafe.Pointer(unsafe.SliceData(buf2))) != base {
		t.Errorf("Get вернул другой буфер: %#x != %#x", unsafe.SliceData(buf2), base)
	}
}

// TestBufferPoolResize: запрос больше свободного буфера — выделение большего;
// меньший свободный переиспользуется для меньшего запроса.
func TestBufferPoolResize(t *testing.T) {
	pool := NewBufferPool(4)
	defer pool.Close()

	small, freeSmall := pool.Get(1024)
	big, freeBig := pool.Get(200 * 1024) // > 64 KiB: resize вверх до 256 KiB
	if len(big) < 200*1024 {
		t.Errorf("размер буфера = %d, меньше запрошенных 200 KiB", len(big))
	}
	if uintptr(unsafe.Pointer(unsafe.SliceData(big)))%allocationGranularity != 0 {
		t.Error("большой буфер не выровнен")
	}

	allocsAfterBig, _ := pool.Stats()
	if allocsAfterBig != 2 {
		t.Errorf("allocs = %d, ожидается 2 (два разных размера)", allocsAfterBig)
	}

	freeBig()
	freeSmall()
	// меньший запрос берёт маленький свободный буфер
	again, freeAgain := pool.Get(1)
	defer freeAgain()
	if uintptr(unsafe.Pointer(unsafe.SliceData(again))) != uintptr(unsafe.Pointer(unsafe.SliceData(small))) {
		t.Error("маленький запрос не взял маленький буфер из пула")
	}
}

// TestBufferPoolEviction: свободных больше maxIdle — лишний освобождается.
func TestBufferPoolEviction(t *testing.T) {
	pool := NewBufferPool(1)
	defer pool.Close()

	_, free1 := pool.Get(1)
	_, free2 := pool.Get(1)
	free1()
	free2()
	if pool.Idle() != 1 {
		t.Errorf("Idle = %d, ожидается 1 (второй буфер освобождён)", pool.Idle())
	}
	allocs, frees := pool.Stats()
	if allocs != 2 || frees != 1 {
		t.Errorf("allocs/frees = %d/%d, ожидается 2/1", allocs, frees)
	}
}

// TestBufferPoolBusyCycles: десять циклов Get/Put — занятые буферы и горутины
// возвращаются к исходным значениям (DoD 7.5).
func TestBufferPoolBusyCycles(t *testing.T) {
	pool := NewBufferPool(2)
	defer pool.Close()

	goroutinesBefore := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		var frees []func()
		for j := 0; j < 5; j++ {
			buf, free := pool.Get(1 << 20)
			if buf == nil {
				t.Fatal("Get вернул nil")
			}
			frees = append(frees, free)
		}
		for _, free := range frees {
			free()
		}
	}
	if pool.Busy() != 0 {
		t.Errorf("после циклов Busy = %d, ожидается 0", pool.Busy())
	}
	if idle := pool.Idle(); idle > 2 {
		t.Errorf("Idle = %d, больше maxIdle 2", idle)
	}
	if got := runtime.NumGoroutine(); got != goroutinesBefore {
		t.Errorf("число горутин изменилось: %d -> %d", goroutinesBefore, got)
	}
}
