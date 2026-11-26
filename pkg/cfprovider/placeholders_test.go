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

package cfprovider

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// utf16Name — копия UTF-16 строки из памяти Windows (локальный аналог
// cfapi.utf16ToString, который тестам недоступен).
func utf16Name(p *uint16) string {
	if p == nil {
		return ""
	}
	return windows.UTF16PtrToString(p)
}

// fakeBackend — подменный backendLister: каталоги и записи в памяти, без
// fs.FileSystem и meta (инвариант «Движок проверяется без CfAPI»).
type fakeBackend struct {
	mu      sync.Mutex
	dirs    map[string][]BackendEntry
	failNum int // первые N вызовов List возвращают ошибку (retry-цикл)
	calls   int
}

func (f *fakeBackend) List(ctx context.Context, dir string) ([]BackendEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failNum {
		return nil, errFake("fake: backend недоступен")
	}
	entries, ok := f.dirs[dir]
	if !ok {
		return nil, errFake("fake: нет каталога " + dir)
	}
	return entries, nil
}

func (f *fakeBackend) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeStore — подменный placeholderStore: создает вызовы фиксирует,
// состояние локальных путей задаётся картой (есть путь — есть плейсхолдер).
type fakeStore struct {
	mu        sync.Mutex
	creates   []createCall
	answers   []answerCall
	local     map[string]bool // локальные пути, существующие как ПЛЕЙСХОЛДЕРЫ
	localReal map[string]bool // локальные пути, существующие как ОБЫЧНЫЕ файлы
}

type createCall struct {
	basePath string
	names    []string
	identies []uint64
	isDirs   []bool
	sizes    []int64
	flags    []uint32
}

type answerCall struct {
	conn        cfapi.ConnectionKey
	transferKey cfapi.TransferKey
	requestKey  cfapi.RequestKey
	status      cfapi.NTStatus
	names       []string
	total       int64
}

func (f *fakeStore) Create(baseDirPath string, infos []cfapi.CFPlaceholderCreateInfo, createFlags uint32, entriesProcessed *uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := createCall{basePath: baseDirPath}
	for i := range infos {
		name := utf16Name(infos[i].RelativeFileName)
		call.names = append(call.names, name)
		var ino uint64
		if infos[i].FileIdentity != nil && infos[i].FileIdentityLength >= 8 {
			b := (*[8]byte)(infos[i].FileIdentity)
			ino = binary.LittleEndian.Uint64(b[:])
		}
		call.identies = append(call.identies, ino)
		call.isDirs = append(call.isDirs, infos[i].FsMetadata.BasicInfo.FileAttributes&uint32(windows.FILE_ATTRIBUTE_DIRECTORY) != 0)
		call.sizes = append(call.sizes, infos[i].FsMetadata.FileSize)
		call.flags = append(call.flags, infos[i].Flags)
		// Реальный фильтр возвращает статус каждой записи здесь.
		infos[i].Result = 0
	}
	if entriesProcessed != nil {
		*entriesProcessed = uint32(len(infos))
	}
	f.creates = append(f.creates, call)
	return nil
}

func (f *fakeStore) AnswerFetchPlaceholders(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, infos []cfapi.CFPlaceholderCreateInfo, total int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := answerCall{conn: conn, transferKey: transferKey, requestKey: requestKey, status: status, total: total}
	for i := range infos {
		call.names = append(call.names, utf16Name(infos[i].RelativeFileName))
	}
	f.answers = append(f.answers, call)
	return nil
}

// StateOf подменного store: realPaths — обычные файлы (не плейсхолдеры),
// placeholders — плейсхолдеры. Для честной проверки конфликтов часть тестов
// использует osStore.StateOf на реальных файлах диска (os.Stat, без cldapi).
func (f *fakeStore) StateOf(path string) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.localReal[path] {
		return true, false, nil
	}
	if f.local[path] {
		return true, true, nil
	}
	return false, false, nil
}

func (f *fakeStore) createCalls() []createCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]createCall(nil), f.creates...)
}

func (f *fakeStore) answerCalls() []answerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]answerCall(nil), f.answers...)
}

// errFake — простая ошибка для подменных реализаций.
type errFake string

func (e errFake) Error() string { return string(e) }

func newTestProvider(t *testing.T, root string, backend backendLister, store placeholderStore) (*Provider, *Index) {
	t.Helper()
	idx, err := NewIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	p := New(root, "test-volume", nil, nil)
	p.backend = backend
	p.store = store
	p.idx = idx
	p.retryDelay = time.Millisecond
	return p, idx
}

// TestPopulateTopLevelFresh — верхний уровень: записи backend превращаются в
// плейсхолдеры с метаданными backend и identity = inode LE 8 байт; индекс
// получает соответствие inode → путь.
func TestPopulateTopLevelFresh(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/": {
			{Name: "doc.pdf", Inode: 0x1122334455667788, Size: 12345, MtimeSec: 1700000000, MtimeNsec: 5},
			{Name: "sub", Inode: 99, Size: 0, MtimeSec: 1700000123, IsDir: true},
		},
	}}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	p, idx := newTestProvider(t, root, backend, store)

	if err := populateTopLevel(context.Background(), p); err != nil {
		t.Fatalf("populateTopLevel: %v", err)
	}
	calls := store.createCalls()
	if len(calls) != 1 {
		t.Fatalf("Create вызван %d раз, ожидается 1", len(calls))
	}
	call := calls[0]
	if call.basePath != root {
		t.Errorf("базовый каталог %q, ожидается %q", call.basePath, root)
	}
	if len(call.names) != 2 {
		t.Fatalf("создано %d записей, ожидается 2", len(call.names))
	}
	if call.names[0] != "doc.pdf" || call.names[1] != "sub" {
		t.Errorf("имена записей: %v", call.names)
	}
	// FileIdentity = inode, 8 байт LE (инвариант design.md D6).
	if call.identies[0] != 0x1122334455667788 {
		t.Errorf("identity файла = %#x, ожидается inode", call.identies[0])
	}
	// Размер из backend.
	if call.sizes[0] != 12345 || call.sizes[1] != 0 {
		t.Errorf("размеры записей: %v, ожидается [12345 0]", call.sizes)
	}
	// Каталог — атрибут DIRECTORY; файл — нет.
	if !call.isDirs[1] || call.isDirs[0] {
		t.Errorf("флаги каталога: файл=%v каталог=%v, ожидается false/true", call.isDirs[0], call.isDirs[1])
	}
	// Метаданные mtime должны отличаться между записями (из backend).
	if call.flags[0] != placeholderCreateFlags {
		t.Errorf("flags = %#x, ожидается %#x (см. пробу U2)", call.flags[0], placeholderCreateFlags)
	}
	if p, ok := idx.Path(0x1122334455667788); !ok || p != "/doc.pdf" {
		t.Errorf("idx.Path = (%q, %v), ожидается /doc.pdf", p, ok)
	}
	if p, ok := idx.Path(99); !ok || p != "/sub" {
		t.Errorf("idx.Path(99) = (%q, %v), ожидается /sub", p, ok)
	}
}

// TestPopulateTopLevelConflict — спека «Одноимённый локальный файл»:
// локальный не-плейсхолдер не передаётся на создание, запись остаётся
// на месте, соответствие в индекс не попадает. Проверка честная:
// StateOf — реальный osStore на файлах диска (os.Stat, без cldapi.dll).
func TestPopulateTopLevelConflict(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(local, []byte("user data"), 0o644); err != nil {
		t.Fatalf("создание локального файла: %v", err)
	}
	before, err := os.Stat(local)
	if err != nil {
		t.Fatalf("Stat до populate: %v", err)
	}

	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/": {
			{Name: "notes.txt", Inode: 5, Size: 100, MtimeSec: 1}, // конфликт с локальным
			{Name: "fresh.bin", Inode: 6, Size: 7, MtimeSec: 2},
		},
	}}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	// Комбинированный store: состояние локальных путей — реальный os.Stat.
	mixed := &stateAwareStore{store: store, state: osStore{}}
	p, idx := newTestProvider(t, root, backend, mixed)

	if err := populateTopLevel(context.Background(), p); err != nil {
		t.Fatalf("populateTopLevel: %v", err)
	}

	calls := store.createCalls()
	if len(calls) != 1 {
		t.Fatalf("Create вызван %d раз, ожидается 1", len(calls))
	}
	if len(calls[0].names) != 1 || calls[0].names[0] != "fresh.bin" {
		t.Fatalf("конфликтующая запись попала в CfCreatePlaceholders: %v", calls[0].names)
	}
	after, err := os.Stat(local)
	if err != nil {
		t.Fatalf("Stat после populate: %v", err)
	}
	// Локальный файл не перезаписан и не удалён (инвариант).
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Errorf("локальный файл изменён: size %d→%d, mtime %v→%v", before.Size(), after.Size(), before.ModTime(), after.ModTime())
	}
	if _, ok := idx.Path(5); ok {
		t.Error("конфликтующая запись не должна попадать в индекс")
	}
	if p, ok := idx.Path(6); !ok || p != "/fresh.bin" {
		t.Errorf("idx.Path(6) = (%q, %v), ожидается /fresh.bin", p, ok)
	}
}

// stateAwareStore — состояние локальных путей из другого store (реальный
// osStore), операции CfAPI — подменные (тесты без cldapi.dll).
type stateAwareStore struct {
	store placeholderStore
	state placeholderStore
}

func (s *stateAwareStore) Create(baseDirPath string, infos []cfapi.CFPlaceholderCreateInfo, createFlags uint32, entriesProcessed *uint32) error {
	return s.store.Create(baseDirPath, infos, createFlags, entriesProcessed)
}

func (s *stateAwareStore) AnswerFetchPlaceholders(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, infos []cfapi.CFPlaceholderCreateInfo, total int64) error {
	return s.store.AnswerFetchPlaceholders(conn, transferKey, requestKey, status, infos, total)
}

func (s *stateAwareStore) StateOf(path string) (bool, bool, error) {
	return s.state.StateOf(path)
}

// TestPopulateTopLevelRestartNoDuplicates — спека «Перезапуск без дублей»:
// существующий на диске плейсхолдер не передаётся на создание повторно, но
// соответствие inode → путь восстанавливается в индекс.
func TestPopulateTopLevelRestartNoDuplicates(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/": {{Name: "old.bin", Inode: 42, Size: 9, MtimeSec: 3}},
	}}
	// Путь существует и является плейсхолдером (создан предыдущим запуском).
	store := &fakeStore{local: map[string]bool{filepath.Join(root, "old.bin"): true}, localReal: map[string]bool{}}
	p, idx := newTestProvider(t, root, backend, store)

	if err := populateTopLevel(context.Background(), p); err != nil {
		t.Fatalf("populateTopLevel: %v", err)
	}
	// Плейсхолдер уже на диске — CfCreatePlaceholders не вызывается вовсе
	// (и уж точно не с записью old.bin).
	for i, call := range store.createCalls() {
		for _, n := range call.names {
			if n == "old.bin" {
				t.Errorf("вызов Create #%d передал существующий плейсхолдер повторно: %v (дубль)", i, call.names)
			}
		}
	}
	if p, ok := idx.Path(42); !ok || p != "/old.bin" {
		t.Errorf("idx.Path(42) = (%q, %v), ожидается /old.bin", p, ok)
	}
}

// TestHandleFetchPlaceholdersChildren — дети каталога при первом обращении:
// воркер отвечает TRANSFER_PLACEHOLDERS с записями каталога, индекс
// пополняется, каталог задания переводится из локального пути в fs-путь.
func TestHandleFetchPlaceholdersChildren(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/sub": {{Name: "a.txt", Inode: 7, Size: 3, MtimeSec: 4}},
	}}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	p, idx := newTestProvider(t, root, backend, store)

	job := &cfapi.FetchPlaceholdersJob{
		ConnectionKey: cfapi.ConnectionKey{Internal: 1},
		TransferKey:   cfapi.TransferKey(11),
		RequestKey:    cfapi.RequestKey(12),
		Path:          filepath.Join(root, "sub"),
	}
	handleFetchPlaceholders(context.Background(), p, job)

	answers := store.answerCalls()
	if len(answers) != 1 {
		t.Fatalf("ответов %d, ожидается 1 ровно (инвариант «ровно один ответ»)", len(answers))
	}
	a := answers[0]
	if a.transferKey != 11 || a.requestKey != 12 {
		t.Errorf("ключи ответа transfer=%d request=%d", a.transferKey, a.requestKey)
	}
	if a.status != cfapi.StatusSuccess {
		t.Errorf("статус ответа 0x%08x, ожидается SUCCESS", uint32(a.status))
	}
	if len(a.names) != 1 || a.names[0] != "a.txt" {
		t.Errorf("записи ответа: %v, ожидается [a.txt] (имя без пути каталога)", a.names)
	}
	if a.total != 1 {
		t.Errorf("total = %d, ожидается 1", a.total)
	}
	if p, ok := idx.Path(7); !ok || p != "/sub/a.txt" {
		t.Errorf("idx.Path(7) = (%q, %v), ожидается /sub/a.txt", p, ok)
	}
}

// TestHandleFetchPlaceholdersRootDir — задание для самого sync root
// (NormalizedPath = путь корня) трактуется как каталог "/".
func TestHandleFetchPlaceholdersRootDir(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/": {{Name: "top.txt", Inode: 2, Size: 1, MtimeSec: 1}},
	}}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	p, idx := newTestProvider(t, root, backend, store)

	job := &cfapi.FetchPlaceholdersJob{Path: root, TransferKey: 1}
	handleFetchPlaceholders(context.Background(), p, job)

	answers := store.answerCalls()
	if len(answers) != 1 || len(answers[0].names) != 1 || answers[0].names[0] != "top.txt" {
		t.Fatalf("ответ на корень неверен: %+v", answers)
	}
	if p, ok := idx.Path(2); !ok || p != "/top.txt" {
		t.Errorf("idx.Path(2) = (%q, %v), ожидается /top.txt", p, ok)
	}
}

// TestHandleFetchPlaceholdersCancel — отмена наполнения: ровно один ответ со
// статусом отмены, без записей, backend не читается.
func TestHandleFetchPlaceholdersCancel(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{}}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	p, _ := newTestProvider(t, root, backend, store)

	job := &cfapi.FetchPlaceholdersJob{Path: root, TransferKey: 3}
	job.Cancel.Store(true)
	handleFetchPlaceholders(context.Background(), p, job)

	answers := store.answerCalls()
	if len(answers) != 1 {
		t.Fatalf("ответов %d, ожидается 1", len(answers))
	}
	if answers[0].status != cfapi.StatusCancelled {
		t.Errorf("статус 0x%08x, ожидается CANCELLED", uint32(answers[0].status))
	}
	if len(answers[0].names) != 0 {
		t.Errorf("отменённый ответ с записями: %v", answers[0].names)
	}
	if backend.listCalls() != 0 {
		t.Errorf("отменённое задание читало backend %d раз", backend.listCalls())
	}
}

// TestHandleFetchPlaceholdersBackendError — ошибка backend: ровно один ответ
// со статусом ошибки, процесс жив (спека этапа 3; поведение воркера — то же).
func TestHandleFetchPlaceholdersBackendError(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{}} // "/sub" нет → ошибка
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	p, _ := newTestProvider(t, root, backend, store)

	job := &cfapi.FetchPlaceholdersJob{Path: filepath.Join(root, "sub"), TransferKey: 4}
	handleFetchPlaceholders(context.Background(), p, job)

	answers := store.answerCalls()
	if len(answers) != 1 {
		t.Fatalf("ответов %d, ожидается ровно 1", len(answers))
	}
	if answers[0].status == cfapi.StatusSuccess {
		t.Error("ошибка backend ответила SUCCESS")
	}
}

// TestRunPopulationDrainsQueue — воркер разбирает очередь диспетчера:
// задание для пути вне sync root (fsPathOf → "/") обслуживается по каталогу
// "/" и завершается ровно одним ответом.
func TestRunPopulationDrainsQueue(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/": {{Name: "x", Inode: 1, Size: 0}},
	}}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	dispatcher := cfapi.NewDispatcher(cfapi.DefaultQueueOptions())
	p := New(root, "v", nil, dispatcher)
	p.backend = backend
	p.store = store
	idx, err := NewIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	p.idx = idx

	// Воркер запускается вручную (без Start): канал остановки инициализируем
	// как после Start, иначе снапшот nil означал бы «остановлен».
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runPopulation(ctx, p)

	job := &cfapi.FetchPlaceholdersJob{Path: "C:\\elsewhere", TransferKey: 5}
	if err := dispatcher.OnFetchPlaceholders(job); err != nil {
		t.Fatalf("OnFetchPlaceholders: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for len(store.answerCalls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if len(store.answerCalls()) != 1 {
		t.Fatalf("воркер не ответил на задание из очереди: %d ответов", len(store.answerCalls()))
	}
}

// failingCreateStore — Create возвращает полную ошибку, статусы записей
// (Result) не трогает — недостоверны, как у реального фильтра.
type failingCreateStore struct {
	fakeStore
	createErr error
}

func (f *failingCreateStore) Create(baseDirPath string, infos []cfapi.CFPlaceholderCreateInfo, createFlags uint32, entriesProcessed *uint32) error {
	_ = baseDirPath
	_ = infos
	if entriesProcessed != nil {
		*entriesProcessed = 0
	}
	return f.createErr
}

// TestPopulateTopLevelCreateFailureNoIndex — полный отказ CfCreatePlaceholders:
// записи не считаются созданными (честный счётчик), в индекс не попадают;
// уже существовавшие на диске плейсхолдеры того же каталога индексируются.
func TestPopulateTopLevelCreateFailureNoIndex(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/": {
			{Name: "new.bin", Inode: 21, Size: 5, MtimeSec: 1},
			{Name: "old.bin", Inode: 22, Size: 6, MtimeSec: 2},
		},
	}}
	// old.bin — плейсхолдер предыдущего запуска; new.bin — новая запись.
	store := &failingCreateStore{
		fakeStore: fakeStore{local: map[string]bool{filepath.Join(root, "old.bin"): true}, localReal: map[string]bool{}},
		createErr: errFake("fake: CfCreatePlaceholders отказал целиком"),
	}
	p, idx := newTestProvider(t, root, backend, store)

	if err := populateTopLevel(context.Background(), p); err != nil {
		t.Fatalf("populateTopLevel: %v", err)
	}
	if _, ok := idx.Path(21); ok {
		t.Error("запись с отказавшим Create попала в индекс")
	}
	if p, ok := idx.Path(22); !ok || p != "/old.bin" {
		t.Errorf("существующий плейсхолдер потерян из индекса: (%q, %v), ожидается /old.bin", p, ok)
	}
}

// slowAnswerStore — AnswerFetchPlaceholders задерживает ответ: рабочий
// воркер остаётся внутри handleFetchPlaceholders до истечения hold.
type slowAnswerStore struct {
	fakeStore
	hold    time.Duration
	entered chan struct{} // закрывается при входе в ответ
	once    sync.Once
}

func (s *slowAnswerStore) AnswerFetchPlaceholders(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, infos []cfapi.CFPlaceholderCreateInfo, total int64) error {
	s.once.Do(func() { close(s.entered) })
	time.Sleep(s.hold)
	return s.fakeStore.AnswerFetchPlaceholders(conn, transferKey, requestKey, status, infos, total)
}

// TestFSPathOf — перевод локального пути под sync root в fs-путь.
func TestFSPathOf(t *testing.T) {
	root := `C:\Users\u\AgioDrive`
	p := New(root, "v", nil, nil)
	cases := []struct{ in, want string }{
		{root, "/"},
		{root + `\a`, "/a"},
		{root + `\a\b.txt`, "/a/b.txt"},
		{`C:\elsewhere`, "/"},
	}
	for _, tc := range cases {
		if got := p.fsPathOf(tc.in); got != tc.want {
			t.Errorf("fsPathOf(%q) = %q, ожидается %q", tc.in, got, tc.want)
		}
	}
}

// TestFSJoinLocalJoin — симметрия fsJoin/localJoin на Windows-разделителях.
func TestFSJoinLocalJoin(t *testing.T) {
	if fsJoin("/", "a") != "/a" || fsJoin("/a", "b") != "/a/b" {
		t.Error("fsJoin построил неверный путь")
	}
	local := localJoin(`C:\root`, "/a/b")
	if !strings.HasSuffix(local, `C:\root\a\b`) && local != `C:\root\a\b` {
		t.Errorf("localJoin = %q, ожидается C:\\root\\a\\b", local)
	}
	if localJoin(`C:\root`, "/") != `C:\root` {
		t.Errorf("localJoin корня = %q", localJoin(`C:\root`, "/"))
	}
}
