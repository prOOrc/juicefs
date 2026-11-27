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

// Bench-обвязка M2 (design.md D11, задача 3.5; юнит-часть этапа 7.2):
// верхняя граница конвейера гидрации на fake backend (данные в памяти,
// Pread — memcpy) и fake store (TransferData без копирования — только
// счётчик байт; стоимость тестовой обвязки в замер не входит). Полный путь
// handleFetchData: индекс → Open → порции fetchBlock → буфер пула
// (VirtualAlloc, вне Go-кучи) → TransferData. Живой замер (S3-GET, ≥1 GiB,
// N=1/8/64) — задача 3.5 (живая часть — оркестратор).
//
// Размеры заданий: 128 KiB — диапазон фильтра по пробе U4; 4 MiB —
// fetchBlock (верхняя граница порции); параллель 8/64 — модель очереди
// FETCH_DATA при потоковой гидрации; MB/s печатает тестовый фреймворк
// (SetBytes).
package cfprovider

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// benchTransferStore — fakeStore с безкопировальным TransferData: байты
// считаются, данные не копируются (реальный фильтр читает VirtualAlloc-буфер
// синхронно внутри CfExecute — копий в замеряемом пути и так нет).
type benchTransferStore struct {
	fakeStore
	bytes atomic.Int64
	calls atomic.Int64
}

func (s *benchTransferStore) TransferData(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, offset, length int64, buf []byte) error {
	s.calls.Add(1)
	s.bytes.Add(length)
	return nil
}

// newBenchFetchProvider — провайдер M2-бенчей (testing.TB — бенчи и
// M3-тест): fakeFetchBackend с одним файлом big.bin в памяти,
// безкопировальный store, реальный диспетчер (Complete), индекс с записью
// fileID → путь. Пул буферов — Provider.bufs.
func newBenchFetchProvider(tb testing.TB, fileSize int64, fileID uint64) (*Provider, *benchTransferStore, *cfapi.Dispatcher) {
	tb.Helper()
	idx, err := NewIndex(tb.TempDir())
	if err != nil {
		tb.Fatalf("NewIndex: %v", err)
	}
	tb.Cleanup(func() { _ = idx.Close() })
	store := &benchTransferStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	backend := &fakeFetchBackend{files: map[string][]byte{"/big.bin": patternBytes(int(fileSize))}}
	d := cfapi.NewDispatcher(cfapi.DefaultQueueOptions())
	p := New(tb.TempDir(), "fetch-bench", nil, d)
	p.idx = idx
	p.store = store
	p.fbackend = backend
	if err := idx.Put(fileID, "/big.bin"); err != nil {
		tb.Fatalf("idx.Put: %v", err)
	}
	return p, store, d
}

// checkFetchBench — общая после-проверка M2-бенчей: все задания получили
// ровно один ответ с ожидаемым числом байт, буферы пула возвращены.
func checkFetchBench(tb testing.TB, p *Provider, store *benchTransferStore, wantCalls, wantBytes int64) {
	tb.Helper()
	if busy := p.bufs.Busy(); busy != 0 {
		tb.Fatalf("buffer leak: %d busy", busy)
	}
	if done := store.calls.Load(); done != wantCalls {
		tb.Fatalf("answered %d of %d jobs", done, wantCalls)
	}
	if got := store.bytes.Load(); got != wantBytes {
		tb.Fatalf("transferred %d bytes, want %d", got, wantBytes)
	}
}

// benchmarkFetch — сериальный прогон: b.N заданий FETCH_DATA по jobSize байт
// (offset циклически по файлу), каждое — полный handleFetchData.
func benchmarkFetch(b *testing.B, jobSize int64) {
	const fileSize = 64 << 20 // 64 MiB памяти достаточно для цикла offset'ов
	const fileID = 5000
	p, store, _ := newBenchFetchProvider(b, fileSize, fileID)
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(jobSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := int64(i) * jobSize % (fileSize - jobSize)
		handleFetchData(ctx, p, fetchJob(fileID, off, jobSize))
	}
	b.StopTimer()
	checkFetchBench(b, p, store, int64(b.N), int64(b.N)*jobSize)
}

// BenchmarkFetchBench128KiB — задание размера фильтра по пробе U4 (128 KiB):
// ns/op = латентность одного FETCH_DATA, MB/s = эквивалент потока.
func BenchmarkFetchBench128KiB(b *testing.B) {
	benchmarkFetch(b, 128<<10)
}

// BenchmarkFetchBench4MiB — задание размером порции чтения backend
// (fetchBlock): один Pread на задание.
func BenchmarkFetchBench4MiB(b *testing.B) {
	benchmarkFetch(b, int64(fetchBlock))
}

// benchmarkFetchParallel — модель потоковой гидрации при фиксированном
// числе параллельных заданий (M2: N=8/64): workers горутин разбирают
// b.N заданий из канала, каждое — полный handleFetchData.
func benchmarkFetchParallel(b *testing.B, workers int, jobSize int64) {
	const fileSize = 64 << 20
	const fileID = 6000
	p, store, _ := newBenchFetchProvider(b, fileSize, fileID)
	ctx := context.Background()
	jobs := make(chan int64, workers*2)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				off := i * jobSize % (fileSize - jobSize)
				handleFetchData(ctx, p, fetchJob(fileID, off, jobSize))
			}
		}()
	}
	b.ReportAllocs()
	b.SetBytes(jobSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		jobs <- int64(i)
	}
	close(jobs)
	wg.Wait()
	b.StopTimer()
	checkFetchBench(b, p, store, int64(b.N), int64(b.N)*jobSize)
}

// BenchmarkFetchBenchParallel8 — 8 параллельных заданий (M2 N=8).
func BenchmarkFetchBenchParallel8(b *testing.B) {
	benchmarkFetchParallel(b, 8, 128<<10)
}

// BenchmarkFetchBenchParallel64 — 64 параллельных задания (M2 N=64).
func BenchmarkFetchBenchParallel64(b *testing.B) {
	benchmarkFetchParallel(b, 64, 128<<10)
}

// BenchmarkFetchBenchPipeline — юнит-верхняя граница полного пула гидрации
// через очередь диспетчера (путь thunk'а: OnFetchData → воркеры fetchWorker,
// как в serve): продюсер не переполняет очередь (пауза при близкой к полной —
// иначе задание ушло бы в overflow с отменным ответом); измеряется
// устойчивая пропускная способность пула (включая добор последнего задания).
// Ключи заданий уникальны — как у реального фильтра (RequestKey различает
// конкурентные FETCH_DATA, design.md D7).
func BenchmarkFetchBenchPipeline(b *testing.B) {
	const fileSize = 64 << 20
	const fileID = 7000
	p, store, d := newBenchFetchProvider(b, fileSize, fileID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	for i := 0; i < DefaultFetchWorkers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.fetchWorker(ctx, d, stop)
		}()
	}
	const jobSize = 128 << 10
	capacity := int64(d.QueueCapacity().FetchData)
	b.ReportAllocs()
	b.SetBytes(jobSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Backpressure без переполнения: не дальше capacity-64 в очереди.
		for d.Enqueued()-store.calls.Load() >= capacity-64 {
			time.Sleep(200 * time.Microsecond)
		}
		off := int64(i) * jobSize % (fileSize - jobSize)
		job := &cfapi.FetchDataJob{
			ConnectionKey:  cfapi.ConnectionKey{Internal: 1},
			TransferKey:    cfapi.TransferKey(1_000_000 + i),
			RequestKey:     cfapi.RequestKey(2_000_000 + i),
			FileID:         fileID,
			RequiredOffset: off,
			RequiredLength: jobSize,
		}
		if err := d.OnFetchData(job); err != nil {
			b.Fatalf("OnFetchData: %v", err)
		}
	}
	// Добор хвоста очереди — в замере (устойчивая пропускная способность).
	deadline := time.Now().Add(60 * time.Second)
	for store.calls.Load() < int64(b.N) && time.Now().Before(deadline) {
		time.Sleep(500 * time.Microsecond)
	}
	b.StopTimer()
	cancel() // воркеры выходят по ctx (очередь уже пуста)
	p.wg.Wait()
	checkFetchBench(b, p, store, int64(b.N), int64(b.N)*jobSize)
}

// BenchmarkFetchBenchBigFile — юнит-репетиция живого M2 (задача 3.5):
// непрерывное последовательное чтение файла 1 GiB заданиями по 128 KiB
// через полный handleFetchData. b.N задаётся явно (-benchtime=3x).
func BenchmarkFetchBenchBigFile(b *testing.B) {
	const fileSize = 1 << 30 // 1 GiB
	const fileID = 8000
	p, store, _ := newBenchFetchProvider(b, fileSize, fileID)
	ctx := context.Background()
	const jobSize = 128 << 10
	const jobsPerPass = (1 << 30) / jobSize
	b.ReportAllocs()
	b.SetBytes(jobsPerPass * jobSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := int64(0); j < jobsPerPass; j++ {
			handleFetchData(ctx, p, fetchJob(fileID, j*jobSize, jobSize))
		}
	}
	b.StopTimer()
	checkFetchBench(b, p, store, int64(b.N)*jobsPerPass, int64(b.N)*jobsPerPass*jobSize)
}
