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

// Bench-обвязка M2/M3 (design.md D11; юнит-часть этапа 7.2): цикл Get/Put
// пула буферов TRANSFER_DATA. Буферы — VirtualAlloc вне Go-кучи: allocs/op
// показывает только стоимость обвязки вызова (release-closure), сами данные
// GC не сканируются. M3-вывод «allocation rate hot path ≈ 0 после прогрева»
// — по allocs/op этого бенча после первых итераций.
package cfapi

import (
	"sync/atomic"
	"testing"
)

// BenchmarkBufferPoolCycle — сериальный цикл Get(128 KiB)/release (размер
// диапазона фильтра по пробе U4). После прогрева VirtualAlloc не вызывается —
// ns/op = выдача/возврат буфера, allocs/op = release-closure (константа).
func BenchmarkBufferPoolCycle(b *testing.B) {
	p := NewBufferPool(8)
	defer p.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, release := p.Get(128 << 10)
		if buf == nil {
			b.Fatal("Get: nil buffer")
		}
		release()
	}
	b.StopTimer()
	if busy := p.Busy(); busy != 0 {
		b.Fatalf("buffer leak: %d busy", busy)
	}
}

// BenchmarkBufferPoolCycleParallel — параллельный Get/Put (пул гидрации
// при параллельной гидрации, M2 N=8/64): масштабирование блокировки пула.
func BenchmarkBufferPoolCycleParallel(b *testing.B) {
	p := NewBufferPool(64)
	defer p.Close()
	var gotNil atomic.Bool
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf, release := p.Get(128 << 10)
			if buf == nil {
				gotNil.Store(true)
				return
			}
			release()
		}
	})
	b.StopTimer()
	if gotNil.Load() {
		b.Fatal("Get: nil buffer")
	}
	if busy := p.Busy(); busy != 0 {
		b.Fatalf("buffer leak: %d busy", busy)
	}
}

// BenchmarkBufferPoolSizes — чередование размеров (128 KiB и 4 MiB):
// best-fit поиск по idle не вырождается на смешанной нагрузке.
func BenchmarkBufferPoolSizes(b *testing.B) {
	p := NewBufferPool(8)
	defer p.Close()
	sizes := []int{128 << 10, 4 << 20}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, release := p.Get(sizes[i%len(sizes)])
		if buf == nil {
			b.Fatal("Get: nil buffer")
		}
		release()
	}
	b.StopTimer()
	if busy := p.Busy(); busy != 0 {
		b.Fatalf("buffer leak: %d busy", busy)
	}
}
