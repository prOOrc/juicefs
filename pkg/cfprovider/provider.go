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

// Package cfprovider — движок провайдера Windows Cloud Files API
// (design.md): жизненный цикл соединения с sync root, индекс,
// плейсхолдеры, пулы гидратации и выгрузки (этапы 2+ лестницы MVP).
//
// Обращения к cldapi.dll — только через интерфейс cfapi.SyncRootAPI:
// движок проверяется на подменной реализации без зарегистрированного
// sync root (design.md, инвариант «Движок проверяется без CfAPI»).
package cfprovider

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
	"github.com/juicedata/juicefs/pkg/fs"
	"github.com/juicedata/juicefs/pkg/utils"
)

var logger = utils.GetLogger("cfprovider")

// Provider — провайдер Cloud Files API (design.md): проверка регистрации,
// единственный экземпляр (именованный mutex), подключение
// CfConnectSyncRoot с таблицей callback'ов; с подключённым data path
// (WithData) — наполнение плейсхолдерами, маркер готовности и воркер
// наполнения детей каталогов (этап 2 лестницы MVP).
type Provider struct {
	root     string // путь sync root, например %USERPROFILE%\AgioDrive
	volume   string // имя тома для логов (маркер готовности)
	api      cfapi.SyncRootAPI
	dispatch cfapi.CallbackDispatcher // nil никогда: New ставит заглушку-логгер

	// Data path этапа 2 (WithData; nil — режим этапа 1 без наполнения):
	fsys    *fs.FileSystem   // pkg/fs поверх meta+chunk (design.md Integration Points)
	idx     *Index           // inode → путь, журнал в каталоге состояния
	backend backendLister    // записи backend (реально — BackendFS)
	store   placeholderStore // локальный диск + CfAPI (реально — osStore)

	// Воркеры этапа 2: жизненный цикл внутренних горутин (наполнение,
	// готовность) — канал stop, закрывается в Stop; wg ждёт их завершения.
	// Отдельный mutex: Stop держит p.mu во время wg.Wait — воркеры читают
	// stop без p.mu, иначе взаимная блокировка.
	stopMu sync.RWMutex
	stop   chan struct{}
	wg     sync.WaitGroup

	// Маркер готовности (спека «Готовность», «Hub недоступен»): выводится
	// ровно один раз за запуск — после соединения и наполнения верхнего
	// уровня; readyLoop повторяет попытки с нарастающей паузой.
	readyOnce  sync.Once
	ready      atomic.Bool
	retryDelay time.Duration // база нарастающей паузы; тесты сокращают

	mu      sync.Mutex
	running bool
	conn    *cfapi.Connection // удерживает таблицу callback'ов и контекст всё соединение (design.md D7)
	mutex   windows.Handle    // именованный mutex единственного экземпляра
}

// New создаёт провайдера для sync root root. api == nil — реальный
// cldapi.dll (cfapi.OS); dispatch == nil — заглушка-логгер: задания
// callback'ов только логируются и не блокируют фильтр (пулы воркеров —
// этапы 2+; в тестах передаётся *cfapi.Dispatcher).
func New(root, volume string, api cfapi.SyncRootAPI, dispatch cfapi.CallbackDispatcher) *Provider {
	if api == nil {
		api = cfapi.OS
	}
	if dispatch == nil {
		dispatch = &loggingDispatcher{}
	}
	return &Provider{
		root:       root,
		volume:     volume,
		api:        api,
		dispatch:   dispatch,
		backend:    nil,
		store:      osStore{},
		retryDelay: 2 * time.Second,
	}
}

// WithData подключает data path (design.md Integration Points): fs.FileSystem
// поверх meta+chunk и индекс плейсхолдеров. Только после этого Start
// запускает наполнение и маркер готовности; без data path провайдер работает
// в режиме этапа 1 (соединение без наполнения — тесты).
func (p *Provider) WithData(fsys *fs.FileSystem, idx *Index) *Provider {
	p.fsys = fsys
	p.idx = idx
	if fsys != nil {
		p.backend = NewBackendFS(fsys)
		idx.SetResolver(func(ctx context.Context, ino uint64) (string, bool) {
			return resolveInoMeta(ctx, p, ino)
		})
	}
	return p
}

// hasDataPath — подключены ли backend и индекс (тесты ставят fake-backend
// напрямую, не через WithData).
func (p *Provider) hasDataPath() bool {
	return p.backend != nil && p.idx != nil
}

// Start запускает провайдера: проверка регистрации sync root, захват
// именованного mutex (второй экземпляр получает ошибку ДО подключения —
// спека «Второй экземпляр»), подключение к sync root. С подключённым data
// path дополнительно запускаются воркер наполнения детей каталогов и цикл
// готовности (наполнение верхнего уровня с повторами и маркер). ctx —
// жизненный цикл движка; внутренние горутины останавливаются также Stop.
func (p *Provider) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return errors.New("cfprovider: provider is already running")
	}
	if err := p.api.Available(); err != nil {
		return fmt.Errorf("cfprovider: cldapi.dll is not available (Windows 10 1709+ required): %w", err)
	}
	if !isSyncRootRegistered(p.api, p.root) {
		return fmt.Errorf("cfprovider: sync root %s is not registered; run 'juicefs clouddrive register --root %s' first", p.root, p.root)
	}

	// Единственный экземпляр: mutex до CfConnectSyncRoot (design.md D4).
	h, err := acquireInstanceMutex(p.root)
	if err != nil {
		return err
	}

	// Приёмник заданий устанавливается до подключения: callback'и могут
	// прийти сразу после CfConnectSyncRoot.
	cfapi.SetDispatcher(p.dispatch)
	conn, err := p.api.Connect(p.root, cfapi.CallbackTable(), 0, cfapi.ConnectFlagRequireFullFilePath)
	if err != nil {
		cfapi.SetDispatcher(nil)
		_ = windows.CloseHandle(h)
		return fmt.Errorf("cfprovider: CfConnectSyncRoot(%s): %w", p.root, err)
	}
	p.conn = conn
	p.mutex = h
	p.stopMu.Lock()
	p.stop = make(chan struct{})
	p.stopMu.Unlock()
	p.running = true
	if p.hasDataPath() {
		p.wg.Add(2)
		go func() {
			defer p.wg.Done()
			runPopulation(ctx, p)
		}()
		go func() {
			defer p.wg.Done()
			p.readyLoop(ctx)
		}()
	}
	logger.Infof("provider connected to sync root %s (volume %s)", p.root, p.volume)
	return nil
}

// Stop останавливает провайдера: отключение CfDisconnectSyncRoot,
// остановка внутренних горутин и освобождение именованного mutex.
// Идемпотентен: на не запущенном провайдере ничего не делает.
func (p *Provider) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return nil
	}
	var firstErr error
	cfapi.SetDispatcher(nil)
	if err := p.api.DisconnectSyncRoot(p.conn.Key); err != nil {
		firstErr = fmt.Errorf("cfprovider: CfDisconnectSyncRoot(%s): %w", p.root, err)
		logger.Error(firstErr)
	}
	p.conn = nil
	// Остановка воркеров этапа 2 до закрытия mutex: горутины завершаются
	// по закрытию stop, Stop ждёт их завершения.
	p.stopMu.Lock()
	if p.stop != nil {
		close(p.stop)
		p.stop = nil
	}
	p.stopMu.Unlock()
	p.wg.Wait()
	if p.mutex != 0 {
		// «Освобождение» mutex — закрытие единственного хендла: объект
		// уничтожается, следующий экземпляр сможет его создать. ReleaseMutex
		// не используется: владение mutex привязано к OS-потоку и ненадёжно
		// для горутин; mutex здесь — маркер существования, а не блокировка.
		if err := windows.CloseHandle(p.mutex); err != nil {
			logger.Warnf("CloseHandle(mutex): %v", err)
		}
		p.mutex = 0
	}
	p.running = false
	logger.Infof("provider stopped for sync root %s", p.root)
	return firstErr
}

// readyLoop добивается наполнения верхнего уровня и после успеха выводит
// маркер готовности ровно один раз (спеки «Готовность», «Hub недоступен»):
// недоступный hub (ошибки чтения backend) не завершает процесс — попытки
// продолжаются с нарастающей паузой до успеха, отмены ctx или Stop.
func (p *Provider) readyLoop(ctx context.Context) {
	const maxDelay = 30 * time.Second
	delay := p.retryDelay
	for attempt := 1; ; attempt++ {
		err := populateTopLevel(ctx, p)
		if err == nil {
			p.markReady()
			return
		}
		stop := p.stopChan()
		if ctx.Err() != nil || stop == nil {
			return
		}
		logger.Warnf("top-level population attempt %d failed: %v; retrying in %s", attempt, err, delay)
		if !sleepCtx(ctx, stop, delay) {
			return
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// stopChan — снимок канала остановки (nil после Stop).
func (p *Provider) stopChan() chan struct{} {
	p.stopMu.RLock()
	defer p.stopMu.RUnlock()
	return p.stop
}

// markReady выводит маркер готовности `OK, <имя тома> is ready` ровно один
// раз за запуск (design.md D4, контракт для supervisor'а agio-drive).
func (p *Provider) markReady() {
	p.readyOnce.Do(func() {
		p.ready.Store(true)
		logger.Infof("OK, %s is ready", p.volume)
	})
}

// Ready — выведен ли уже маркер готовности.
func (p *Provider) Ready() bool {
	return p.ready.Load()
}

// sleepCtx ждёт d либо первой из отмен: ctx или закрытия stop.
func sleepCtx(ctx context.Context, stop chan struct{}, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		case <-stop:
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

// fsPathOf переводит локальный путь под sync root в путь в стиле fs
// (разделитель "/", от корня); путь вне корня (в т.ч. ".."-относительный)
// и сам корень — "/".
func (p *Provider) fsPathOf(localPath string) string {
	rel, err := filepath.Rel(p.root, localPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, `..\`) {
		return "/"
	}
	return "/" + filepath.ToSlash(rel)
}

// acquireInstanceMutex захватывает именованный mutex единственного экземпляра
// провайдера для корня root. Имя производное от пути sync root (санитизирован:
// запрещённые в имени mutex символы заменяются), пространство имён Local —
// контроль в рамках сессии пользователя (сервисов у прототипа нет; поведение
// второго CfConnectSyncRoot на тот же корень — проба U10). Хендл закрывается
// в Stop — «освобождение» единственного экземпляра.
func acquireInstanceMutex(root string) (windows.Handle, error) {
	name := mutexName(root)
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("cfprovider: mutex name %q: %w", name, err)
	}
	h, err := windows.CreateMutex(nil, false, namePtr)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			// Хендл валиден даже при ERROR_ALREADY_EXISTS — закрываем его.
			_ = windows.CloseHandle(h)
			return 0, fmt.Errorf("cfprovider: another provider instance for %s is already running (mutex %s)", root, name)
		}
		return 0, fmt.Errorf("cfprovider: CreateMutex(%s): %w", name, err)
	}
	return h, nil
}

// mutexName — имя mutex для sync root root.
func mutexName(root string) string {
	safe := strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '"', '<', '>', '|', '?', '*':
			return '_'
		}
		return r
	}, root)
	return `Local\juicefs-clouddrive-` + safe
}

// isSyncRootRegistered проверяет регистрацию sync root: успешный запрос
// CF_SYNC_ROOT_BASIC_INFO по пути означает, что путь под зарегистрированным
// sync root (ошибка — не зарегистрирован или cldflt не видит корень;
// живое поведение — проба U1, в тестах — подменный api).
func isSyncRootRegistered(api cfapi.SyncRootAPI, root string) bool {
	var info cfapi.CFSyncRootBasicInfo
	buf := (*[unsafe.Sizeof(info)]byte)(unsafe.Pointer(&info))[:]
	var returned uint32
	return api.GetSyncRootInfoByPath(root, cfapi.SyncRootInfoBasic, buf, &returned) == nil
}

// loggingDispatcher — заглушка приёмника заданий этапа 1: каждое задание
// callback'а логируется и отбрасывается без ввода-вывода и без блокировки —
// фильтр не ломается (thunk возвращается сразу). Пулы воркеров движка,
// разбирающие очереди, — задачи этапов 2+.
type loggingDispatcher struct{}

func (loggingDispatcher) OnFetchData(job *cfapi.FetchDataJob) error {
	logger.Debugf("drop FETCH_DATA: transferKey=%d file=%d off=%d len=%d", job.TransferKey, job.FileID, job.RequiredOffset, job.RequiredLength)
	return nil
}

func (loggingDispatcher) OnCancelFetchData(job *cfapi.CancelFetchDataJob) error {
	logger.Debugf("drop CANCEL_FETCH_DATA: transferKey=%d", job.TransferKey)
	return nil
}

func (loggingDispatcher) OnFetchPlaceholders(job *cfapi.FetchPlaceholdersJob) error {
	logger.Debugf("drop FETCH_PLACEHOLDERS: transferKey=%d pattern=%q", job.TransferKey, job.Pattern)
	return nil
}

func (loggingDispatcher) OnCancelFetchPlaceholders(job *cfapi.CancelFetchPlaceholdersJob) error {
	logger.Debugf("drop CANCEL_FETCH_PLACEHOLDERS: transferKey=%d", job.TransferKey)
	return nil
}

func (loggingDispatcher) OnNotifyFileCloseCompletion(job *cfapi.NotifyFileCloseCompletionJob) error {
	logger.Debugf("drop NOTIFY_FILE_CLOSE_COMPLETION: path=%q size=%d", job.Path, job.FileSize)
	return nil
}

func (loggingDispatcher) OnNotifyFileRenamed(job *cfapi.NotifyFileRenamedJob) error {
	logger.Debugf("drop NOTIFY_FILE_RENAMED: %q -> %q", job.SourcePath, job.TargetPath)
	return nil
}

func (loggingDispatcher) OnNotifyFileDeleted(job *cfapi.NotifyFileDeletedJob) error {
	logger.Debugf("drop NOTIFY_FILE_DELETED: path=%q", job.Path)
	return nil
}

func (loggingDispatcher) OnNotifyDirectoryCreated(job *cfapi.NotifyDirectoryCreatedJob) error {
	logger.Debugf("drop NOTIFY_DIRECTORY_CREATED: path=%q", job.Path)
	return nil
}

func (loggingDispatcher) OnNotifyDirectoryRenamed(job *cfapi.NotifyDirectoryRenamedJob) error {
	logger.Debugf("drop NOTIFY_DIRECTORY_RENAMED: %q -> %q", job.SourcePath, job.TargetPath)
	return nil
}

func (loggingDispatcher) OnNotifyDirectoryDeleted(job *cfapi.NotifyDirectoryDeletedJob) error {
	logger.Debugf("drop NOTIFY_DIRECTORY_DELETED: path=%q", job.Path)
	return nil
}
