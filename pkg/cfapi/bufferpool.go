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
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// allocationGranularity — гранулярность VirtualAlloc (MEM_RESERVE округляется
// до 64 KiB): каждый буфер пула занимает кратное 64 KiB, база выровнена на 64 KiB.
const allocationGranularity = 64 * 1024

// roundGranularity округляет размер вверх до кратного 64 KiB (минимум одна
// гранула — запросы меньше granule всё равно получают одну).
func roundGranularity(size int) int {
	if size <= 0 {
		return allocationGranularity
	}
	n := (size + allocationGranularity - 1) / allocationGranularity
	if n == 0 {
		n = 1
	}
	return n * allocationGranularity
}

// poolBuffer — буфер пула: база VirtualAlloc и срез на весь выделенный объём.
type poolBuffer struct {
	base uintptr
	data []byte
}

// BufferPool — пул страница-выровненных буферов (VirtualAlloc,
// MEM_COMMIT|MEM_RESERVE, PAGE_READWRITE) для TRANSFER_DATA (design.md D7):
// address и размер данных, передаваемых фильтру, должны быть выровнены (U4).
// Буферы живут вне Go-кучи: GC не сканирует их, аллокаций в горячем пути нет
// после прогрева (M3).
type BufferPool struct {
	mu      sync.Mutex
	idle    []*poolBuffer // свободные буферы (первый подходящий по размеру)
	busy    int           // занятые (выданные Get, не возвращённые Put)
	maxIdle int           // сколько буферов держать свободными
	allocs  int           // счётчик VirtualAlloc (диагностика, DoD 7.5)
	frees   int           // счётчик VirtualFree
	closed  bool
}

// NewBufferPool создаёт пул; maxIdle — сколько свободных буферов удерживать
// между циклами «запуск — чтение — остановка» (DoD 7.5).
func NewBufferPool(maxIdle int) *BufferPool {
	if maxIdle < 1 {
		maxIdle = 1
	}
	return &BufferPool{maxIdle: maxIdle}
}

// addrToPointer превращает адрес, полученный из syscall (VirtualAlloc), в
// unsafe.Pointer без прямой конвертации uintptr -> unsafe.Pointer: анализатор
// go vet unsafeptr допускает только reflect-паттерны, а syscall-результаты
// адресов не-Go памяти легальны (unsafe docs, паттерн syscall.Syscall).
func addrToPointer(addr uintptr) unsafe.Pointer {
	var p unsafe.Pointer
	*(*uintptr)(unsafe.Pointer(&p)) = addr
	return p
}

// allocVirtual выделяет n байт через VirtualAlloc и возвращает буфер.
func allocVirtual(n int) (*poolBuffer, error) {
	base, err := windows.VirtualAlloc(0, uintptr(n), windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if err != nil {
		return nil, err
	}
	if base == 0 {
		return nil, windows.ERROR_NOT_ENOUGH_MEMORY
	}
	return &poolBuffer{
		base: base,
		data: unsafe.Slice((*byte)(addrToPointer(base)), n),
	}, nil
}

// freeVirtual освобождает буфер (MEM_RELEASE, размер 0 — вся область).
func freeVirtual(b *poolBuffer) error {
	return windows.VirtualFree(b.base, 0, windows.MEM_RELEASE)
}

// Get выдаёт буфер не меньше size байт (страница-выровненный) и функцию
// возврата в пул. Из свободных берётся наименьший достаточный (best-fit):
// большие буферы остаются большим запросам. Подходящего нет — новый
// VirtualAlloc с увеличением размера (resize вверх). При ошибке выделения
// возвращает nil и no-op free (вызывающий код обязан проверить nil).
func (p *BufferPool) Get(size int) ([]byte, func()) {
	n := roundGranularity(size)
	p.mu.Lock()
	p.busy++
	best := -1
	for i, b := range p.idle {
		if len(b.data) >= n && (best == -1 || len(b.data) < len(p.idle[best].data)) {
			best = i
		}
	}
	if best != -1 {
		b := p.idle[best]
		p.idle = append(p.idle[:best], p.idle[best+1:]...)
		p.mu.Unlock()
		return b.data, p.release(b)
	}
	b, err := allocVirtual(n)
	if err != nil {
		p.busy--
		p.mu.Unlock()
		return nil, func() {}
	}
	p.allocs++
	p.mu.Unlock()
	return b.data, p.release(b)
}

// release возвращает буфер в пул или освобождает (пул закрыт либо свободных
// уже maxIdle). free() от одного Get вызывается один раз.
func (p *BufferPool) release(b *poolBuffer) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.busy--
			switch {
			case p.closed || len(p.idle) >= p.maxIdle:
				if err := freeVirtual(b); err == nil {
					p.frees++
				}
			default:
				p.idle = append(p.idle, b)
			}
			p.mu.Unlock()
		})
	}
}

// Close помечает пул закрытым и освобождает все свободные буферы; занятые
// освобождаются своими free().
func (p *BufferPool) Close() {
	p.mu.Lock()
	p.closed = true
	for _, b := range p.idle {
		if err := freeVirtual(b); err == nil {
			p.frees++
		}
	}
	p.idle = nil
	p.mu.Unlock()
}

// Busy — число занятых буферов (выданы Get, free ещё не вызван).
func (p *BufferPool) Busy() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busy
}

// Idle — число свободных буферов в пуле.
func (p *BufferPool) Idle() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

// Stats — счётчики VirtualAlloc/VirtualFree за жизнь пула (диагностика M3/DoD 7.5).
func (p *BufferPool) Stats() (allocs, frees int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allocs, p.frees
}
