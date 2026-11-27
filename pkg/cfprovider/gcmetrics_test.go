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

// M3-скрипт-тест (design.md D11, задача 7.8; юнит-часть этапа 7.2):
// прогон рабочих нагрузок WalkBench+FetchBench со снапшотами runtime/metrics
// до/после — частота GC, паузы (включая число пауз > 10 мс — целевое
// «отсутствуют»), heap, allocation rate. Требует CLAUDDrive_M3=1: без неё
// обычные прогоны тестов остаются быстрыми (t.Skip).
//
// Запуск: CLAUDDrive_M3=1 go test -run TestM3GCMetrics -v ./pkg/cfprovider
// Живой профиль serve (pprof :6060, GODEBUG=gctrace=1) — задача 7.8
// (живая часть — оркестратор).
package cfprovider

import (
	"context"
	"os"
	"runtime/metrics"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// m3Sample — снапшот метрик M3 (кумулятивные значения runtime/metrics).
type m3Sample struct {
	pauses     *metrics.Float64Histogram // /gc/pauses:seconds
	gcCycles   uint64                    // /gc/cycles/total:gc-cycles
	allocBytes uint64                    // /gc/heap/allocs:bytes
	allocObjs  uint64                    // /gc/heap/allocs:objects
	liveBytes  uint64                    // /gc/heap/live:bytes
}

func m3Take() m3Sample {
	s := m3Sample{}
	samples := []metrics.Sample{
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/gc/heap/allocs:bytes"},
		{Name: "/gc/heap/allocs:objects"},
		{Name: "/gc/heap/live:bytes"},
		{Name: "/gc/pauses:seconds"},
	}
	metrics.Read(samples)
	s.gcCycles = samples[0].Value.Uint64()
	s.allocBytes = samples[1].Value.Uint64()
	s.allocObjs = samples[2].Value.Uint64()
	s.liveBytes = samples[3].Value.Uint64()
	s.pauses = samples[4].Value.Float64Histogram() // свежие срезы от Read — копировать не нужно
	return s
}

// histogramTotal — полное число наблюдений в гистограмме runtime/metrics
// (Counts[i] — счёт ТОЛЬКО своего бакета (Buckets[i], Buckets[i+1]],
// гистограммы не кумулятивные).
func histogramTotal(h *metrics.Float64Histogram) uint64 {
	var total uint64
	for _, c := range h.Counts {
		total += c
	}
	return total
}

// pausesOverThresh — число GC-пауз дольше thresh секунд: сумма бакетов,
// чья НИЖНЯЯ граница ≥ thresh (бакет целиком выше порога; бакет, содержащий
// сам порог, не считается — занижение в пределах ширины бакета ~10мс).
func pausesOverThresh(h *metrics.Float64Histogram, thresh float64) uint64 {
	var total uint64
	for i, c := range h.Counts {
		if h.Buckets[i] >= thresh {
			total += c
		}
	}
	return total
}

// TestM3GCMetrics — юнит-часть M3: walk (20k записей, свежий корень) +
// гидрация (256 MiB заданиями по 128 KiB) между снапшотами; вывод — строка
// для гейт-отчёта D11. Утверждение о пуле: Busy()==0 после нагрузок
// (аналог инварианта циклов 7.5 в юните).
func TestM3GCMetrics(t *testing.T) {
	if os.Getenv("CLAUDDrive_M3") == "" {
		t.Skip("CLAUDDrive_M3 не задан — M3-прогон опционален (см. комментарий файла)")
	}
	const walkN = 20_000
	const fetchBytes = 256 << 20
	const jobSize = 128 << 10

	before := m3Take()
	started := time.Now()

	// Walk-нагрузка (примитив «обработать каталог», свежий корень).
	entries := benchWalkEntries(walkN, 3_000_000)
	backend := &fakeBackend{dirs: map[string][]BackendEntry{"/": entries}}
	root, err := os.MkdirTemp("", "m3walk-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	store := &benchWalkStore{fakeStore{local: map[string]bool{}, localReal: map[string]bool{}, standard: map[string]*cfapi.CFPlaceholderStandardInfo{}}}
	p, idx := newBenchWalkProvider(t, root, backend, store, fakeLocal{&store.fakeStore})
	defer func() { _ = idx.Close() }()
	res := runWalkDirectory(context.Background(), p, "/", entries)
	if res.createdFiles != walkN {
		t.Fatalf("walk result unexpected: %+v", res)
	}

	// Fetch-нагрузка: тот же провайдер не годится (нет fbackend) — отдельный
	// M2-провайдер с буферным пулом того же типа.
	fp, fstore, _ := newBenchFetchProvider(t, fetchBytes, 9000)
	const jobs = 256 << 20 / (128 << 10)
	for j := 0; j < jobs; j++ {
		handleFetchData(context.Background(), fp, fetchJob(9000, int64(j)*jobSize, jobSize))
	}
	checkFetchBench(t, fp, fstore, jobs, fetchBytes)
	if busy := p.bufs.Busy(); busy != 0 {
		t.Fatalf("walk provider buffer leak: %d busy", busy)
	}

	elapsed := time.Since(started)
	after := m3Take()

	dCycles := after.gcCycles - before.gcCycles
	dBytes := after.allocBytes - before.allocBytes
	dObjs := after.allocObjs - before.allocObjs
	pauses := int64(histogramTotal(after.pauses)) - int64(histogramTotal(before.pauses))
	over10ms := int64(pausesOverThresh(after.pauses, 0.010)) - int64(pausesOverThresh(before.pauses, 0.010))
	t.Logf("M3 юнит-часть: elapsed=%s walk_entries=%d fetch_bytes=%d", elapsed.Truncate(time.Millisecond), walkN, int64(fetchBytes))
	t.Logf("M3 юнит-часть: GC cycles=%d pauses=%d pauses>10ms=%d", dCycles, pauses, over10ms)
	t.Logf("M3 юнит-часть: heap allocs=%d bytes (%.1f MiB/s), objects=%d, live heap=%d bytes",
		dBytes, float64(dBytes)/elapsed.Seconds()/(1<<20), dObjs, after.liveBytes)
}
