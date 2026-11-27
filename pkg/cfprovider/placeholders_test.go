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

// fakeStore — подменный placeholderStore: convert/update вызовы фиксирует,
// состояние локальных путей задаётся картой (есть путь — есть плейсхолдер).
type fakeStore struct {
	mu        sync.Mutex
	converts  []convertCall
	updates   []updateCall
	answers   []answerCall
	transfers []transferCall
	local     map[string]bool // локальные пути, существующие как ПЛЕЙСХОЛДЕРЫ
	localReal map[string]bool // локальные пути, существующие как ОБЫЧНЫЕ файлы
	// standard — CF_PLACEHOLDER_STANDARD_INFO по путям (усыновление);
	// отсутствие записи — дегидратированный плейсхолдер (OnDiskDataSize 0).
	standard map[string]*cfapi.CFPlaceholderStandardInfo
	// answerResults — симуляция записи фильтром per-entry Result в наш
	// массив при обработке TRANSFER-ответа (имя записи → Result).
	answerResults map[string]cfapi.HRESULT
}

// convertCall — фиксация convert-примитива (design.md D6.4): путь, тип
// записи, identity (inode) и CF_CONVERT_FLAGS.
type convertCall struct {
	path  string
	isDir bool
	ino   uint64
	flags uint32
}

// updateCall — фиксация fallback-дегидратации (CfUpdatePlaceholder).
type updateCall struct {
	path          string
	size          int64
	flags         uint32
	lastWriteTime int64 // LastWriteTime из CF_FS_METADATA (проверка честных метаданных)
}

type answerCall struct {
	conn        cfapi.ConnectionKey
	transferKey cfapi.TransferKey
	requestKey  cfapi.RequestKey
	status      cfapi.NTStatus
	names       []string
	total       int64
	processed   uint32
}

// transferCall — фиксация ответов FETCH_DATA (гидрация, этап 3): статус,
// диапазон задания и копия данных (реальный буфер — VirtualAlloc вне
// Go-кучи, в тестах копируется).
type transferCall struct {
	conn        cfapi.ConnectionKey
	transferKey cfapi.TransferKey
	requestKey  cfapi.RequestKey
	status      cfapi.NTStatus
	offset      int64
	length      int64
	data        []byte
}

func (f *fakeStore) Convert(path string, isDir bool, identity []byte, flags uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ino uint64
	if len(identity) >= 8 {
		ino = binary.LittleEndian.Uint64(identity)
	}
	f.converts = append(f.converts, convertCall{path: path, isDir: isDir, ino: ino, flags: flags})
	// Модель реального фильтра: после успешной конверсии путь — плейсхолдер.
	f.local[path] = true
	return nil
}

func (f *fakeStore) Update(path string, fsMetadata cfapi.CFFileMetadata, flags uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, updateCall{path: path, size: fsMetadata.FileSize, flags: flags, lastWriteTime: fsMetadata.BasicInfo.LastWriteTime})
	return nil
}

func (f *fakeStore) AnswerFetchPlaceholders(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, infos []cfapi.CFPlaceholderCreateInfo, total int64, entriesProcessed uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := answerCall{conn: conn, transferKey: transferKey, requestKey: requestKey, status: status, total: total, processed: entriesProcessed}
	for i := range infos {
		name := utf16Name(infos[i].RelativeFileName)
		call.names = append(call.names, name)
		// Фильтр пишет per-entry Result в НАШ буфер при обработке ответа —
		// подменная реализация симулирует это тем же способом.
		if res, ok := f.answerResults[name]; ok {
			infos[i].Result = res
		}
	}
	f.answers = append(f.answers, call)
	return nil
}

// TransferData — фиксация ответов FETCH_DATA (гидрация, этап 3): данные
// копируются (реальный буфер — VirtualAlloc вне Go-кучи).
func (f *fakeStore) TransferData(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, offset, length int64, buf []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data := make([]byte, len(buf))
	copy(data, buf)
	f.transfers = append(f.transfers, transferCall{
		conn: conn, transferKey: transferKey, requestKey: requestKey,
		status: status, offset: offset, length: length, data: data,
	})
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

// StandardInfo подменного store: карта standard; по умолчанию —
// дегидратированный плейсхолдер (OnDiskDataSize = 0).
func (f *fakeStore) StandardInfo(path string) (*cfapi.CFPlaceholderStandardInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info, ok := f.standard[path]; ok {
		return info, nil
	}
	return &cfapi.CFPlaceholderStandardInfo{OnDiskDataSize: 0}, nil
}

// SetInSync — фиксация пометки in-sync (интерфейс ingestStore; сверка
// reconcileDir/queueUpload использует StandardInfo того же store — тесты
// walk/sверки остаются DLL-независимыми).
func (f *fakeStore) SetInSync(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.standard[path]
	if !ok {
		info = &cfapi.CFPlaceholderStandardInfo{}
		f.standard[path] = info
	}
	info.InSyncState = cfapi.InSyncStateInSync
	return nil
}

func (f *fakeStore) convertCalls() []convertCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]convertCall(nil), f.converts...)
}

func (f *fakeStore) updateCalls() []updateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]updateCall(nil), f.updates...)
}

func (f *fakeStore) answerCalls() []answerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]answerCall(nil), f.answers...)
}

func (f *fakeStore) transferCalls() []transferCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]transferCall(nil), f.transfers...)
}

// errFake — простая ошибка для подменных реализаций.
type errFake string

func (e errFake) Error() string { return string(e) }

// fakeLocal — localLister поверх карт fakeStore (плейсхолдеры + реальные
// файлы): локальное перечисление каталога без реального диска — для тестов,
// где «диск» изображён в fakeStore (классификация design.md D6.5).
type fakeLocal struct{ s *fakeStore }

func (f fakeLocal) ListNames(localDir string) ([]string, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	seen := map[string]bool{}
	var names []string
	for _, m := range []map[string]bool{f.s.local, f.s.localReal} {
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
	// istore = store: классификация состояния сверки (reconcileDir) и
	// ingestStore без обращения к cldapi.dll (инвариант «Движок проверяется
	// без CfAPI»; fakeStore/simStore реализуют StandardInfo + SetInSync).
	if s, ok := store.(interface {
		StandardInfo(string) (*cfapi.CFPlaceholderStandardInfo, error)
		SetInSync(string) error
	}); ok {
		p.istore = s
	}
	p.retryDelay = time.Millisecond
	return p, idx
}

// TestPopulateTopLevelFresh — верхний уровень: записи hub создаются
// convert-примитивом (design.md D6.4, задача 2.8) — реальная запись на диске
// (размер Truncate'ом, mtime Chtimes'ом) + конверсия с identity = inode
// LE 8 байт; файл конвертируется с DEHYDRATE|MARK_IN_SYNC, каталог —
// MARK_IN_SYNC; индекс получает соответствие inode → путь.
func TestPopulateTopLevelFresh(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/": {
			{Name: "doc.pdf", Inode: 0x1122334455667788, Size: 12345, MtimeSec: 1700000000, MtimeNsec: 500},
			{Name: "sub", Inode: 99, Size: 0, MtimeSec: 1700000123, IsDir: true},
		},
	}}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	p, idx := newTestProvider(t, root, backend, store)

	if err := populateTopLevel(context.Background(), p); err != nil {
		t.Fatalf("populateTopLevel: %v", err)
	}
	// Поштучные конверсии: по одной на запись, без батчей.
	converts := store.convertCalls()
	if len(converts) != 2 {
		t.Fatalf("конверсий %d, ожидается 2 (по одной на запись)", len(converts))
	}
	// Файл: identity = inode LE 8 байт, флаги MARK_IN_SYNC|DEHYDRATE
	// (явная дегидратация обязательна — design.md D6.4).
	file := converts[0]
	if file.path != filepath.Join(root, "doc.pdf") {
		t.Errorf("путь конверсии %q, ожидается %q", file.path, filepath.Join(root, "doc.pdf"))
	}
	if file.isDir {
		t.Error("doc.pdf сконвертирован как каталог")
	}
	if file.ino != 0x1122334455667788 {
		t.Errorf("identity файла = %#x, ожидается inode", file.ino)
	}
	if file.flags != cfapi.ConvertFlagMarkInSync|cfapi.ConvertFlagDehydrate {
		t.Errorf("флаги файла = %#x, ожидается MARK_IN_SYNC|DEHYDRATE", file.flags)
	}
	// Каталог: MARK_IN_SYNC без DEHYDRATE.
	dir := converts[1]
	if !dir.isDir || dir.ino != 99 || dir.path != filepath.Join(root, "sub") {
		t.Errorf("конверсия каталога: %+v, ожидается sub/ino 99/isDir", dir)
	}
	if dir.flags != cfapi.ConvertFlagMarkInSync {
		t.Errorf("флаги каталога = %#x, ожидается MARK_IN_SYNC", dir.flags)
	}
	// Реальная запись файла на диске: размер из backend (SetEndOfFile) и
	// mtime из backend (SetFileTime) — без записи данных.
	fi, err := os.Stat(filepath.Join(root, "doc.pdf"))
	if err != nil {
		t.Fatalf("stat doc.pdf: %v", err)
	}
	if fi.Size() != 12345 {
		t.Errorf("размер файла %d, ожидается 12345 (логический EOF)", fi.Size())
	}
	if want := time.Unix(1700000000, 500); !fi.ModTime().Equal(want) {
		t.Errorf("mtime файла %v, ожидается %v (из backend)", fi.ModTime(), want)
	}
	// Каталог создан на диске.
	fi, err = os.Stat(filepath.Join(root, "sub"))
	if err != nil || !fi.IsDir() {
		t.Errorf("sub: stat = %v, ожидается каталог", err)
	}
	if n := len(store.updateCalls()); n != 0 {
		t.Errorf("fallback-обновлений %d, ожидается 0 (convert(DEHYDRATE) принят)", n)
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

	converts := store.convertCalls()
	if len(converts) != 1 || converts[0].path != filepath.Join(root, "fresh.bin") {
		t.Fatalf("конверсии %v, ожидается ровно одна по fresh.bin (конфликт пропущен)", converts)
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

func (s *stateAwareStore) Convert(path string, isDir bool, identity []byte, flags uint32) error {
	return s.store.Convert(path, isDir, identity, flags)
}

func (s *stateAwareStore) Update(path string, fsMetadata cfapi.CFFileMetadata, flags uint32) error {
	return s.store.Update(path, fsMetadata, flags)
}

func (s *stateAwareStore) StandardInfo(path string) (*cfapi.CFPlaceholderStandardInfo, error) {
	return s.store.StandardInfo(path)
}

func (s *stateAwareStore) AnswerFetchPlaceholders(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, infos []cfapi.CFPlaceholderCreateInfo, total int64, entriesProcessed uint32) error {
	return s.store.AnswerFetchPlaceholders(conn, transferKey, requestKey, status, infos, total, entriesProcessed)
}

func (s *stateAwareStore) TransferData(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, offset, length int64, buf []byte) error {
	return s.store.TransferData(conn, transferKey, requestKey, status, offset, length, buf)
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
	// «Диск» — в fakeStore: локальное перечисление по его картам.
	p.local = fakeLocal{store}

	if err := populateTopLevel(context.Background(), p); err != nil {
		t.Fatalf("populateTopLevel: %v", err)
	}
	// Плейсхолдер уже на диске — создание не повторяется вовсе: конверсий
	// нет (и уж точно не по old.bin).
	if n := len(store.convertCalls()); n != 0 {
		t.Errorf("существующий плейсхолдер сконвертирован повторно: %d вызовов (дубль)", n)
	}
	if p, ok := idx.Path(42); !ok || p != "/old.bin" {
		t.Errorf("idx.Path(42) = (%q, %v), ожидается /old.bin", p, ok)
	}
}

// TestHandleFetchPlaceholdersChildren — fallback FETCH_PLACEHOLDERS:
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
// задание с путём вне sync root (fsPathOf → "", FileID не резолвится)
// обслуживается fallback-веткой по каталогу "/" и завершается ровно одним
// ответом.
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

// failingConvertStore — convert-примитив отказывает всегда (симуляция
// отказа CfConvertToPlaceholder).
type failingConvertStore struct {
	fakeStore
	convertErr error
}

func (f *failingConvertStore) Convert(path string, isDir bool, identity []byte, flags uint32) error {
	return f.convertErr
}

// TestPopulateTopLevelCreateFailureNoIndex — отказ конверсии (design.md
// D6.4 «Отказ конверсии записи»): запись не доведена до конечного
// состояния — в индекс не попадает, созданная реальная запись удалена
// (откат), существовавшие на диске плейсхолдеры того же каталога
// индексируются.
func TestPopulateTopLevelCreateFailureNoIndex(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/": {
			{Name: "new.bin", Inode: 21, Size: 5, MtimeSec: 1},
			{Name: "old.bin", Inode: 22, Size: 6, MtimeSec: 2},
		},
	}}
	// old.bin — плейсхолдер предыдущего запуска; new.bin — новая запись.
	store := &failingConvertStore{
		fakeStore:  fakeStore{local: map[string]bool{filepath.Join(root, "old.bin"): true}, localReal: map[string]bool{}},
		convertErr: errFake("fake: CfConvertToPlaceholder отказал"),
	}
	p, idx := newTestProvider(t, root, backend, store)
	// «Диск» — в fakeStore: локальное перечисление по его картам.
	p.local = fakeLocal{&store.fakeStore}

	if err := populateTopLevel(context.Background(), p); err != nil {
		t.Fatalf("populateTopLevel: %v", err)
	}
	if _, ok := idx.Path(21); ok {
		t.Error("запись с отказавшей конверсией попала в индекс")
	}
	if p, ok := idx.Path(22); !ok || p != "/old.bin" {
		t.Errorf("существующий плейсхолдер потерян из индекса: (%q, %v), ожидается /old.bin", p, ok)
	}
	// Откат: реальный файл new.bin удалён — ни неконвертированных записей,
	// ни гидратированных нулей не остаётся (design.md D6.4).
	if _, err := os.Stat(filepath.Join(root, "new.bin")); !os.IsNotExist(err) {
		t.Errorf("созданный файл не удалён при откате: stat err = %v", err)
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

func (s *slowAnswerStore) AnswerFetchPlaceholders(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, infos []cfapi.CFPlaceholderCreateInfo, total int64, entriesProcessed uint32) error {
	s.once.Do(func() { close(s.entered) })
	time.Sleep(s.hold)
	return s.fakeStore.AnswerFetchPlaceholders(conn, transferKey, requestKey, status, infos, total, entriesProcessed)
}

// TestHandleFetchPlaceholdersIncludesExistingCompleteBatch — канонический
// «batch complete»-ответ (проба 4, 27.11): массив несёт ВСЕ записи каталога
// (fresh + existing; фильтр отвечает existing per-entry ALREADY_EXISTS —
// классификатор отличает от отказа), EntriesProcessed =
// PlaceholderTotalCount = полное число записей — иначе population не
// завершается (create пендится вечно: «0 из 1»).
func TestHandleFetchPlaceholdersIncludesExistingCompleteBatch(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/sub": {
			{Name: "a.txt", Inode: 7, Size: 3, MtimeSec: 4},
			{Name: "b.bin", Inode: 8, Size: 4, MtimeSec: 5}, // уже плейсхолдер на диске
		},
	}}
	store := &fakeStore{
		local: map[string]bool{filepath.Join(root, "sub", "b.bin"): true},
		// Фильтр на существующую запись отвечает ALREADY_EXISTS.
		answerResults: map[string]cfapi.HRESULT{"b.bin": errorAlreadyExistsResult},
	}
	p, idx := newTestProvider(t, root, backend, store)

	job := &cfapi.FetchPlaceholdersJob{
		ConnectionKey: cfapi.ConnectionKey{Internal: 1},
		TransferKey:   cfapi.TransferKey(21),
		RequestKey:    cfapi.RequestKey(22),
		Path:          filepath.Join(root, "sub"),
	}
	handleFetchPlaceholders(context.Background(), p, job)

	answers := store.answerCalls()
	if len(answers) != 1 {
		t.Fatalf("ответов %d, ожидается 1", len(answers))
	}
	a := answers[0]
	if a.status != cfapi.StatusSuccess {
		t.Errorf("статус 0x%08x, ожидается SUCCESS", uint32(a.status))
	}
	// Массив: fresh + existing (batch complete).
	if len(a.names) != 2 || a.names[0] != "a.txt" || a.names[1] != "b.bin" {
		t.Fatalf("в ответе %v, ожидается [a.txt b.bin]", a.names)
	}
	// total = processed = полное число записей каталога — иначе population
	// не завершается (create пендится).
	if a.total != 2 {
		t.Errorf("total = %d, ожидается 2 (полный backend-листинг)", a.total)
	}
	if a.processed != 2 {
		t.Errorf("processed = %d, ожидается 2 (== count при single-shot; processed >= total завершает population)", a.processed)
	}
	// ALREADY_EXISTS от фильтра — не отказ: обе записи в индексе.
	for ino, want := range map[uint64]string{7: "/sub/a.txt", 8: "/sub/b.bin"} {
		if p, ok := idx.Path(ino); !ok || p != want {
			t.Errorf("idx.Path(%d) = (%q, %v), ожидается %s", ino, p, ok, want)
		}
	}
}

// TestHandleFetchPlaceholdersTotalIsFullBackendCount — PlaceholderTotalCount
// = EntriesProcessed = полное число записей backend каталога (fresh +
// existing + конфликты): population завершается по processed >= total; EOF =
// «никогда» (проба 27.11: два FETCH с дельтой 60 c). Конфликт входит в
// total/processed (иначе счётчик не дойдёт до total), но НЕ в массив.
func TestHandleFetchPlaceholdersTotalIsFullBackendCount(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/mix": {
			{Name: "fresh.txt", Inode: 91, Size: 1, MtimeSec: 1},
			{Name: "known.bin", Inode: 92, Size: 2, MtimeSec: 2},    // existing-плейсхолдер
			{Name: "conflict.log", Inode: 93, Size: 3, MtimeSec: 3}, // локальный не-плейсхолдер
		},
	}}
	store := &fakeStore{
		local:     map[string]bool{filepath.Join(root, "mix", "known.bin"): true},
		localReal: map[string]bool{filepath.Join(root, "mix", "conflict.log"): true},
	}
	p, idx := newTestProvider(t, root, backend, store)

	job := &cfapi.FetchPlaceholdersJob{Path: filepath.Join(root, "mix"), TransferKey: 90}
	handleFetchPlaceholders(context.Background(), p, job)

	answers := store.answerCalls()
	if len(answers) != 1 {
		t.Fatalf("ответов %d, ожидается 1", len(answers))
	}
	a := answers[0]
	// total и processed — полное число записей каталога, включая конфликт.
	if a.total != 3 {
		t.Errorf("total = %d, ожидается 3 (fresh + existing + конфликт)", a.total)
	}
	if a.processed != 3 {
		t.Errorf("processed = %d, ожидается 3 (processed >= total завершает population)", a.processed)
	}
	// В массиве fresh + existing; конфликт исключён (инвариант — локальный
	// файл не перезаписывается).
	if len(a.names) != 2 || a.names[0] != "fresh.txt" || a.names[1] != "known.bin" {
		t.Errorf("в массиве %v, ожидается [fresh.txt known.bin]", a.names)
	}
	// existing — в индекс; конфликт — нет.
	if _, ok := idx.Path(92); !ok {
		t.Error("existing не попал в индекс")
	}
	if _, ok := idx.Path(93); ok {
		t.Error("конфликт попал в индекс")
	}
}

// TestHandleFetchPlaceholdersEmptyDirectoryPopulated — existing-only каталог
// несёт запись в batch-ответе (count=total=processed=1); истинно пустой
// каталог — count=total=processed=0, SUCCESS: population завершается сразу.
func TestHandleFetchPlaceholdersEmptyDirectoryPopulated(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/full": {{Name: "only.bin", Inode: 41, Size: 1, MtimeSec: 1}}, // весь каталог — existing
		"/void": {},                                                    // истинно пустой в backend
	}}
	store := &fakeStore{
		local:         map[string]bool{filepath.Join(root, "full", "only.bin"): true},
		localReal:     map[string]bool{},
		answerResults: map[string]cfapi.HRESULT{"only.bin": errorAlreadyExistsResult},
	}
	p, idx := newTestProvider(t, root, backend, store)

	// Ожидания по каталогам: /full — одна backend-запись (existing) в
	// batch-ответе; /void — истинно пустой: count=total=processed=0.
	cases := []struct {
		dir           string
		wantNames     []string
		wantTotal     int64
		wantProcessed uint32
	}{
		{"/full", []string{"only.bin"}, 1, 1},
		{"/void", nil, 0, 0},
	}
	for i, tc := range cases {
		job := &cfapi.FetchPlaceholdersJob{
			ConnectionKey: cfapi.ConnectionKey{Internal: 1},
			TransferKey:   cfapi.TransferKey(50 + i),
			RequestKey:    cfapi.RequestKey(60 + i),
			Path:          filepath.Join(root, tc.dir),
		}
		handleFetchPlaceholders(context.Background(), p, job)
	}
	answers := store.answerCalls()
	if len(answers) != 2 {
		t.Fatalf("ответов %d, ожидается 2 (по одному на каталог)", len(answers))
	}
	for i, tc := range cases {
		a := answers[i]
		if a.status != cfapi.StatusSuccess {
			t.Errorf("каталог %s: статус 0x%08x, ожидается SUCCESS (населено)", tc.dir, uint32(a.status))
		}
		if len(a.names) != len(tc.wantNames) {
			t.Fatalf("каталог %s: в ответе %v, ожидается %v", tc.dir, a.names, tc.wantNames)
		}
		for j, n := range tc.wantNames {
			if a.names[j] != n {
				t.Errorf("каталог %s: запись %q, ожидается %q", tc.dir, a.names[j], n)
			}
		}
		if a.total != tc.wantTotal || a.processed != tc.wantProcessed {
			t.Errorf("каталог %s: total=%d processed=%d, ожидается %d/%d (population завершается сразу)", tc.dir, a.total, a.processed, tc.wantTotal, tc.wantProcessed)
		}
	}
	// ALREADY_EXISTS на existing — не отказ: запись в индексе.
	if p, ok := idx.Path(41); !ok || p != "/full/only.bin" {
		t.Errorf("idx.Path(41) = (%q, %v), ожидается /full/only.bin", p, ok)
	}
}

// TestHandleFetchPlaceholdersReadsFilterResults — per-entry Result нашего
// массива после ответа читается: отказ фильтра на запись исключает её из
// индекса (симуляция записи Result — в fakeStore, как делает фильтр).
func TestHandleFetchPlaceholdersReadsFilterResults(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/sub": {
			{Name: "ok.txt", Inode: 71, Size: 1, MtimeSec: 1},
			{Name: "bad.bin", Inode: 72, Size: 2, MtimeSec: 2},
		},
	}}
	store := &fakeStore{
		local:     map[string]bool{},
		localReal: map[string]bool{},
		answerResults: map[string]cfapi.HRESULT{
			"bad.bin": cfapi.HRESULT(-2147024809), // 0x80070057 E_INVALIDARG
		},
	}
	p, idx := newTestProvider(t, root, backend, store)

	job := &cfapi.FetchPlaceholdersJob{Path: filepath.Join(root, "sub"), TransferKey: 80}
	handleFetchPlaceholders(context.Background(), p, job)

	if p, ok := idx.Path(71); !ok || p != "/sub/ok.txt" {
		t.Errorf("idx.Path(71) = (%q, %v), ожидается /sub/ok.txt", p, ok)
	}
	if _, ok := idx.Path(72); ok {
		t.Error("запись с отказом фильтра (Result != 0) попала в индекс")
	}
}

// TestClassifyPlaceholderResult — классификация per-entry Result (ответы
// FETCH_PLACEHOLDERS): 0 = создана, ALREADY_EXISTS = уже была, прочее = отказ.
func TestClassifyPlaceholderResult(t *testing.T) {
	cases := []struct {
		res  cfapi.HRESULT
		want placeholderResultKind
	}{
		{0, resultCreated},
		{errorAlreadyExistsResult, resultExists},
		{cfapi.HRESULT(-2147024809), resultFailed}, // 0x80070057
		{cfapi.HRESULT(-1), resultFailed},
	}
	for _, tc := range cases {
		if got := classifyPlaceholderResult(tc.res); got != tc.want {
			t.Errorf("classifyPlaceholderResult(%d) = %v, ожидается %v", int64(tc.res), got, tc.want)
		}
	}
}

// TestHandleFetchPlaceholdersDerivesViaFileIdentity — регресс живой пробы
// U2 («nested companies»): NormalizedPath вне корня (fsPathOf → "") —
// каталог деривируется через FileIdentity (наш inode), а не fallback'ом по
// корню: отвечаются записи "/sub", а не корневые.
func TestHandleFetchPlaceholdersDerivesViaFileIdentity(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/sub": {{Name: "c.txt", Inode: 12, Size: 1}},
	}}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	p, idx := newTestProvider(t, root, backend, store)
	// Резолвер подменяет meta-подъём (fs в тесте нет): inode каталога → путь.
	idx.SetResolver(func(ctx context.Context, ino uint64) (string, bool) {
		if ino == 99 {
			return "/sub", true
		}
		return "", false
	})

	job := &cfapi.FetchPlaceholdersJob{
		ConnectionKey: cfapi.ConnectionKey{Internal: 1},
		TransferKey:   cfapi.TransferKey(31),
		RequestKey:    cfapi.RequestKey(32),
		Path:          `C:\elsewhere`,
		FileID:        99,
	}
	handleFetchPlaceholders(context.Background(), p, job)

	answers := store.answerCalls()
	if len(answers) != 1 {
		t.Fatalf("ответов %d, ожидается 1", len(answers))
	}
	if len(answers[0].names) != 1 || answers[0].names[0] != "c.txt" {
		t.Fatalf("записи ответа %v, ожидается [c.txt] — записи каталога /sub, не корня", answers[0].names)
	}
	if p, ok := idx.Path(12); !ok || p != "/sub/c.txt" {
		t.Errorf("idx.Path(12) = (%q, %v), ожидается /sub/c.txt", p, ok)
	}
}

// TestFSPathOf — перевод локального пути в fs-путь: полный путь с томом и
// БЕЗ тома (формат NormalizedPath на живом фильтре, проба U2), другой
// регистр, относительный путь, пустой и мусор (вне корня, побег, чужой
// префикс-каталог — "").
func TestFSPathOf(t *testing.T) {
	p := New(`C:\Users\u\AgioDrive`, "v", nil, nil)
	cases := []struct{ in, want string }{
		// Полный путь под sync root (с буквой диска).
		{`C:\Users\u\AgioDrive`, "/"},
		{`C:\Users\u\AgioDrive\`, "/"},
		{`C:\Users\u\AgioDrive\companies`, "/companies"},
		{`C:\Users\u\AgioDrive\companies\deep`, "/companies/deep"},
		// Без буквы диска — фактический формат NormalizedPath на живом
		// фильтре (VolumeDosName-relative, ведущий разделитель).
		{`\Users\u\AgioDrive`, "/"},
		{`\Users\u\AgioDrive\companies`, "/companies"},
		{`\Users\u\AgioDrive\companies\deep`, "/companies/deep"},
		// Удвоенные ведущие разделители и прямой слэш — после Clean.
		{`//Users/u/AgioDrive/companies`, "/companies"},
		// Другой регистр: Windows FS регистронезависим.
		{`c:\users\u\agiodrive\COMPANIES`, "/companies"},
		{`\USERS\U\AGIODRIVE\companies`, "/companies"},
		// Относительный путь — относительно корня.
		{"companies", "/companies"},
		{"companies/deep", "/companies/deep"},
		// Не парсится — "" (воркер падает в резолв через FileIdentity).
		{"", ""},
		{`C:\elsewhere`, ""},
		{`C:\Users\u\AgioDriveEvil`, ""},
		{`C:\Users\u\AgioDrive\..\secret`, ""},
		{`..\outside`, ""},
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

// TestPopulateDirectoryExistingByIndexNoJournal — задача 2.9: existing по
// индексу (имя есть, inode известен, путь совпадает) — skip без Warn и без
// записи в журнал; изменившийся путь в индексе — «изменившееся», строка
// пишется (design.md D6.7).
func TestPopulateDirectoryExistingByIndexNoJournal(t *testing.T) {
	root := t.TempDir()
	entries := []BackendEntry{{Name: "same.bin", Inode: 60, Size: 1, MtimeSec: 1}}
	backend := &fakeBackend{dirs: map[string][]BackendEntry{"/kept": entries}}
	store := &simStore{fakeStore: fakeStore{
		local:     map[string]bool{filepath.Join(root, "kept", "same.bin"): true},
		localReal: map[string]bool{},
	}}
	p, idx := newTestProvider(t, root, backend, store)
	p.local = mapLocal{store}

	hook := &logCapture{}
	logger.AddHook(hook)

	// Индекс уже знает запись с тем же путём (предыдущий walk).
	if err := idx.Put(60, "/kept/same.bin"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	rows := journalLines(t, idx)

	res := populateDirectory(context.Background(), p, "/kept", entries)

	if res.skipped != 1 || res.createdFiles != 0 || res.conflicts != 0 {
		t.Errorf("skipped=%d created=%d conflicts=%d, ожидается 1/0/0 (existing по индексу)", res.skipped, res.createdFiles, res.conflicts)
	}
	if n := journalLines(t, idx); n != rows {
		t.Errorf("existing по индексу добавил строки журнала: %d → %d, ожидается без изменений", rows, n)
	}
	if n := hook.count("walk conflict"); n != 0 {
		t.Errorf("existing по индексу дал Warn о конфликте: %d, ожидается 0", n)
	}

	// Путь в индексе отличается — «изменившееся»: skip, но строка пишется.
	if err := idx.Put(60, "/kept/renamed.bin"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	rows = journalLines(t, idx)
	res = populateDirectory(context.Background(), p, "/kept", entries)
	if res.skipped != 1 {
		t.Errorf("skipped = %d, ожидается 1", res.skipped)
	}
	if n := journalLines(t, idx); n != rows+1 {
		t.Errorf("изменившийся путь не записан в журнал: %d → %d, ожидается +1", rows, n)
	}
	if got, ok := idx.Path(60); !ok || got != "/kept/same.bin" {
		t.Errorf("idx.Path(60) = (%q, %v), ожидается /kept/same.bin (актуализация индекса)", got, ok)
	}
}

// --- Фикс-итерация 1 (ревью этапа 6) ---

// raceConvertStore — Convert-«гонка» (fix-1.7): путь уже сконвертирован
// параллельной операцией (notifyDirCreate/повтор журнала) — store помечает
// плейсхолдер и ОТКАЗЫВАЕТ нашу конверсию (фильтр отвергает повторную
// конверсию плейсхолдера).
type raceConvertStore struct {
	fakeStore
}

func (s *raceConvertStore) Convert(path string, isDir bool, identity []byte, flags uint32) error {
	s.mu.Lock()
	s.local[path] = true // гонка успела: путь уже плейсхолдер
	s.mu.Unlock()
	return errFake("fake: already a placeholder (convert rejected)")
}

// TestCreateEntryAdoptsRaceConvertedEntry — fix-1.7: между созданием реальной
// записи и нашей конверсией запись сконвертирована параллельной операцией —
// усыновление (resultExists) БЕЗ отката: откат удалил бы валидный
// плейсхолдер.
func TestCreateEntryAdoptsRaceConvertedEntry(t *testing.T) {
	root := t.TempDir()
	store := &raceConvertStore{fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}}
	p, _ := newTestProvider(t, root, &fakeBackend{dirs: map[string][]BackendEntry{}}, store)

	// Файл: реальная запись создана, конверсия отвергнута гонкой.
	e := BackendEntry{Name: "raced.txt", Inode: 70, Size: 5, MtimeSec: 100}
	kind, err := createEntry(p, "/", e)
	if kind != resultExists || err != nil {
		t.Fatalf("createEntry file = (%v, %v), ожидается (resultExists, nil)", kind, err)
	}
	local := filepath.Join(root, "raced.txt")
	if _, serr := os.Stat(local); serr != nil {
		t.Fatalf("валидный плейсхолдер удалён откатом: %v", serr)
	}

	// Каталог: та же гонка.
	de := BackendEntry{Name: "raceddir", Inode: 71, MtimeSec: 100, IsDir: true}
	kind, err = createEntry(p, "/", de)
	if kind != resultExists || err != nil {
		t.Fatalf("createEntry dir = (%v, %v), ожидается (resultExists, nil)", kind, err)
	}
	if _, serr := os.Stat(filepath.Join(root, "raceddir")); serr != nil {
		t.Fatalf("каталог удалён откатом: %v", serr)
	}
}
