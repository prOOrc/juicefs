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
// выгрузка нового файла с заменой на дегидратированный плейсхолдер после
// подтверждённого Flush (U18-α replace-after-upload — конверсия user-файла
// из подключённого serve-процесса отвергается фильтром), изменённый
// плейсхолдер (SetInSync после Flush), пропуск in-sync, отказ Flush без
// финализации (write-back выключен), коалисинг по пути с injectable часами,
// повтор при отказе в совместном доступе, занятость файла при удалении на
// финализации, отказ/гонка пересоздания, сквозной прогон очереди диспетчера.
// cldapi.dll не вызывается — ingestStore, ingestBackend, placeholderStore и
// remover подменные (инвариант «Движок проверяется без CfAPI»).
package cfprovider

import (
	"bytes"
	"context"
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
// Каталоги — карта dirs (этап 6: mkdir/rmdir/rename каталогов). Ошибки
// deleteErr/renameErr/mkdirErr — симуляция недоступного hub (журнал/replay);
// flushGate/writeGate — блокировки для тестов отмены выгрузки (6.5).
type fakeIngestBackend struct {
	mu        sync.Mutex
	log       *opLog
	files     map[string][]byte // существующие записи backend
	dirs      map[string]bool   // существующие каталоги backend
	inodes    map[string]uint64 // inode записи backend по пути
	nextIno   uint64
	flushErr  syscall.Errno // ошибка Flush созданных/открытых файлов
	deleteErr syscall.Errno // ошибка Delete/Rmdir (недоступный hub)
	// deleteErrPaths — отказ Delete по конкретным путям (симуляция частичного
	// сбоя — ротация replayer'а, fix-1.2).
	deleteErrPaths map[string]syscall.Errno
	renameErr      syscall.Errno // ошибка Rename
	mkdirErr       syscall.Errno // ошибка Mkdir
	creates        int
	opens          int
	deletes        int
	rmdirs         int
	mkdirs         int
	renames        int
	flushGate      chan struct{} // Flush блокируется до закрытия канала (6.5)
	writeGate      chan struct{} // первая Pwrite блокируется до закрытия канала
}

func newFakeIngestBackend(log *opLog) *fakeIngestBackend {
	return &fakeIngestBackend{log: log, files: map[string][]byte{}, dirs: map[string]bool{}, inodes: map[string]uint64{}, nextIno: 100}
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

func (b *fakeIngestBackend) Rename(ctx meta.Context, oldpath, newpath string) syscall.Errno {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.renames++
	if b.renameErr != 0 {
		return b.renameErr
	}
	data, ok := b.files[oldpath]
	if !ok {
		if !b.dirs[oldpath] {
			return syscall.ENOENT
		}
		delete(b.dirs, oldpath)
		b.dirs[newpath] = true
		ino := b.inodes[oldpath]
		delete(b.inodes, oldpath)
		b.inodes[newpath] = ino
		b.log.add("rename:" + oldpath + "->" + newpath)
		return 0
	}
	delete(b.files, oldpath)
	b.files[newpath] = data
	ino := b.inodes[oldpath]
	delete(b.inodes, oldpath)
	b.inodes[newpath] = ino
	b.log.add("rename:" + oldpath + "->" + newpath)
	return 0
}

func (b *fakeIngestBackend) Delete(ctx meta.Context, path string) syscall.Errno {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deletes++
	if eno, ok := b.deleteErrPaths[path]; ok {
		return eno
	}
	if b.deleteErr != 0 {
		return b.deleteErr
	}
	if _, ok := b.files[path]; ok {
		delete(b.files, path)
		delete(b.inodes, path)
		b.log.add("delete:" + path)
		return 0
	}
	if b.dirs[path] {
		// Модель fs.Delete (Delete0): каталог — Rmdir; непустой — ENOTEMPTY
		// (деконпозиция executeDeleteOp, fix-1.2).
		for other := range b.dirs {
			if other != path && strings.HasPrefix(other, path+"/") {
				return syscall.ENOTEMPTY
			}
		}
		for other := range b.files {
			if strings.HasPrefix(other, path+"/") {
				return syscall.ENOTEMPTY
			}
		}
		delete(b.dirs, path)
		delete(b.inodes, path)
		b.log.add("rmdir:" + path)
		return 0
	}
	return syscall.ENOENT
}

func (b *fakeIngestBackend) Rmdir(ctx meta.Context, path string) syscall.Errno {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rmdirs++
	if b.deleteErr != 0 {
		return b.deleteErr
	}
	if !b.dirs[path] {
		return syscall.ENOENT
	}
	delete(b.dirs, path)
	delete(b.inodes, path)
	b.log.add("rmdir:" + path)
	return 0
}

func (b *fakeIngestBackend) Mkdir(ctx meta.Context, path string) syscall.Errno {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mkdirs++
	if b.mkdirErr != 0 {
		return b.mkdirErr
	}
	if b.dirs[path] {
		return syscall.EEXIST
	}
	if _, ok := b.files[path]; ok {
		return syscall.EEXIST
	}
	b.nextIno++
	b.dirs[path] = true
	b.inodes[path] = b.nextIno
	b.log.add("mkdir:" + path)
	return 0
}

func (b *fakeIngestBackend) Stat(ctx meta.Context, path string) (uint64, bool, syscall.Errno) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.files[path]; ok {
		return b.inodes[path], false, 0
	}
	if b.dirs[path] {
		return b.inodes[path], true, 0
	}
	return 0, false, syscall.ENOENT
}

func (b *fakeIngestBackend) content(path string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.files[path]...)
}

func (b *fakeIngestBackend) hasDir(path string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dirs[path]
}

func (b *fakeIngestBackend) inodeOf(path string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inodes[path]
}

// fakeIngestFile — подменный открытый файл backend: данные пишутся в память
// сразу, Flush/Close фиксируются в журнале, Flush — с настраиваемой ошибкой;
// flushGate/writeGate backend'а — блокировки тестов отмены (6.5).
type fakeIngestFile struct {
	b        *fakeIngestBackend
	path     string
	ino      uint64
	flushErr syscall.Errno
}

func (f *fakeIngestFile) Pwrite(ctx meta.Context, b []byte, off int64) (int, syscall.Errno) {
	f.b.mu.Lock()
	gate := f.b.writeGate
	f.b.mu.Unlock()
	if gate != nil {
		<-gate
	}
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
	gate := f.b.flushGate
	f.b.mu.Unlock()
	if gate != nil {
		<-gate
	}
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

// fakeIngestStore — подменный ingestStore: STANDARD-состояния путей в карте
// (запись без entry — plain-файл, запрос отвечает ошибкой — модель фильтра),
// SetInSync фиксируется (успех переводит состояние в placeholder|in-sync —
// модель фильтра). Ответы на blocked-уведомления (U19) — ackDelete/ackRename
// фиксируются (ровно один на job, статус).
type fakeIngestStore struct {
	mu          sync.Mutex
	log         *opLog
	states      map[string]uint32
	standard    map[string]*cfapi.CFPlaceholderStandardInfo
	standardErr error
	// standardGate/standardGatePath — симуляция блокирующего STANDARD-запроса
	// (pending-delete, U19): StandardInfo блокируется на gate для пути
	// standardGatePath (пустой — для всех), пока тест не закроет канал.
	standardGate     chan struct{}
	standardGatePath string
	setSyncErr       error
	setSyncs         []string
	deleteAcks       []fakeAck
	renameAcks       []fakeAck
}

// fakeAck — фиксация ответа на blocked-уведомление (U19).
type fakeAck struct {
	conn        cfapi.ConnectionKey
	transferKey cfapi.TransferKey
	requestKey  cfapi.RequestKey
	status      cfapi.NTStatus
}

func newFakeIngestStore(log *opLog) *fakeIngestStore {
	return &fakeIngestStore{log: log, states: map[string]uint32{}, standard: map[string]*cfapi.CFPlaceholderStandardInfo{}}
}

func (s *fakeIngestStore) StandardInfo(path string) (*cfapi.CFPlaceholderStandardInfo, error) {
	s.mu.Lock()
	gate, gatePath := s.standardGate, s.standardGatePath
	s.mu.Unlock()
	if gate != nil && (gatePath == "" || gatePath == path) {
		<-gate // pending-delete: CreateFile блокируется (U19)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.standardErr != nil {
		return nil, s.standardErr
	}
	if info, ok := s.standard[path]; ok {
		return info, nil
	}
	// Нет записи — у пути нет плейсхолдерности (plain-файл).
	return nil, errFake("fake: not a cloud file")
}

// setStandard помечает путь плейсхолдером с заданным CF_IN_SYNC_STATE.
func (s *fakeIngestStore) setStandard(path string, inSync uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.standard[path] = &cfapi.CFPlaceholderStandardInfo{InSyncState: inSync}
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

func (s *fakeIngestStore) AnswerDelete(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, flags uint32, status cfapi.NTStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteAcks = append(s.deleteAcks, fakeAck{conn: conn, transferKey: transferKey, requestKey: requestKey, status: status})
	return nil
}

func (s *fakeIngestStore) AnswerRename(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, flags uint32, status cfapi.NTStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renameAcks = append(s.renameAcks, fakeAck{conn: conn, transferKey: transferKey, requestKey: requestKey, status: status})
	return nil
}

// deleteAckCount/lastDeleteAck/renameAckCount/lastRenameAck — синхронизация
// тестов (ровно один ack на job, статус исхода).
func (s *fakeIngestStore) deleteAckCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deleteAcks)
}

func (s *fakeIngestStore) lastDeleteAck() fakeAck {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteAcks[len(s.deleteAcks)-1]
}

func (s *fakeIngestStore) renameAckCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.renameAcks)
}

func (s *fakeIngestStore) lastRenameAck() fakeAck {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renameAcks[len(s.renameAcks)-1]
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
// задержками (коалисинг 5 мс, retry 1 мс): Start не вызывается. Финализация
// U18-α (пересоздание) идёт через p.store (placeholderStore —
// createEntry-примитив walk на реальном диске); fstore — общая подмена
// store, remover — osRemover (реальное os.Remove).
func newIngestTestProvider(t *testing.T) (*Provider, *fakeIngestBackend, *fakeIngestStore, *fakeStore, *Index, *opLog) {
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
	p.store = &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}, standard: map[string]*cfapi.CFPlaceholderStandardInfo{}}
	p.ingestDelay = 5 * time.Millisecond
	p.ingestRetryDelay = time.Millisecond
	p.ingestMaxRetryDelay = 4 * time.Millisecond
	p.ingestWorkers = 1
	return p, backend, store, p.store.(*fakeStore), idx, log
}

// recreatingRemover — localRemover, который после успешного удаления
// мгновенно создаёт обычный файл по тому же пути (детерминированная
// симуляция гонки «пользователь пересоздал файл» между удалением и
// пересозданием, U18-α: EEXIST-ветка recreateUploaded).
type recreatingRemover struct {
	inner   localRemover
	path    string
	content []byte
}

func (r recreatingRemover) Remove(p string) error {
	if err := r.inner.Remove(p); err != nil {
		return err
	}
	if strings.EqualFold(p, r.path) {
		return os.WriteFile(p, r.content, 0o644)
	}
	return nil
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

// TestIngestNewFileRecreatedAsPlaceholder — спека «Новый файл» + U18-α
// replace-after-upload: содержимое выгружено в backend байт-в-байт, Flush
// подтверждает версию, ПОСЛЕ него локальный файл удалён и пересоздан
// createEntry-примитивом walk (тот же, что в walk — конверсия user-файла
// из подключённого serve фильтр отвергает, U18): конверсия флагами
// MARK_IN_SYNC|DEHYDRATE с identity = inode backend, размер и mtime —
// локальной записи, индекс получил соответствие inode → путь, содержимое
// на диске заменено (дегидратирован по замыслу — контент в backend).
func TestIngestNewFileRecreatedAsPlaceholder(t *testing.T) {
	p, backend, _, fstore, idx, log := newIngestTestProvider(t)
	local := filepath.Join(p.root, "new.txt")
	data := patternBytes(5000)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	mt, err := os.Stat(local)
	if err != nil {
		t.Fatalf("stat local: %v", err)
	}
	mtime0 := mt.ModTime()

	p.runIngest(context.Background(), ingestTask{fsPath: "/new.txt", local: local, key: "/new.txt"})

	if got := backend.content("/new.txt"); !bytes.Equal(got, data) {
		t.Fatalf("содержимое backend не совпало с локальным (%d vs %d байт)", len(got), len(data))
	}
	// Порядок операций backend: create → write… → truncate → flush → close
	// (defer); конверсия пересоздания фиксируется в fstore, не в opLog.
	kinds := log.kinds()
	if len(kinds) == 0 || kinds[0] != "create" || kinds[len(kinds)-1] != "close" {
		t.Fatalf("порядок операций %v, ожидается create…close", kinds)
	}
	if kinds[len(kinds)-2] != "flush" {
		t.Fatalf("замена выполнена до Flush: %v (write-back выключен — финализация только после Flush)", kinds)
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
	// Пересоздание: ровно одна конверсия — флаги MARK_IN_SYNC|DEHYDRATE,
	// identity — inode backend.
	ino := backend.inodeOf("/new.txt")
	converts := fstore.convertCalls()
	if len(converts) != 1 {
		t.Fatalf("конверсий = %d, ожидается 1", len(converts))
	}
	if converts[0].flags != cfapi.ConvertFlagMarkInSync|cfapi.ConvertFlagDehydrate {
		t.Errorf("флаги конверсии = 0x%x, ожидается MARK_IN_SYNC|DEHYDRATE (0x%x)", converts[0].flags, cfapi.ConvertFlagMarkInSync|cfapi.ConvertFlagDehydrate)
	}
	if converts[0].ino != ino {
		t.Errorf("identity конверсии = %d, ожидается inode backend %d", converts[0].ino, ino)
	}
	if converts[0].path != local {
		t.Errorf("путь конверсии = %q, ожидается %q", converts[0].path, local)
	}
	if got, ok := idx.Path(ino); !ok || got != "/new.txt" {
		t.Errorf("idx.Path(%d) = (%q, %v), ожидается /new.txt", ino, got, ok)
	}
	// Локальная запись заменена плейсхолдер-структурой: размер выгруженной
	// версии (SetEndOfFile), mtime локальной записи сохранён (SetFileTime).
	fi, err := os.Stat(local)
	if err != nil {
		t.Fatalf("stat recreated: %v", err)
	}
	if fi.Size() != int64(len(data)) {
		t.Errorf("размер пересозданной записи = %d, ожидается %d", fi.Size(), len(data))
	}
	if fi.ModTime().Unix() != mtime0.Unix() {
		t.Errorf("mtime пересозданной записи = %v, ожидается %v", fi.ModTime(), mtime0)
	}
	// Дегидратирован по замыслу: содержимое на диске не хранится
	// (SetEndOfFile — достоверные нули; контент — в backend, гидрация при
	// доступе — пул FETCH_DATA).
	if got, rerr := os.ReadFile(local); rerr != nil || !bytes.Equal(got, make([]byte, len(data))) {
		t.Errorf("пересозданная запись не дегидратирована: %d байт данных на диске, err=%v", len(got), rerr)
	}
	// Модель фильтра: путь стал плейсхолдером.
	if !fstore.local[local] {
		t.Error("пересозданный путь не отмечен плейсхолдером в модели store")
	}
}

// TestIngestModifiedPlaceholder — спека «Изменённый плейсхолдер»: запись
// backend открыта на запись (Create → EEXIST → OpenWrite), сжатие локальной
// версией доведено Truncate'ом, Flush подтверждает, SetInSync после Flush,
// пересоздания нет (файл уже плейсхолдер), локальная запись не тронута.
func TestIngestModifiedPlaceholder(t *testing.T) {
	p, backend, store, fstore, idx, log := newIngestTestProvider(t)
	local := filepath.Join(p.root, "doc.txt")
	data := []byte("short")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	backend.mu.Lock()
	backend.files["/doc.txt"] = []byte("very old content")
	backend.inodes["/doc.txt"] = 42
	backend.mu.Unlock()
	store.setStandard(local, cfapi.InSyncStateNotInSync) // плейсхолдер вне in-sync

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
	if n := len(fstore.convertCalls()); n != 0 {
		t.Errorf("пересоздание выполнено для существующего плейсхолдера: %d конверсий", n)
	}
	if len(store.setSyncs) != 1 || store.setSyncs[0] != local {
		t.Errorf("SetInSync = %v, ожидается [%s]", store.setSyncs, local)
	}
	if got, ok := idx.Path(42); !ok || got != "/doc.txt" {
		t.Errorf("idx.Path(42) = (%q, %v), ожидается /doc.txt", got, ok)
	}
	// Локальная запись не тронута (SetInSync-ветка без замены).
	if got, err := os.ReadFile(local); err != nil || !bytes.Equal(got, data) {
		t.Errorf("локальный файл изменён: %v", err)
	}
}

// TestIngestInSyncSkipped — плейсхолдер in-sync (создан walk-ом или уже
// выгружен) не выгружается повторно: backend не затрагивается.
func TestIngestInSyncSkipped(t *testing.T) {
	p, backend, store, _, _, log := newIngestTestProvider(t)
	local := filepath.Join(p.root, "synced.txt")
	if err := os.WriteFile(local, []byte("data"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	store.setStandard(local, cfapi.InSyncStateInSync)

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
	p, backend, _, _, _, log := newIngestTestProvider(t)

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
// design.md D9): отказ Flush не финализирует плейсхолдер (ни замены, ни
// SetInSync), индекс не получает запись, локальный файл не изменён и не
// удалён (инвариант U18-α: файл не теряется ДО успешного Flush).
func TestIngestFlushFailureNoFinalize(t *testing.T) {
	p, backend, store, _, idx, log := newIngestTestProvider(t)
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
	if _, err := os.Stat(local); err != nil {
		t.Errorf("локальный файл отсутствует после отказавшего Flush: %v", err)
	}
}

// TestIngestExistingBackendAdoptsLocalPlainFile — имя занято в backend
// (гонка с другим клиентом), локальный файл обычный: содержимое пишется
// поверх существующей записи, после Flush локальный файл заменён
// дегидратированным плейсхолдером (U18-α), identity — inode существующей
// записи.
func TestIngestExistingBackendAdoptsLocalPlainFile(t *testing.T) {
	p, backend, _, fstore, idx, log := newIngestTestProvider(t)
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
	if len(fstore.convertCalls()) != 1 || fstore.convertCalls()[0].ino != 77 {
		t.Fatalf("конверсия = %+v, ожидается convert с identity 77", fstore.convertCalls())
	}
	if flags := fstore.convertCalls()[0].flags; flags != cfapi.ConvertFlagMarkInSync|cfapi.ConvertFlagDehydrate {
		t.Errorf("флаги конверсии = 0x%x, ожидается MARK_IN_SYNC|DEHYDRATE", flags)
	}
	if got, ok := idx.Path(77); !ok || got != "/race.txt" {
		t.Errorf("idx.Path(77) = (%q, %v), ожидается /race.txt", got, ok)
	}
	kinds := log.kinds()
	if len(kinds) < 2 || kinds[0] != "open" || kinds[len(kinds)-1] != "close" {
		t.Errorf("порядок операций %v, ожидается open…close", kinds)
	}
	// Замена: локальная запись — плейсхолдер-структура (размер версии).
	if fi, err := os.Stat(local); err != nil || fi.Size() != int64(len(data)) {
		t.Errorf("пересозданная запись: size=%d err=%v, ожидается %d", fi.Size(), err, len(data))
	}
}

// TestIngestClassifyCloseRouting — классификация close-уведомлений
// (задача 5.2): plain-файл и изменённый плейсхолдер ставятся в очередь,
// in-sync/каталоги/вне корня — игнорируются; путь NormalizedPath без буквы
// диска распознаётся (fsPathOf, живая проба U2). Состояние — честный
// CF_PLACEHOLDER_STANDARD_INFO (после convert-DEHYDRATE FileBasicInfo-
// эвристика возвращает 0 для реального плейсхолдера — живой ретест 27.11).
func TestIngestClassifyCloseRouting(t *testing.T) {
	p, _, store, _, _, _ := newIngestTestProvider(t)

	plain := filepath.Join(p.root, "plain.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	// Нет STANDARD-записи — у файла нет плейсхолдерности (plain-файл,
	// живой ретест 27.11: FileBasicInfo-эвристика после convert-DEHYDRATE
	// ненадёжна — классификация отвечает CfGetPlaceholderInfo): новый файл
	// → to-upload.
	task, ok := p.classifyNotifyClose(context.Background(), &cfapi.NotifyFileCloseCompletionJob{Path: p.root + `\plain.txt`, FileSize: 1})
	if !ok || task.fsPath != "/plain.txt" || task.local != plain || task.key != "/plain.txt" {
		t.Fatalf("plain файл (state=0) не поставлен в очередь: ok=%v task=%+v", ok, task)
	}

	// NormalizedPath без буквы диска (только хвост от корня тома).
	noVol := `\` + strings.TrimPrefix(strings.ToLower(filepath.Clean(p.root)), strings.ToLower(filepath.VolumeName(p.root)))
	modified := filepath.Join(p.root, "modified.txt")
	if err := os.WriteFile(modified, []byte("y"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	store.setStandard(modified, cfapi.InSyncStateNotInSync)
	task, ok = p.classifyNotifyClose(context.Background(), &cfapi.NotifyFileCloseCompletionJob{Path: noVol + `\modified.txt`})
	if !ok || task.fsPath != "/modified.txt" {
		t.Fatalf("путь без диска не распознан: ok=%v task=%+v (normalized=%q)", ok, task, noVol+`\modified.txt`)
	}

	// Каталог игнорируется (создание каталогов — этап 6, design.md D5).
	dir := filepath.Join(p.root, "subdir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, ok := p.classifyNotifyClose(context.Background(), &cfapi.NotifyFileCloseCompletionJob{Path: dir}); ok {
		t.Error("каталог поставлен в очередь выгрузки")
	}

	// Корень игнорируется.
	if _, ok := p.classifyNotifyClose(context.Background(), &cfapi.NotifyFileCloseCompletionJob{Path: p.root}); ok {
		t.Error("корень sync root поставлен в очередь")
	}

	// Вне sync root игнорируется.
	if _, ok := p.classifyNotifyClose(context.Background(), &cfapi.NotifyFileCloseCompletionJob{Path: `C:\elsewhere\other.txt`}); ok {
		t.Error("путь вне sync root поставлен в очередь")
	}

	// Плейсхолдер in-sync игнорируется (design.md D8 шаг 1).
	synced := filepath.Join(p.root, "synced.txt")
	if err := os.WriteFile(synced, []byte("z"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	store.setStandard(synced, cfapi.InSyncStateInSync)
	for i := 0; i < 3; i++ {
		// Повторные close-completion по пересозданной записи (живой ретест
		// 27.11): placeholder+in-sync → ignore, 0 повторных PUT.
		if _, ok := p.classifyNotifyClose(context.Background(), &cfapi.NotifyFileCloseCompletionJob{Path: synced}); ok {
			t.Error("in-sync плейсхолдер поставлен в очередь (повторный PUT)")
		}
	}
}

// TestIngestClassifyQueryErrorAsPlain — отказ STANDARD-запроса (не
// sharing violation — те повторяются в placeholderSyncState) трактуется
// как plain-файл → to-upload (живая семантика фикса U6-эвристики).
func TestIngestClassifyQueryErrorAsPlain(t *testing.T) {
	p, backend, store, _, _, _ := newIngestTestProvider(t)
	local := filepath.Join(p.root, "qerr.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	store.mu.Lock()
	store.standardErr = errFake("fake: cldapi недоступна")
	store.mu.Unlock()
	task, ok := p.classifyNotifyClose(context.Background(), &cfapi.NotifyFileCloseCompletionJob{Path: local})
	if !ok || task.fsPath != "/qerr.txt" {
		t.Fatalf("отказ STANDARD-запроса не поставил plain-файл в очередь: ok=%v task=%+v", ok, task)
	}
	if backend.creates != 0 {
		t.Errorf("классификация не должна трогать backend: creates=%d", backend.creates)
	}
}

// TestIngestReadOnlyIgnoresClose — спека «Режим без записи» (задача 5.6):
// --read-only игнорирует все close-уведомления.
func TestIngestReadOnlyIgnoresClose(t *testing.T) {
	p, backend, _, _, _, log := newIngestTestProvider(t)
	p.readOnly = true
	local := filepath.Join(p.root, "ro.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, ok := p.classifyNotifyClose(context.Background(), &cfapi.NotifyFileCloseCompletionJob{Path: local}); ok {
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
	p, backend, _, _, _, _ := newIngestTestProvider(t)
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
// диспетчера → маршрутизатор → коалисер → воркер → backend + замена на
// плейсхолдер (U18-α); остановка (закрытие stop) завершает все горутины
// пула.
func TestIngestPoolEndToEnd(t *testing.T) {
	p, backend, store, _, idx, _ := newIngestTestProvider(t)
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
	// Замена (U18-α): плейсхолдер пересоздан, индекс получил запись.
	waitFor(t, func() bool {
		got, ok := idx.Path(backend.inodeOf("/pool.txt"))
		return ok && got == "/pool.txt"
	})
	// Пересоздание порождает события watcher'а; на живом фильтре
	// плейсхолдер in-sync игнорируется классификатором — та же модель
	// в подменном store (иначе цикл повторных выгрузок идентичного
	// контента до конца теста).
	store.setStandard(local, cfapi.InSyncStateInSync)

	// Остановка: горутины пула завершаются (wg пуст).
	p.stopMu.Lock()
	close(p.stop)
	p.stop = nil
	p.stopMu.Unlock()
	p.wg.Wait()
}

// TestIngestRemoveBusyRetriesAfterUpload — U18-α: пользователь переоткрыл
// файл (чужой хендл без FILE_SHARE_DELETE) сразу после выгрузки: удаление
// невозможно — повтор с нарастающей паузой до ограничения попыток, задание
// снимается с Warn «pending (file busy)»; контент выгружен, локальный файл
// НЕ удалён. Освобождение файла и следующий close завершают замену.
func TestIngestRemoveBusyRetriesAfterUpload(t *testing.T) {
	p, backend, _, fstore, idx, _ := newIngestTestProvider(t)
	local := filepath.Join(p.root, "busy.txt")
	data := []byte("uploaded content")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	// Чужой хендл: чтение разрешено (выгрузка пройдёт), DELETE — нет
	// (os.Remove получит sharing violation).
	path16, err := windows.UTF16PtrFromString(local)
	if err != nil {
		t.Fatalf("utf16: %v", err)
	}
	h, err := windows.CreateFile(path16, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Skipf("не удалось взять читающий хендл без DELETE-share (окружение): %v", err)
	}
	hook := &logCapture{}
	logger.AddHook(hook)
	task := ingestTask{fsPath: "/busy.txt", local: local, key: "/busy.txt"}

	p.runIngest(context.Background(), task)
	if err := windows.CloseHandle(h); err != nil {
		t.Fatalf("close holder handle: %v", err)
	}

	// Контент выгружен (Flush подтверждён), файл не удалён, пересоздания
	// не было, задание снято (Warn; занятость — не отказ пересоздания).
	if got := backend.content("/busy.txt"); !bytes.Equal(got, data) {
		t.Fatalf("содержимое backend = %q, ожидается %q", got, data)
	}
	if got, err := os.ReadFile(local); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("локальный файл изменён/удалён при занятости: %v", err)
	}
	if n := len(fstore.convertCalls()); n != 0 {
		t.Errorf("пересоздание выполнено при занятом файле: %d конверсий", n)
	}
	if n := hook.count("pending (file busy), content uploaded, will retry on next close/restart"); n != 1 {
		t.Errorf("строк Warn «pending (file busy)»: %d, ожидается 1", n)
	}
	if p.ingestFailed.Load() != 0 {
		t.Errorf("failed = %d, ожидается 0 (занятость — не отказ пересоздания)", p.ingestFailed.Load())
	}
	if _, ok := idx.Path(backend.inodeOf("/busy.txt")); ok {
		t.Error("индекс получил запись при занятом файле")
	}

	// Следующий close (файл свободен) — замена завершается.
	p.runIngest(context.Background(), task)
	converts := fstore.convertCalls()
	if len(converts) != 1 {
		t.Fatalf("после освобождения файла конверсий = %d, ожидается 1", len(converts))
	}
	if flags := converts[0].flags; flags != cfapi.ConvertFlagMarkInSync|cfapi.ConvertFlagDehydrate {
		t.Errorf("флаги конверсии = 0x%x, ожидается MARK_IN_SYNC|DEHYDRATE", flags)
	}
	if got, ok := idx.Path(backend.inodeOf("/busy.txt")); !ok || got != "/busy.txt" {
		t.Errorf("idx.Path = (%q, %v), ожидается /busy.txt", got, ok)
	}
}

// TestIngestRecreateFailureKeepsBackend — отказ пересоздания (U18-α):
// createEntry откатывает созданную запись (ни старого файла, ни
// неконвертированной записи — восстановление без данных невозможно),
// failed++, Warn; контент выгружен в backend — не теряется, повторит
// следующий close/рестарт; индекс не получил запись.
func TestIngestRecreateFailureKeepsBackend(t *testing.T) {
	p, backend, _, _, idx, _ := newIngestTestProvider(t)
	local := filepath.Join(p.root, "recreate-fail.txt")
	data := patternBytes(64)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	p.store = &convertFailStore{
		fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}},
		failPaths: map[string]error{local: errFake("fake: convert failed")},
	}
	hook := &logCapture{}
	logger.AddHook(hook)

	p.runIngest(context.Background(), ingestTask{fsPath: "/recreate-fail.txt", local: local, key: "/recreate-fail.txt"})

	// Контент в backend (fake-состояние) — ничего не потеряно.
	if got := backend.content("/recreate-fail.txt"); !bytes.Equal(got, data) {
		t.Fatalf("содержимое backend не совпало с локальным (контент должен быть выгружен)")
	}
	// failed++ и Warn.
	if p.ingestFailed.Load() != 1 {
		t.Errorf("failed = %d, ожидается 1 (отказ пересоздания)", p.ingestFailed.Load())
	}
	if n := hook.count("recreate placeholder"); n != 1 {
		t.Errorf("строк Warn о пересоздании: %d, ожидается 1", n)
	}
	// Откат createEntry: ни старого файла, ни неконвертированной записи.
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Errorf("сбойная запись не удалена: stat err = %v", err)
	}
	if _, ok := idx.Path(backend.inodeOf("/recreate-fail.txt")); ok {
		t.Error("индекс получил запись при отказе пересоздания")
	}
}

// TestIngestRecreateExistsConflict — EEXIST при пересоздании (U18-α:
// пользователь пересоздал обычный файл между удалением и созданием):
// переклассификация как в walk — конфликт; файл пользователя не тронут,
// индекс не получил запись, повторной конверсии не было, контент выгружен.
func TestIngestRecreateExistsConflict(t *testing.T) {
	p, backend, _, fstore, idx, _ := newIngestTestProvider(t)
	local := filepath.Join(p.root, "clash.txt")
	data := []byte("uploaded content")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	// После успешного удаления путь мгновенно занят пересозданным файлом
	// пользователя (детерминированная симуляция гонки «между remove и
	// create» — подмена remover'а); модель фильтра: обычный файл.
	p.remover = recreatingRemover{inner: osRemover{}, path: local, content: []byte("user remade")}
	fstore.localReal[local] = true

	p.runIngest(context.Background(), ingestTask{fsPath: "/clash.txt", local: local, key: "/clash.txt"})

	if got := backend.content("/clash.txt"); !bytes.Equal(got, data) {
		t.Fatalf("содержимое backend не совпало с локальным")
	}
	if got, err := os.ReadFile(local); err != nil || !bytes.Equal(got, []byte("user remade")) {
		t.Errorf("файл пользователя изменён: %q, %v", got, err)
	}
	if _, ok := idx.Path(backend.inodeOf("/clash.txt")); ok {
		t.Error("конфликт попал в индекс")
	}
	if n := len(fstore.convertCalls()); n != 0 {
		t.Errorf("EEXIST-ветка конвертировала запись: %d вызовов", n)
	}
}

// TestIngestRecreateExistsAdoptsOwnPlaceholder — EEXIST при пересоздании на
// СВОЁМ плейсхолдере (гонка с параллельным walk, U18-α): переклассификация
// как в walk восстанавливает запись в индекс без конверсии и дегидратации.
func TestIngestRecreateExistsAdoptsOwnPlaceholder(t *testing.T) {
	p, backend, _, fstore, idx, _ := newIngestTestProvider(t)
	local := filepath.Join(p.root, "adopt.txt")
	data := []byte("uploaded content")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	p.remover = recreatingRemover{inner: osRemover{}, path: local, content: []byte("user remade")}
	// Модель фильтра: путь уже наш плейсхолдер (создан параллельным walk),
	// дегидратированный (OnDiskDataSize 0 — дефолт подменного store).
	fstore.local[local] = true

	p.runIngest(context.Background(), ingestTask{fsPath: "/adopt.txt", local: local, key: "/adopt.txt"})

	ino := backend.inodeOf("/adopt.txt")
	if got, ok := idx.Path(ino); !ok || got != "/adopt.txt" {
		t.Errorf("idx.Path(%d) = (%q, %v), ожидается /adopt.txt (переклассифицирован в existing)", ino, got, ok)
	}
	if n := len(fstore.convertCalls()); n != 0 {
		t.Errorf("переклассификация сконвертировала запись: %d вызовов", n)
	}
	if n := len(fstore.updateCalls()); n != 0 {
		t.Errorf("переклассификация дегидратировала запись: %d вызовов", n)
	}
	if got := backend.content("/adopt.txt"); !bytes.Equal(got, data) {
		t.Errorf("содержимое backend не совпало с локальным")
	}
}

// TestIngestSelfEventSuppression — супрессия собственных событий watcher'а
// (U18-α replace-after-upload, живой ретест 27.11: remove+create
// финализацией классифицировались «plain → upload» — петля повторных
// выгрузок). Событие в окне (10 c от финализации) — ignore, backend не
// тронут; после истечения окна — честная классификация по STANDARD
// (fake: нет записи → plain → to-upload). Часы injectable.
func TestIngestSelfEventSuppression(t *testing.T) {
	p, backend, _, _, _, _ := newIngestTestProvider(t)
	clock := newFakeClock(time.Unix(0, 0))
	p.clock = clock
	local := filepath.Join(p.root, "self.txt")
	data := patternBytes(100)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}

	// Полный цикл выгрузки с replace-after-upload: финализация ставит окно
	// супрессии на путь.
	p.runIngest(context.Background(), ingestTask{fsPath: "/self.txt", local: local, key: "/self.txt"})

	job := &cfapi.NotifyFileCloseCompletionJob{Path: local}
	// Собственные события recreate (в окне) — ignore, 0 повторных PUT.
	for i := 0; i < 3; i++ {
		if _, ok := p.classifyNotifyClose(context.Background(), job); ok {
			t.Fatal("собственное событие в окне супрессии поставлено в очередь (петля повторных выгрузок)")
		}
	}
	if n := backend.creates; n != 1 {
		t.Errorf("попыток Create = %d, ожидается 1 (исходная выгрузка; дубликатов нет)", n)
	}

	// После истечения окна — честная классификация: решение по STANDARD
	// (в подмене нет записи → plain → to-upload; на живом фильтре
	// пересозданный плейсхолдер ответил бы in-sync → ignore).
	clock.advance(selfEventSuppressWindow + time.Second)
	if _, ok := p.classifyNotifyClose(context.Background(), job); !ok {
		t.Fatal("после истечения окна классификация не вернула честное решение")
	}
}

// TestIngestRecreateExistsAdoptsHydratedHonestMetadata — ревью этапа 5
// (мажор 2): EEXIST-усыновление СВОЕГО ГИДРАТИРОВАННОГО плейсхолдера
// (OnDiskDataSize > 0) вызывает ensureDehydrated → Update с ПОЛНЫМИ
// метаданными записи — size/mtime локальной версии на момент выгрузки
// (урезанный entry дегидратировал бы в нулевой размер/эпоху).
func TestIngestRecreateExistsAdoptsHydratedHonestMetadata(t *testing.T) {
	p, backend, _, fstore, idx, _ := newIngestTestProvider(t)
	local := filepath.Join(p.root, "adopt-hydrated.txt")
	data := []byte("uploaded content")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	fi, err := os.Stat(local)
	if err != nil {
		t.Fatalf("stat local: %v", err)
	}
	modTime := fi.ModTime()
	p.remover = recreatingRemover{inner: osRemover{}, path: local, content: []byte("user remade")}
	// Модель фильтра: путь уже наш плейсхолдер, ГИДРАТИРОВАННЫЙ
	// (OnDiskDataSize > 0) — ensureDehydrated обязан дегидратировать
	// повторным Update с честными метаданными.
	fstore.local[local] = true
	fstore.standard[local] = &cfapi.CFPlaceholderStandardInfo{OnDiskDataSize: 12345}

	p.runIngest(context.Background(), ingestTask{fsPath: "/adopt-hydrated.txt", local: local, key: "/adopt-hydrated.txt"})

	updates := fstore.updateCalls()
	if len(updates) != 1 {
		t.Fatalf("Update вызовов = %d, ожидается 1 (дегидратация усыновлённого)", len(updates))
	}
	u := updates[0]
	if u.path != local {
		t.Errorf("Update path = %q, ожидается %q", u.path, local)
	}
	if u.size != int64(len(data)) {
		t.Errorf("Update size = %d, ожидается %d (честный размер локальной версии)", u.size, len(data))
	}
	ft := windows.NsecToFiletime(modTime.Unix()*1e9 + int64(uint32(modTime.Nanosecond())))
	wantLastWrite := int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
	if u.lastWriteTime != wantLastWrite {
		t.Errorf("Update LastWriteTime = %d, ожидается %d (честный mtime локальной версии)", u.lastWriteTime, wantLastWrite)
	}
	if u.flags != cfapi.UpdateFlagDehydrate|cfapi.UpdateFlagMarkInSync {
		t.Errorf("Update flags = 0x%x, ожидается DEHYDRATE|MARK_IN_SYNC", u.flags)
	}
	ino := backend.inodeOf("/adopt-hydrated.txt")
	if got, ok := idx.Path(ino); !ok || got != "/adopt-hydrated.txt" {
		t.Errorf("idx.Path(%d) = (%q, %v), ожидается /adopt-hydrated.txt (усыновлён)", ino, got, ok)
	}
}

// --- Этап 6: распространение операций (задачи 6.1–6.2), отмена выгрузки
// --- (задача 6.5), журнал (задача 6.3).

// Тестовые ключи blocked-уведомлений (U19): обработчик обязана ответить
// ровно один раз — факт и статус фиксирует fakeIngestStore.
var (
	testConn     = cfapi.ConnectionKey{Internal: 7}
	testTransfer = cfapi.TransferKey(8)
	testRequest  = cfapi.RequestKey(9)
)

// notifyJournalTestProvider — провайдер этапа 6 с журналом (без replayer —
// исполнение операций вызывается синхронно из теста).
func notifyJournalTestProvider(t *testing.T) (*Provider, *fakeIngestBackend, *fakeIngestStore, *fakeStore, *Index, *Journal, *opLog) {
	t.Helper()
	p, backend, store, fstore, idx, log := newIngestTestProvider(t)
	j, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	p.journal = j
	return p, backend, store, fstore, idx, j, log
}

// TestNotifyDeleteKnownPlaceholder — удаление известного плейсхолдера
// доходит до backend (спека «Удаление плейсхолдера»), индекс чистится,
// ровно один ACK_DELETE со статусом успеха (U19: без него rm висит).
func TestNotifyDeleteKnownPlaceholder(t *testing.T) {
	p, backend, store, _, idx, j, _ := notifyJournalTestProvider(t)
	if err := idx.Put(42, "/del.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	backend.mu.Lock()
	backend.files["/del.txt"] = []byte("x")
	backend.inodes["/del.txt"] = 42
	backend.mu.Unlock()

	p.notifyDeletePath(filepath.Join(p.root, "del.txt"), 42, false, testConn, testTransfer, testRequest)

	if backend.deletes != 1 {
		t.Fatalf("deletes = %d, ожидается 1", backend.deletes)
	}
	if len(backend.content("/del.txt")) != 0 {
		t.Error("backend-запись не удалена")
	}
	if _, ok := idx.Path(42); ok {
		t.Error("индекс не почищен")
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пусто (исполнено сразу)", j.Ops())
	}
	// РОВНО ОДИН ack на job со статусом успеха и ключами уведомления (U19).
	if n := store.deleteAckCount(); n != 1 {
		t.Fatalf("ack'ов = %d, ожидается ровно 1", n)
	}
	ack := store.lastDeleteAck()
	if ack.status != cfapi.StatusSuccess {
		t.Errorf("ack status = 0x%x, ожидается StatusSuccess", ack.status)
	}
	if ack.conn != testConn || ack.transferKey != testTransfer || ack.requestKey != testRequest {
		t.Errorf("ack ключи = %+v, ожидаются тестовые", ack)
	}
}

// TestNotifyDeleteUnknownIgnored — неизвестная запись (локальный plain-файл
// пользователя, инвариант задачи 6.2) в backend не передаётся никогда.
func TestNotifyDeleteUnknownIgnored(t *testing.T) {
	p, backend, store, _, _, j, _ := notifyJournalTestProvider(t)
	local := filepath.Join(p.root, "plain-user.txt")
	if err := os.WriteFile(local, []byte("user data"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}

	p.notifyDeletePath(local, 0, false, testConn, testTransfer, testRequest)

	if backend.deletes != 0 {
		t.Errorf("deletes = %d, ожидается 0 (неизвестная запись)", backend.deletes)
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пусто", j.Ops())
	}
	if _, err := os.Stat(local); err != nil {
		t.Errorf("локальный файл пользователя затронут: %v", err)
	}
	// Неизвестное (plain) — ack Success: локальное удаление не блокируется.
	if n := store.deleteAckCount(); n != 1 {
		t.Fatalf("ack'ов = %d, ожидается ровно 1", n)
	}
	if ack := store.lastDeleteAck(); ack.status != cfapi.StatusSuccess {
		t.Errorf("ack status = 0x%x, ожидается StatusSuccess (не блокировать пользователя)", ack.status)
	}
}

// TestNotifyDeleteSyncRootNever — инвариант (спека «Удаление sync root»):
// удаление sync root НИКОГДА не передаётся в backend; Warn однократный.
func TestNotifyDeleteSyncRootNever(t *testing.T) {
	p, backend, store, _, _, j, _ := notifyJournalTestProvider(t)
	hook := &logCapture{}
	logger.AddHook(hook)
	if err := p.idx.Put(1, "/"); err != nil {
		t.Fatalf("idx.Put корня: %v", err)
	}
	backend.mu.Lock()
	backend.dirs["/"] = true
	backend.inodes["/"] = 1
	backend.mu.Unlock()

	for i := 0; i < 3; i++ {
		p.notifyDeletePath(p.root, 1, i == 2, testConn, testTransfer, testRequest) // и как файл, и как каталог
	}
	if backend.deletes != 0 || backend.rmdirs != 0 {
		t.Errorf("backend затронут: deletes=%d rmdirs=%d, ожидается 0/0", backend.deletes, backend.rmdirs)
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пусто", j.Ops())
	}
	if n := hook.count("never propagated to the backend"); n != 1 {
		t.Errorf("строк Warn «never propagated»: %d, ожидается 1 (однократный)", n)
	}
	// Каждый вызов — ровно один ack Success (rm корня не блокируется).
	if n := store.deleteAckCount(); n != 3 {
		t.Errorf("ack'ов = %d, ожидается 3 (по одному на уведомление)", n)
	}
	if ack := store.lastDeleteAck(); ack.status != cfapi.StatusSuccess {
		t.Errorf("ack status = 0x%x, ожидается StatusSuccess", ack.status)
	}
}

// TestNotifyDeleteOfflineAcksError — U19 (blocked-style) + спека «Удаление
// офлайн»: hub недоступен — ack с кодом отказа (rm пользователя завершается
// ошибкой, локальная запись ОСТАЁТСЯ — состояние консистентно), backend и
// индекс не тронуты, журнала нет (повтор — rm пользователя после
// восстановления; «после повтора» ls не содержит запись).
func TestNotifyDeleteOfflineAcksError(t *testing.T) {
	p, backend, store, _, idx, j, _ := notifyJournalTestProvider(t)
	if err := idx.Put(43, "/off.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	backend.mu.Lock()
	backend.files["/off.txt"] = []byte("x")
	backend.inodes["/off.txt"] = 43
	backend.deleteErr = syscall.EIO // hub недоступен
	backend.mu.Unlock()

	p.notifyDeletePath(filepath.Join(p.root, "off.txt"), 43, false, testConn, testTransfer, testRequest)

	// Ровно один ack с кодом отказа (EIO → StatusIODeviceError); ключи job
	// доставлены без изменений.
	if n := store.deleteAckCount(); n != 1 {
		t.Fatalf("ack'ов = %d, ожидается ровно 1", n)
	}
	ack := store.lastDeleteAck()
	if ack.status != cfapi.StatusFromErrno(syscall.EIO) {
		t.Errorf("ack status = 0x%x, ожидается StatusFromErrno(EIO) 0x%x", ack.status, cfapi.StatusFromErrno(syscall.EIO))
	}
	if ack.conn != testConn || ack.transferKey != testTransfer || ack.requestKey != testRequest {
		t.Errorf("ack ключи = %+v, ожидаются тестовые", ack)
	}
	// Backend и индекс не тронуты (rm не удался — расхождения нет).
	if len(backend.content("/off.txt")) == 0 {
		t.Error("backend-запись удалена при недоступном hub")
	}
	if _, ok := idx.Path(43); !ok {
		t.Error("индекс потерял запись")
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пуст (локальный rm не удался — распространять нечего)", j.Ops())
	}
}

// TestNotifyRenameKnownPlaceholder — спека «Переименование»: известный
// плейсхолдер переименовывается в backend, индекс переносится на новый путь
// (inode сохраняется, пере-резолв через backend Stat), журнал пуст.
func TestNotifyRenameKnownPlaceholder(t *testing.T) {
	p, backend, store, _, idx, j, _ := notifyJournalTestProvider(t)
	data := []byte("renamed content")
	if err := idx.Put(9, "/old.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	backend.mu.Lock()
	backend.files["/old.txt"] = data
	backend.inodes["/old.txt"] = 9
	backend.mu.Unlock()
	store.setStandard(filepath.Join(p.root, "new.txt"), cfapi.InSyncStateInSync)

	p.notifyRenamePaths(context.Background(), filepath.Join(p.root, "old.txt"), filepath.Join(p.root, "new.txt"), 9, false, testConn, testTransfer, testRequest)

	if backend.renames != 1 {
		t.Fatalf("renames = %d, ожидается 1", backend.renames)
	}
	if got := backend.content("/new.txt"); string(got) != string(data) {
		t.Errorf("содержимое /new.txt = %q, ожидается %q", got, data)
	}
	if len(backend.content("/old.txt")) != 0 {
		t.Error("старая запись backend не переехала")
	}
	if got, ok := idx.Path(9); !ok || got != "/new.txt" {
		t.Errorf("idx.Path(9) = (%q, %v), ожидается /new.txt (inode сохранён)", got, ok)
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пусто", j.Ops())
	}
	// РОВНО ОДИН ACK_RENAME со статусом успеха (U19: без него mv висит).
	if n := store.renameAckCount(); n != 1 {
		t.Fatalf("ack'ов = %d, ожидается ровно 1", n)
	}
	if ack := store.lastRenameAck(); ack.status != cfapi.StatusSuccess {
		t.Errorf("ack status = 0x%x, ожидается StatusSuccess", ack.status)
	}
}

// TestNotifyRenameUnknownIgnored — переименование не-плейсхолдера в backend
// не передаётся (задача 6.2); ack Success — локальный mv не блокируется.
func TestNotifyRenameUnknownIgnored(t *testing.T) {
	p, backend, store, _, _, j, _ := notifyJournalTestProvider(t)
	if err := os.WriteFile(filepath.Join(p.root, "u1.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p.root, "u2.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	p.notifyRenamePaths(context.Background(), filepath.Join(p.root, "u1.txt"), filepath.Join(p.root, "u2.txt"), 0, false, testConn, testTransfer, testRequest)

	if backend.renames != 0 {
		t.Errorf("renames = %d, ожидается 0", backend.renames)
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пусто", j.Ops())
	}
	if n := store.renameAckCount(); n != 1 {
		t.Fatalf("ack'ов = %d, ожидается ровно 1", n)
	}
	if ack := store.lastRenameAck(); ack.status != cfapi.StatusSuccess {
		t.Errorf("ack status = 0x%x, ожидается StatusSuccess", ack.status)
	}
}

// TestNotifyRenameOfflineAcksError — U19 (blocked-style): hub недоступен —
// ack с кодом отказа (mv пользователя завершается ошибкой, запись остаётся
// на старом имени локально и в backend — состояние консистентно), журнал
// пуст (повтор — mv пользователя после восстановления).
func TestNotifyRenameOfflineAcksError(t *testing.T) {
	p, backend, store, _, idx, j, _ := notifyJournalTestProvider(t)
	if err := idx.Put(11, "/r1.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	backend.mu.Lock()
	backend.files["/r1.txt"] = []byte("x")
	backend.inodes["/r1.txt"] = 11
	backend.renameErr = syscall.EIO
	backend.mu.Unlock()

	p.notifyRenamePaths(context.Background(), filepath.Join(p.root, "r1.txt"), filepath.Join(p.root, "r2.txt"), 11, false, testConn, testTransfer, testRequest)

	if n := store.renameAckCount(); n != 1 {
		t.Fatalf("ack'ов = %d, ожидается ровно 1", n)
	}
	if ack := store.lastRenameAck(); ack.status != cfapi.StatusFromErrno(syscall.EIO) {
		t.Errorf("ack status = 0x%x, ожидается StatusFromErrno(EIO)", ack.status)
	}
	// Backend и индекс не тронуты (mv не удался).
	if len(backend.content("/r1.txt")) == 0 || len(backend.content("/r2.txt")) != 0 {
		t.Error("backend-запись переименована при недоступном hub")
	}
	if got, ok := idx.Path(11); !ok || got != "/r1.txt" {
		t.Errorf("idx.Path(11) = (%q, %v), ожидается /r1.txt", got, ok)
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пуст (локальный mv не удался)", j.Ops())
	}
}

// TestNotifyDirCreate — спека «Новый каталог»: пользовательский каталог
// создаётся в backend (Mkdir), конвертируется в плейсхолдер (MARK_IN_SYNC),
// попадает в индекс.
func TestNotifyDirCreate(t *testing.T) {
	p, backend, _, fstore, idx, j, _ := notifyJournalTestProvider(t)
	local := filepath.Join(p.root, "brand-new")
	if err := os.Mkdir(local, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	p.notifyDirCreate(context.Background(), local)

	if backend.mkdirs != 1 {
		t.Fatalf("mkdirs = %d, ожидается 1", backend.mkdirs)
	}
	if !backend.hasDir("/brand-new") {
		t.Error("каталог не создан в backend")
	}
	converts := fstore.convertCalls()
	if len(converts) != 1 || !converts[0].isDir || converts[0].flags != cfapi.ConvertFlagMarkInSync {
		t.Fatalf("конверсии = %+v, ожидается конверсия каталога MARK_IN_SYNC", converts)
	}
	ino, _, _ := backend.Stat(noopCtx(), "/brand-new")
	if got, ok := idx.Path(ino); !ok || got != "/brand-new" {
		t.Errorf("idx.Path(%d) = (%q, %v), ожидается /brand-new", ino, got, ok)
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пусто", j.Ops())
	}
}

// TestNotifyDirCreateKnownSkip — каталог, уже известный индексу (создан
// walk-ом), повторно не создаётся и не конвертируется.
func TestNotifyDirCreateKnownSkip(t *testing.T) {
	p, backend, _, fstore, _, j, _ := notifyJournalTestProvider(t)
	if err := p.idx.Put(77, "/walkdir"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	local := filepath.Join(p.root, "walkdir")
	if err := os.Mkdir(local, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	p.notifyDirCreate(context.Background(), local)

	if backend.mkdirs != 0 {
		t.Errorf("mkdirs = %d, ожидается 0 (известный плейсхолдер)", backend.mkdirs)
	}
	if n := len(fstore.convertCalls()); n != 0 {
		t.Errorf("конверсий = %d, ожидается 0", n)
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пусто", j.Ops())
	}
}

// TestNotifyDirCreateOfflineJournals — hub недоступен: mkdir в журнале;
// исполнение создаёт каталог в backend и доводит локальный до плейсхолдера.
func TestNotifyDirCreateOfflineJournals(t *testing.T) {
	p, backend, _, fstore, idx, j, _ := notifyJournalTestProvider(t)
	local := filepath.Join(p.root, "offline-dir")
	if err := os.Mkdir(local, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	backend.mu.Lock()
	backend.mkdirErr = syscall.EIO
	backend.mu.Unlock()

	p.notifyDirCreate(context.Background(), local)

	ops := j.Ops()
	if len(ops) != 1 || ops[0].Type != OpMkdir || ops[0].Path != "/offline-dir" {
		t.Fatalf("журнал = %+v, ожидается mkdir /offline-dir", ops)
	}
	backend.mu.Lock()
	backend.mkdirErr = 0
	backend.mu.Unlock()
	if err := p.executeJournalOp(context.Background(), ops[0]); err != nil {
		t.Fatalf("executeJournalOp: %v", err)
	}
	if !backend.hasDir("/offline-dir") {
		t.Error("каталог не создан в backend после восстановления")
	}
	if n := len(fstore.convertCalls()); n != 1 || !fstore.convertCalls()[0].isDir {
		t.Errorf("конверсии = %+v, ожидается одна конверсия каталога", fstore.convertCalls())
	}
	ino, _, _ := backend.Stat(noopCtx(), "/offline-dir")
	if _, ok := idx.Path(ino); !ok {
		t.Error("каталог не попал в индекс")
	}
}

// TestNotifyDirDelete — удаление каталога-плейсхолдера: Rmdir в backend,
// индекс чистится с потомками (DeletePrefix).
func TestNotifyDirDelete(t *testing.T) {
	p, backend, store, _, idx, _, _ := notifyJournalTestProvider(t)
	if err := idx.Put(5, "/gone-dir"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	if err := idx.Put(6, "/gone-dir/inner.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	backend.mu.Lock()
	backend.dirs["/gone-dir"] = true
	backend.inodes["/gone-dir"] = 5
	backend.mu.Unlock()

	p.notifyDeletePath(filepath.Join(p.root, "gone-dir"), 0, true, testConn, testTransfer, testRequest)

	if backend.rmdirs != 1 {
		t.Fatalf("rmdirs = %d, ожидается 1", backend.rmdirs)
	}
	if backend.hasDir("/gone-dir") {
		t.Error("каталог backend не удалён")
	}
	if _, ok := idx.Path(5); ok {
		t.Error("индекс каталога не почищен")
	}
	if _, ok := idx.Path(6); ok {
		t.Error("потомок каталога не снят с индекса (DeletePrefix)")
	}
	// Каталог идёт через тот же ACK_DELETE (NOTIFY_DELETE + IS_DIRECTORY).
	if n := store.deleteAckCount(); n != 1 {
		t.Fatalf("ack'ов = %d, ожидается ровно 1", n)
	}
	if ack := store.lastDeleteAck(); ack.status != cfapi.StatusSuccess {
		t.Errorf("ack status = 0x%x, ожидается StatusSuccess", ack.status)
	}
}

// TestNotifyDirRename — переименование каталога-плейсхолдера: backend
// rename, пути потомков в индексе переносит RenamePrefix.
func TestNotifyDirRename(t *testing.T) {
	p, backend, store, _, idx, _, _ := notifyJournalTestProvider(t)
	backend.mu.Lock()
	backend.dirs["/odir"] = true
	backend.inodes["/odir"] = 5
	backend.mu.Unlock()
	if err := idx.Put(5, "/odir"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	if err := idx.Put(6, "/odir/kid.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}

	p.notifyRenamePaths(context.Background(), filepath.Join(p.root, "odir"), filepath.Join(p.root, "ndir"), 0, true, testConn, testTransfer, testRequest)

	if backend.renames != 1 {
		t.Fatalf("renames = %d, ожидается 1", backend.renames)
	}
	if !backend.hasDir("/ndir") || backend.hasDir("/odir") {
		t.Error("каталог backend не переименован")
	}
	if got, ok := idx.Path(5); !ok || got != "/ndir" {
		t.Errorf("idx.Path(5) = (%q, %v), ожидается /ndir", got, ok)
	}
	if got, ok := idx.Path(6); !ok || got != "/ndir/kid.txt" {
		t.Errorf("idx.Path(6) = (%q, %v), ожидается /ndir/kid.txt", got, ok)
	}
	// Каталог-rename идёт через тот же ACK_RENAME (NOTIFY_RENAME + IS_DIRECTORY).
	if n := store.renameAckCount(); n != 1 {
		t.Fatalf("ack'ов = %d, ожидается ровно 1", n)
	}
	if ack := store.lastRenameAck(); ack.status != cfapi.StatusSuccess {
		t.Errorf("ack status = 0x%x, ожидается StatusSuccess", ack.status)
	}
}

// TestUploadCanceledAfterFlushJournalsDelete — задача 6.5 (design.md D9
// «Удаление локального файла с незавершённой выгрузкой»): REMOVED в окне
// выгрузки ПОСЛЕ подтверждённого Flush — версия уже в backend, в журнал
// ставится удаление по inode; исполнение оставляет backend без файла.
func TestUploadCanceledAfterFlushJournalsDelete(t *testing.T) {
	p, backend, _, fstore, _, j, log := notifyJournalTestProvider(t)
	local := filepath.Join(p.root, "cancel.txt")
	data := patternBytes(200)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	// Flush блокируется до команды теста — окно «версия подтверждается».
	gate := make(chan struct{})
	backend.mu.Lock()
	backend.flushGate = gate
	backend.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- p.runIngest(ctx, ingestTask{fsPath: "/cancel.txt", local: local, key: "/cancel.txt"})
	}()
	waitFor(t, func() bool { return containsKind(log, "truncate") }) // до Flush дошёл

	// Пользователь удаляет файл в окне выгрузки → REMOVED. Реальное окно
	// удаления — до открытия/после закрытия read-хендла (os.Open не несёт
	// FILE_SHARE_DELETE — само удаление блокируется до закрытия; событие
	// REMOVED доходит до handleLocalRemoved асинхронно в любом случае), в
	// тесте — прямая подача события.
	p.handleLocalRemoved(local)

	// Выгрузка завершается (Flush подтверждает версию).
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("runIngest = %v, ожидается nil", err)
	}
	// Финализации не было (файл удалён), журнал: удаление по inode.
	if n := len(fstore.convertCalls()); n != 0 {
		t.Errorf("конверсий = %d, ожидается 0 (финализация отменена)", n)
	}
	ops := j.Ops()
	if len(ops) != 1 || ops[0].Type != OpDelete || ops[0].Path != "/cancel.txt" {
		t.Fatalf("журнал = %+v, ожидается delete /cancel.txt", ops)
	}
	if ops[0].Ino == 0 || ops[0].Ino != backend.inodeOf("/cancel.txt") {
		t.Errorf("op.Ino = %d, ожидается inode backend %d", ops[0].Ino, backend.inodeOf("/cancel.txt"))
	}
	// Локальная запись удалена (read-хендл закрыт — теперь удаление возможно);
	// исполнение очереди: backend без файла (DoD «backend без файла»).
	if err := os.Remove(local); err != nil {
		t.Fatalf("remove local после выгрузки: %v", err)
	}
	if err := p.executeJournalOp(context.Background(), ops[0]); err != nil {
		t.Fatalf("executeJournalOp: %v", err)
	}
	if len(backend.content("/cancel.txt")) != 0 {
		t.Error("backend не очищен от подтверждённой версии удалённого файла")
	}
}

// containsKind — проверка наличия вида операции в журнале вызовов.
func containsKind(l *opLog, kind string) bool {
	for _, k := range l.kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// noopCtx — meta.Context для подменного backend (им не используется).
func noopCtx() meta.Context {
	return meta.WrapWithCancel(context.Background(), uint32(os.Getpid()), 0, nil)
}

// TestCancelBeforeFlushNoJournal — REMOVED в окне выгрузки ДО подтверждения
// Flush (блокировка первой записи, порции по 16 байт): задание снимается
// без журнала — версии в backend нет; удаление существовавшей версии
// донесёт NOTIFY_FILE_DELETED (плейсхолдер).
func TestCancelBeforeFlushNoJournal(t *testing.T) {
	p, backend, _, fstore, _, j, log := notifyJournalTestProvider(t)
	local := filepath.Join(p.root, "precancel.txt")
	if err := os.WriteFile(local, patternBytes(64), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	wg := make(chan struct{})
	backend.mu.Lock()
	backend.writeGate = wg
	backend.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- p.runIngest(ctx, ingestTask{fsPath: "/precancel.txt", local: local, key: "/precancel.txt"})
	}()
	waitFor(t, func() bool { return containsKind(log, "create") })

	// Пользователь удаляет файл в окне записи → REMOVED → отмена (событие
	// подаётся напрямую — см. комментарий в TestUploadCanceledAfterFlushJournalsDelete).
	p.handleLocalRemoved(local)
	close(wg)
	if err := <-done; err != nil {
		t.Fatalf("runIngest = %v, ожидается nil (отмена — не ошибка)", err)
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пусто (версия не подтверждена)", j.Ops())
	}
	if containsKind(log, "flush") || containsKind(log, "truncate") {
		t.Errorf("подтверждение версии при отмене: %v", log.kinds())
	}
	if n := len(fstore.convertCalls()); n != 0 {
		t.Errorf("конверсий = %d, ожидается 0", n)
	}
}

// TestRemovedReplacesPendingUploadWithDelete — задача 6.5: REMOVED при
// ожидающей в журнале выгрузке БЕЗ активного задания (снятое занятостью/
// остановкой) — выгрузка заменяется удалением; исполнение очищает backend
// (ENOENT — исполнено).
func TestRemovedReplacesPendingUploadWithDelete(t *testing.T) {
	p, backend, _, _, _, j, _ := notifyJournalTestProvider(t)
	local := filepath.Join(p.root, "busy2.txt")
	if err := os.WriteFile(local, []byte("confirmed"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	if err := j.Add(JournalOp{Type: OpUpload, Path: "/busy2.txt"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	backend.mu.Lock()
	backend.files["/busy2.txt"] = []byte("confirmed")
	backend.inodes["/busy2.txt"] = 21
	backend.mu.Unlock()
	// Запись известна индексу — inode попадает в замену (fix-1.4).
	if err := p.idx.Put(21, "/busy2.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}

	// Файл удалён, активной выгрузки нет — выгрузка меняется на удаление.
	if err := os.Remove(local); err != nil {
		t.Fatalf("remove local: %v", err)
	}
	p.handleLocalRemoved(local)

	ops := j.Ops()
	if len(ops) != 1 || ops[0].Type != OpDelete || ops[0].Path != "/busy2.txt" {
		t.Fatalf("журнал = %+v, ожидается delete /busy2.txt", ops)
	}
	if ops[0].Ino != 21 {
		t.Errorf("op.Ino = %d, ожидается 21 (inode из индекса, fix-1.4)", ops[0].Ino)
	}
	if err := p.executeJournalOp(context.Background(), ops[0]); err != nil {
		t.Fatalf("executeJournalOp: %v", err)
	}
	if len(backend.content("/busy2.txt")) != 0 {
		t.Error("подтверждённая версия удалённого файла не вычищена")
	}
}

// TestRemovedOwnEventSuppressed — собственное событие финализации
// (replace-after-upload) не отменяет выгрузку: в окне супрессии REMOVED
// игнорируется, журнал пуст.
func TestRemovedOwnEventSuppressed(t *testing.T) {
	p, _, _, _, _, j, _ := notifyJournalTestProvider(t)
	clock := newFakeClock(time.Unix(0, 0))
	p.clock = clock
	local := filepath.Join(p.root, "own.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	p.markSelfEvent("/own.txt")

	p.handleLocalRemoved(local)

	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пусто (собственное событие)", j.Ops())
	}
}

// TestNotifyRenameFalseENOENTKeepsEntry — fix-1.8: ENOENT при rename при
// существующей цели — гонка (переименование уже исполнено другим источником);
// валидная запись не сносится, индекс переносится на новый путь.
func TestNotifyRenameFalseENOENTKeepsEntry(t *testing.T) {
	p, backend, _, _, idx, j, _ := notifyJournalTestProvider(t)
	// «Переименование уже исполнено» другим источником: старой записи нет,
	// новая есть с тем же inode, backend rename отвечает ENOENT.
	backend.mu.Lock()
	backend.files["/done.txt"] = []byte("x")
	backend.inodes["/done.txt"] = 12
	backend.renameErr = syscall.ENOENT
	backend.mu.Unlock()
	if err := idx.Put(12, "/src.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}

	p.notifyRenamePaths(context.Background(), filepath.Join(p.root, "src.txt"), filepath.Join(p.root, "done.txt"), 12, false, testConn, testTransfer, testRequest)

	if got, ok := idx.Path(12); !ok || got != "/done.txt" {
		t.Fatalf("idx.Path(12) = (%q, %v), ожидается /done.txt (запись сохранена)", got, ok)
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("журнал = %+v, ожидается пусто (исполнено)", j.Ops())
	}
}

// TestClassifyStateQueryTimeoutKeepsRouterAlive — U19 (живая трассировка
// 27.11): STANDARD-запрос по записи в pending-delete блокируется в
// CreateFile навсегда и без потолка убивал горутину-роутер (все уведомления
// и выгрузка стояли). Классификация с блокированным STANDARD по таймауту
// возвращает plain → to-upload, РОУТЕР ПРОДОЛЖАЕТ: следующий job
// обрабатывается (второй файл выходит из коалисера).
func TestClassifyStateQueryTimeoutKeepsRouterAlive(t *testing.T) {
	p, _, store, _, _, _ := newIngestTestProvider(t)
	p.stateQueryTimeout = 50 * time.Millisecond
	stuck := filepath.Join(p.root, "stuck.txt")
	if err := os.WriteFile(stuck, []byte("x"), 0o644); err != nil {
		t.Fatalf("write stuck: %v", err)
	}
	next := filepath.Join(p.root, "next.txt")
	if err := os.WriteFile(next, []byte("y"), 0o644); err != nil {
		t.Fatalf("write next: %v", err)
	}
	gate := make(chan struct{})
	store.mu.Lock()
	store.standardGate = gate
	store.standardGatePath = stuck // блокируется ТОЛЬКО pending-delete путь
	store.mu.Unlock()
	defer close(gate) // освободить горутину запроса (леак прототипа — по возврату из syscall)

	// Роутер + коалисер: два close-уведомления, оба должны дойти до out.
	d := cfapi.NewDispatcher(cfapi.DefaultQueueOptions())
	db := newIngestDebouncer(5*time.Millisecond, wallClock{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	routerDone := make(chan struct{})
	go func() {
		defer close(routerDone)
		p.ingestRouter(ctx, db, d, stop)
	}()
	dbDone := make(chan struct{})
	go func() {
		defer close(dbDone)
		db.run(ctx, stop)
	}()
	defer func() {
		close(stop)
		<-routerDone
		<-dbDone
	}()

	if err := d.OnNotifyFileCloseCompletion(&cfapi.NotifyFileCloseCompletionJob{Path: stuck}); err != nil {
		t.Fatalf("enqueue stuck: %v", err)
	}
	// Первый job: STANDARD блокирован → таймаут 50мс → plain → to-upload.
	task := recvTask(t, db.out)
	if task.fsPath != "/stuck.txt" {
		t.Fatalf("задание = %+v, ожидается /stuck.txt (таймаут → plain → upload)", task)
	}
	// РОУТЕР ЖИВ: следующий job (обычный plain-файл) обрабатывается.
	if err := d.OnNotifyFileCloseCompletion(&cfapi.NotifyFileCloseCompletionJob{Path: next}); err != nil {
		t.Fatalf("enqueue next: %v", err)
	}
	task = recvTask(t, db.out)
	if task.fsPath != "/next.txt" {
		t.Fatalf("второе задание = %+v, ожидается /next.txt (роутер не должен быть мёртв)", task)
	}
}
