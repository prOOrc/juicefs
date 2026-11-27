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

// Тесты пула гидратации (задачи 3.1–3.4): ровно один CfExecute(TRANSFER_DATA)
// на задание, чтение порциями не больше fetchBlock, отмена между порциями,
// ошибки backend, разрешение inode → путь (карта + ResolveIno, U12),
// переполнение очереди (U13), отсутствие утечек горутин. cldapi.dll не
// вызывается — fetchBackend и placeholderStore подменные (инвариант
// «Движок проверяется без CfAPI»).
package cfprovider

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/cfapi"
	"github.com/juicedata/juicefs/pkg/meta"
)

// patternBytes — детерминированные данные для проверки байт-в-байт.
func patternBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

// fakeFetchFile — подменный открытый файл: данные в памяти, хук после
// чтения (установка job.Cancel между порциями), настраиваемая ошибка.
type fakeFetchFile struct {
	data    []byte
	onRead  func(call int, off int64)
	readErr error
	calls   int
}

func (f *fakeFetchFile) Pread(ctx meta.Context, b []byte, off int64) (int, error) {
	f.calls++
	if f.onRead != nil {
		f.onRead(f.calls, off)
	}
	if f.readErr != nil {
		return 0, f.readErr
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	return copy(b, f.data[off:]), nil
}

func (f *fakeFetchFile) Close(ctx meta.Context) syscall.Errno { return 0 }

// fakeFetchBackend — подменный fetchBackend: файлы в памяти, настраиваемая
// ошибка Open.
type fakeFetchBackend struct {
	mu       sync.Mutex
	files    map[string][]byte
	openErr  syscall.Errno
	openCnt  int
	lastFile *fakeFetchFile
	fileHook func(f *fakeFetchFile)
}

func (b *fakeFetchBackend) Open(ctx meta.Context, path string) (fetchFile, syscall.Errno) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.openCnt++
	if b.openErr != 0 {
		return nil, b.openErr
	}
	f := &fakeFetchFile{data: b.files[path]}
	if b.fileHook != nil {
		b.fileHook(f)
	}
	b.lastFile = f
	return f, 0
}

func (b *fakeFetchBackend) opens() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.openCnt
}

func (b *fakeFetchBackend) file() *fakeFetchFile {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastFile
}

// fetchJob — задание FETCH_DATA с ключами по fileID (удобно для проверок
// «ровно один ответ на задание»).
func fetchJob(fileID uint64, off, length int64) *cfapi.FetchDataJob {
	return &cfapi.FetchDataJob{
		ConnectionKey:  cfapi.ConnectionKey{Internal: 1},
		TransferKey:    cfapi.TransferKey(100 + fileID),
		RequestKey:     cfapi.RequestKey(200 + fileID),
		FileID:         fileID,
		RequiredOffset: off,
		RequiredLength: length,
	}
}

// newFetchTestProvider — провайдер с подменными backend/store и реальным
// диспетчером; Start не вызывается (воркеры запускаются тестом явно).
func newFetchTestProvider(t *testing.T) (*Provider, *fakeFetchBackend, *fakeStore, *Index, *cfapi.Dispatcher) {
	t.Helper()
	idx, err := NewIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	backend := &fakeFetchBackend{files: map[string][]byte{}}
	d := cfapi.NewDispatcher(cfapi.DefaultQueueOptions())
	p := New(t.TempDir(), "fetch-vol", &fakeSyncRootAPI{registered: true}, d)
	p.idx = idx
	p.store = store
	p.fbackend = backend
	return p, backend, store, idx, d
}

// waitFor крутит условие до дедлайна (воркеры асинхронны).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !cond() {
		t.Fatal("условие не выполнено за 3 c")
	}
}

// TestFetchHydrationHappyPath — спека «Содержимое совпадает с backend» и
// «Ровно один ответ на задание»: чтение 10 MiB идёт порциями не больше
// fetchBlock (3 вызова Pread), данные байт-в-байт совпадают с backend,
// ответ TRANSFER_DATA ровно один, буфер возвращён в пул, задание снято
// с учёта. Второе задание с ненулевым offset читает середину файла.
func TestFetchHydrationHappyPath(t *testing.T) {
	p, backend, store, idx, _ := newFetchTestProvider(t)
	data := patternBytes(10 << 20)
	backend.files["/file.bin"] = data
	if err := idx.Put(7, "/file.bin"); err != nil {
		t.Fatalf("index put: %v", err)
	}

	job := fetchJob(7, 0, int64(len(data)))
	handleFetchData(context.Background(), p, job)

	calls := store.transferCalls()
	if len(calls) != 1 {
		t.Fatalf("ответов TRANSFER_DATA = %d, ожидается ровно 1", len(calls))
	}
	c := calls[0]
	if c.status != cfapi.StatusSuccess {
		t.Errorf("status = %#x, ожидается SUCCESS", uint32(c.status))
	}
	if c.transferKey != job.TransferKey || c.requestKey != job.RequestKey {
		t.Error("ключи ответа не совпали с заданием")
	}
	if c.length != int64(len(data)) {
		t.Errorf("length = %d, ожидается %d", c.length, len(data))
	}
	if !bytes.Equal(c.data, data) {
		t.Error("данные ответа не совпали с backend (byte-for-byte)")
	}
	if f := backend.file(); f == nil || f.calls != 3 {
		t.Errorf("вызовов Pread = %d, ожидается 3 (10 MiB / 4 MiB fetchBlock)", backend.file().calls)
	}
	if backend.opens() != 1 {
		t.Errorf("открытий файла = %d, ожидается 1", backend.opens())
	}
	if p.bufs.Busy() != 0 {
		t.Errorf("буфер не возвращён в пул: Busy = %d", p.bufs.Busy())
	}
	if d := p.dispatch.(*cfapi.Dispatcher); d.ActiveFetch() != 0 {
		t.Errorf("задание не снято с учёта: ActiveFetch = %d", d.ActiveFetch())
	}

	// Частичная гидрация: чтение середины файла (спека «Частичная
	// гидрация» — читается диапазон, не весь файл).
	jobMid := fetchJob(7, 5<<20, 3<<20)
	handleFetchData(context.Background(), p, jobMid)
	calls = store.transferCalls()
	if len(calls) != 2 {
		t.Fatalf("после второго задания ответов = %d, ожидается 2", len(calls))
	}
	c = calls[1]
	if c.status != cfapi.StatusSuccess || c.offset != 5<<20 || c.length != 3<<20 {
		t.Errorf("второй ответ: status=%#x offset=%d length=%d", uint32(c.status), c.offset, c.length)
	}
	if !bytes.Equal(c.data, data[5<<20:8<<20]) {
		t.Error("данные середины файла не совпали с backend")
	}
}

// TestFetchCancelBeforeStart — задание, отменённое в очереди, отвечает
// статусом отмены без открытия файла.
func TestFetchCancelBeforeStart(t *testing.T) {
	p, backend, store, _, _ := newFetchTestProvider(t)
	job := fetchJob(9, 0, 1<<20)
	job.Cancel.Store(true)

	handleFetchData(context.Background(), p, job)

	calls := store.transferCalls()
	if len(calls) != 1 {
		t.Fatalf("ответов = %d, ожидается 1 (ровно один CfExecute и при отмене)", len(calls))
	}
	if calls[0].status != cfapi.StatusCloudFileRequestCanceled {
		t.Errorf("status = %#x, ожидается CLOUD_FILE_REQUEST_CANCELED", uint32(calls[0].status))
	}
	if calls[0].length != 0 || len(calls[0].data) != 0 {
		t.Errorf("ответ отмены несёт данные: length=%d data=%d", calls[0].length, len(calls[0].data))
	}
	if backend.opens() != 0 {
		t.Errorf("файл открыт при отменённом задании: %d", backend.opens())
	}
	if p.bufs.Busy() != 0 {
		t.Errorf("буфер не возвращён: Busy = %d", p.bufs.Busy())
	}
}

// TestFetchCancelMidRead — спека «Прерванное чтение» (задача 3.3): отмена
// во время чтения останавливает цикл между порциями, ответ — статус отмены,
// буфер возвращён в пул.
func TestFetchCancelMidRead(t *testing.T) {
	p, backend, store, idx, _ := newFetchTestProvider(t)
	data := patternBytes(10 << 20) // 3 порции: 4 + 4 + 2 MiB
	backend.files["/big.bin"] = data
	if err := idx.Put(8, "/big.bin"); err != nil {
		t.Fatalf("index put: %v", err)
	}
	job := fetchJob(8, 0, int64(len(data)))
	backend.mu.Lock()
	backend.fileHook = func(f *fakeFetchFile) {
		f.onRead = func(call int, off int64) {
			if call == 1 {
				// Отмена приходит во время первой порции: воркер увидит флаг
				// перед второй (проверка между порциями).
				job.Cancel.Store(true)
			}
		}
	}
	backend.mu.Unlock()

	handleFetchData(context.Background(), p, job)

	calls := store.transferCalls()
	if len(calls) != 1 {
		t.Fatalf("ответов = %d, ожидается 1", len(calls))
	}
	if calls[0].status != cfapi.StatusCloudFileRequestCanceled {
		t.Errorf("status = %#x, ожидается CLOUD_FILE_REQUEST_CANCELED", uint32(calls[0].status))
	}
	if calls[0].length != 0 {
		t.Errorf("ответ отмены несёт length=%d, ожидается 0", calls[0].length)
	}
	if f := backend.file(); f.calls != 1 {
		t.Errorf("вызовов Pread = %d, ожидается 1 (отмена между порциями)", f.calls)
	}
	if p.bufs.Busy() != 0 {
		t.Errorf("буфер не возвращён в пул: Busy = %d", p.bufs.Busy())
	}
}

// TestFetchResolveInoFallback — U12 (задача 3.2): промах карты индекса —
// подъём по родителям через ResolveIno; найденный путь обслуживает задание.
func TestFetchResolveInoFallback(t *testing.T) {
	p, backend, store, idx, _ := newFetchTestProvider(t)
	data := patternBytes(1 << 20)
	backend.files["/resolved.bin"] = data
	resolved := false
	idx.SetResolver(func(ctx context.Context, ino uint64) (string, bool) {
		resolved = true
		return "/resolved.bin", true
	})

	job := fetchJob(11, 0, int64(len(data)))
	handleFetchData(context.Background(), p, job)

	calls := store.transferCalls()
	if len(calls) != 1 {
		t.Fatalf("ответов = %d, ожидается 1", len(calls))
	}
	if calls[0].status != cfapi.StatusSuccess {
		t.Fatalf("status = %#x, ожидается SUCCESS", uint32(calls[0].status))
	}
	if !bytes.Equal(calls[0].data, data) {
		t.Error("данные после ResolveIno не совпали с backend")
	}
	if !resolved {
		t.Error("резолвер подъёма по родителям не вызван (промах карты не обработан)")
	}
}

// TestFetchPathNotFound — ни карта, ни подъём не знают inode: ответ
// STATUS_OBJECT_NAME_NOT_FOUND (класс отказа), ровно один, без открытия.
func TestFetchPathNotFound(t *testing.T) {
	p, backend, store, _, _ := newFetchTestProvider(t)
	job := fetchJob(404, 0, 1<<20)

	handleFetchData(context.Background(), p, job)

	calls := store.transferCalls()
	if len(calls) != 1 {
		t.Fatalf("ответов = %d, ожидается 1", len(calls))
	}
	if calls[0].status != cfapi.StatusObjectNameNotFound {
		t.Errorf("status = %#x, ожидается OBJECT_NAME_NOT_FOUND", uint32(calls[0].status))
	}
	if backend.opens() != 0 {
		t.Errorf("файл открыт при неразрешённом пути: %d", backend.opens())
	}
}

// TestFetchOpenError — задача 3.4: ошибка Open отвечает кодом ошибки
// (ENOENT → OBJECT_NAME_NOT_FOUND через StatusFromErrno), процесс жив.
func TestFetchOpenError(t *testing.T) {
	p, backend, store, idx, _ := newFetchTestProvider(t)
	if err := idx.Put(12, "/gone.bin"); err != nil {
		t.Fatalf("index put: %v", err)
	}
	backend.mu.Lock()
	backend.openErr = syscall.ENOENT
	backend.mu.Unlock()

	handleFetchData(context.Background(), p, fetchJob(12, 0, 1<<20))

	calls := store.transferCalls()
	if len(calls) != 1 {
		t.Fatalf("ответов = %d, ожидается 1", len(calls))
	}
	if calls[0].status != cfapi.StatusObjectNameNotFound {
		t.Errorf("status = %#x, ожидается OBJECT_NAME_NOT_FOUND (ENOENT)", uint32(calls[0].status))
	}
}

// TestFetchReadError — задача 3.4: ошибка чтения (EIO в порции) — один
// ответ STATUS_IO_DEVICE_ERROR, буфер возвращён.
func TestFetchReadError(t *testing.T) {
	p, backend, store, idx, _ := newFetchTestProvider(t)
	backend.files["/ioerr.bin"] = patternBytes(1 << 20)
	if err := idx.Put(13, "/ioerr.bin"); err != nil {
		t.Fatalf("index put: %v", err)
	}
	backend.mu.Lock()
	backend.fileHook = func(f *fakeFetchFile) {
		f.readErr = syscall.EIO
	}
	backend.mu.Unlock()

	handleFetchData(context.Background(), p, fetchJob(13, 0, 1<<20))

	calls := store.transferCalls()
	if len(calls) != 1 {
		t.Fatalf("ответов = %d, ожидается 1", len(calls))
	}
	if calls[0].status != cfapi.StatusIODeviceError {
		t.Errorf("status = %#x, ожидается IO_DEVICE_ERROR (EIO)", uint32(calls[0].status))
	}
	if p.bufs.Busy() != 0 {
		t.Errorf("буфер не возвращён: Busy = %d", p.bufs.Busy())
	}
}

// TestFetchEOFShorterBackend — backend короче плейсхолдера (гонка усечения):
// Pread за концом возвращает io.EOF → ответ STATUS_END_OF_FILE, один.
func TestFetchEOFShorterBackend(t *testing.T) {
	p, backend, store, idx, _ := newFetchTestProvider(t)
	backend.files["/short.bin"] = patternBytes(16) // гораздо меньше запрошенного
	if err := idx.Put(14, "/short.bin"); err != nil {
		t.Fatalf("index put: %v", err)
	}

	handleFetchData(context.Background(), p, fetchJob(14, 0, 4<<20))

	calls := store.transferCalls()
	if len(calls) != 1 {
		t.Fatalf("ответов = %d, ожидается 1", len(calls))
	}
	if calls[0].status != cfapi.StatusEndOfFile {
		t.Errorf("status = %#x, ожидается END_OF_FILE", uint32(calls[0].status))
	}
}

// TestFetchNegativeOffsetRejected — границы диапазона проверяются ДО длины:
// отрицательный offset при RequiredLength == 0 отвечает отказом
// INVALID_REQUEST, а не успехом без данных.
func TestFetchNegativeOffsetRejected(t *testing.T) {
	p, backend, store, _, _ := newFetchTestProvider(t)
	job := fetchJob(15, -1, 0)

	handleFetchData(context.Background(), p, job)

	calls := store.transferCalls()
	if len(calls) != 1 {
		t.Fatalf("ответов = %d, ожидается 1", len(calls))
	}
	if calls[0].status != cfapi.StatusCloudFileInvalidRequest {
		t.Errorf("status = %#x, ожидается CLOUD_FILE_INVALID_REQUEST", uint32(calls[0].status))
	}
	if backend.opens() != 0 {
		t.Errorf("файл открыт при некорректном диапазоне: %d", backend.opens())
	}
}

// TestFetchContextCanceled — остановка движка (ctx serve отменён, mctx с
// ним): ошибка context.Canceled от чтения отвечает отменным статусом,
// а не STATUS_INTERNAL_ERROR.
func TestFetchContextCanceled(t *testing.T) {
	p, backend, store, idx, _ := newFetchTestProvider(t)
	backend.files["/ctx.bin"] = patternBytes(1 << 20)
	if err := idx.Put(16, "/ctx.bin"); err != nil {
		t.Fatalf("index put: %v", err)
	}
	backend.mu.Lock()
	backend.fileHook = func(f *fakeFetchFile) {
		f.readErr = context.Canceled
	}
	backend.mu.Unlock()

	handleFetchData(context.Background(), p, fetchJob(16, 0, 1<<20))

	calls := store.transferCalls()
	if len(calls) != 1 {
		t.Fatalf("ответов = %d, ожидается 1", len(calls))
	}
	if calls[0].status != cfapi.StatusCloudFileRequestCanceled {
		t.Errorf("status = %#x, ожидается CLOUD_FILE_REQUEST_CANCELED", uint32(calls[0].status))
	}
	if p.bufs.Busy() != 0 {
		t.Errorf("буфер не возвращён: Busy = %d", p.bufs.Busy())
	}
}

// TestFetchOverflowAnswer — U13: задание, отброшенное при переполнении
// очереди, получает ровно один ответ STATUS_CLOUD_FILE_REQUEST_CANCELED из
// горутины переполнения (не из thunk'а), постановленное задание — обычный
// ответ успеха.
func TestFetchOverflowAnswer(t *testing.T) {
	p, backend, store, idx, _ := newFetchTestProvider(t)
	data := patternBytes(1 << 20)
	backend.files["/a.bin"] = data
	backend.files["/b.bin"] = data
	if err := idx.Put(21, "/a.bin"); err != nil {
		t.Fatalf("index put: %v", err)
	}
	if err := idx.Put(22, "/b.bin"); err != nil {
		t.Fatalf("index put: %v", err)
	}
	small := cfapi.NewDispatcher(cfapi.QueueOptions{FetchData: 1, FetchDataOverflow: 4})
	p.dispatch = small

	queued := fetchJob(21, 0, int64(len(data)))
	if err := small.OnFetchData(queued); err != nil {
		t.Fatalf("первое задание не встало в очередь: %v", err)
	}
	dropped := fetchJob(22, 0, int64(len(data)))
	if err := small.OnFetchData(dropped); err == nil {
		t.Fatal("переполнение не вернуло ошибку")
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.fetchOverflowDrain(context.Background(), small, stop)
	}()
	waitFor(t, func() bool { return len(store.transferCalls()) == 1 })
	close(stop)
	wg.Wait()

	handleFetchData(context.Background(), p, queued) // постановленное — обычный путь

	calls := store.transferCalls()
	if len(calls) != 2 {
		t.Fatalf("всего ответов = %d, ожидается 2 (по одному на задание)", len(calls))
	}
	var canceled, success int
	for _, c := range calls {
		switch c.status {
		case cfapi.StatusCloudFileRequestCanceled:
			canceled++
			if c.transferKey != dropped.TransferKey || c.length != 0 {
				t.Errorf("ответ отмены: keys=%d/%d length=%d", c.transferKey, c.requestKey, c.length)
			}
		case cfapi.StatusSuccess:
			success++
			if c.transferKey != queued.TransferKey || c.length != int64(len(data)) {
				t.Errorf("ответ успеха: keys=%d/%d length=%d", c.transferKey, c.requestKey, c.length)
			}
		default:
			t.Errorf("неожиданный статус ответа: %#x", uint32(c.status))
		}
	}
	if canceled != 1 || success != 1 {
		t.Errorf("ответов cancel/success = %d/%d, ожидается 1/1", canceled, success)
	}
	if small.ActiveFetch() != 0 {
		t.Errorf("ActiveFetch = %d, ожидается 0 (оба задания сняты с учёта)", small.ActiveFetch())
	}
	if small.Dropped() != 1 {
		t.Errorf("Dropped = %d, ожидается 1", small.Dropped())
	}
}

// TestFetchWorkersConcurrent — пул воркеров разбирает очередь FETCH_DATA
// (интеграция с диспетчером, как runPopulation): все задания получают ровно
// один ответ; остановка (закрытие stop) завершает воркеров без утечек
// горутин (задача 3.3).
func TestFetchWorkersConcurrent(t *testing.T) {
	p, backend, store, idx, d := newFetchTestProvider(t)
	files := []string{"/c.bin", "/d.bin", "/e.bin", "/f.bin"}
	data := patternBytes(256 << 10)
	for i, name := range files {
		backend.files[name] = data
		if err := idx.Put(uint64(30+i), name); err != nil {
			t.Fatalf("index put: %v", err)
		}
	}

	baseline := runtime.NumGoroutine()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	workers := 4
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			p.fetchWorker(context.Background(), d, stop)
		}()
	}
	const jobs = 12
	for i := 0; i < jobs; i++ {
		job := fetchJob(uint64(30+i%len(files)), 0, int64(len(data)))
		job.RequestKey = cfapi.RequestKey(1000 + i) // уникальные пары ключей
		if err := d.OnFetchData(job); err != nil {
			t.Fatalf("OnFetchData(%d): %v", i, err)
		}
	}
	waitFor(t, func() bool { return len(store.transferCalls()) == jobs })

	calls := store.transferCalls()
	for _, c := range calls {
		if c.status != cfapi.StatusSuccess {
			t.Errorf("статус ответа %#x, ожидается SUCCESS", uint32(c.status))
		}
		if !bytes.Equal(c.data, data) {
			t.Error("данные конкурентного ответа не совпали с backend")
		}
	}
	close(stop)
	wg.Wait()
	if p.bufs.Busy() != 0 {
		t.Errorf("буферы не возвращены: Busy = %d", p.bufs.Busy())
	}
	// Воркеры завершились (wg.Wait) — горутины не текут.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Errorf("утечка горутин: было %d, стало %d", baseline, n)
	}
}

// TestFetchPoolViaStartStop — проводка этапа 3: Start запускает пул
// гидратации (воркеры очереди + переполнение), Stop дожидается его через
// wg и закрывает пул буферов; повторный Start работает (пул пересоздан).
func TestFetchPoolViaStartStop(t *testing.T) {
	api := &fakeSyncRootAPI{registered: true}
	backend := &fakeBackend{dirs: map[string][]BackendEntry{"/": nil}}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	dataBackend := &fakeFetchBackend{files: map[string][]byte{"/g.bin": patternBytes(64 << 10)}}
	idx, err := NewIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Put(40, "/g.bin"); err != nil {
		t.Fatalf("index put: %v", err)
	}

	p := New(t.TempDir(), "pool-vol", api, cfapi.NewDispatcher(cfapi.QueueOptions{FetchData: 4}))
	p.backend = backend
	p.store = store
	p.idx = idx
	p.fbackend = dataBackend
	p.retryDelay = time.Millisecond
	p.fetchWorkers = 2

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	d := p.dispatch.(*cfapi.Dispatcher)
	if d.QueueCapacity().FetchData != 4 {
		t.Errorf("ёмкость очереди = %d, ожидается 4", d.QueueCapacity().FetchData)
	}
	job := fetchJob(40, 0, 64<<10)
	if err := d.OnFetchData(job); err != nil {
		t.Fatalf("OnFetchData: %v", err)
	}
	waitFor(t, func() bool { return len(store.transferCalls()) == 1 })
	if c := store.transferCalls()[0]; c.status != cfapi.StatusSuccess || c.length != 64<<10 {
		t.Errorf("ответ через пул: status=%#x length=%d", uint32(c.status), c.length)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if p.bufs.Busy() != 0 || p.bufs.Idle() != 0 {
		t.Errorf("пул буферов после Stop: Busy=%d Idle=%d, ожидается 0/0 (закрыт)", p.bufs.Busy(), p.bufs.Idle())
	}
	// Повторный Start: пул пересоздан, очередь обслуживается снова.
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("повторный Start: %v", err)
	}
	job2 := fetchJob(40, 0, 64<<10)
	job2.RequestKey = 999
	if err := d.OnFetchData(job2); err != nil {
		t.Fatalf("OnFetchData после рестарта: %v", err)
	}
	waitFor(t, func() bool { return len(store.transferCalls()) == 2 })
	if err := p.Stop(); err != nil {
		t.Fatalf("повторный Stop: %v", err)
	}
}
