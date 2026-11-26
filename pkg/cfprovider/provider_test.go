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
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// logCapture — хук логгера: собирает сообщения для проверок «ровно один раз».
type logCapture struct {
	mu       sync.Mutex
	messages []string
}

func (h *logCapture) Fire(e *logrus.Entry) error {
	h.mu.Lock()
	h.messages = append(h.messages, e.Message)
	h.mu.Unlock()
	return nil
}

func (h *logCapture) Levels() []logrus.Level { return logrus.AllLevels }

func (h *logCapture) count(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.messages {
		if strings.Contains(m, substr) {
			n++
		}
	}
	return n
}

// fakeSyncRootAPI — подменная реализация cfapi.SyncRootAPI: считает вызовы,
// не обращается к cldapi.dll (спека «Движок проверяется без Cloud Files API»).
type fakeSyncRootAPI struct {
	registered   bool
	availableErr error
	connectErr   error

	connects    int
	disconnects int
	lastTable   []cfapi.CFCallbackRegistration
	lastRoot    string
	lastFlags   uint32
}

func (f *fakeSyncRootAPI) Available() error {
	return f.availableErr
}

func (f *fakeSyncRootAPI) RegisterSyncRoot(syncRootPath string, registration *cfapi.CFSyncRegistration, policies *cfapi.CFSyncPolicies, registerFlags uint32) error {
	return errors.New("fake: RegisterSyncRoot не используется провайдером")
}

func (f *fakeSyncRootAPI) UnregisterSyncRoot(syncRootPath string) error {
	return errors.New("fake: UnregisterSyncRoot не используется провайдером")
}

func (f *fakeSyncRootAPI) GetSyncRootInfoByPath(filePath string, infoClass uint32, infoBuffer []byte, returnedLength *uint32) error {
	if !f.registered {
		return errors.New("fake: sync root не зарегистрирован")
	}
	return nil
}

func (f *fakeSyncRootAPI) Connect(syncRootPath string, callbackTable []cfapi.CFCallbackRegistration, callbackContext uintptr, connectFlags uint32) (*cfapi.Connection, error) {
	f.connects++
	f.lastRoot = syncRootPath
	f.lastFlags = connectFlags
	f.lastTable = callbackTable
	if f.connectErr != nil {
		return nil, f.connectErr
	}
	return &cfapi.Connection{SyncRootPath: syncRootPath, Key: cfapi.ConnectionKey{Internal: int64(f.connects)}}, nil
}

func (f *fakeSyncRootAPI) DisconnectSyncRoot(connectionKey cfapi.ConnectionKey) error {
	f.disconnects++
	return nil
}

// TestProviderNotRegistered — синхро-сценарий: при отсутствии регистрации
// Start завершается ошибкой и НЕ подключается к sync root.
func TestProviderNotRegistered(t *testing.T) {
	api := &fakeSyncRootAPI{registered: false}
	p := New(t.TempDir(), "test-volume", api, nil)
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("Start без регистрации должен вернуть ошибку")
	} else if !strings.Contains(err.Error(), "is not registered") {
		t.Errorf("ошибка без упоминания регистрации: %v", err)
	}
	if api.connects != 0 {
		t.Errorf("Connect вызван %d раз без регистрации, ожидается 0", api.connects)
	}
	// Stop на не запущенном провайдере — no-op без ошибки.
	if err := p.Stop(); err != nil {
		t.Errorf("Stop не запущенного провайдера вернул %v", err)
	}
	if api.disconnects != 0 {
		t.Errorf("DisconnectSyncRoot вызван %d раз без соединения, ожидается 0", api.disconnects)
	}
}

// TestProviderStartStop — полный цикл: подключение с таблицей callback'ов,
// отключение через api, идемпотентный Stop, повторный запуск.
func TestProviderStartStop(t *testing.T) {
	api := &fakeSyncRootAPI{registered: true}
	root := t.TempDir()
	p := New(root, "test-volume", api, nil)

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if api.connects != 1 {
		t.Fatalf("Connect вызван %d раз, ожидается 1", api.connects)
	}
	if api.lastRoot != root {
		t.Errorf("Connect по пути %q, ожидается %q", api.lastRoot, root)
	}
	if api.lastFlags != cfapi.ConnectFlagRequireFullFilePath {
		t.Errorf("Connect flags = %#x, ожидается REQUIRE_FULL_FILE_PATH", api.lastFlags)
	}
	// Контракт времени жизни (P2-2): провайдер подключается с полной таблицей
	// callback'ов, удерживаемой объектом Connection (cfapi.Connect копирует
	// срез в поля соединения).
	if len(api.lastTable) != 8 || api.lastTable[7].Type != cfapi.CallbackTypeNone {
		t.Errorf("таблица callback'ов: %d записей, ожидается 7 callback'ов + END", len(api.lastTable))
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if api.disconnects != 1 {
		t.Errorf("DisconnectSyncRoot вызван %d раз, ожидается 1", api.disconnects)
	}
	// Повторный Stop — идемпотентен.
	if err := p.Stop(); err != nil {
		t.Errorf("повторный Stop вернул %v", err)
	}
	if api.disconnects != 1 {
		t.Errorf("повторный Stop вызвал DisconnectSyncRoot: %d, ожидается 1", api.disconnects)
	}
	// После Stop провайдер запускается снова.
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("повторный Start: %v", err)
	}
	if api.connects != 2 {
		t.Errorf("повторный Start: Connect вызван %d раз, ожидается 2", api.connects)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("финальный Stop: %v", err)
	}
}

// TestProviderSecondInstance — спека «Второй экземпляр»: второй serve на тот
// же корень получает ошибку именованного mutex ДО подключения; после Stop
// первого корень освобождается. Mutex реальный (kernel32), cldapi.dll не
// вызывается.
func TestProviderSecondInstance(t *testing.T) {
	root := t.TempDir()
	api1 := &fakeSyncRootAPI{registered: true}
	p1 := New(root, "test-volume", api1, nil)
	if err := p1.Start(context.Background()); err != nil {
		t.Fatalf("Start первого экземпляра: %v", err)
	}

	api2 := &fakeSyncRootAPI{registered: true}
	p2 := New(root, "test-volume", api2, nil)
	err := p2.Start(context.Background())
	if err == nil {
		t.Fatal("Start второго экземпляра должен вернуть ошибку")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("ошибка второго экземпляра без упоминания занятого mutex: %v", err)
	}
	if api2.connects != 0 {
		t.Errorf("второй экземпляр подключился %d раз, ожидается 0 (ошибка ДО подключения)", api2.connects)
	}

	// Первый продолжает обслуживание; после его остановки mutex освобождён.
	if err := p1.Stop(); err != nil {
		t.Fatalf("Stop первого: %v", err)
	}
	if err := p2.Start(context.Background()); err != nil {
		t.Fatalf("Start после освобождения mutex: %v", err)
	}
	if err := p2.Stop(); err != nil {
		t.Fatalf("Stop второго: %v", err)
	}
}

// TestProviderMutexAvailableCheck — провайдер не подключается, если cldapi.dll
// недоступна (Available ошибку) — на машине без Cloud Files API.
func TestProviderMutexAvailableCheck(t *testing.T) {
	api := &fakeSyncRootAPI{registered: true, availableErr: errors.New("fake: dll не найдена")}
	p := New(t.TempDir(), "test-volume", api, nil)
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("Start при недоступной cldapi.dll должен вернуть ошибку")
	}
	if api.connects != 0 {
		t.Errorf("Connect вызван при недоступной DLL: %d раз, ожидается 0", api.connects)
	}
}

// TestProviderReadyMarkerOnce — спеки «Готовность» и «Hub недоступен»:
// после соединения и успешного наполнения верхнего уровня маркер
// `OK, <имя тома> is ready` выводится ровно один раз; повторное наполнение
// и второй Start маркер не дублируют. Первый List backend'а падает —
// цикл готовности повторяет попытки и завершается успехом.
func TestProviderReadyMarkerOnce(t *testing.T) {
	root := t.TempDir()
	api := &fakeSyncRootAPI{registered: true}
	backend := &fakeBackend{
		dirs: map[string][]BackendEntry{
			"/": {{Name: "top.txt", Inode: 3, Size: 1}},
		},
		failNum: 1, // первая попытка — отказ backend (недоступный hub)
	}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	idx, err := NewIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	p := New(root, "vol-one", api, cfapi.NewDispatcher(cfapi.DefaultQueueOptions()))
	p.backend = backend
	p.store = store
	p.idx = idx
	p.retryDelay = 2 * time.Millisecond

	hook := &logCapture{}
	logger.AddHook(hook)
	baseline := hook.count("OK, vol-one is ready")

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := p.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	// Маркер появляется после retry-цикла (первая попытка неудачна).
	deadline := time.Now().Add(5 * time.Second)
	for !p.Ready() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !p.Ready() {
		t.Fatal("маркер готовности не появился после восстановления backend")
	}
	if n := hook.count("OK, vol-one is ready"); n-baseline != 1 {
		t.Errorf("маркер в логе %d раз, ожидается ровно 1", n-baseline)
	}
	// Повторное наполнение — маркер не дублируется.
	if err := populateTopLevel(context.Background(), p); err != nil {
		t.Fatalf("повторное populateTopLevel: %v", err)
	}
	if n := hook.count("OK, vol-one is ready"); n-baseline != 1 {
		t.Errorf("после повторного наполнения маркер выведен %d раз, ожидается 1", n-baseline)
	}
}

// TestProviderReadyNoMarkerWhenBackendDown — спека «Hub недоступен»:
// наполнение не удаётся — маркера нет, процесс запущен и продолжает
// попытки (Stop их корректно завершает).
func TestProviderReadyNoMarkerWhenBackendDown(t *testing.T) {
	root := t.TempDir()
	api := &fakeSyncRootAPI{registered: true}
	backend := &fakeBackend{
		dirs:    map[string][]BackendEntry{},
		failNum: 1 << 30, // все попытки неудачны
	}
	store := &fakeStore{local: map[string]bool{}, localReal: map[string]bool{}}
	idx, err := NewIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	p := New(root, "vol-down", api, cfapi.NewDispatcher(cfapi.DefaultQueueOptions()))
	p.backend = backend
	p.store = store
	p.idx = idx
	p.retryDelay = time.Millisecond

	hook := &logCapture{}
	logger.AddHook(hook)
	baseline := hook.count("is ready")

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start при недоступном backend: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if p.Ready() {
		t.Error("маркер готовности выведен при недоступном backend")
	}
	if n := hook.count("is ready"); n-baseline != 0 {
		t.Errorf("строка маркера в логе: %d раз, ожидается 0", n-baseline)
	}
	// Попытки продолжаются, Stop завершает воркеров без зависания.
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestProviderStopDuringPopulation — Stop во время активной обработки
// FETCH_PLACEHOLDERS не зависает: воркер завершает текущее задание (wg.Wait
// ждёт его), затем выходит по снапшоту остановки (регрессия гонки p.stop).
func TestProviderStopDuringPopulation(t *testing.T) {
	root := t.TempDir()
	api := &fakeSyncRootAPI{registered: true}
	backend := &fakeBackend{dirs: map[string][]BackendEntry{
		"/":    {{Name: "top.bin", Inode: 1, Size: 1}},
		"/sub": {{Name: "a.txt", Inode: 2, Size: 1}},
	}}
	store := &slowAnswerStore{
		fakeStore: fakeStore{local: map[string]bool{}, localReal: map[string]bool{}},
		hold:      150 * time.Millisecond,
		entered:   make(chan struct{}),
	}
	idx, err := NewIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	dispatcher := cfapi.NewDispatcher(cfapi.DefaultQueueOptions())
	p := New(root, "vol-stop", api, dispatcher)
	p.backend = backend
	p.store = store
	p.idx = idx
	p.retryDelay = time.Millisecond
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Верхний уровень наполнен — цикл готовности завершился.
	deadline := time.Now().Add(5 * time.Second)
	for !p.Ready() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !p.Ready() {
		t.Fatal("верхний уровень не наполнился")
	}

	// Задание наполнения: воркер входит в задержанный ответ.
	if err := dispatcher.OnFetchPlaceholders(&cfapi.FetchPlaceholdersJob{
		Path:        filepath.Join(root, "sub"),
		TransferKey: cfapi.TransferKey(9),
	}); err != nil {
		t.Fatalf("OnFetchPlaceholders: %v", err)
	}
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("воркер не начал отвечать на задание наполнения")
	}

	// Stop ждёт завершения in-flight задания (wg.Wait), но не зависает:
	// после него воркер обязан выйти, а не застрять на nil-канале.
	done := make(chan error, 1)
	go func() {
		done <- p.Stop()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Stop вернул ошибку: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop завис во время активной обработки задания")
	}
	if len(store.answerCalls()) != 1 {
		t.Errorf("in-flight задание не завершено ответом: %d ответов", len(store.answerCalls()))
	}
}

// TestMutexName — имя mutex санитизировано: запрещённые символы пути
// заменены, разные корни дают разные имена.
func TestMutexName(t *testing.T) {
	name := mutexName(`C:\Users\u\AgioDrive`)
	if !strings.HasPrefix(name, `Local\juicefs-clouddrive-`) {
		t.Errorf("имя mutex без префикса: %q", name)
	}
	afterPrefix := strings.TrimPrefix(name, `Local\`)
	if strings.ContainsAny(afterPrefix, `\/:"<>|?*`) {
		t.Errorf("имя mutex содержит запрещённые символы: %q", name)
	}
	if mutexName(`C:\a`) == mutexName(`C:\b`) {
		t.Error("разные корни дали одно имя mutex")
	}
	// Имя реально допустимо для CreateMutex: захват и повторный захват
	// возвращают ERROR_ALREADY_EXISTS (kernel32, не cldapi.dll).
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	h1, err := windows.CreateMutex(nil, false, namePtr)
	if err != nil {
		t.Fatalf("первый CreateMutex: %v", err)
	}
	h2, err2 := windows.CreateMutex(nil, false, namePtr)
	if !errors.Is(err2, windows.ERROR_ALREADY_EXISTS) {
		t.Errorf("повторный CreateMutex вернул %v, ожидается ERROR_ALREADY_EXISTS", err2)
	}
	// Хендл валиден и при ALREADY_EXISTS — закрываем оба, чтобы не оставить
	// объект на следующие проверки.
	if h2 != 0 {
		_ = windows.CloseHandle(h2)
	}
	_ = windows.CloseHandle(h1)
	// Захваченный «чужой» процессом mutex (тот же процесс) блокирует второй
	// экземпляр — acquireInstanceMutex возвращает ошибку и не течёт хендлом.
	h3, err := windows.CreateMutex(nil, false, namePtr)
	if err != nil {
		t.Fatalf("CreateMutex: %v", err)
	}
	if _, err := acquireInstanceMutex(`C:\Users\u\AgioDrive`); err == nil {
		t.Error("acquireInstanceMutex захватил чужой mutex")
	}
	_ = windows.CloseHandle(h3)
}
