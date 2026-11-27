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

// Тесты фонового walk (design.md D6, такт 2; задачи 2.7–2.11): логика
// проверяется на подменных lister/store без cldapi.dll (инвариант
// «Движок проверяется без CfAPI») — каждый каталог один раз, дети после
// родителя, cancel, идемпотентность, сбои листинга.
package cfprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// simStore — «диск + фильтр» в памяти для walk-тестов: успешная конверсия
// (fakeStore.Convert) добавляет плейсхолдер в общую карту, ListNames читает
// её — повторный walk видит созданное первым (проверка идемпотентности,
// задача 2.9).
type simStore struct {
	fakeStore
}

func (s *simStore) listNames(localDir string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var names []string
	for _, m := range []map[string]bool{s.local, s.localReal} {
		for p := range m {
			if filepath.Dir(p) == localDir {
				n := filepath.Base(p)
				if !seen[n] {
					seen[n] = true
					names = append(names, n)
				}
			}
		}
	}
	return names, nil
}

// mapLocal — localLister поверх карт simStore: перечисление локального
// каталога синхронно с созданием плейсхолдеров.
type mapLocal struct{ s *simStore }

func (m mapLocal) ListNames(localDir string) ([]string, error) {
	return m.s.listNames(localDir)
}

// walkRecordingBackend — fakeBackend с записью последовательности листинга
// (порядок визитов, число вызовов на каталог) и перманентными отказами по
// каталогам (failDirs).
type walkRecordingBackend struct {
	fakeBackend
	mu       sync.Mutex
	seq      map[string]int   // каталог → порядковый номер последнего вызова
	calls    map[string]int   // каталог → число вызовов List
	failDirs map[string]error // перманентная ошибка листинга каталога
	counter  int
}

func (r *walkRecordingBackend) List(ctx context.Context, dir string) ([]BackendEntry, error) {
	r.mu.Lock()
	r.counter++
	r.seq[dir] = r.counter
	r.calls[dir]++
	ferr := r.failDirs[dir]
	r.mu.Unlock()
	if ferr != nil {
		return nil, ferr
	}
	return r.fakeBackend.List(ctx, dir)
}

func (r *walkRecordingBackend) callCount(dir string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[dir]
}

func (r *walkRecordingBackend) visitSeq(dir string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq[dir]
}

// blockingDirBackend — List указанного каталога блокируется до release или
// отмены (симуляция каталога «в работе»).
type blockingDirBackend struct {
	backend backendLister
	dir     string
	release chan struct{}

	mu      sync.Mutex
	entered int
}

func (b *blockingDirBackend) List(ctx context.Context, dir string) ([]BackendEntry, error) {
	if dir == b.dir {
		b.mu.Lock()
		b.entered++
		b.mu.Unlock()
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return b.backend.List(ctx, dir)
}

func (b *blockingDirBackend) enteredCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.entered
}

// newWalkTestProvider — провайдер walk-тестов: подменные lister/store и
// локальное перечисление, сокращённые интервалы (быстрые retry/прогресс).
func newWalkTestProvider(t *testing.T, root string, backend backendLister, store placeholderStore, local localLister) (*Provider, *Index) {
	t.Helper()
	p, idx := newTestProvider(t, root, backend, store)
	p.local = local
	p.walkRetryDelay = time.Millisecond
	p.walkRetryMax = 20 * time.Millisecond
	p.walkProgressInterval = 20 * time.Millisecond
	return p, idx
}

// newRecordingBackend — backend walk-тестов поверх дерева walkTree.
func newRecordingBackend(dirs map[string][]BackendEntry) *walkRecordingBackend {
	return &walkRecordingBackend{
		fakeBackend: fakeBackend{dirs: dirs},
		seq:         map[string]int{},
		calls:       map[string]int{},
	}
}

// runWalkOnce запускает координатор walk как StartWalk, но без требования
// running — логика BFS на подменных реализациях. Блокируется до финальной
// строки (`walk complete:` / `walk cancelled:` пишутся до возврата).
func runWalkOnce(t *testing.T, p *Provider, initial []string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.runWalk(context.Background(), initial)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("walk не завершился за 10с")
	}
}

// journalLines — число строк журнала индекса (задача 2.9: повторный walk на
// неизменном hub не добавляет строк).
func journalLines(t *testing.T, idx *Index) int {
	t.Helper()
	b, err := os.ReadFile(idx.jPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	return strings.Count(string(b), "\n")
}

// mkdirPlaceholder создаёт каталог-плейсхолдер на реальном диске: walk
// спускается только в уже созданные плейсхолдеры-каталоги (design.md D6.3 —
// каталог обрабатывается после создания своего плейсхолдера); тесты, не
// проходящие populateTopLevel, готовят родителей сами.
func mkdirPlaceholder(t *testing.T, root, fsPath string) {
	t.Helper()
	if err := os.MkdirAll(localJoin(root, fsPath), 0o755); err != nil {
		t.Fatalf("mkdir placeholder %s: %v", fsPath, err)
	}
}

// walkTree — дерево для walk-тестов: "/" → a(dir), b(dir), f.txt;
// /a → c(dir); /a/c → g.bin; /b пуст.
func walkTree() map[string][]BackendEntry {
	return map[string][]BackendEntry{
		"/": {
			{Name: "a", Inode: 1, IsDir: true, MtimeSec: 1},
			{Name: "b", Inode: 2, IsDir: true, MtimeSec: 2},
			{Name: "f.txt", Inode: 3, Size: 5, MtimeSec: 3},
		},
		"/a":   {{Name: "c", Inode: 4, IsDir: true, MtimeSec: 4}},
		"/a/c": {{Name: "g.bin", Inode: 5, Size: 7, MtimeSec: 5}},
		"/b":   {},
	}
}

// TestWalkVisitsEachDirOnce — задача 2.7: каждый каталог посещён ровно один
// раз, дети после родителей, корень walk повторно не листит (такт 1 —
// populateTopLevel), финальная строка `walk complete:` ровно одна.
func TestWalkVisitsEachDirOnce(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(walkTree())
	store := &simStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	p, idx := newWalkTestProvider(t, root, backend, store, mapLocal{store})

	hook := &logCapture{}
	logger.AddHook(hook)

	// Боевая проводка: такт 1 (populateTopLevel) заселяет корень и
	// формирует стартовую очередь walk (design.md D6, шаги 4–5).
	if err := populateTopLevel(context.Background(), p); err != nil {
		t.Fatalf("populateTopLevel: %v", err)
	}
	initial := p.takeRootSubdirs()
	if len(initial) != 2 {
		t.Fatalf("стартовая очередь %v, ожидается [/a /b]", initial)
	}
	rootCalls := backend.callCount("/") // листинг корня — только такт 1
	runWalkOnce(t, p, initial)

	for _, dir := range []string{"/a", "/b", "/a/c"} {
		if n := backend.callCount(dir); n != 1 {
			t.Errorf("каталог %s листился %d раз, ожидается 1", dir, n)
		}
	}
	// Дети после родителя (BFS, design.md D6.3) при любом чередовании
	// воркеров: ребёнок попадает в очередь только от родителя.
	if backend.visitSeq("/a/c") <= backend.visitSeq("/a") {
		t.Errorf("потомок /a/c листился раньше родителя /a (%d <= %d)", backend.visitSeq("/a/c"), backend.visitSeq("/a"))
	}
	if n := backend.callCount("/"); n != rootCalls {
		t.Errorf("корень листился walk'ом повторно: %d → %d, ожидается без изменений (такт 1)", rootCalls, n)
	}
	// Все записи в индексе (путь по inode — инвариант FileIdentity).
	for ino, want := range map[uint64]string{
		1: "/a", 2: "/b", 3: "/f.txt", 4: "/a/c", 5: "/a/c/g.bin",
	} {
		if got, ok := idx.Path(ino); !ok || got != want {
			t.Errorf("idx.Path(%d) = (%q, %v), ожидается %s", ino, got, ok, want)
		}
	}
	if n := hook.count("walk complete:"); n != 1 {
		t.Errorf("строк `walk complete:` %d, ожидается ровно 1", n)
	}
	if n := hook.count("walk cancelled:"); n != 0 {
		t.Errorf("строк `walk cancelled:` %d, ожидается 0", n)
	}
}

// TestWalkCancel — задача 2.7: отмена (cancel + закрытие stop, как при
// остановке serve) — воркер дорабатывает текущий каталог, финальная строка
// `walk cancelled:` ровно одна, `walk complete:` нет.
func TestWalkCancel(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(walkTree())
	store := &simStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	p, _ := newWalkTestProvider(t, root, backend, store, mapLocal{store})

	// Канал остановки — как после Start (Stop закрывает его и зануляет).
	p.stopMu.Lock()
	p.stop = make(chan struct{})
	p.stopMu.Unlock()
	t.Cleanup(func() {
		p.stopMu.Lock()
		if p.stop != nil {
			close(p.stop)
			p.stop = nil
		}
		p.stopMu.Unlock()
	})

	// Листинг /b блокируется до release — каталог «в работе» на момент cancel.
	release := make(chan struct{})
	blocking := &blockingDirBackend{backend: backend, dir: "/b", release: release}
	p.backend = blocking

	hook := &logCapture{}
	logger.AddHook(hook)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.runWalk(ctx, []string{"/a", "/b"})
	}()

	// Ждём, пока воркер войдёт в блокированный листинг /b («в работе»).
	deadline := time.Now().Add(5 * time.Second)
	for blocking.enteredCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if blocking.enteredCount() == 0 {
		t.Fatal("воркер не начал обработку /b")
	}

	// Остановка serve: cancel + закрытие stop; затем «доработка» текущего
	// каталога (release).
	cancel()
	p.stopMu.Lock()
	close(p.stop)
	p.stop = nil
	p.stopMu.Unlock()
	close(release)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("walk не завершился после cancel")
	}
	if n := hook.count("walk cancelled:"); n != 1 {
		t.Errorf("строк `walk cancelled:` %d, ожидается ровно 1", n)
	}
	if n := hook.count("walk complete:"); n != 0 {
		t.Errorf("строк `walk complete:` %d, ожидается 0", n)
	}
}

// TestWalkIdempotentSecondRun — задача 2.9: повторный walk на неизменном
// hub — 0 созданий, 0 новых строк журнала, 0 Warn о конфликтах;
// existing-каталоги снова ставятся в очередь и обходятся.
func TestWalkIdempotentSecondRun(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(walkTree())
	store := &simStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	p, idx := newWalkTestProvider(t, root, backend, store, mapLocal{store})

	hook := &logCapture{}
	logger.AddHook(hook)

	// Боевая проводка: такт 1 формирует стартовую очередь (design.md D6).
	if err := populateTopLevel(context.Background(), p); err != nil {
		t.Fatalf("populateTopLevel: %v", err)
	}
	initial := p.takeRootSubdirs()
	runWalkOnce(t, p, initial)
	convertsAfterFirst := len(store.convertCalls())
	rowsAfterFirst := journalLines(t, idx)
	if rowsAfterFirst == 0 {
		t.Fatal("первый walk не оставил строк журнала (нечего сравнивать)")
	}

	// Повторный walk того же (неизменного) hub.
	runWalkOnce(t, p, initial)

	if n := len(store.convertCalls()); n != convertsAfterFirst {
		t.Errorf("повторный walk создал записи: конверсий %d после, %d до, ожидается без изменений", n, convertsAfterFirst)
	}
	if n := journalLines(t, idx); n != rowsAfterFirst {
		t.Errorf("повторный walk добавил строки журнала: %d → %d, ожидается без изменений", rowsAfterFirst, n)
	}
	if n := hook.count("walk conflict"); n != 0 {
		t.Errorf("повторный walk выдал Warn о конфликтах: %d, ожидается 0", n)
	}
	// existing-каталоги ставятся в очередь и обходятся снова (design.md D6.3).
	if n := backend.callCount("/a/c"); n != 2 {
		t.Errorf("existing-каталог /a/c листился %d раз за два walk, ожидается 2", n)
	}
	if n := hook.count("walk complete:"); n != 2 {
		t.Errorf("строк `walk complete:` %d, ожидается 2 (по одной на walk)", n)
	}
}

// TestWalkConflictDirSkipsSubtree — задача 2.9 (и 2.4 для walk):
// конфликт-каталог (одноимённый локальный не-плейсхолдер, не в индексе) —
// один Warn на родительский каталог (формат design.md D6.8), конфликт-каталог
// и его поддерево не обходятся, в индекс не попадают.
func TestWalkConflictDirSkipsSubtree(t *testing.T) {
	root := t.TempDir()
	dirs := walkTree()
	dirs["/a"] = append(dirs["/a"], BackendEntry{Name: "locked", Inode: 10, IsDir: true, MtimeSec: 6})
	dirs["/a/locked"] = []BackendEntry{{Name: "inner", Inode: 11, Size: 1, MtimeSec: 7}}
	backend := newRecordingBackend(dirs)
	store := &simStore{fakeStore: fakeStore{
		local:     map[string]bool{},
		localReal: map[string]bool{filepath.Join(root, "a", "locked"): true}, // локальный не-плейсхолдер
	}}
	p, idx := newWalkTestProvider(t, root, backend, store, mapLocal{store})

	hook := &logCapture{}
	logger.AddHook(hook)

	mkdirPlaceholder(t, root, "/a") // walk спускается только в созданные плейсхолдеры-каталоги
	runWalkOnce(t, p, []string{"/a"})

	if n := backend.callCount("/a/locked"); n != 0 {
		t.Errorf("поддерево конфликт-каталога обходится: /a/locked листился %d раз, ожидается 0", n)
	}
	if _, ok := idx.Path(11); ok {
		t.Error("запись из поддерева конфликт-каталога попала в индекс")
	}
	if _, ok := idx.Path(10); ok {
		t.Error("конфликт-каталог попал в индекс")
	}
	want := "walk conflict: /a: skipped 1 hub entries, e.g. [locked]"
	if n := hook.count(want); n != 1 {
		t.Errorf("строк %q: %d, ожидается ровно 1 (один Warn на каталог)", want, n)
	}
	// Остальное поддерево /a достроено.
	if got, ok := idx.Path(5); !ok || got != "/a/c/g.bin" {
		t.Errorf("idx.Path(5) = (%q, %v), ожидается /a/c/g.bin", got, ok)
	}
	if n := hook.count("walk complete:"); n != 1 {
		t.Errorf("строк `walk complete:` %d, ожидается 1", n)
	}
}

// TestWalkConflictFileEntry — конфликт-файл внутри обходимого каталога:
// skip, имя в Warn каталога, в индекс не попадает (инвариант: локальный
// файл не перезаписывается), соседние записи создаются.
func TestWalkConflictFileEntry(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(map[string][]BackendEntry{
		"/a": {
			{Name: "notes.txt", Inode: 20, Size: 3, MtimeSec: 1}, // конфликт
			{Name: "fresh.bin", Inode: 21, Size: 4, MtimeSec: 2},
		},
	})
	store := &simStore{fakeStore: fakeStore{
		local:     map[string]bool{},
		localReal: map[string]bool{filepath.Join(root, "a", "notes.txt"): true},
	}}
	p, idx := newWalkTestProvider(t, root, backend, store, mapLocal{store})

	hook := &logCapture{}
	logger.AddHook(hook)

	mkdirPlaceholder(t, root, "/a") // walk спускается только в созданные плейсхолдеры-каталоги
	runWalkOnce(t, p, []string{"/a"})

	if _, ok := idx.Path(20); ok {
		t.Error("конфликт-файл попал в индекс")
	}
	if got, ok := idx.Path(21); !ok || got != "/a/fresh.bin" {
		t.Errorf("idx.Path(21) = (%q, %v), ожидается /a/fresh.bin", got, ok)
	}
	want := "walk conflict: /a: skipped 1 hub entries, e.g. [notes.txt]"
	if n := hook.count(want); n != 1 {
		t.Errorf("строк %q: %d, ожидается ровно 1", want, n)
	}
}

// TestWalkEmptyInitialQueue — пустая стартовая очередь (верхний уровень без
// каталогов) — `walk complete:` сразу, ровно одна.
func TestWalkEmptyInitialQueue(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(walkTree())
	store := &simStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	p, _ := newWalkTestProvider(t, root, backend, store, mapLocal{store})

	hook := &logCapture{}
	logger.AddHook(hook)

	runWalkOnce(t, p, []string{})

	if n := hook.count("walk complete:"); n != 1 {
		t.Errorf("строк `walk complete:` %d, ожидается 1", n)
	}
	if n := backend.callCount("/a"); n != 0 {
		t.Errorf("из пустой очереди walk листил каталоги: %d", n)
	}
}

// TestWalkTransientListErrorRetriesThenComplete — задача 2.10: транзиентные
// ошибки листинга — retry backoff без ограничения, `walk complete:` только
// после успеха; все листья обработаны.
func TestWalkTransientListErrorRetriesThenComplete(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(walkTree())
	backend.fakeBackend.failNum = 6 // первые 6 вызовов List — отказ
	store := &simStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	p, idx := newWalkTestProvider(t, root, backend, store, mapLocal{store})

	hook := &logCapture{}
	logger.AddHook(hook)

	mkdirPlaceholder(t, root, "/a")
	runWalkOnce(t, p, []string{"/a", "/b"})

	if got, ok := idx.Path(5); !ok || got != "/a/c/g.bin" {
		t.Errorf("idx.Path(5) = (%q, %v), ожидается /a/c/g.bin (после восстановления листинга)", got, ok)
	}
	if n := backend.callCount("/a"); n < 1 {
		t.Errorf("каталог /a не листился успешно (вызовов %d)", n)
	}
	if n := hook.count("walk complete:"); n != 1 {
		t.Errorf("строк `walk complete:` %d, ожидается ровно 1 (только после успеха)", n)
	}
	if n := hook.count("walk cancelled:"); n != 0 {
		t.Errorf("строк `walk cancelled:` %d, ожидается 0", n)
	}
	if n := hook.count("walk failed:"); n != 0 {
		t.Errorf("транзиентные ошибки не должны давать `walk failed:`: %d", n)
	}
}

// TestWalkPermanentListErrorSkipsSubtree — задача 2.10: перманентная ошибка
// листинга (аналог 4xx) — Warn `walk failed: <dir>: %v (subtree skipped)`,
// поддерево пропускается, остальные ветки достраиваются, `walk complete:`
// выводится.
func TestWalkPermanentListErrorSkipsSubtree(t *testing.T) {
	root := t.TempDir()
	dirs := walkTree()
	dirs["/"] = append(dirs["/"], BackendEntry{Name: "bad", Inode: 30, IsDir: true, MtimeSec: 8})
	dirs["/bad"] = []BackendEntry{{Name: "hidden", Inode: 31, Size: 1, MtimeSec: 9}}
	backend := newRecordingBackend(dirs)
	backend.failDirs = map[string]error{"/bad": &permanentListError{errFake("fake: 404 gone")}}
	store := &simStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	p, idx := newWalkTestProvider(t, root, backend, store, mapLocal{store})

	hook := &logCapture{}
	logger.AddHook(hook)

	mkdirPlaceholder(t, root, "/a")
	runWalkOnce(t, p, []string{"/bad", "/a"})

	want := "walk failed: /bad: fake: 404 gone (subtree skipped)"
	if n := hook.count(want); n != 1 {
		t.Errorf("строк %q: %d, ожидается ровно 1", want, n)
	}
	if n := backend.callCount("/bad"); n != 1 {
		t.Errorf("перманентный отказ ретраится: /bad листился %d раз, ожидается 1 (без повторов)", n)
	}
	if n := backend.callCount("/bad/hidden"); n != 0 {
		t.Errorf("поддерево перманентно отказавшего каталога обходится: %d вызовов, ожидается 0", n)
	}
	if _, ok := idx.Path(31); ok {
		t.Error("запись пропущенного поддерева попала в индекс")
	}
	if got, ok := idx.Path(5); !ok || got != "/a/c/g.bin" {
		t.Errorf("idx.Path(5) = (%q, %v), ожидается /a/c/g.bin (остальные ветки достроены)", got, ok)
	}
	if n := hook.count("walk complete:"); n != 1 {
		t.Errorf("строк `walk complete:` %d, ожидается 1 (перманентный отказ не блокирует walk)", n)
	}
}

// TestClassifyListError — классификация ошибок листинга (design.md D6.6):
// перманентные — явный маркер и errno-аналоги 4xx; сеть/прочее —
// транзиентные (retry без ограничения).
func TestClassifyListError(t *testing.T) {
	if classifyListError(&permanentListError{errFake("x")}) != listPermanent {
		t.Error("permanentListError не распознан как перманентная")
	}
	if classifyListError(errFake("fake: backend недоступен")) != listTransient {
		t.Error("ошибка без классификации должна быть транзиентной (перманентные только 4xx)")
	}
	if classifyListError(fmt.Errorf("open /x: %w", syscall.ENOENT)) != listPermanent {
		t.Error("ENOENT должен быть перманентным (аналог 404)")
	}
	if classifyListError(errors.New("connection refused")) != listTransient {
		t.Error("сетевая ошибка должна быть транзиентной")
	}
}

// convertFailStore — convert-примитив отказывает для выбранных путей
// (симуляция «отказа конверсии записи», design.md D6.4): реальная запись
// остаётся не сконвертированной — createEntry обязан её удалить (откат).
type convertFailStore struct {
	fakeStore
	failPaths map[string]error
}

func (s *convertFailStore) Convert(path string, isDir bool, identity []byte, flags uint32) error {
	if err, ok := s.failPaths[path]; ok {
		return err
	}
	return s.fakeStore.Convert(path, isDir, identity, flags)
}

// dehydrateRejectStore — convert с DEHYDRATE отвергается, convert
// (MARK_IN_SYNC) проходит — fallback через UpdatePlaceholder (design.md
// D6.4, вариант A fallback).
type dehydrateRejectStore struct {
	fakeStore
}

func (s *dehydrateRejectStore) Convert(path string, isDir bool, identity []byte, flags uint32) error {
	if flags&cfapi.ConvertFlagDehydrate != 0 {
		return errFake("fake: convert(DEHYDRATE) rejected 0x8007017c")
	}
	return s.fakeStore.Convert(path, isDir, identity, flags)
}

// hidingLocal — localLister, скрывающий имя из перечисления (симуляция
// гонки: запись появилась между перечислением каталога и созданием —
// design.md D6.4 «отказ уже существует»).
type hidingLocal struct {
	inner localLister
	hide  string
}

func (h hidingLocal) ListNames(localDir string) ([]string, error) {
	names, err := h.inner.ListNames(localDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if !strings.EqualFold(n, h.hide) {
			out = append(out, n)
		}
	}
	return out, nil
}

// TestWalkCreatesEntriesSequentially — convert-примитив поштучный
// (design.md D6.4): каждая запись hub — ровно одна конверсия, записи
// каталога обрабатываются последовательно в порядке backend (один воркер
// на каталог), все доведены до конечного состояния и в индексе.
func TestWalkCreatesEntriesSequentially(t *testing.T) {
	root := t.TempDir()
	entries := make([]BackendEntry, 0, 100)
	for i := 0; i < 100; i++ {
		entries = append(entries, BackendEntry{Name: fmt.Sprintf("f%03d.bin", i), Inode: uint64(600 + i), Size: int64(i), MtimeSec: 1})
	}
	backend := newRecordingBackend(map[string][]BackendEntry{"/bulk": entries})
	store := &simStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	p, idx := newWalkTestProvider(t, root, backend, store, mapLocal{store})
	hook := &logCapture{}
	logger.AddHook(hook)

	mkdirPlaceholder(t, root, "/bulk")
	runWalkOnce(t, p, []string{"/bulk"})

	converts := store.convertCalls()
	if len(converts) != len(entries) {
		t.Fatalf("конверсий %d, ожидается %d (поштучно, по одной на запись)", len(converts), len(entries))
	}
	for i, c := range converts {
		if c.ino != uint64(600+i) || c.path != filepath.Join(root, "bulk", fmt.Sprintf("f%03d.bin", i)) || c.isDir {
			t.Errorf("конверсия #%d: %+v, ожидается f%03d.bin (последовательный порядок backend)", i, c, i)
		}
		if c.flags != cfapi.ConvertFlagMarkInSync|cfapi.ConvertFlagDehydrate {
			t.Errorf("конверсия #%d: флаги %#x, ожидается MARK_IN_SYNC|DEHYDRATE", i, c.flags)
		}
		if got, ok := idx.Path(uint64(600 + i)); !ok || got != "/bulk/"+fmt.Sprintf("f%03d.bin", i) {
			t.Errorf("idx.Path(%d) = (%q, %v), ожидается /bulk/f%03d.bin", 600+i, got, ok, i)
		}
	}
	if n := hook.count("walk entry failed"); n != 0 {
		t.Errorf("поштучное создание дало `walk entry failed:`: %d", n)
	}
}

// TestWalkRollbackOnConvertFailure — откат при отказе конверсии (design.md
// D6.4, «создано но не сконвертировано» удаляется): сбойная запись
// отсутствует на диске и в индексе, соседи созданы, failed++ и групповой
// Warn на каталог.
func TestWalkRollbackOnConvertFailure(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(map[string][]BackendEntry{
		"/deep": {
			{Name: "ok1.bin", Inode: 111, Size: 1, MtimeSec: 1},
			{Name: "bad.bin", Inode: 112, Size: 2, MtimeSec: 2},
			{Name: "ok2.bin", Inode: 113, Size: 3, MtimeSec: 3},
		},
	})
	store := &convertFailStore{
		fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}},
		failPaths: map[string]error{filepath.Join(root, "deep", "bad.bin"): errFake("fake: convert failed")},
	}
	p, idx := newWalkTestProvider(t, root, backend, store, fakeLocal{&store.fakeStore})
	hook := &logCapture{}
	logger.AddHook(hook)

	mkdirPlaceholder(t, root, "/deep")
	runWalkOnce(t, p, []string{"/deep"})

	for ino, want := range map[uint64]string{111: "/deep/ok1.bin", 113: "/deep/ok2.bin"} {
		if got, ok := idx.Path(ino); !ok || got != want {
			t.Errorf("idx.Path(%d) = (%q, %v), ожидается %s", ino, got, ok, want)
		}
	}
	if _, ok := idx.Path(112); ok {
		t.Error("сбойная конверсия попала в индекс")
	}
	// Откат: «создано но не сконвертировано» удалено; соседи на диске.
	if _, err := os.Stat(filepath.Join(root, "deep", "bad.bin")); !os.IsNotExist(err) {
		t.Errorf("несконвертированная запись не удалена: stat err = %v", err)
	}
	for _, n := range []string{"ok1.bin", "ok2.bin"} {
		if _, err := os.Stat(filepath.Join(root, "deep", n)); err != nil {
			t.Errorf("сосед %s не создан: %v", n, err)
		}
	}
	want := "walk entry failed: /deep: skipped 1, e.g. [bad.bin]"
	if n := hook.count(want); n != 1 {
		t.Errorf("строк %q: %d, ожидается ровно 1 (групповой Warn на каталог)", want, n)
	}
}

// TestWalkInvalidNameSkipped — Windows-запрещённый символ в имени записи
// backend ('|'): os-создание отказано, записи не осталось, failed++,
// остальные записи созданы.
func TestWalkInvalidNameSkipped(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(map[string][]BackendEntry{
		"/odd": {
			{Name: "good1.bin", Inode: 121, Size: 1, MtimeSec: 1},
			{Name: "bad|name.bin", Inode: 122, Size: 2, MtimeSec: 2}, // запрещён на Windows
			{Name: "good2.bin", Inode: 123, Size: 3, MtimeSec: 3},
		},
	})
	store := &simStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	p, idx := newWalkTestProvider(t, root, backend, store, mapLocal{store})
	hook := &logCapture{}
	logger.AddHook(hook)

	mkdirPlaceholder(t, root, "/odd")
	runWalkOnce(t, p, []string{"/odd"})

	for ino, want := range map[uint64]string{121: "/odd/good1.bin", 123: "/odd/good2.bin"} {
		if got, ok := idx.Path(ino); !ok || got != want {
			t.Errorf("idx.Path(%d) = (%q, %v), ожидается %s", ino, got, ok, want)
		}
	}
	if _, ok := idx.Path(122); ok {
		t.Error("запись с запрещённым именем попала в индекс")
	}
	// На диске только соседние записи — мусора от отказавшей нет.
	des, err := os.ReadDir(filepath.Join(root, "odd"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	if len(names) != 2 {
		t.Errorf("на диске %v, ожидается [good1.bin good2.bin] (запись не осталась)", names)
	}
	want := "walk entry failed: /odd: skipped 1, e.g. [bad|name.bin]"
	if n := hook.count(want); n != 1 {
		t.Errorf("строк %q: %d, ожидается ровно 1", want, n)
	}
}

// TestWalkFallbackUpdateOnDehydrateReject — fallback design.md D6.4: отказ
// convert(MARK_IN_SYNC|DEHYDRATE) → convert(MARK_IN_SYNC) →
// UpdatePlaceholder(FsMetadata, DEHYDRATE|MARK_IN_SYNC); запись доведена до
// конечного состояния (в индексе), откат не нужен.
func TestWalkFallbackUpdateOnDehydrateReject(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(map[string][]BackendEntry{
		"/fb": {{Name: "only.bin", Inode: 141, Size: 9, MtimeSec: 4}},
	})
	store := &dehydrateRejectStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	p, idx := newWalkTestProvider(t, root, backend, store, fakeLocal{&store.fakeStore})
	hook := &logCapture{}
	logger.AddHook(hook)

	mkdirPlaceholder(t, root, "/fb")
	runWalkOnce(t, p, []string{"/fb"})

	if got, ok := idx.Path(141); !ok || got != "/fb/only.bin" {
		t.Fatalf("idx.Path(141) = (%q, %v), ожидается /fb/only.bin", got, ok)
	}
	// Записана одна конверсия — прошедшая MARK_IN_SYNC (отвергнутый вызов
	// с DEHYDRATE до записи не дошёл: подменный store отказывает до
	// фиксации); плюс одна дегидратация с метаданными fallback'а.
	converts := store.convertCalls()
	if len(converts) != 1 || converts[0].flags != cfapi.ConvertFlagMarkInSync {
		t.Fatalf("конверсии %v, ожидается ровно одна MARK_IN_SYNC (повтор после DEHYDRATE-отказа)", converts)
	}
	updates := store.updateCalls()
	if len(updates) != 1 {
		t.Fatalf("обновлений %d, ожидается 1 (fallback UpdatePlaceholder)", len(updates))
	}
	u := updates[0]
	if u.path != filepath.Join(root, "fb", "only.bin") || u.size != 9 {
		t.Errorf("fallback: %+v, ожидается only.bin size 9", u)
	}
	if u.flags != cfapi.UpdateFlagDehydrate|cfapi.UpdateFlagMarkInSync {
		t.Errorf("флаги fallback = %#x, ожидается DEHYDRATE|MARK_IN_SYNC", u.flags)
	}
	if n := hook.count("fallback convert+update"); n != 1 {
		t.Errorf("строк пробы fallback: %d, ожидается ровно 1", n)
	}
	if n := hook.count("walk entry failed"); n != 0 {
		t.Errorf("fallback дал отказ записи: %d", n)
	}
}

// TestWalkExistsReclassifyRestore — CREATE_NEW-отказ на нашем плейсхолдере
// (гонка: запись появилась после перечисления — design.md D6.4
// «переклассификация»): existing — восстановление в индекс, не ошибка и не
// создание.
func TestWalkExistsReclassifyRestore(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(map[string][]BackendEntry{
		"/racedir": {{Name: "raced.bin", Inode: 131, Size: 1, MtimeSec: 1}},
	})
	// Реальная запись на диске (её создал параллельный процесс) и
	// «плейсхолдер» в модели store; из перечисления имя скрыто.
	mkdirPlaceholder(t, root, "/racedir")
	if err := os.WriteFile(filepath.Join(root, "racedir", "raced.bin"), []byte{0, 0}, 0o644); err != nil {
		t.Fatalf("создание гонки: %v", err)
	}
	store := &simStore{fakeStore: fakeStore{
		local:     map[string]bool{filepath.Join(root, "racedir", "raced.bin"): true},
		localReal: map[string]bool{},
	}}
	p, idx := newWalkTestProvider(t, root, backend, store, hidingLocal{inner: mapLocal{store}, hide: "raced.bin"})
	hook := &logCapture{}
	logger.AddHook(hook)

	runWalkOnce(t, p, []string{"/racedir"})

	if got, ok := idx.Path(131); !ok || got != "/racedir/raced.bin" {
		t.Errorf("idx.Path(131) = (%q, %v), ожидается /racedir/raced.bin (переклассифицирован в existing)", got, ok)
	}
	if n := len(store.convertCalls()); n != 0 {
		t.Errorf("переклассификация сконвертировала запись: %d вызовов", n)
	}
	if n := hook.count("walk entry failed"); n != 0 {
		t.Errorf("EEXIST-переклассификация логируется как отказ: %d", n)
	}
	if n := hook.count("walk conflict"); n != 0 {
		t.Errorf("наш плейсхолдер классифицирован конфликтом: %d", n)
	}
}

// TestWalkExistsReclassifyConflict — CREATE_NEW-отказ на одноимённом
// локальном файле пользователя (не плейсхолдер, не в индексе):
// переклассификация в конфликт — файл не тронут, запись не в индексе.
func TestWalkExistsReclassifyConflict(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(map[string][]BackendEntry{
		"/confdir": {{Name: "clash.bin", Inode: 132, Size: 1, MtimeSec: 1}},
	})
	local := filepath.Join(root, "confdir", "clash.bin")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(local, []byte("user data"), 0o644); err != nil {
		t.Fatalf("создание локального файла: %v", err)
	}
	// Состояние — реальный os.Stat (osStore), CfAPI — подменные.
	store := &stateAwareStore{
		store: &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}},
		state: osStore{},
	}
	p, idx := newWalkTestProvider(t, root, backend, store, hidingLocal{inner: osLocalLister{}, hide: "clash.bin"})
	hook := &logCapture{}
	logger.AddHook(hook)

	runWalkOnce(t, p, []string{"/confdir"})

	if _, ok := idx.Path(132); ok {
		t.Error("конфликт попал в индекс")
	}
	if n := len(store.store.(*fakeStore).convertCalls()); n != 0 {
		t.Errorf("конфликт сконвертирован: %d вызовов", n)
	}
	want := "walk conflict: /confdir: skipped 1 hub entries, e.g. [clash.bin]"
	if n := hook.count(want); n != 1 {
		t.Errorf("строк %q: %d, ожидается ровно 1", want, n)
	}
}

// quarantineStore — Convert держит реальный файл открытым без
// FILE_SHARE_DELETE и отказывает: os.Remove при откате получает sharing
// violation — карантин (design.md D6.4: «удаление отказано (другой процесс
// открыл)»).
type quarantineStore struct {
	fakeStore
	holdPath string // единственная запись, чей файл «открыт другим процессом»
	held     windows.Handle
}

func (s *quarantineStore) Convert(path string, isDir bool, identity []byte, flags uint32) error {
	if path != s.holdPath {
		return s.fakeStore.Convert(path, isDir, identity, flags)
	}
	if s.held != 0 {
		// Повторный вызов (fallback convert): хендл уже держится.
		return errFake("fake: convert rejected")
	}
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p16, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	s.held = h // хендл без FILE_SHARE_DELETE — удаление при откате невозможно
	return errFake("fake: convert rejected")
}

// TestWalkQuarantineOnRollbackSharingViolation — сбойная запись, чей файл
// занят другим процессом, не удаляется при откате: `walk quarantine:` в
// логе, failed++, в индекс не попадает (на следующем walk это конфликт).
func TestWalkQuarantineOnRollbackSharingViolation(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(map[string][]BackendEntry{
		"/q": {
			{Name: "held.bin", Inode: 151, Size: 1, MtimeSec: 1},
			{Name: "free.bin", Inode: 152, Size: 2, MtimeSec: 2},
		},
	})
	store := &quarantineStore{
		fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}},
		holdPath:  filepath.Join(root, "q", "held.bin"),
	}
	p, idx := newWalkTestProvider(t, root, backend, store, fakeLocal{&store.fakeStore})
	hook := &logCapture{}
	logger.AddHook(hook)
	// Хендл держит «другой процесс» — закрываем в конце теста, чтобы
	// t.TempDir мог удалить дерево.
	t.Cleanup(func() {
		if store.held != 0 {
			_ = windows.CloseHandle(store.held)
			store.held = 0
		}
	})

	mkdirPlaceholder(t, root, "/q")
	runWalkOnce(t, p, []string{"/q"})

	if _, ok := idx.Path(151); ok {
		t.Error("карантинная запись попала в индекс")
	}
	if got, ok := idx.Path(152); !ok || got != "/q/free.bin" {
		t.Errorf("idx.Path(152) = (%q, %v), ожидается /q/free.bin", got, ok)
	}
	want := "walk quarantine: "
	if n := hook.count(want); n != 1 {
		t.Errorf("строк `walk quarantine:` %d, ожидается ровно 1 (held.bin занят)", n)
	}
	if n := hook.count("walk entry failed: /q: skipped 1"); n != 1 {
		t.Errorf("карантинная запись не учтена в failed: %d", n)
	}
}

// adoptFailStore — Update отказывает (нормализация усыновления невозможна).
type adoptFailStore struct {
	fakeStore
	failPath string
}

func (s *adoptFailStore) Update(path string, fsMetadata cfapi.CFFileMetadata, flags uint32) error {
	if path == s.failPath {
		return errFake("fake: dehydrate on adopt failed")
	}
	return s.fakeStore.Update(path, fsMetadata, flags)
}

// TestWalkAdoptionNormalizesHydratedPlaceholder — краш-окно fallback-
// дегидратации (design.md D6.4): плейсхолдер без индексной записи с данными
// на диске (OnDiskDataSize > 0 — convert прошёл, Update(DEHYDRATE) упал
// вместе с процессом) усыновляется только после повторной дегидратации
// Update(DEHYDRATE|MARK_IN_SYNC); отказ нормализации — откат записи
// (failed++, не в индексе), уже дегидратированный проходит без записи.
func TestWalkAdoptionNormalizesHydratedPlaceholder(t *testing.T) {
	// 1. Гидратированный плейсхолдер: усыновление с нормализацией.
	root := t.TempDir()
	backend := newRecordingBackend(map[string][]BackendEntry{
		"/adopt": {{Name: "lost.bin", Inode: 161, Size: 3, MtimeSec: 1}},
	})
	store := &fakeStore{
		local: map[string]bool{filepath.Join(root, "adopt", "lost.bin"): true},
		standard: map[string]*cfapi.CFPlaceholderStandardInfo{
			filepath.Join(root, "adopt", "lost.bin"): {OnDiskDataSize: 3}, // EOF-нули на диске
		},
	}
	p, idx := newWalkTestProvider(t, root, backend, store, fakeLocal{store})
	hook := &logCapture{}
	logger.AddHook(hook)

	mkdirPlaceholder(t, root, "/adopt")
	runWalkOnce(t, p, []string{"/adopt"})

	if got, ok := idx.Path(161); !ok || got != "/adopt/lost.bin" {
		t.Fatalf("idx.Path(161) = (%q, %v), ожидается /adopt/lost.bin (усыновлён после нормализации)", got, ok)
	}
	updates := store.updateCalls()
	if len(updates) != 1 {
		t.Fatalf("обновлений %d, ожидается 1 (повторная дегидратация при усыновлении)", len(updates))
	}
	if u := updates[0]; u.path != filepath.Join(root, "adopt", "lost.bin") || u.size != 3 ||
		u.flags != cfapi.UpdateFlagDehydrate|cfapi.UpdateFlagMarkInSync {
		t.Errorf("нормализация: %+v, ожидается lost.bin size 3 DEHYDRATE|MARK_IN_SYNC", u)
	}
	if n := len(store.convertCalls()); n != 0 {
		t.Errorf("усыновление переконвертировало запись: %d вызовов", n)
	}
	if n := hook.count("walk entry failed"); n != 0 {
		t.Errorf("нормализуемое усыновление логируется как отказ: %d", n)
	}

	// 2. Отказ нормализации: запись откатывается — failed++, не в индексе.
	root2 := t.TempDir()
	backend2 := newRecordingBackend(map[string][]BackendEntry{
		"/adopt": {{Name: "lost.bin", Inode: 161, Size: 3, MtimeSec: 1}},
	})
	store2 := &adoptFailStore{
		fakeStore: fakeStore{
			local: map[string]bool{filepath.Join(root2, "adopt", "lost.bin"): true},
			standard: map[string]*cfapi.CFPlaceholderStandardInfo{
				filepath.Join(root2, "adopt", "lost.bin"): {OnDiskDataSize: 3},
			},
		},
		failPath: filepath.Join(root2, "adopt", "lost.bin"),
	}
	p2, idx2 := newWalkTestProvider(t, root2, backend2, store2, fakeLocal{&store2.fakeStore})
	hook2 := &logCapture{}
	logger.AddHook(hook2)

	mkdirPlaceholder(t, root2, "/adopt")
	runWalkOnce(t, p2, []string{"/adopt"})

	if _, ok := idx2.Path(161); ok {
		t.Error("плейсхолдер с ненормализованными данными усыновлён")
	}
	want := "walk entry failed: /adopt: skipped 1, e.g. [lost.bin]"
	if n := hook2.count(want); n != 1 {
		t.Errorf("строк %q: %d, ожидается ровно 1 (отказ нормализации)", want, n)
	}
	if n := hook2.count("walk quarantine:"); n != 0 {
		t.Errorf("отсутствие файла на диске должно быть обычным сбоем, не карантином: %d", n)
	}
}

// TestWalkAdoptionDehydratedSkipsNormalization — обычное усыновление
// дегидратированного плейсхолдера (утерянный журнал при живом диске):
// без лишнего Update, прямо в индекс.
func TestWalkAdoptionDehydratedSkipsNormalization(t *testing.T) {
	root := t.TempDir()
	backend := newRecordingBackend(map[string][]BackendEntry{
		"/adopt": {{Name: "old.bin", Inode: 171, Size: 4, MtimeSec: 1}},
	})
	store := &fakeStore{
		local:     map[string]bool{filepath.Join(root, "adopt", "old.bin"): true},
		localReal: map[string]bool{},
	}
	p, idx := newWalkTestProvider(t, root, backend, store, fakeLocal{store})
	hook := &logCapture{}
	logger.AddHook(hook)

	mkdirPlaceholder(t, root, "/adopt")
	runWalkOnce(t, p, []string{"/adopt"})

	if got, ok := idx.Path(171); !ok || got != "/adopt/old.bin" {
		t.Fatalf("idx.Path(171) = (%q, %v), ожидается /adopt/old.bin", got, ok)
	}
	if n := len(store.updateCalls()); n != 0 {
		t.Errorf("дегидратированный плейсхолдер прошёл Update: %d вызовов", n)
	}
	if n := hook.count("walk entry failed"); n != 0 {
		t.Errorf("обычное усыновление логируется как отказ: %d", n)
	}
}
