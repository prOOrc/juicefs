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

package cfapi

import (
	"errors"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fakeDispatcher — подменная реализация CallbackDispatcher: только запись
// заданий, никакого I/O и обращений к cldapi.dll (спека «Движок проверяется
// без Cloud Files API»). ioOps всегда 0 — путей ввода-вывода нет
// (спека «Очередь без воркеров»).
type fakeDispatcher struct {
	fetchJobs             []*FetchDataJob
	cancelJobs            []*CancelFetchDataJob
	placeholderJobs       []*FetchPlaceholdersJob
	cancelPlaceholderJobs []*CancelFetchPlaceholdersJob
	closeJobs             []*NotifyFileCloseCompletionJob
	fileRenamedJobs       []*NotifyFileRenamedJob
	fileDeletedJobs       []*NotifyFileDeletedJob
	dirCreatedJobs        []*NotifyDirectoryCreatedJob
	dirRenamedJobs        []*NotifyDirectoryRenamedJob
	dirDeletedJobs        []*NotifyDirectoryDeletedJob
	ioOps                 int64
}

func (f *fakeDispatcher) OnFetchData(job *FetchDataJob) error {
	f.fetchJobs = append(f.fetchJobs, job)
	return nil
}

func (f *fakeDispatcher) OnCancelFetchData(job *CancelFetchDataJob) error {
	f.cancelJobs = append(f.cancelJobs, job)
	return nil
}

func (f *fakeDispatcher) OnFetchPlaceholders(job *FetchPlaceholdersJob) error {
	f.placeholderJobs = append(f.placeholderJobs, job)
	return nil
}

func (f *fakeDispatcher) OnCancelFetchPlaceholders(job *CancelFetchPlaceholdersJob) error {
	f.cancelPlaceholderJobs = append(f.cancelPlaceholderJobs, job)
	return nil
}

func (f *fakeDispatcher) OnNotifyFileCloseCompletion(job *NotifyFileCloseCompletionJob) error {
	f.closeJobs = append(f.closeJobs, job)
	return nil
}

func (f *fakeDispatcher) OnNotifyFileRenamed(job *NotifyFileRenamedJob) error {
	f.fileRenamedJobs = append(f.fileRenamedJobs, job)
	return nil
}

func (f *fakeDispatcher) OnNotifyFileDeleted(job *NotifyFileDeletedJob) error {
	f.fileDeletedJobs = append(f.fileDeletedJobs, job)
	return nil
}

func (f *fakeDispatcher) OnNotifyDirectoryCreated(job *NotifyDirectoryCreatedJob) error {
	f.dirCreatedJobs = append(f.dirCreatedJobs, job)
	return nil
}

func (f *fakeDispatcher) OnNotifyDirectoryRenamed(job *NotifyDirectoryRenamedJob) error {
	f.dirRenamedJobs = append(f.dirRenamedJobs, job)
	return nil
}

func (f *fakeDispatcher) OnNotifyDirectoryDeleted(job *NotifyDirectoryDeletedJob) error {
	f.dirDeletedJobs = append(f.dirDeletedJobs, job)
	return nil
}

// Компилируемая проверка: *Dispatcher реализует CallbackDispatcher.
var _ CallbackDispatcher = (*Dispatcher)(nil)
var _ CallbackDispatcher = (*fakeDispatcher)(nil)

// utf16TestPtr — UTF-16 строка в памяти Go для полей вида CF_CALLBACK_INFO.
func utf16TestPtr(t *testing.T, s string) *uint16 {
	t.Helper()
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q): %v", s, err)
	}
	return p
}

// TestDispatcherQueueNoWorkers — спека «Очередь без воркеров»: 1000 событий
// FETCH_DATA при остановленных воркерах возвращаются суммарно < 1 c,
// ни одна горутина не запущена (диспетчер не имеет путей I/O).
func TestDispatcherQueueNoWorkers(t *testing.T) {
	d := NewDispatcher(DefaultQueueOptions())
	SetDispatcher(d)
	defer SetDispatcher(nil)

	goroutinesBefore := runtime.NumGoroutine()
	start := time.Now()
	for i := 0; i < 1000; i++ {
		job := &FetchDataJob{TransferKey: TransferKey(i), RequiredLength: 4096}
		if err := d.OnFetchData(job); err != nil {
			t.Fatalf("OnFetchData(%d): %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Errorf("1000 событий заняли %v, лимит 1 c", elapsed)
	}
	if got := runtime.NumGoroutine(); got != goroutinesBefore {
		t.Errorf("диспетчер запустил горутины: %d -> %d", goroutinesBefore, got)
	}
	if d.Dropped() != 0 {
		t.Errorf("Dropped = %d, ожидается 0", d.Dropped())
	}
	if d.Enqueued() != 1000 {
		t.Errorf("Enqueued = %d, ожидается 1000", d.Enqueued())
	}
	if d.ActiveFetch() != 1000 {
		t.Errorf("ActiveFetch = %d, ожидается 1000", d.ActiveFetch())
	}
}

// TestDispatcherQueueNoWorkersNoIO — дословный сценарий спеки «Очередь без
// воркеров»: 1000 событий FETCH_DATA проходят через диспетчер (thunk →
// CallbackDispatcher) при остановленных воркерах; все вызовы возвращаются
// суммарно менее чем за 1 секунду, счётчик операций ввода-вывода подменной
// реализации равен нулю (в одном тесте: и время, и отсутствие I/O).
func TestDispatcherQueueNoWorkersNoIO(t *testing.T) {
	fake := &fakeDispatcher{}
	SetDispatcher(fake)
	defer SetDispatcher(nil)

	var info CFCallbackInfo
	info.ConnectionKey = ConnectionKey{Internal: 1}
	info.TransferKey = 11
	var params CFCallbackParameters
	p := params.FetchDataParams()
	p.RequiredFileOffset = 0
	p.RequiredLength = 4096

	goroutinesBefore := runtime.NumGoroutine()
	start := time.Now()
	for i := 0; i < 1000; i++ {
		info.TransferKey = TransferKey(i)
		if rc := fetchDataCallback(&info, &params); rc != 0 {
			t.Fatalf("fetchDataCallback(%d) вернул %d, ожидается 0 (VOID)", i, rc)
		}
	}
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Errorf("1000 событий FETCH_DATA заняли %v, лимит 1 c", elapsed)
	}
	if fake.ioOps != 0 {
		t.Errorf("счётчик I/O подменной реализации = %d, ожидается 0", fake.ioOps)
	}
	if got := runtime.NumGoroutine(); got != goroutinesBefore {
		t.Errorf("диспетчер запустил горутины: %d -> %d", goroutinesBefore, got)
	}
	if len(fake.fetchJobs) != 1000 {
		t.Errorf("fetchJobs = %d, ожидается 1000", len(fake.fetchJobs))
	}
	for i, job := range fake.fetchJobs {
		if job.TransferKey != TransferKey(i) {
			t.Fatalf("fetchJobs[%d].TransferKey = %d, ожидается %d", i, job.TransferKey, i)
		}
	}
}

// TestDispatcherOverflow — спека «Переполненная очередь»: заполненная очередь
// возвращает ErrQueueFull, задание не ставится и не блокирует.
func TestDispatcherOverflow(t *testing.T) {
	opts := DefaultQueueOptions()
	opts.FetchData = 4
	d := NewDispatcher(opts)

	for i := 0; i < 4; i++ {
		if err := d.OnFetchData(&FetchDataJob{TransferKey: TransferKey(i)}); err != nil {
			t.Fatalf("OnFetchData(%d): %v", i, err)
		}
	}
	err := d.OnFetchData(&FetchDataJob{TransferKey: 99})
	if !errors.Is(err, ErrQueueFull) {
		t.Errorf("переполнение вернуло %v, ожидается ErrQueueFull", err)
	}
	if d.Dropped() != 1 {
		t.Errorf("Dropped = %d, ожидается 1", d.Dropped())
	}
	if d.ActiveFetch() != 4 {
		t.Errorf("ActiveFetch = %d, ожидается 4 (отброшенное снято с учёта)", d.ActiveFetch())
	}
}

// TestDispatcherCancelFetchData — CANCEL_FETCH_DATA ставит флаг отмены
// активному заданию по TransferKey.
func TestDispatcherCancelFetchData(t *testing.T) {
	d := NewDispatcher(DefaultQueueOptions())

	job := &FetchDataJob{TransferKey: 42}
	if err := d.OnFetchData(job); err != nil {
		t.Fatalf("OnFetchData: %v", err)
	}
	if job.Cancel.Load() {
		t.Fatal("флаг отмены установлен до CANCEL_FETCH_DATA")
	}
	if err := d.OnCancelFetchData(&CancelFetchDataJob{TransferKey: 42}); err != nil {
		t.Fatalf("OnCancelFetchData: %v", err)
	}
	if !job.Cancel.Load() {
		t.Error("CANCEL_FETCH_DATA не поставил флаг отмены")
	}
	if d.Cancelled() != 1 {
		t.Errorf("Cancelled = %d, ожидается 1", d.Cancelled())
	}

	// Отмена по незнакомому ключу (задание завершено) — не ошибка и не паника.
	if err := d.OnCancelFetchData(&CancelFetchDataJob{TransferKey: 777}); err != nil {
		t.Errorf("cancel незнакомого ключа вернул %v", err)
	}

	// После Complete задание снято с учёта: новое задание с тем же ключом
	// не получает отмену, адресованную другому заданию.
	d.Complete(job)
	if d.ActiveFetch() != 0 {
		t.Errorf("ActiveFetch после Complete = %d, ожидается 0", d.ActiveFetch())
	}
	job2 := &FetchDataJob{TransferKey: 42}
	if err := d.OnFetchData(job2); err != nil {
		t.Fatalf("повторный OnFetchData с тем же ключом: %v", err)
	}
	if err := d.OnCancelFetchData(&CancelFetchDataJob{TransferKey: 999}); err != nil {
		t.Fatalf("cancel по чужому ключу: %v", err)
	}
	if job2.Cancel.Load() {
		t.Error("флаг отмены поставлен заданию с другим TransferKey")
	}
	d.Complete(job2)
}

// TestDispatcherCancelFetchDataRequestKey — контракт этапа 3 (design.md D7):
// два конкурентных FETCH_DATA по одному файлу (общий TransferKey, разные
// RequestKey) отменяются адресно по RequestKey; отмена с точным RequestKey
// не задевает соседнее задание. Запас на непроверенную семантику: точный
// промах при ровно одном активном задании с этим TransferKey отменяет его;
// при нескольких — никто (чужой запрос не отменяется).
func TestDispatcherCancelFetchDataRequestKey(t *testing.T) {
	d := NewDispatcher(DefaultQueueOptions())

	job1 := &FetchDataJob{TransferKey: 42, RequestKey: 1}
	job2 := &FetchDataJob{TransferKey: 42, RequestKey: 2}
	if err := d.OnFetchData(job1); err != nil {
		t.Fatalf("OnFetchData job1: %v", err)
	}
	if err := d.OnFetchData(job2); err != nil {
		t.Fatalf("OnFetchData job2: %v", err)
	}
	if d.ActiveFetch() != 2 {
		t.Fatalf("ActiveFetch = %d, ожидается 2", d.ActiveFetch())
	}
	// Точная адресация: отменяется только задание с RequestKey 1.
	if err := d.OnCancelFetchData(&CancelFetchDataJob{TransferKey: 42, RequestKey: 1}); err != nil {
		t.Fatalf("OnCancelFetchData: %v", err)
	}
	if !job1.Cancel.Load() {
		t.Error("отмена по RequestKey 1 не дошла до job1")
	}
	if job2.Cancel.Load() {
		t.Error("отмена по RequestKey 1 задела job2 (другой RequestKey)")
	}

	// Точный промах при двух активных заданиях — никто не отменяется.
	if err := d.OnCancelFetchData(&CancelFetchDataJob{TransferKey: 42, RequestKey: 99}); err != nil {
		t.Fatalf("OnCancelFetchData с чужим RequestKey: %v", err)
	}
	if job2.Cancel.Load() {
		t.Error("неоднозначная отмена (2 активных, чужой RequestKey) задела job2")
	}

	// Запас: после Complete job2 остаётся ровно одно активное задание с
	// этим TransferKey — отмена с чужим RequestKey его отменяет.
	d.Complete(job1)
	if d.ActiveFetch() != 1 {
		t.Fatalf("ActiveFetch после Complete job1 = %d, ожидается 1", d.ActiveFetch())
	}
	if err := d.OnCancelFetchData(&CancelFetchDataJob{TransferKey: 42, RequestKey: 99}); err != nil {
		t.Fatalf("OnCancelFetchData одиночного: %v", err)
	}
	if !job2.Cancel.Load() {
		t.Error("отмена одиночного активного задания (точный промах RequestKey) не сработала")
	}
	d.Complete(job2)
	if d.ActiveFetch() != 0 {
		t.Errorf("ActiveFetch после Complete job2 = %d, ожидается 0", d.ActiveFetch())
	}
}

// TestDispatcherOverflowAnswersCanceled — U13 (этап 3): при переполнении
// очереди FETCH_DATA задание не ставится, OnFetchData возвращает
// ErrQueueFull, а задание попадает в очередь переполнения — воркер движка
// ответит на него CfExecute'ом со статусом отмены (сам Dispatcher I/O
// не выполняет).
func TestDispatcherOverflowAnswersCanceled(t *testing.T) {
	d := NewDispatcher(QueueOptions{FetchData: 1, FetchDataOverflow: 4})

	enqueued := &FetchDataJob{TransferKey: 1, RequestKey: 1}
	if err := d.OnFetchData(enqueued); err != nil {
		t.Fatalf("первое задание должно встать в очередь: %v", err)
	}
	dropped := &FetchDataJob{TransferKey: 2, RequestKey: 2}
	if err := d.OnFetchData(dropped); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("переполнение: err = %v, ожидается ErrQueueFull", err)
	}
	if d.Dropped() != 1 {
		t.Errorf("Dropped = %d, ожидается 1", d.Dropped())
	}
	// Отброшенное задание снято с учёта и доступно потребителю переполнения.
	if d.ActiveFetch() != 1 {
		t.Errorf("ActiveFetch = %d, ожидается 1 (только постановленное)", d.ActiveFetch())
	}
	select {
	case got := <-d.FetchDataOverflow():
		if got != dropped {
			t.Error("в очередь переполнения попало не отброшенное задание")
		}
	default:
		t.Fatal("очередь переполнения пуста — отброшенное задание потеряно, ответа не будет")
	}
}

// TestDispatcherCancelPlaceholders — отмена наполнения по реестру.
func TestDispatcherCancelPlaceholders(t *testing.T) {
	d := NewDispatcher(DefaultQueueOptions())

	job := &FetchPlaceholdersJob{TransferKey: 7, Pattern: "*"}
	if err := d.OnFetchPlaceholders(job); err != nil {
		t.Fatalf("OnFetchPlaceholders: %v", err)
	}
	if err := d.OnCancelFetchPlaceholders(&CancelFetchPlaceholdersJob{TransferKey: 7}); err != nil {
		t.Fatalf("OnCancelFetchPlaceholders: %v", err)
	}
	if !job.Cancel.Load() {
		t.Error("CANCEL_FETCH_PLACEHOLDERS не поставил флаг отмены")
	}
	d.CompletePlaceholders(job)
	if d.ActivePlaceholders() != 0 {
		t.Errorf("ActivePlaceholders после CompletePlaceholders = %d, ожидается 0", d.ActivePlaceholders())
	}
}

// TestDispatcherMarshalThunks — thunk'и копируют поля в задания и вызывают
// диспетчер без I/O; NOTIFY_RENAME/NOTIFY_DELETE маршрутизируют файл/каталог
// по флагам IS_DIRECTORY. cldapi.dll не вызывается (фейковый диспетчер).
func TestDispatcherMarshalThunks(t *testing.T) {
	fake := &fakeDispatcher{}
	SetDispatcher(fake)
	defer SetDispatcher(nil)

	var info CFCallbackInfo
	info.ConnectionKey = ConnectionKey{Internal: 1}
	info.TransferKey = 11
	info.RequestKey = 22
	info.PriorityHint = 3
	var identity [8]byte
	identity[0] = 0x34 // inode 0x1234, little-endian
	identity[1] = 0x12
	info.FileIdentity = unsafe.Pointer(&identity[0])
	info.FileIdentityLength = 8

	// FETCH_DATA
	var params CFCallbackParameters
	p := params.FetchDataParams()
	p.Flags = CallbackFetchDataFlagRecovery
	p.RequiredFileOffset = 1024
	p.RequiredLength = 65536
	p.OptionalFileOffset = 2048
	p.OptionalLength = 131072
	if rc := fetchDataCallback(&info, &params); rc != 0 {
		t.Errorf("fetchDataCallback вернул %d, ожидается 0 (VOID)", rc)
	}
	if len(fake.fetchJobs) != 1 {
		t.Fatalf("fetchJobs = %d, ожидается 1", len(fake.fetchJobs))
	}
	job := fake.fetchJobs[0]
	if job.ConnectionKey != info.ConnectionKey || job.TransferKey != 11 || job.RequestKey != 22 {
		t.Error("ключи задания гидратации не совпали")
	}
	if job.FileID != 0x1234 {
		t.Errorf("FileID = %#x, ожидается 0x1234", job.FileID)
	}
	if job.Flags != CallbackFetchDataFlagRecovery ||
		job.RequiredOffset != 1024 || job.RequiredLength != 65536 ||
		job.OptionalOffset != 2048 || job.OptionalLength != 131072 {
		t.Errorf("параметры FETCH_DATA не совпали: %+v", job)
	}
	if job.PriorityHint != 3 {
		t.Errorf("PriorityHint = %d, ожидается 3", job.PriorityHint)
	}

	// CANCEL_FETCH_DATA
	var cancelParams CFCallbackParameters
	cp := cancelParams.CancelParams()
	cp.Flags = CallbackCancelFlagIOAborted
	cp.FileOffset = 4096
	cp.Length = 8192
	if rc := cancelFetchDataCallback(&info, &cancelParams); rc != 0 {
		t.Errorf("cancelFetchDataCallback вернул %d", rc)
	}
	if len(fake.cancelJobs) != 1 {
		t.Fatalf("cancelJobs = %d, ожидается 1", len(fake.cancelJobs))
	}
	cj := fake.cancelJobs[0]
	if cj.Flags != CallbackCancelFlagIOAborted || cj.FileOffset != 4096 || cj.Length != 8192 {
		t.Errorf("параметры CANCEL_FETCH_DATA не совпали: %+v", cj)
	}

	// FETCH_PLACEHOLDERS
	var fpParams CFCallbackParameters
	fp := fpParams.FetchPlaceholdersParams()
	fp.Pattern = utf16TestPtr(t, "*.txt")
	fetchPlaceholdersCallback(&info, &fpParams)
	if len(fake.placeholderJobs) != 1 || fake.placeholderJobs[0].Pattern != "*.txt" {
		t.Errorf("Pattern не скопирован: %+v", fake.placeholderJobs)
	}

	// CANCEL_FETCH_PLACEHOLDERS
	cancelFetchPlaceholdersCallback(&info, &cancelParams)
	if len(fake.cancelPlaceholderJobs) != 1 {
		t.Fatalf("cancelPlaceholderJobs = %d, ожидается 1", len(fake.cancelPlaceholderJobs))
	}

	// NOTIFY_FILE_CLOSE_COMPLETION
	info.NormalizedPath = utf16TestPtr(t, `C:\sync\file.bin`)
	info.FileSize = 12345
	var ccParams CFCallbackParameters
	cc := ccParams.CloseCompletionParams()
	cc.Flags = CallbackCloseCompletionFlagDeleted
	notifyFileCloseCompletionCallback(&info, &ccParams)
	if len(fake.closeJobs) != 1 {
		t.Fatalf("closeJobs = %d, ожидается 1", len(fake.closeJobs))
	}
	ccj := fake.closeJobs[0]
	if ccj.Path != `C:\sync\file.bin` || ccj.FileSize != 12345 || ccj.Flags != CallbackCloseCompletionFlagDeleted || ccj.FileID != 0x1234 {
		t.Errorf("параметры NOTIFY_FILE_CLOSE_COMPLETION не совпали: %+v", ccj)
	}

	// NOTIFY_RENAME: файл
	info.NormalizedPath = utf16TestPtr(t, `C:\sync\old.txt`)
	var renameParams CFCallbackParameters
	rp := renameParams.RenameParams()
	rp.TargetPath = utf16TestPtr(t, `C:\sync\new.txt`)
	notifyRenameCallback(&info, &renameParams)
	if len(fake.fileRenamedJobs) != 1 || len(fake.dirRenamedJobs) != 0 {
		t.Fatalf("маршрутизация RENAME файла неверна: file=%d dir=%d", len(fake.fileRenamedJobs), len(fake.dirRenamedJobs))
	}
	fj := fake.fileRenamedJobs[0]
	if fj.SourcePath != `C:\sync\old.txt` || fj.TargetPath != `C:\sync\new.txt` {
		t.Errorf("пути RENAME файла не совпали: %+v", fj)
	}

	// NOTIFY_RENAME: каталог (флаг IS_DIRECTORY)
	rp.Flags = CallbackRenameFlagIsDirectory
	notifyRenameCallback(&info, &renameParams)
	if len(fake.dirRenamedJobs) != 1 || len(fake.fileRenamedJobs) != 1 {
		t.Errorf("маршрутизация RENAME каталога неверна: file=%d dir=%d", len(fake.fileRenamedJobs), len(fake.dirRenamedJobs))
	}

	// NOTIFY_DELETE: файл
	info.NormalizedPath = utf16TestPtr(t, `C:\sync\gone.txt`)
	var deleteParams CFCallbackParameters
	dp := deleteParams.DeleteParams()
	notifyDeleteCallback(&info, &deleteParams)
	if len(fake.fileDeletedJobs) != 1 || len(fake.dirDeletedJobs) != 0 {
		t.Fatalf("маршрутизация DELETE файла неверна: file=%d dir=%d", len(fake.fileDeletedJobs), len(fake.dirDeletedJobs))
	}
	if fake.fileDeletedJobs[0].Path != `C:\sync\gone.txt` {
		t.Errorf("путь DELETE файла не совпал: %+v", fake.fileDeletedJobs[0])
	}

	// NOTIFY_DELETE: каталог
	dp.Flags = CallbackDeleteFlagIsDirectory
	notifyDeleteCallback(&info, &deleteParams)
	if len(fake.dirDeletedJobs) != 1 || len(fake.fileDeletedJobs) != 1 {
		t.Errorf("маршрутизация DELETE каталога неверна: file=%d dir=%d", len(fake.fileDeletedJobs), len(fake.dirDeletedJobs))
	}

	if fake.ioOps != 0 {
		t.Errorf("счётчик I/O подменной реализации = %d, ожидается 0", fake.ioOps)
	}

	// nil-диспетчер: thunk обязан молча отбросить задание, без паники.
	SetDispatcher(nil)
	if rc := fetchDataCallback(&info, &params); rc != 0 {
		t.Errorf("thunk без диспетчера вернул %d", rc)
	}
}

// TestCallbackTable — таблица регистрации покрывает семь OS-типов D5 и
// завершается CF_CALLBACK_REGISTRATION_END.
func TestCallbackTable(t *testing.T) {
	table := CallbackTable()
	if len(table) != 8 {
		t.Fatalf("таблица = %d записей, ожидается 7 callback'ов + END", len(table))
	}
	wantTypes := []uint32{
		CallbackTypeFetchData,
		CallbackTypeCancelFetchData,
		CallbackTypeFetchPlaceholders,
		CallbackTypeCancelFetchPlaceholders,
		CallbackTypeNotifyFileCloseCompletion,
		CallbackTypeNotifyRename,
		CallbackTypeNotifyDelete,
		CallbackTypeNone,
	}
	for i, w := range wantTypes {
		if table[i].Type != w {
			t.Errorf("table[%d].Type = %d, ожидается %d", i, table[i].Type, w)
		}
		if i < 7 && table[i].Callback == 0 {
			t.Errorf("table[%d].Callback пуст", i)
		}
	}
}
