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

// Bench-обвязка M1 (design.md D11, задачи 2.13/6.6; юнит-часть этапа 7.2):
// верхние границы движка walk и индекса на подменных backend/store/local
// (инвариант «Движок проверяется без CfAPI»). WalkBench — классификация +
// create-примитив (createEntry: реальные записи на диске, вариант A
// design.md D6.4; стоимость CfConvertToPlaceholder реального фильтра НЕ
// входит — fakeStore); WalkRestartBench — классификация повторного walk
// (рестарт без дублей, всё already-existing); IndexBench — Put/Path/ResolveIno
// на 100k записей (журнал — временный каталог).
//
// Живой замер с реальным фильтром и sync root — задача 6.6/2.13 (живая
// часть — оркестратор); эти числа — верхние границы для гейта D11.
package cfprovider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// benchWalkEntries — n записей-файлов одного каталога backend (M1: каталог
// с 100k записей); имена/размеры детерминированы, inode уникальны от base.
func benchWalkEntries(n int, baseIno uint64) []BackendEntry {
	entries := make([]BackendEntry, n)
	for i := 0; i < n; i++ {
		entries[i] = BackendEntry{
			Name:      fmt.Sprintf("frame_%06d.bin", i),
			Inode:     baseIno + uint64(i),
			Size:      4096,
			MtimeSec:  1700000000,
			MtimeNsec: 0,
			IsDir:     false,
		}
	}
	return entries
}

// benchWalkStore — fakeStore с честной моделью MARK_IN_SYNC: успешный
// Convert с флагом помечает запись in-sync (реальный фильтр после
// MARK_IN_SYNC-конверсии отвечает InSyncStateInSync на STANDARD-запрос).
// Без этого reconcileDir (вызов populateDirectory) ставил бы выгрузку на
// каждую запись — не путь M1 (наполнение/рестарт), а ложный after-crash
// сценарий; и istore бенча — этот store (в New стоит osStore с реальным
// cldapi.dll — в юнит-бенчах DLL не участвует).
type benchWalkStore struct {
	fakeStore
}

func (s *benchWalkStore) Convert(path string, isDir bool, identity []byte, flags uint32) error {
	if err := s.fakeStore.Convert(path, isDir, identity, flags); err != nil {
		return err
	}
	if flags&cfapi.ConvertFlagMarkInSync != 0 {
		_ = s.fakeStore.SetInSync(path)
	}
	return nil
}

// newBenchWalkProvider — провайдер walk-бенчей (аналог newWalkTestProvider
// для testing.TB — бенчи и M3-тест): подменные lister/store/local, индекс во
// временном каталоге, сокращённые retry-интервалы; istore — тот же store
// (классификация сверки без cldapi.dll, как в walk-тестах).
func newBenchWalkProvider(tb testing.TB, root string, backend backendLister, store placeholderStore, local localLister) (*Provider, *Index) {
	tb.Helper()
	idx, err := NewIndex(tb.TempDir())
	if err != nil {
		tb.Fatalf("NewIndex: %v", err)
	}
	p := New(root, "bench-volume", nil, nil)
	p.backend = backend
	p.store = store
	p.local = local
	p.idx = idx
	if s, ok := store.(interface {
		StandardInfo(string) (*cfapi.CFPlaceholderStandardInfo, error)
		SetInSync(string) error
		AnswerDelete(cfapi.ConnectionKey, cfapi.TransferKey, cfapi.RequestKey, uint32, cfapi.NTStatus) error
		AnswerRename(cfapi.ConnectionKey, cfapi.TransferKey, cfapi.RequestKey, uint32, cfapi.NTStatus) error
	}); ok {
		p.istore = s
	}
	p.walkRetryDelay = time.Millisecond
	p.walkRetryMax = 20 * time.Millisecond
	p.walkProgressInterval = 20 * time.Millisecond
	return p, idx
}

// runWalkDirectory — примитив «обработать каталог» над готовым списком
// записей (листинг fakeBackend уже не нужен — populateDirectory получает
// entries напрямую); общий для WalkBench/WalkRestartBench/M3.
func runWalkDirectory(ctx context.Context, p *Provider, dir string, entries []BackendEntry) *dirPopulation {
	return populateDirectory(ctx, p, dir, entries)
}

// BenchmarkWalkBench — M1 юнит-верхняя граница наполнения (задача 2.13):
// один каталог backend со 100k записей, свежий корень на каждую итерацию —
// классификация (локальное перечисление, сверка индекса) + create-примитив
// (реальные CREATE_NEW/Truncate/Chtimes/Close на диске + fakeStore.Convert)
// + индекс (журнал во временном каталоге) + сверка каталога (reconcileDir).
// СТОИМОСТЬ РЕАЛЬНОГО ФИЛЬТРА (CfConvertToPlaceholder) НЕ ВХОДИТ.
// Запуск: go test -bench BenchmarkWalkBench -benchmem -benchtime=1x ./pkg/cfprovider
func BenchmarkWalkBench(b *testing.B) {
	const n = 100_000
	entries := benchWalkEntries(n, 1_000_000)
	backend := &fakeBackend{dirs: map[string][]BackendEntry{"/": entries}}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		root, err := os.MkdirTemp("", "walkbench-")
		if err != nil {
			b.Fatalf("MkdirTemp: %v", err)
		}
		store := &benchWalkStore{fakeStore{local: map[string]bool{}, localReal: map[string]bool{}, standard: map[string]*cfapi.CFPlaceholderStandardInfo{}}}
		p, idx := newBenchWalkProvider(b, root, backend, store, fakeLocal{&store.fakeStore})
		res := runWalkDirectory(ctx, p, "/", entries)
		if res.createdFiles != n || res.failedEntries != 0 || res.conflicts != 0 {
			b.Fatalf("walk result unexpected: %+v", res)
		}
		b.StopTimer()
		_ = idx.Close()
		_ = os.RemoveAll(root)
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N*n)/b.Elapsed().Seconds(), "entries/s")
}

// BenchmarkWalkBenchParallel4 — та же нагрузка топологией живого walk
// (design.md D6.3: пул из 4 воркеров BFS): 100k записей в 4 каталогах
// обрабатываются 4 горутинами populateDirectory параллельно — верхняя
// граница с учётом реального параллелизма движка. Родительские каталоги
// создаются до замера (в живом walk их создаёт walk верхнего уровня).
func BenchmarkWalkBenchParallel4(b *testing.B) {
	const dirs = 4
	const perDir = 25_000
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		root, err := os.MkdirTemp("", "walkbenchp-")
		if err != nil {
			b.Fatalf("MkdirTemp: %v", err)
		}
		store := &benchWalkStore{fakeStore{local: map[string]bool{}, localReal: map[string]bool{}, standard: map[string]*cfapi.CFPlaceholderStandardInfo{}}}
		p, idx := newBenchWalkProvider(b, root, nil, store, fakeLocal{&store.fakeStore})
		entries := make(map[string][]BackendEntry, dirs)
		for d := 0; d < dirs; d++ {
			name := fmt.Sprintf("/d%d", d)
			if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("d%d", d)), 0o755); err != nil {
				b.Fatalf("Mkdir: %v", err)
			}
			entries[name] = benchWalkEntries(perDir, uint64(1_000_000+d*perDir))
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for d := 0; d < dirs; d++ {
			dir := fmt.Sprintf("/d%d", d)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				res := runWalkDirectory(ctx, p, dir, entries[dir])
				if res.createdFiles != perDir || res.failedEntries != 0 {
					b.Errorf("walk result unexpected for %s: %+v", dir, res)
				}
			}()
		}
		close(start)
		wg.Wait()
		b.StopTimer()
		_ = idx.Close()
		_ = os.RemoveAll(root)
		b.StartTimer()
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N*dirs*perDir)/b.Elapsed().Seconds(), "entries/s")
}

// BenchmarkWalkRestartBench — M1 юнит-верхняя граница рестарта (спека
// «Перезапуск без дублей», задача 2.9): все 100k записей уже на «диске»
// (fakeStore.local) и в индексе — повторный walk только классифицирует
// (skip без Warn, 0 create, 0 строк журнала). Диск-операций нет.
func BenchmarkWalkRestartBench(b *testing.B) {
	const n = 100_000
	entries := benchWalkEntries(n, 2_000_000)
	backend := &fakeBackend{dirs: map[string][]BackendEntry{"/": entries}}
	root := b.TempDir()
	store := &benchWalkStore{fakeStore{local: map[string]bool{}, localReal: map[string]bool{}, standard: map[string]*cfapi.CFPlaceholderStandardInfo{}}}
	p, idx := newBenchWalkProvider(b, root, backend, store, fakeLocal{&store.fakeStore})
	defer func() {
		_ = idx.Close()
	}()
	// Прогрев: первый walk создаёт индекс и «плейсхолдеры» — после него
	// каждая итерация измеряет чистую классификацию рестарта.
	warm := runWalkDirectory(context.Background(), p, "/", entries)
	if warm.createdFiles != n {
		b.Fatalf("warmup walk result unexpected: %+v", warm)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := runWalkDirectory(context.Background(), p, "/", entries)
		if res.createdFiles != 0 || res.skipped != n {
			b.Fatalf("restart walk result unexpected: %+v", res)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N*n)/b.Elapsed().Seconds(), "entries/s")
}

// BenchmarkIndexPut — юнит-стоимость одной записи индекса (Put: обе карты +
// append строки журнала; журнал — временный каталог). Пропускная способность
// walk ограничена снизу этой операцией (M1: idx.Put на каждую запись).
func BenchmarkIndexPut(b *testing.B) {
	idx, err := NewIndex(b.TempDir())
	if err != nil {
		b.Fatalf("NewIndex: %v", err)
	}
	defer func() {
		_ = idx.Close()
	}()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.Put(uint64(i), fmt.Sprintf("/frame_%06d.bin", i)); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
}

// BenchmarkIndexPath — юнит-стоимость lookup по inode (память, RLock):
// горячий путь readFetch перед ResolveIno (100k записей в индексе).
func BenchmarkIndexPath(b *testing.B) {
	const n = 100_000
	idx, err := NewIndex(b.TempDir())
	if err != nil {
		b.Fatalf("NewIndex: %v", err)
	}
	defer func() {
		_ = idx.Close()
	}()
	for i := 0; i < n; i++ {
		if err := idx.Put(uint64(i), fmt.Sprintf("/frame_%06d.bin", i)); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := idx.Path(uint64(i % n)); !ok {
			b.Fatalf("Path(%d): not found", i%n)
		}
	}
}

// BenchmarkIndexResolveIno — юнит-стоимость промаха индекса с подъёмом по
// родителям (U12): resolver подменный (возвращает путь), найденное кешируется
// Put'ом (строка журнала). Inode всегда новые — каждый op = полный путь
// resolver → кеш.
func BenchmarkIndexResolveIno(b *testing.B) {
	idx, err := NewIndex(b.TempDir())
	if err != nil {
		b.Fatalf("NewIndex: %v", err)
	}
	defer func() {
		_ = idx.Close()
	}()
	idx.SetResolver(func(ctx context.Context, ino uint64) (string, bool) {
		return fmt.Sprintf("/warm/%d.bin", ino), true
	})
	ctx := context.Background()
	base := uint64(1) << 40 // заведомо вне прогретых записей
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := idx.ResolveIno(ctx, base+uint64(i)); !ok {
			b.Fatalf("ResolveIno(%d): not resolved", base+uint64(i))
		}
	}
}
