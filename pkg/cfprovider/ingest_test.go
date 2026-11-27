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

// Тесты пула выгрузки (задачи 5.2–5.4): классификация close-уведомлений,
// выгрузка нового файла (convert БЕЗ дегидратации после подтверждённого
// Flush), изменённый плейсхолдер (SetInSync после Flush), пропуск in-sync,
// отказ Flush без финализации (write-back выключен), коалисинг по пути с
// injectable часами, повтор при отказе в совместном доступе, сквозной
// прогон очереди диспетчера. cldapi.dll не вызывается — ingestStore и
// ingestBackend подменные (инвариант «Движок проверяется без CfAPI»).
package cfprovider

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"github.com/juicedata/juicefs/pkg/meta"
)

// opLog — общий журнал вызовов backend/store: порядок операций выгрузки
// (create/write/truncate/flush → convert или insync).
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) add(op string) {
	l.mu.Lock()
	l.ops = append(l.ops, op)
	l.mu.Unlock()
}

func (l *opLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ops...)
}

func (l *opLog) kinds() []string {
	ops := l.snapshot()
	kinds := make([]string, len(ops))
	for i, op := range ops {
		kinds[i] = strings.SplitN(op, ":", 2)[0]
	}
	return kinds
}

// fakeIngestBackend — подменный ingestBackend: записи в памяти; существующие
// записи (карта files) Create отвечает EEXIST — проверка OpenWrite-ветки.
type fakeIngestBackend struct {
	mu       sync.Mutex
	log      *opLog
	files    map[string][]byte // существующие записи backend
	inodes   map[string]uint64 // inode записи backend по пути
	nextIno  uint64
	flushErr syscall.Errno // ошибка Flush созданных/открытых файлов
	creates  int
	opens    int
}

func newFakeIngestBackend(log *opLog) *fakeIngestBackend {
	return &fakeIngestBackend{log: log, files: map[string][]byte{}, inodes: map[string]uint64{}, nextIno: 100}
}

func (b *fakeIngestBackend) Create(ctx meta.Context, path string) (ingestFile, syscall.Errno) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.creates++
	if _, ok := b.files[path]; ok {
		return nil, syscall.EEXIST
	}
	b.nextIno++
	b.files[path] = nil
	b.inodes[path] = b.nextIno
	b.log.add("create:" + path)
	return &fakeIngestFile{b: b, path: path, ino: b.nextIno, flushErr: b.flushErr}, 0
}

func (b *fakeIngestBackend) OpenWrite(ctx meta.Context, path string) (ingestFile, syscall.Errno) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.files[path]; !ok {
		return nil, syscall.ENOENT
	}
	b.opens++
	b.log.add("open:" + path)
	return &fakeIngestFile{b: b, path: path, ino: b.inodes[path], flushErr: b.flushErr}, 0
}

func (b *fakeIngestBackend) content(path string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.files[path]...)
}

func (b *fakeIngestBackend) inodeOf(path string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inodes[path]
}

// fakeIngestFile — подменный открытый файл backend: данные пишутся в память
// сразу, Flush/Close фиксируются в журнале, Flush — с настраиваемой ошибкой.
type fakeIngestFile struct {
	b        *fakeIngestBackend
	path     string
	ino      uint64
	flushErr syscall.Errno
}

func (f *fakeIngestFile) Pwrite(ctx meta.Context, b []byte, off int64) (int, syscall.Errno) {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	data := f.b.files[f.path]
	end := int(off) + len(b)
	if end > len(data) {
		grown := make([]byte, end)
		copy(grown, data)
		data = grown
	}
	copy(data[off:], b)
	f.b.files[f.path] = data
	f.b.log.add(fmt.Sprintf("write:%s:%d:%d", f.path, off, len(b)))
	return len(b), 0
}

func (f *fakeIngestFile) Truncate(ctx meta.Context, length uint64) syscall.Errno {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	data := f.b.files[f.path]
	if int64(length) <= int64(len(data)) {
		data = data[:length]
	} else {
		grown := make([]byte, length)
		copy(grown, data)
		data = grown
	}
	f.b.files[f.path] = data
	f.b.log.add(fmt.Sprintf("truncate:%s:%d", f.path, length))
	return 0
}

func (f *fakeIngestFile) Flush(ctx meta.Context) syscall.Errno {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	f.b.log.add("flush:" + f.path)
	return f.flushErr
}

func (f *fakeIngestFile) Close(ctx meta.Context) syscall.Errno {
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	f.b.log.add("close:" + f.path)
	return 0
}

func (f *fakeIngestFile) Inode() meta.Ino { return meta.Ino(f.ino) }

// convertFileCall — фиксация конверсии нового файла в плейсхолдер.
type convertFileCall struct {
	path  string
	ino   uint64
	flags uint32
}

// fakeIngestStore — подменный ingestStore: состояния путей в карте,
// SetInSync/ConvertFile фиксируются (успех переводит состояние в
// placeholder|in-sync — модель фильтра).
type fakeIngestStore struct {
	mu         sync.Mutex
	log        *opLog
	states     map[string]uint32
	stateErr   error
	setSyncErr error
	convertErr error
	// convertFailures — первые N попыток ConvertFile отвечают convertErr
	// (retry-конверсия после закрытия записи, живой ретест этапа 5).
	convertFailures int
	setSyncs        []string
	converts        []convertFileCall
}

func newFakeIngestStore(log *opLog) *fakeIngestStore {
	return &fakeIngestStore{log: log, states: map[string]uint32{}}
}

func (s *fakeIngestStore) PlaceholderState(path string) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stateErr != nil {
		return 0, s.stateErr
	}
	return s.states[path], nil
}

func (s *fakeIngestStore) SetInSync(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log.add("insync:" + path)
	if s.setSyncErr != nil {
		return s.setSyncErr
	}
	s.setSyncs = append(s.setSyncs, path)
	s.states[path] |= cfapi.PlaceholderStatePlaceholder | cfapi.PlaceholderStateInSync
	return nil
}

func (s *fakeIngestStore) ConvertFile(path string, identity []byte, flags uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ino uint64
	if len(identity) >= 8 {
		ino = binary.LittleEndian.Uint64(identity)
	}
	s.log.add(fmt.Sprintf("convert:%s:%d:0x%x", path, ino, flags))
	// Пытка фиксируется и при отказе (подсчёт попыток retry-конверсии).
	s.converts = append(s.converts, convertFileCall{path: path, ino: ino, flags: flags})
	if s.convertFailures > 0 {
		s.convertFailures--
		return s.convertErr
	}
	s.states[path] = cfapi.PlaceholderStatePlaceholder | cfapi.PlaceholderStateInSync
	return nil
}

func (s *fakeIngestStore) stateOf(path string) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[path]
}

func (s *fakeIngestStore) setState(path string, state uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[path] = state
}

// fakeClock — injectable часы коалисера (задача 5.4): время двигает тест,
// таймеры делят общий канал срабатывания.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	fireC chan time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, fireC: make(chan time.Time, 16)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) ingestTimer { return fakeTimer{c: c.fireC} }

// advance двигает время и посылает срабатывание таймера (буфер 16 —
// последовательные advance'ы теста не переполняют).
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	ts := c.now
	c.mu.Unlock()
	select {
	case c.fireC <- ts:
	default:
	}
}

// fakeTimer — ingestTimer под контролем теста.
type fakeTimer struct{ c chan time.Time }

func (t fakeTimer) Fired() <-chan time.Time { return t.c }

func (t fakeTimer) Stop() {}

func (t fakeTimer) Reset(time.Duration) {}

// newIngestTestProvider — провайдер с подменными store/backend и короткими
// задержками (коалисинг 5 мс, retry 1 мс): Start не вызывается.
func newIngestTestProvider(t *testing.T) (*Provider, *fakeIngestBackend, *fakeIngestStore, *Index, *opLog) {
	t.Helper()
	idx, err := NewIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	log := &opLog{}
	store := newFakeIngestStore(log)
	backend := newFakeIngestBackend(log)
	p := New(t.TempDir(), "ingest-vol", &fakeSyncRootAPI{registered: true}, nil)
	p.idx = idx
	p.istore = store
	p.ibackend = backend
	p.ingestDelay = 5 * time.Millisecond
	p.ingestRetryDelay = time.Millisecond
	p.ingestMaxRetryDelay = 4 * time.Millisecond
	p.ingestWorkers = 1
	return p, backend, store, idx, log
}

// recvTask ждёт задание из канала коалисера (воркеры асинхронны).
func recvTask(t *testing.T, ch <-chan ingestTask) ingestTask {
	t.Helper()
	select {
	case task := <-ch:
		return task
	case <-time.After(3 * time.Second):
		t.Fatal("коалисер не выдал задание за 3 c")
		return ingestTask{}
	}
}

// assertNoTask — в канале коалисера нет задания (слишком ранний выпуск).
func assertNoTask(t *testing.T, ch <-chan ingestTask) {
	t.Helper()
	select {
	case task := <-ch:
		t.Fatalf("коалисер выдал лишнее задание %+v", task)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestIngestNewFileConvertsToPlaceholder — спека «Новый файл»: содержимое
// выгружено в backend байт-в-байт, Flush подтверждает версию, ПОСЛЕ него
// файл конвертирован в плейсхолдер флагом MARK_IN_SYNC БЕЗ DEHYDRATE
// (данные остаются локально полными), identity — inode backend, индекс
// получил соответствие inode → путь, локальный файл не изменён.
func TestIngestNewFileConvertsToPlaceholder(t *testing.T) {
	p, backend, store, idx, log := newIngestTestProvider(t)
	local := filepath.Join(p.root, "new.txt")
	data := patternBytes(5000)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}

	p.runIngest(context.Background(), ingestTask{fsPath: "/new.txt", local: local, key: "/new.txt"})

	if got := backend.content("/new.txt"); !bytes.Equal(got, data) {
		t.Fatalf("содержимое backend не совпало с локальным (%d vs %d байт)", len(got), len(data))
	}
	// Порядок операций: create → write… → truncate → flush → convert →
	// close (defer).
	kinds := log.kinds()
	if len(kinds) == 0 || kinds[0] != "create" || kinds[len(kinds)-1] != "close" {
		t.Fatalf("порядок операций %v, ожидается create…close", kinds)
	}
	if kinds[len(kinds)-2] != "convert" {
		t.Fatalf("последняя операция перед close = %s, ожидается convert: %v", kinds[len(kinds)-2], kinds)
	}
	if kinds[len(kinds)-3] != "flush" {
		t.Fatalf("convert выполнен до Flush: %v (write-back выключен — финализация только после Flush)", kinds)
	}
	for _, k := range kinds {
		if k == "insync" || k == "open" {
			t.Fatalf("лишняя операция %q для нового файла: %v", k, kinds)
		}
	}
	// Порции: 5000 байт одной записью (блок по умолчанию 4 MiB).
	writes := 0
	for _, k := range kinds {
		if k == "write" {
			writes++
		}
	}
	if writes != 1 {
		t.Errorf("записей = %d, ожидается 1 (5000 байт < BlockSize)", writes)
	}
	// Конверсия: флаги MARK_IN_SYNC без DEHYDRATE, identity — inode backend.
	converts := store.converts
	if len(converts) != 1 {
		t.Fatalf("конверсий = %d, ожидается 1", len(converts))
	}
	ino := backend.inodeOf("/new.txt")
	if converts[0].flags != cfapi.ConvertFlagMarkInSync {
		t.Errorf("флаги конверсии = 0x%x, ожидается MARK_IN_SYNC (0x%x) без DEHYDRATE", converts[0].flags, cfapi.ConvertFlagMarkInSync)
	}
	if converts[0].ino != ino {
		t.Errorf("identity конверсии = %d, ожидается inode backend %d", converts[0].ino, ino)
	}
	if got, ok := idx.Path(ino); !ok || got != "/new.txt" {
		t.Errorf("idx.Path(%d) = (%q, %v), ожидается /new.txt", ino, got, ok)
	}
	// Локальный файл остался без изменений (инвариант выгрузки).
	if got, err := os.ReadFile(local); err != nil || !bytes.Equal(got, data) {
		t.Errorf("локальный файл изменён: %v", err)
	}
	// Модель фильтра: путь стал плейсхолдером in-sync.
	if st := store.stateOf(local); st != cfapi.PlaceholderStatePlaceholder|cfapi.PlaceholderStateInSync {
		t.Errorf("состояние локального пути = 0x%x, ожидается placeholder|in-sync", st)
	}
}

// TestIngestModifiedPlaceholder — спека «Изменённый плейсхолдер»: запись
// backend открыта на запись (Create → EEXIST → OpenWrite), сжатие локальной
// версией доведено Truncate'ом, Flush подтверждает, SetInSync после Flush,
// конверсии нет (файл уже плейсхолдер).
func TestIngestModifiedPlaceholder(t *testing.T) {
	p, backend, store, idx, log := newIngestTestProvider(t)
	local := filepath.Join(p.root, "doc.txt")
	data := []byte("short")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	backend.mu.Lock()
	backend.files["/doc.txt"] = []byte("very old content")
	backend.inodes["/doc.txt"] = 42
	backend.mu.Unlock()
	store.setState(local, cfapi.PlaceholderStatePlaceholder) // вне in-sync

	p.runIngest(context.Background(), ingestTask{fsPath: "/doc.txt", local: local, key: "/doc.txt"})

	if got := backend.content("/doc.txt"); !bytes.Equal(got, data) {
		t.Fatalf("содержимое backend = %q, ожидается %q", got, data)
	}
	kinds := log.kinds()
	// Create отклонён EEXIST (без записи в журнале операций) → open.
	if backend.creates != 1 {
		t.Errorf("попыток Create = %d, ожидается 1", backend.creates)
	}
	if len(kinds) == 0 || kinds[0] != "open" || kinds[len(kinds)-1] != "close" {
		t.Fatalf("порядок операций %v, ожидается open…close", kinds)
	}
	if kinds[len(kinds)-2] != "insync" {
		t.Fatalf("последняя операция перед close = %s, ожидается insync: %v", kinds[len(kinds)-2], kinds)
	}
	if kinds[len(kinds)-3] != "flush" {
		t.Fatalf("SetInSync выполнен до Flush: %v", kinds)
	}
	foundTruncate := false
	for _, op := range log.snapshot() {
		if op == "truncate:/doc.txt:5" {
			foundTruncate = true
		}
	}
	if !foundTruncate {
		t.Errorf("Truncate(%d) не зафиксирован: %v (сжатие должно быть доведено)", len(data), log.snapshot())
	}
	if len(store.converts) != 0 {
		t.Errorf("конверсия выполнена для существующего плейсхолдера: %v", store.converts)
	}
	if len(store.setSyncs) != 1 || store.setSyncs[0] != local {
		t.Errorf("SetInSync = %v, ожидается [%s]", store.setSyncs, local)
	}
	if got, ok := idx.Path(42); !ok || got != "/doc.txt" {
		t.Errorf("idx.Path(42) = (%q, %v), ожидается /doc.txt", got, ok)
	}
}

// TestIngestInSyncSkipped — плейсхолдер in-sync (создан walk-ом или уже
// выгружен) не выгружается повторно: backend не затрагивается.
func TestIngestInSyncSkipped(t *testing.T) {
	p, backend, store, _, log := newIngestTestProvider(t)
	local := filepath.Join(p.root, "synced.txt")
	if err := os.WriteFile(local, []byte("data"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	store.setState(local, cfapi.PlaceholderStatePlaceholder|cfapi.PlaceholderStateInSync)

	p.runIngest(context.Background(), ingestTask{fsPath: "/synced.txt", local: local, key: "/synced.txt"})

	if n := len(log.snapshot()); n != 0 {
		t.Errorf("операций backend = %d, ожидается 0: %v", n, log.snapshot())
	}
	backend.mu.Lock()
	creates := backend.creates
	backend.mu.Unlock()
	if creates != 0 {
		t.Errorf("попыток Create = %d, ожидается 0", creates)
	}
}

// TestIngestMissingLocalSkipped — файл удалён до выгрузки: задание
// отменяется без операций backend (сценарий «Удаление локального файла до
// выгрузки»; удаление подтверждённой версии — этап 6).
func TestIngestMissingLocalSkipped(t *testing.T) {
	p, backend, _, _, log := newIngestTestProvider(t)

	p.runIngest(context.Background(), ingestTask{fsPath: "/gone.txt", local: filepath.Join(p.root, "gone.txt"), key: "/gone.txt"})

	if n := len(log.snapshot()); n != 0 {
		t.Errorf("операций backend = %d, ожидается 0: %v", n, log.snapshot())
	}
	backend.mu.Lock()
	creates := backend.creates
	backend.mu.Unlock()
	if creates != 0 {
		t.Errorf("попыток Create = %d, ожидается 0", creates)
	}
}

// TestIngestFlushFailureNoFinalize — write-back выключен (инвариант,
// design.md D9): отказ Flush не финализирует плейсхолдер (ни convert, ни
// SetInSync), индекс не получает запись, локальный файл не изменён.
func TestIngestFlushFailureNoFinalize(t *testing.T) {
	p, backend, store, idx, log := newIngestTestProvider(t)
	local := filepath.Join(p.root, "fail.txt")
	data := patternBytes(100)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	backend.mu.Lock()
	backend.flushErr = syscall.EIO
	backend.mu.Unlock()

	p.runIngest(context.Background(), ingestTask{fsPath: "/fail.txt", local: local, key: "/fail.txt"})

	kinds := log.kinds()
	foundFlush := false
	for _, k := range kinds {
		if k == "flush" {
			foundFlush = true
		}
		if k == "convert" || k == "insync" {
			t.Fatalf("финализация после неудачного Flush: %v", kinds)
		}
	}
	if !foundFlush {
		t.Errorf("Flush не выполнялся: %v", kinds)
	}
	if got, ok := idx.Path(backend.inodeOf("/fail.txt")); ok {
		t.Errorf("индекс получил запись при неудачном Flush: %q", got)
	}
	if st := store.stateOf(local); st != 0 {
		t.Errorf("состояние локального пути = 0x%x, ожидается 0 (вне in-sync — задание повторится)", st)
	}
	if got, err := os.ReadFile(local); err != nil || !bytes.Equal(got, data) {
		t.Errorf("локальный файл изменён: %v", err)
	}
}

// TestIngestExistingBackendAdoptsLocalPlainFile — имя занято в backend
// (гонка с другим клиентом), локальный файл обычный: содержимое пишется
// поверх существующей записи, после Flush локальный файл конвертируется
// (adopt), identity — inode существующей записи.
func TestIngestExistingBackendAdoptsLocalPlainFile(t *testing.T) {
	p, backend, store, idx, log := newIngestTestProvider(t)
	local := filepath.Join(p.root, "race.txt")
	data := []byte("mine now")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	backend.mu.Lock()
	backend.files["/race.txt"] = []byte("theirs")
	backend.inodes["/race.txt"] = 77
	backend.mu.Unlock()

	p.runIngest(context.Background(), ingestTask{fsPath: "/race.txt", local: local, key: "/race.txt"})

	if got := backend.content("/race.txt"); !bytes.Equal(got, data) {
		t.Fatalf("содержимое backend = %q, ожидается %q", got, data)
	}
	if len(store.converts) != 1 || store.converts[0].ino != 77 {
		t.Fatalf("конверсия = %+v, ожидается convert с identity 77", store.converts)
	}
	if got, ok := idx.Path(77); !ok || got != "/race.txt" {
		t.Errorf("idx.Path(77) = (%q, %v), ожидается /race.txt", got, ok)
	}
	kinds := log.kinds()
	if len(kinds) < 2 || kinds[len(kinds)-2] != "convert" || kinds[len(kinds)-1] != "close" {
		t.Errorf("порядок операций %v, ожидается …convert close", kinds)
	}
}

// TestIngestClassifyCloseRouting — классификация close-уведомлений
// (задача 5.2): обычный файл и изменённый плейсхолдер ставятся в очередь,
// in-sync/каталоги/вне корня/ошибки состояния — игнорируются; путь
// NormalizedPath без буквы диска распознаётся (fsPathOf, живая проба U2).
func TestIngestClassifyCloseRouting(t *testing.T) {
	p, _, store, _, _ := newIngestTestProvider(t)

	plain := filepath.Join(p.root, "plain.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	// Явно state=0 (NO_STATES — файл не плейсхолдер; живая проба этапа 5:
	// CfGetPlaceholderStateFromFileInfo вернул INVALID для plain-файла,
	// cfapi.PlaceholderStateByPath приводит его к 0): новый файл → to-upload.
	store.setState(plain, 0)
	task, ok := p.classifyNotifyClose(&cfapi.NotifyFileCloseCompletionJob{Path: p.root + `\plain.txt`, FileSize: 1})
	if !ok || task.fsPath != "/plain.txt" || task.local != plain || task.key != "/plain.txt" {
		t.Fatalf("plain файл (state=0) не поставлен в очередь: ok=%v task=%+v", ok, task)
	}

	// NormalizedPath без буквы диска (только хвост от корня тома).
	noVol := `\` + strings.TrimPrefix(strings.ToLower(filepath.Clean(p.root)), strings.ToLower(filepath.VolumeName(p.root)))
	modified := filepath.Join(p.root, "modified.txt")
	if err := os.WriteFile(modified, []byte("y"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	store.setState(modified, cfapi.PlaceholderStatePlaceholder)
	task, ok = p.classifyNotifyClose(&cfapi.NotifyFileCloseCompletionJob{Path: noVol + `\modified.txt`})
	if !ok || task.fsPath != "/modified.txt" {
		t.Fatalf("путь без диска не распознан: ok=%v task=%+v (normalized=%q)", ok, task, noVol+`\modified.txt`)
	}

	// Каталог игнорируется (создание каталогов — этап 6, design.md D5).
	dir := filepath.Join(p.root, "subdir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, ok := p.classifyNotifyClose(&cfapi.NotifyFileCloseCompletionJob{Path: dir}); ok {
		t.Error("каталог поставлен в очередь выгрузки")
	}

	// Корень игнорируется.
	if _, ok := p.classifyNotifyClose(&cfapi.NotifyFileCloseCompletionJob{Path: p.root}); ok {
		t.Error("корень sync root поставлен в очередь")
	}

	// Вне sync root игнорируется.
	if _, ok := p.classifyNotifyClose(&cfapi.NotifyFileCloseCompletionJob{Path: `C:\elsewhere\other.txt`}); ok {
		t.Error("путь вне sync root поставлен в очередь")
	}

	// Плейсхолдер in-sync игнорируется (design.md D8 шаг 1).
	synced := filepath.Join(p.root, "synced.txt")
	if err := os.WriteFile(synced, []byte("z"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	store.setState(synced, cfapi.PlaceholderStatePlaceholder|cfapi.PlaceholderStateInSync)
	if _, ok := p.classifyNotifyClose(&cfapi.NotifyFileCloseCompletionJob{Path: synced}); ok {
		t.Error("in-sync плейсхолдер поставлен в очередь")
	}

	// Ошибка состояния — игнор (Warn).
	store.mu.Lock()
	store.stateErr = errFake("fake: cldapi недоступна")
	store.mu.Unlock()
	if _, ok := p.classifyNotifyClose(&cfapi.NotifyFileCloseCompletionJob{Path: plain}); ok {
		t.Error("путь с недоступным состоянием поставлен в очередь")
	}
	store.mu.Lock()
	store.stateErr = nil
	store.mu.Unlock()
}

// TestIngestReadOnlyIgnoresClose — спека «Режим без записи» (задача 5.6):
// --read-only игнорирует все close-уведомления.
func TestIngestReadOnlyIgnoresClose(t *testing.T) {
	p, backend, _, _, log := newIngestTestProvider(t)
	p.readOnly = true
	local := filepath.Join(p.root, "ro.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, ok := p.classifyNotifyClose(&cfapi.NotifyFileCloseCompletionJob{Path: local}); ok {
			t.Fatal("read-only поставил задание в очередь")
		}
	}
	if n := len(log.snapshot()); n != 0 {
		t.Errorf("операций backend = %d, ожидается 0", n)
	}
	if backend.creates != 0 {
		t.Errorf("попыток Create = %d, ожидается 0", backend.creates)
	}
}

// TestIngestCoalescing — задача 5.4: уведомления одного пути сливаются,
// задание выходит через delay после последнего уведомления (debounce —
// повторное уведомление отодвигает дедлайн); разные пути выходят отдельно.
func TestIngestCoalescing(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	db := newIngestDebouncer(2*time.Second, clock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		db.run(ctx, stop)
	}()
	defer func() {
		close(stop)
		<-done
	}()

	// Три уведомления одного пути — одно задание после 2 c. Синхронизация
	// по счётчикам коалисера: submit'ы гарантированно обработаны до
	// advance (дедлайн = момент обработки + delay), fire — до проверок.
	a := ingestTask{fsPath: "/a.txt", local: "a", key: "/a.txt"}
	db.submit(a)
	db.submit(a)
	db.submit(a)
	waitFor(t, func() bool { return db.Received() == 3 })
	clock.advance(time.Second)
	waitFor(t, func() bool { return db.FiredConsumed() == 1 })
	assertNoTask(t, db.out) // дедлайн (2 c от последнего уведомления) не наступил
	clock.advance(time.Second)
	waitFor(t, func() bool { return db.FiredConsumed() == 2 })
	if got := recvTask(t, db.out); got.fsPath != "/a.txt" {
		t.Fatalf("задание = %+v, ожидается /a.txt", got)
	}
	assertNoTask(t, db.out) // три уведомления слились в одно

	// Debounce: повторное уведомление отодвигает дедлайн (2 c от него).
	b := ingestTask{fsPath: "/b.txt", local: "b", key: "/b.txt"}
	db.submit(b)
	waitFor(t, func() bool { return db.Received() == 4 }) // t=0, дедлайн 2 c
	clock.advance(1500 * time.Millisecond)                // t=1.5 c — не вышел
	waitFor(t, func() bool { return db.FiredConsumed() == 3 })
	db.submit(b) // обработается ПОСЛЕ fire(1.5 c) — дедлайн 3.5 c
	waitFor(t, func() bool { return db.Received() == 5 })
	clock.advance(600 * time.Millisecond) // t=2.1 c — прежний дедлайн прошёл бы
	waitFor(t, func() bool { return db.FiredConsumed() == 4 })
	assertNoTask(t, db.out)
	clock.advance(1400 * time.Millisecond) // t=3.5 c — отодвинутый дедлайн
	waitFor(t, func() bool { return db.FiredConsumed() == 5 })
	if got := recvTask(t, db.out); got.fsPath != "/b.txt" {
		t.Fatalf("задание = %+v, ожидается /b.txt", got)
	}

	// Разные пути выходят отдельными заданиями.
	db.submit(ingestTask{fsPath: "/c1.txt", local: "c1", key: "/c1.txt"})
	db.submit(ingestTask{fsPath: "/c2.txt", local: "c2", key: "/c2.txt"})
	waitFor(t, func() bool { return db.Received() == 7 })
	clock.advance(2 * time.Second)
	waitFor(t, func() bool { return db.FiredConsumed() == 6 })
	first := recvTask(t, db.out)
	second := recvTask(t, db.out)
	if first.fsPath == second.fsPath {
		t.Fatalf("разные пути слились: %+v и %+v", first, second)
	}
}

// TestIngestSharingViolationRetries — design.md D8 шаг 3: отказ открытия
// локального файла в совместном доступе — задание повторяется, выгрузка
// завершается после освобождения файла.
func TestIngestSharingViolationRetries(t *testing.T) {
	p, backend, _, _, _ := newIngestTestProvider(t)
	local := filepath.Join(p.root, "busy.txt")
	data := []byte("eventual upload")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	// Чужой exclusive-хендл (share mode 0): os.Open получает
	// ERROR_SHARING_VIOLATION.
	path16, err := windows.UTF16PtrFromString(local)
	if err != nil {
		t.Fatalf("utf16: %v", err)
	}
	h, err := windows.CreateFile(path16, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Skipf("не удалось взять exclusive-хендл (окружение): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.runIngest(ctx, ingestTask{fsPath: "/busy.txt", local: local, key: "/busy.txt"})
	}()
	time.Sleep(50 * time.Millisecond)
	if got := backend.content("/busy.txt"); len(got) != 0 {
		_ = windows.CloseHandle(h)
		t.Fatalf("выгрузка выполнена при занятом файле: %d байт", len(got))
	}
	if err := windows.CloseHandle(h); err != nil {
		t.Fatalf("close exclusive handle: %v", err)
	}
	waitFor(t, func() bool { return bytes.Equal(backend.content("/busy.txt"), data) })
	<-done
}

// TestIngestPoolEndToEnd — сквозной прогон: close-уведомление из очереди
// диспетчера → маршрутизатор → коалисер → воркер → backend + in-sync;
// остановка (закрытие stop) завершает все горутины пула.
func TestIngestPoolEndToEnd(t *testing.T) {
	p, backend, store, _, _ := newIngestTestProvider(t)
	d := cfapi.NewDispatcher(cfapi.DefaultQueueOptions())
	p.dispatch = d
	local := filepath.Join(p.root, "pool.txt")
	data := patternBytes(1000)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	// Снимок канала остановки как в Start (без DLL): startIngestPool требует
	// p.stop (в проде его создаёт Start).
	p.stopMu.Lock()
	p.stop = make(chan struct{})
	p.stopMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.startIngestPool(ctx, d)

	if err := d.OnNotifyFileCloseCompletion(&cfapi.NotifyFileCloseCompletionJob{Path: p.root + `\pool.txt`, FileSize: int64(len(data))}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitFor(t, func() bool { return bytes.Equal(backend.content("/pool.txt"), data) })
	waitFor(t, func() bool {
		return store.stateOf(local)&cfapi.PlaceholderStateInSync != 0
	})

	// Остановка: горутины пула завершаются (wg пуст).
	p.stopMu.Lock()
	close(p.stop)
	p.stop = nil
	p.stopMu.Unlock()
	p.wg.Wait()
}

// TestIngestConvertRetryAfterClose — живой ретест этапа 5: фильтр короткое
// время не принимает convert после закрытия записи (0x8007017c), «на
// холодную» тот же convert проходит. Первые две попытки отвергнуты,
// третья успешна: файл доведён до плейсхолдера (index/state), содержимое
// выгружено; промежуточные отказы — Debug, не Warn.
func TestIngestConvertRetryAfterClose(t *testing.T) {
	p, backend, store, idx, _ := newIngestTestProvider(t)
	p.convertRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	local := filepath.Join(p.root, "retry.txt")
	data := patternBytes(100)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	store.mu.Lock()
	store.convertErr = errFake("fake: 0x8007017c (convert rejected after close)")
	store.convertFailures = 2
	store.mu.Unlock()

	p.runIngest(context.Background(), ingestTask{fsPath: "/retry.txt", local: local, key: "/retry.txt"})

	if len(store.converts) != 3 {
		t.Fatalf("попыток convert = %d, ожидается 3 (2 отказа + успех)", len(store.converts))
	}
	ino := backend.inodeOf("/retry.txt")
	if got, ok := idx.Path(ino); !ok || got != "/retry.txt" {
		t.Errorf("idx.Path(%d) = (%q, %v), ожидается /retry.txt", ino, got, ok)
	}
	if st := store.stateOf(local); st != cfapi.PlaceholderStatePlaceholder|cfapi.PlaceholderStateInSync {
		t.Errorf("состояние локального пути = 0x%x, ожидается placeholder|in-sync", st)
	}
	if got := backend.content("/retry.txt"); !bytes.Equal(got, data) {
		t.Errorf("содержимое backend не совпало с локальным")
	}
	if flags := store.converts[len(store.converts)-1].flags; flags != cfapi.ConvertFlagMarkInSync {
		t.Errorf("флаги успешной конверсии = 0x%x, ожидается MARK_IN_SYNC", flags)
	}
}

// TestIngestConvertRetryExhausted — все попытки отвергнуты: Warn
// «conversion pending», содержимое выгружено (не fatal), плейсхолдер НЕ
// выставлен, индекс не получил запись — конверсию повторит следующий
// watcher-MODIFIED либо рестарт.
func TestIngestConvertRetryExhausted(t *testing.T) {
	p, backend, store, idx, log := newIngestTestProvider(t)
	p.convertRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	local := filepath.Join(p.root, "pending.txt")
	data := patternBytes(50)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	store.mu.Lock()
	store.convertErr = errFake("fake: 0x8007017c (convert rejected after close)")
	store.convertFailures = 100
	store.mu.Unlock()

	p.runIngest(context.Background(), ingestTask{fsPath: "/pending.txt", local: local, key: "/pending.txt"})

	// 1 немедленная + 3 повтора.
	if len(store.converts) != len(p.convertRetryDelays)+1 {
		t.Fatalf("попыток convert = %d, ожидается %d", len(store.converts), len(p.convertRetryDelays)+1)
	}
	if st := store.stateOf(local); st != 0 {
		t.Errorf("состояние локального пути = 0x%x, ожидается 0 (вне in-sync)", st)
	}
	if got, ok := idx.Path(backend.inodeOf("/pending.txt")); ok {
		t.Errorf("индекс получил запись при неудавшейся конверсии: %q", got)
	}
	if got := backend.content("/pending.txt"); !bytes.Equal(got, data) {
		t.Errorf("содержимое backend не совпало с локальным (выгрузка не должна пострадать от отказа convert)")
	}
	foundFlush := false
	for _, k := range log.kinds() {
		if k == "flush" {
			foundFlush = true
		}
	}
	if !foundFlush {
		t.Errorf("Flush не зафиксирован: %v", log.snapshot())
	}
}
