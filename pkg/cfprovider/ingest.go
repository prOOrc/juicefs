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

// Пул выгрузки (design.md D8/D9, задачи 5.2–5.5): локальные изменения
// (новые файлы и изменённые плейсхолдеры) передаются в backend после
// закрытия файла. Источники заданий два: очередь NOTIFY_FILE_CLOSE_COMPLETION
// диспетчера (плейсхолдер-операции) и watcher ReadDirectoryChangesW
// (watcher.go — основной детектор plain-файлов после живой трассировки 27.11,
// design.md U6). Оба сходятся в классификацию (задача 5.2): путь через
// fsPathOf, состояние через PlaceholderState — плейсхолдер in-sync и
// каталоги игнорируются, прочее ставится в коалисер. Коалисер объединяет
// задания по пути с задержкой 2 с (задача 5.4; часы injectable — тесты).
// Пул воркеров (по умолчанию 2) выгружает: локальный файл читается os.Open,
// содержимое пишется в backend через write-API pkg/fs порциями не больше
// BlockSize из format, Truncate доводит длину, Flush — атомарное
// подтверждение версии (write-back выключен, задача 5.5); ТОЛЬКО после
// успешного Flush локальный user-файл заменяется дегидратированным
// плейсхолдером (U18-α replace-after-upload): конверсия user-файла из
// подключённого serve-процесса отвергается фильтром (INVALID_REQUEST при
// population=ALWAYS_FULL, живой ретест 27.11), поэтому имя удаляется и
// пересоздаётся createEntry-примитивом walk (design.md D6.4 — на своих
// файлах конверсия доказана): identity = inode backend, DEHYDRATE|MARK_IN_SYNC
// (контент идентичен только что выгруженному backend-у; гидрация при
// доступе — пул этапа 3). Инвариант (design.md D8, уточнение U18-α):
// локальный user-файл не теряется ДО успешного Flush; после Flush он
// заменяется дегидратированным плейсхолдером. Изменённый плейсхолдер
// помечается in-sync (CfSetInSyncState; inSync=TrackNone при регистрации —
// состояние переключает провайдер).
//
// Изоляция пулов (design.md D3): пул выгрузки НЕ обслуживает FETCH_DATA —
// тот приходит отдельному пулу гидратации; чтение локального
// файла-плейсхолдера с частичной гидрацией триггерит FETCH_DATA на фильтре,
// и его отвечает пул гидратации того же процесса — взаимной блокировки нет
// (запасной путь U7 — CfHydratePlaceholder из пула выгрузки — по D3
// разрешён, но для прототипа не нужен: recall-on-data-access фильтр
// выполняет сам).
//
// Write-back выключен (инвариант design.md): задание считается выполненным
// только после завершённого Flush. До Flush локальный файл только читается
// и не изменяется; отказ Flush оставляет плейсхолдер вне in-sync — задание
// повторится следующим close-уведомлением либо сверкой при старте (D9).
//
// U11 (задача 5.5): staging-файлы chunk store при падении процесса во время
// Flush остаются в CacheDir (pkg/chunk/staging); провайдер их не чистит —
// живая проба U11 решает, нужна ли очистка при старте; WriteJournalPath в
// обвязке serve не заполняется (cmd/clouddrive.go).
//
// cldapi.dll вызывается только через интерфейс ingestStore, write-API
// pkg/fs — через ingestBackend (инвариант «Движок проверяется без CfAPI»).
package cfprovider

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
	"github.com/juicedata/juicefs/pkg/fs"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/juicedata/juicefs/pkg/vfs"
)

// DefaultIngestWorkers — размер пула выгрузки по умолчанию (design.md D8).
const DefaultIngestWorkers = 2

// ingestDebounceDelay — задержка объединения заданий по пути (design.md D8
// шаг 2: 2 c — за это время индексатор и антивирус успевают переоткрыть
// файл; повторные закрытия не вызывают выгрузку).
const ingestDebounceDelay = 2 * time.Second

// ingestRemoveAttempts — ограничение повторов удаления локального файла
// после успешного Flush (U18-α replace-after-upload): пользователь мог
// переоткрыть файл (хендл без FILE_SHARE_DELETE — удаление невозможно).
// Попытки с нарастающей паузой ingestRetryDelay → ingestMaxRetryDelay;
// исчерпание — Warn «pending (file busy)», задание снимается: контент
// выгружен, повторит следующий close/рестарт.
const ingestRemoveAttempts = 5

// ingestFile — открытый на запись файл backend: минимальная поверхность
// *fs.File, нужная выгрузке. Реальная реализация — *fs.File
// (fs.Create / fs.Open с vfs.MODE_MASK_W); подменная — в тестах.
type ingestFile interface {
	// Pwrite пишет b с offset (порция выгрузки).
	Pwrite(ctx meta.Context, b []byte, off int64) (int, syscall.Errno)
	// Truncate доводит длину записи до length (в т.ч. сжатие относительно
	// прежней версии backend).
	Truncate(ctx meta.Context, length uint64) syscall.Errno
	// Flush — атомарное подтверждение версии (design.md D9).
	Flush(ctx meta.Context) syscall.Errno
	// Close закрывает файл (вызов defer'ом).
	Close(ctx meta.Context) syscall.Errno
	// Inode — inode записи backend (identity плейсхолдера, U8/D8).
	Inode() meta.Ino
}

// Сверка write-API pkg/fs (задача 5.1, design.md D8 шаг 1): API полный —
// fs.Create (O_EXCL), fs.Open(vfs.MODE_MASK_W), File.Write/Pwrite,
// File.Truncate, File.Flush (wdata.Flush — ожидание коммитов m.Write всех
// срезов), File.Fsync, File.Close, File.Inode; новый метод не потребовался,
// pkg/fs не менялся. *fs.File обязан удовлетворять интерфейсу выгрузки —
// проверка на этапе компиляции.
var _ ingestFile = (*fs.File)(nil)

// ingestBackend — write-API pkg/fs: минимальная поверхность fs.FileSystem,
// нужная выгрузке. Реальная реализация — fsIngestBackend; подменная — в
// тестах (инвариант «Движок проверяется без CfAPI»).
type ingestBackend interface {
	// Create создаёт новую запись backend (O_EXCL; занятое имя — EEXIST).
	Create(ctx meta.Context, path string) (ingestFile, syscall.Errno)
	// OpenWrite открывает существующую запись backend на запись.
	OpenWrite(ctx meta.Context, path string) (ingestFile, syscall.Errno)
}

// fsIngestBackend — ingestBackend на fs.FileSystem этапа 2 (WithData).
type fsIngestBackend struct {
	fsys *fs.FileSystem
}

func (b fsIngestBackend) Create(ctx meta.Context, path string) (ingestFile, syscall.Errno) {
	return b.fsys.Create(ctx, path, 0o644, 0)
}

func (b fsIngestBackend) OpenWrite(ctx meta.Context, path string) (ingestFile, syscall.Errno) {
	return b.fsys.Open(ctx, path, vfs.MODE_MASK_W)
}

// ingestStore — CfAPI-операции плейсхолдеров write-пути (design.md D8/U8):
// классификация состояния, пометка in-sync изменённого плейсхолдера.
// Пересоздание нового файла — createEntry-примитив через placeholderStore
// (U18-α). Реальная реализация — osStore; подменная — в тестах.
type ingestStore interface {
	// StandardInfo — CF_PLACEHOLDER_STANDARD_INFO локальной записи
	// (CfGetPlaceholderInfo): честное состояние плейсхолдера после
	// convert-DEHYDRATE — фильтр чистит basic-атрибуты, и FileBasicInfo-
	// эвристика (CF_PLACEHOLDER_STATE) возвращает 0 для реального
	// плейсхолдера → повторные выгрузки того же контента (write
	// amplification, живой ретест 27.11). Ошибка запроса — у записи нет
	// плейсхолдерности (plain-файл).
	StandardInfo(path string) (*cfapi.CFPlaceholderStandardInfo, error)
	// SetInSync — CfSetInSyncState(InSyncStateInSync) по пути.
	SetInSync(path string) error
}

// localRemover — удаление локального пути на финализации выгрузки
// (U18-α replace-after-upload: имя освобождается для пересоздания
// createEntry-примитивом). Реальная реализация — osRemover (os.Remove);
// подменная — в тестах (симуляция занятости и «пользователь пересоздал
// файл» между удалением и пересозданием).
type localRemover interface {
	Remove(path string) error
}

// osRemover — localRemover на os.Remove.
type osRemover struct{}

func (osRemover) Remove(path string) error { return os.Remove(path) }

// ingestTimer — таймер коалисера: минимальная поверхность time.Timer,
// подменяемая в тестах (канал срабатывания тестируемый).
type ingestTimer interface {
	Fired() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// wallTimer — ingestTimer на time.Timer.
type wallTimer struct{ t *time.Timer }

func newWallTimer(d time.Duration) ingestTimer {
	return wallTimer{t: time.NewTimer(d)}
}

func (w wallTimer) Fired() <-chan time.Time { return w.t.C }

func (w wallTimer) Stop() { w.t.Stop() }

func (w wallTimer) Reset(d time.Duration) { w.t.Reset(d) }

// ingestClock — часы коалисера (задача 5.4: injectable для тестов):
// текущее время и таймеры. Реальная реализация — wallClock.
type ingestClock interface {
	Now() time.Time
	NewTimer(d time.Duration) ingestTimer
}

// wallClock — ingestClock на package time.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) NewTimer(d time.Duration) ingestTimer { return newWallTimer(d) }

// ingestTask — задание выгрузки: путь записи (стиль fs) и локальный путь.
type ingestTask struct {
	fsPath string // "/a/b"
	local  string // <root>\a\b
	key    string // ключ коалисинга — fsPath в нижнем регистре (Windows-семантика)
}

// ingestDebouncer — объединение заданий по пути (задача 5.4, design.md D8
// шаг 2): уведомления о закрытии одного пути сливаются, задание выходит в
// out через delay после ПОСЛЕДНЕГО уведомления (классический debounce —
// серия переоткрытий от индексатора/антивируса выгружается один раз).
// Один цикл владеет картой отложенных заданий и единственным таймером
// ближайшего дедлайна; часы clock — injectable (тесты).
type ingestDebouncer struct {
	reqs  chan ingestTask
	out   chan ingestTask
	delay time.Duration
	clock ingestClock

	// Счётчики диагностики (по образцу счётчиков Dispatcher) — прогресс
	// юнит-тестов коалисинга: received — обработанные уведомления, fired —
	// потреблённые срабатывания таймера.
	mu       sync.Mutex
	received int64
	fired    int64
}

func newIngestDebouncer(delay time.Duration, clock ingestClock) *ingestDebouncer {
	return &ingestDebouncer{
		reqs:  make(chan ingestTask, 1024),
		out:   make(chan ingestTask),
		delay: delay,
		clock: clock,
	}
}

// submit ставит уведомление в коалисер; переполнение reqs блокирует
// маршрутизатор — естественная обратная связь в очередь диспетчера
// (CloseCompletion, ёмкость 4096) до коалисера.
func (db *ingestDebouncer) submit(task ingestTask) { db.reqs <- task }

// Received — число обработанных коалисером уведомлений (диагностика,
// синхронизация юнит-тестов задачи 5.4).
func (db *ingestDebouncer) Received() int64 {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.received
}

// FiredConsumed — число потреблённых срабатываний таймера (диагностика,
// синхронизация юнит-тестов задачи 5.4).
func (db *ingestDebouncer) FiredConsumed() int64 {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.fired
}

// run — цикл коалисера. Выход — отмена ctx или закрытие stop: отложенные
// задания отбрасываются (после перезапуска плейсхолдеры вне in-sync ставит
// в очередь сверка при старте — design.md D9, задача 6.4).
func (db *ingestDebouncer) run(ctx context.Context, stop chan struct{}) {
	pending := make(map[string]time.Time) // key → дедлайн
	tasks := make(map[string]ingestTask)  // key → задание (последнее побеждает)
	var timer ingestTimer
	// reschedule переставляет таймер на ближайший дедлайн (или снимает,
	// когда отложенных заданий нет).
	reschedule := func() {
		if len(pending) == 0 {
			if timer != nil {
				timer.Stop()
				timer = nil
			}
			return
		}
		var earliest time.Time
		first := true
		for _, dl := range pending {
			if first || dl.Before(earliest) {
				earliest = dl
				first = false
			}
		}
		d := earliest.Sub(db.clock.Now())
		if d < 0 {
			d = 0
		}
		if timer == nil {
			timer = db.clock.NewTimer(d)
		} else {
			timer.Reset(d)
		}
	}
	for {
		var fire <-chan time.Time
		if timer != nil {
			fire = timer.Fired()
		}
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case task := <-db.reqs:
			// Повторное уведомление по тому же пути отодвигает дедлайн —
			// серия закрытий выгружается один раз, последней версией.
			pending[task.key] = db.clock.Now().Add(db.delay)
			tasks[task.key] = task
			db.mu.Lock()
			db.received++
			db.mu.Unlock()
			reschedule()
		case now := <-fire:
			db.mu.Lock()
			db.fired++
			db.mu.Unlock()
			// Дедлайн: выпустить все задания, чей срок наступил.
			for key, dl := range pending {
				if dl.After(now) {
					continue
				}
				select {
				case db.out <- tasks[key]:
					delete(pending, key)
					delete(tasks, key)
				case <-ctx.Done():
					return
				case <-stop:
					return
				}
			}
			reschedule()
		}
	}
}

// selfEventSuppressWindow — окно супрессии собственных событий watcher'а
// после replace-after-upload финализации (U18-α): remove+create+следовые
// close записи укладываются с запасом (живой ретест 27.11: петля
// «upload → recreate → plain → upload» каждые 2-3 c без супрессии).
const selfEventSuppressWindow = 10 * time.Second

// selfEventSuppressed — стоит ли окно супрессии собственных событий на ключ
// (lower fsPath); until — дедлайн (диагностика тестов).
func (p *Provider) selfEventSuppressed(key string) (suppressed bool, until time.Time) {
	p.selfEventMu.Lock()
	defer p.selfEventMu.Unlock()
	until, ok := p.selfEventUntil[key]
	return ok && p.clock.Now().Before(until), until
}

// markSelfEvent ставит окно супрессии собственных событий на ключ
// (lower fsPath) от текущего времени часов провайдера (injectable — тесты).
func (p *Provider) markSelfEvent(key string) {
	p.selfEventMu.Lock()
	defer p.selfEventMu.Unlock()
	p.selfEventUntil[key] = p.clock.Now().Add(selfEventSuppressWindow)
}

// placeholderSyncState — честное состояние записи sync root через
// CF_PLACEHOLDER_STANDARD_INFO (CfGetPlaceholderInfo): isPlaceholder —
// у записи есть плейсхолдерность, inSync — она помечена синхронизированной.
// После convert-DEHYDRATE фильтр чистит basic-атрибуты — FileBasicInfo-
// эвристика (CF_PLACEHOLDER_STATE) возвращает 0 для РЕАЛЬНОГО плейсхолдера,
// что давало повторные выгрузки того же контента (write amplification,
// живой ретест 27.11); STANDARD-запрос работает и там. Отказ запроса —
// у записи нет плейсхолдерности (plain-файл, new file). Отказ из-за
// занятости (sharing violation — писатель держит запись) — повтор с
// нарастающей паузой до ingestMaxRetryDelay: писатель закроет запись.
func (p *Provider) placeholderSyncState(ctx context.Context, local string) (isPlaceholder, inSync bool, err error) {
	delay := p.ingestRetryDelay
	for attempt := 1; ; attempt++ {
		info, qerr := p.istore.StandardInfo(local)
		if qerr == nil {
			return true, info.InSyncState == cfapi.InSyncStateInSync, nil
		}
		if !isSharingViolation(qerr) {
			return false, false, qerr
		}
		if ctx.Err() != nil {
			return false, false, qerr
		}
		logger.Debugf("ingest %s: local record busy (attempt %d): %v; retrying in %s", local, attempt, qerr, delay)
		if !sleepCtx(ctx, p.stopChan(), delay) {
			return false, false, qerr
		}
		delay *= 2
		if delay > p.ingestMaxRetryDelay {
			delay = p.ingestMaxRetryDelay
		}
	}
}

// classifyNotifyClose — классификация NOTIFY_FILE_CLOSE_COMPLETION и
// watcher-событий (единая точка; задача 5.2, design.md D8 шаг 1): файл без
// плейсхолдера и плейсхолдер вне in-sync ставятся в очередь выгрузки;
// плейсхолдер in-sync, каталоги и пути вне sync root игнорируются.
// --read-only (спека «Режим без записи») игнорирует всё с однократным логом.
func (p *Provider) classifyNotifyClose(ctx context.Context, job *cfapi.NotifyFileCloseCompletionJob) (ingestTask, bool) {
	if p.readOnly {
		p.readOnlyOnce.Do(func() {
			logger.Info("read-only mode: local changes are not uploaded to the volume (--read-only)")
		})
		return ingestTask{}, false
	}
	fsPath := p.fsPathOf(job.Path)
	if fsPath == "" || fsPath == "/" {
		logger.Debugf("close completion %q: outside sync root, ignored", job.Path)
		return ingestTask{}, false
	}
	// Супрессия собственных событий (U18-α replace-after-upload): remove/
	// create записи финализацией — события watcher'а на САМОГО себя; в окне
	// отсутствия/пересоздания STANDARD-запрос отвечал «plain → upload» →
	// самоподдерживающаяся петля повторных выгрузок (живой ретест 27.11).
	if suppressed, _ := p.selfEventSuppressed(strings.ToLower(fsPath)); suppressed {
		logger.Debugf("close completion %s: self event suppressed (recreate window)", fsPath)
		return ingestTask{}, false
	}
	local := localJoin(p.root, fsPath)
	if fi, err := os.Stat(local); err == nil && fi.IsDir() {
		// Каталоги — этап 6 (создание/переименование/удаление каталогов,
		// design.md D5): close-completion каталогов выгрузке не подлежит.
		return ingestTask{}, false
	}
	// Классификация по честному STANDARD-состоянию (см. placeholderSyncState):
	// отказ запроса — plain-файл, новый файл пользователя → to-upload;
	// placeholder+in-sync — выгружать нечего (создан walk-ом/уже выгружен) →
	// ignore; placeholder без in-sync — изменённый → to-upload.
	isPlaceholder, inSync, err := p.placeholderSyncState(ctx, local)
	if err != nil {
		logger.Debugf("close completion %s: no placeholder record (%v) -> plain file -> upload", fsPath, err)
		return ingestTask{fsPath: fsPath, local: local, key: strings.ToLower(fsPath)}, true
	}
	if isPlaceholder && inSync {
		logger.Debugf("close completion %s: placeholder in sync -> ignore", fsPath)
		return ingestTask{}, false
	}
	if isPlaceholder {
		logger.Debugf("close completion %s: placeholder modified -> upload", fsPath)
	} else {
		logger.Debugf("close completion %s: plain file -> upload", fsPath)
	}
	return ingestTask{fsPath: fsPath, local: local, key: strings.ToLower(fsPath)}, true
}

// ingestRouter — маршрутизатор close-уведомлений (design.md D8 шаг 1):
// задания очереди CloseCompletion диспетчера классифицируются и ставятся в
// коалисер. NOTIFY-задания не требуют ответа CfExecute — Complete не нужен.
func (p *Provider) ingestRouter(ctx context.Context, db *ingestDebouncer, d *cfapi.Dispatcher, stop chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case job := <-d.CloseCompletion():
			if job == nil {
				continue
			}
			logger.Debugf("NOTIFY_FILE_CLOSE_COMPLETION: path=%q size=%d flags=%#x", job.Path, job.FileSize, job.Flags)
			if task, ok := p.classifyNotifyClose(ctx, job); ok {
				db.submit(task)
			}
		}
	}
}

// ingestWorker — воркер пула выгрузки: задание из коалисера → runIngest.
// Остановка — отмена ctx или закрытие stop (воркер дорабатывает текущий
// файл; недоведённые задания повторятся сверкой при старте — design.md D9).
func (p *Provider) ingestWorker(ctx context.Context, in <-chan ingestTask, stop chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case task := <-in:
			if task.fsPath == "" {
				continue
			}
			p.runIngest(ctx, task)
		}
	}
}

// runIngest выгружает один файл (design.md D8 шаги 3–5, D9): локальный файл
// открывается на чтение (повтор при отказе в совместном доступе),
// содержимое пишется в backend порциями не больше BlockSize, Truncate
// доводит длину, Flush подтверждает версию атомарно; только после
// успешного Flush — финализация (finalizeIngest): заменой на плейсхолдер
// (U18-α) либо пометкой in-sync изменённого плейсхолдера.
func (p *Provider) runIngest(ctx context.Context, task ingestTask) {
	started := time.Now()
	// Повторная классификация на момент обработки: за задержку коалисинга
	// состояние могло измениться (выгружено параллельным заданием, сверткой,
	// файл удалён/переименован пользователем) — тот же честный
	// STANDARD-запрос, что и в классификации (см. placeholderSyncState).
	isPlaceholder, inSync, serr := p.placeholderSyncState(ctx, task.local)
	if serr != nil {
		if ctx.Err() != nil {
			// Остановка во время повтора занятой записи — задание снимается
			// (недоведённое повторится следующим событием/рестартом).
			return
		}
		// Отказ запроса — у записи нет плейсхолдерности (plain-файл):
		// выгружаем как новый (семантика классификации).
		logger.Debugf("ingest %s: no placeholder record (%v) -> uploading as new file", task.fsPath, serr)
		isPlaceholder, inSync = false, false
	}
	if isPlaceholder && inSync {
		logger.Debugf("ingest %s: already in sync, skipped", task.fsPath)
		return
	}
	src, fi, err := p.openLocalSharedRead(ctx, task.local)
	if err != nil {
		// Файл удалён до выгрузки (или нечитаем): задание отменяется —
		// удаление в backend для подтверждённых версий — этап 6 (design.md
		// D9, сценарий «Удаление локального файла до выгрузки»).
		if ctx.Err() == nil {
			logger.Warnf("ingest %s: open local: %v; skipped", task.fsPath, err)
		}
		return
	}
	// Закрыть ДО финализации (defer-страховка — для ранних выходов): convert/
	// update плейсхолдера не должен видеть лишних открытых хендлов процесса
	// (живая диагностика этапа 5: файл оставался залоченным, пока serve жив).
	srcClosed := false
	defer func() {
		if !srcClosed {
			_ = src.Close()
		}
	}()
	if fi.IsDir() {
		return
	}
	size := fi.Size()

	mctx := meta.WrapWithCancel(ctx, uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
	defer mctx.Cancel()

	f, errno := p.ibackend.Create(mctx, task.fsPath)
	if errno != 0 {
		if errno != syscall.EEXIST {
			logger.Warnf("ingest %s: create backend entry: %v; skipped", task.fsPath, errno)
			return
		}
		// Имя занято в backend (гонка с другим клиентом либо наш плейсхолдер
		// без индексной записи — утерянный журнал): содержимое пишется
		// поверх существующей записи; финализация — по локальному состоянию.
		if f, errno = p.ibackend.OpenWrite(mctx, task.fsPath); errno != 0 {
			logger.Warnf("ingest %s: open backend entry: %v; skipped", task.fsPath, errno)
			return
		}
	}
	defer func() {
		if errno := f.Close(mctx); errno != 0 {
			logger.Warnf("ingest %s: close backend: %v", task.fsPath, errno)
		}
	}()

	buf := make([]byte, p.ingestBlockSize())
	var off int64
	for {
		if ctx.Err() != nil {
			// Остановка: версия не подтверждена — рестарт повторит выгрузку
			// (design.md D9 «Обрыв выгрузки»: backend не содержит частичной
			// версии; локальный файл не тронут).
			return
		}
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			if _, werrno := f.Pwrite(mctx, buf[:n], off); werrno != 0 {
				logger.Warnf("ingest %s: write backend: %v; skipped", task.fsPath, werrno)
				return
			}
			off += int64(n)
		}
		if rerr == nil {
			continue
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		logger.Warnf("ingest %s: read local: %v; skipped", task.fsPath, rerr)
		return
	}
	if errno := f.Truncate(mctx, uint64(size)); errno != 0 {
		logger.Warnf("ingest %s: truncate backend to %d: %v; skipped", task.fsPath, size, errno)
		return
	}
	// Атомарная фиксация (design.md D9, спека «Видимость версии»): версия
	// файла видна в backend ровно после Flush; финализация ниже — только
	// при успехе (write-back выключен, задача 5.5).
	if errno := f.Flush(mctx); errno != 0 {
		logger.Warnf("ingest %s: flush: %v (version not confirmed; local file untouched)", task.fsPath, errno)
		return
	}
	// Локальный хендл закрывается ДО финализации: пересоздание начинается
	// с удаления файла (U18-α), а собственный хендл без FILE_SHARE_DELETE
	// сделал бы удаление невозможным (0x80070020); во время convert/update
	// у записи также не остаётся других открытых хендлов serve (диагноз
	// «файл залочен, пока serve жив»).
	if err := src.Close(); err != nil {
		logger.Debugf("ingest %s: close local: %v", task.fsPath, err)
	}
	srcClosed = true
	p.finalizeIngest(ctx, task, isPlaceholder, f, size, fi.ModTime())
	logger.Debugf("ingest done: %s bytes=%d dur=%s", task.fsPath, size, time.Since(started).Truncate(time.Microsecond))
}

// finalizeIngest доводит локальную запись до конечного состояния (design.md
// D8 шаг 5) ПОСЛЕ подтверждённого Flush: изменённый плейсхолдер —
// CfSetInSyncState(InSyncStateInSync); новый user-файл — замена на
// дегидратированный плейсхолдер (U18-α replace-after-upload): локальное
// имя удаляется и пересоздаётся createEntry-примитивом walk (design.md
// D6.4) с identity = inode backend (U8) флагами MARK_IN_SYNC|DEHYDRATE —
// конверсия user-файла из подключённого serve-процесса отвергается
// фильтром (U18, INVALID_REQUEST при population=ALWAYS_FULL), а на своих
// файлах примитив работает; контент к этому моменту выгружен, гидрация
// при доступе — пул FETCH_DATA этапа 3. Отказ пересоздания — Warn,
// failed++, задание снимается: контент в backend не теряется, повторит
// следующий close/рестарт (восстановление удалённого файла данными
// невозможно — инвариант: файл не удаляется до успешного Flush).
// EEXIST при пересоздании (пользователь пересоздал файл между удалением
// и созданием) — переклассификация как в walk (classifyOccupied).
// Индекс актуализируется при изменении пути (D6.7).
func (p *Provider) finalizeIngest(ctx context.Context, task ingestTask, isPlaceholder bool, f ingestFile, size int64, mtime time.Time) {
	ino := uint64(f.Inode())
	if isPlaceholder {
		if err := p.istore.SetInSync(task.local); err != nil {
			logger.Warnf("ingest %s: mark in sync: %v (content uploaded; state pending)", task.fsPath, err)
			return
		}
	} else {
		// U18-α replace-after-upload: удалить локальный файл и пересоздать
		// его createEntry-примитивом (тот же, что walk) — дегидратированный
		// плейсхолдер с размером/mtime локальной записи. Перед удалением —
		// окно супрессии собственных событий watcher'а (remove/create на
		// себя): без него classify в окне отсутствия/пересоздания отвечает
		// «plain → upload» — самоподдерживающаяся петля (живой ретест).
		// Окно истекает естественно; при отказе пересоздания остаётся до
		// истечения — следующий close решит честно.
		p.markSelfEvent(strings.ToLower(task.fsPath))
		if !p.removeUploadedLocal(ctx, task) {
			return
		}
		kind, rerr := p.recreateUploaded(task, ino, size, mtime)
		switch kind {
		case resultCreated:
		// Пересоздан — индекс и финальная строка ниже (общий хвост).
		case resultExists:
			// Имя снова занято (пользователь пересоздал файл между
			// удалением и созданием): переклассификация как в walk
			// (design.md D6.4) — наш плейсхолдер восстанавливается в
			// индекс, не-плейсхолдер — конфликт (файл не трогается);
			// повторного создания нет. Контент выгружен.
			logger.Debugf("ingest %s: recreated name occupied after upload; reclassifying", task.fsPath)
			var examples []string
			classifyOccupied(p, &dirPopulation{dir: path.Dir(task.fsPath)}, BackendEntry{
				Name:  path.Base(task.fsPath),
				Inode: ino,
			}, task.fsPath, task.local, false, &examples)
			return
		default:
			p.ingestFailed.Add(1)
			logger.Warnf("ingest %s: recreate placeholder: %v (content uploaded; will retry on next close/restart)", task.fsPath, rerr)
			return
		}
	}
	if prev, ok := p.idx.Path(ino); !ok || prev != task.fsPath {
		if err := p.idx.Put(ino, task.fsPath); err != nil {
			logger.Warnf("ingest %s: index: %v", task.fsPath, err)
		}
	}
	logger.Infof("ingest %s: uploaded %d bytes, in sync", task.fsPath, size)
}

// removeUploadedLocal удаляет локальный файл после подтверждённого Flush
// (U18-α replace-after-upload). Занятость (пользователь переоткрыл файл —
// хендл без FILE_SHARE_DELETE) — повтор с нарастающей паузой
// ingestRetryDelay → ingestMaxRetryDelay, до ingestRemoveAttempts попыток;
// исчерпание — Warn, задание снимается: контент выгружен, повторит
// следующий close/рестарт. Файл, исчезнувший до удаления, — не ошибка:
// пересоздавать нечего (передача удаления подтверждённой версии в backend
// — этап 6, design.md D9).
func (p *Provider) removeUploadedLocal(ctx context.Context, task ingestTask) bool {
	delay := p.ingestRetryDelay
	for attempt := 1; ; attempt++ {
		err := p.remover.Remove(task.local)
		if err == nil {
			return true
		}
		if os.IsNotExist(err) {
			logger.Debugf("ingest %s: local file already gone after upload (content in backend); recreate skipped", task.fsPath)
			return false
		}
		if attempt >= ingestRemoveAttempts || !isSharingViolation(err) {
			logger.Warnf("ingest %s: pending (file busy), content uploaded, will retry on next close/restart: %v", task.fsPath, err)
			return false
		}
		logger.Debugf("ingest %s: local file busy after upload (attempt %d): %v; retrying in %s", task.fsPath, attempt, err, delay)
		if !sleepCtx(ctx, p.stopChan(), delay) {
			logger.Infof("ingest %s: stopped while local file busy after upload (content in backend; will retry on next close/restart)", task.fsPath)
			return false
		}
		delay *= 2
		if delay > p.ingestMaxRetryDelay {
			delay = p.ingestMaxRetryDelay
		}
	}
}

// recreateUploaded пересоздаёт выгруженный файл плейсхолдером
// createEntry-примитивом (U18-α; design.md D6.4): CREATE_NEW →
// SetEndOfFile(size) → SetFileTime(mtime локальной записи) →
// convert(MARK_IN_SYNC|DEHYDRATE). Размер и mtime — локальной записи
// (источник только что выгруженной версии); identity — inode backend.
func (p *Provider) recreateUploaded(task ingestTask, ino uint64, size int64, mtime time.Time) (placeholderResultKind, error) {
	return createEntry(p, path.Dir(task.fsPath), BackendEntry{
		Name:      path.Base(task.fsPath),
		Inode:     ino,
		Size:      size,
		MtimeSec:  mtime.Unix(),
		MtimeNsec: uint32(mtime.Nanosecond()),
		IsDir:     false,
	})
}

// openLocalSharedRead открывает локальный файл источника только на чтение.
//
// ФАКТ (Go): os.Open передаёт шаринг FILE_SHARE_READ|FILE_SHARE_WRITE —
// параллельная запись пользователем во время выгрузки НЕ исключена
// (design.md D8 шаг 3 предполагал FILE_SHARE_READ; содержимое при этом
// читается целостной версией: каждая выгрузка атомарна в backend — Flush,
// а новое закрытие файла ставит новое задание, последняя версия выгружается
// повторно). Отказ открытия (no-share-писатель или file mapping) — повтор с
// нарастающей паузой до успеха, остановки или отмены ctx (design.md D8
// шаг 3: «при отказе в совместном доступе задание повторяется»).
func (p *Provider) openLocalSharedRead(ctx context.Context, path string) (*os.File, os.FileInfo, error) {
	delay := p.ingestRetryDelay
	for attempt := 1; ; attempt++ {
		f, err := os.Open(path)
		if err == nil {
			fi, ferr := f.Stat()
			if ferr == nil {
				return f, fi, nil
			}
			_ = f.Close()
			err = ferr
		}
		if !isSharingViolation(err) {
			return nil, nil, err
		}
		logger.Debugf("ingest %s: local file busy (attempt %d): %v; retrying in %s", path, attempt, err, delay)
		// stop может быть nil (тесты логики без Start): sleepCtx ждёт
		// реальное время; остановка провайдера доходит закрытием stop либо
		// отменой ctx (serve отменяет его при завершении).
		if !sleepCtx(ctx, p.stopChan(), delay) {
			return nil, nil, err
		}
		delay *= 2
		if delay > p.ingestMaxRetryDelay {
			delay = p.ingestMaxRetryDelay
		}
	}
}

// isSharingViolation — отказ совместного доступа при открытии локального
// файла: ERROR_SHARING_VIOLATION (no-share-писатель), ERROR_LOCK_VIOLATION
// (диапазонная блокировка), ERROR_USER_MAPPED_FILE (file mapping). Прочие
// ошибки (ENOENT, EACCES) — не повторяются.
func isSharingViolation(err error) bool {
	var eno syscall.Errno
	if errors.As(err, &eno) {
		switch eno {
		case windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION, windows.ERROR_USER_MAPPED_FILE:
			return true
		}
	}
	return false
}

// ingestBlockSize — порция записи backend (design.md D8 шаг 4: блоки,
// кратные BlockSize из format; Format.BlockSize — KiB): нулевой формат или
// отсутствие data path — дефолт 4 MiB.
func (p *Provider) ingestBlockSize() int {
	const defaultBlock = 4 << 20
	if p.fsys == nil {
		return defaultBlock
	}
	if format := p.fsys.Meta().GetFormat(); format.BlockSize > 0 {
		return format.BlockSize * 1024
	}
	return defaultBlock
}

// StartIngest запускает пул выгрузки (этап 5, design.md D8) — вызывается из
// serve после маркера готовности, вместе с StartWalk (design.md D6, такт 2).
// Повторный вызов, вызов на остановленном провайдере или без data path /
// реального диспетчера пул не создают: false.
func (p *Provider) StartIngest(ctx context.Context) bool {
	p.ingestMu.Lock()
	defer p.ingestMu.Unlock()
	if !p.running || p.ingestStarted {
		return false
	}
	d, ok := p.dispatch.(*cfapi.Dispatcher)
	if !ok || !p.hasDataPath() || p.ibackend == nil {
		// Режим этапа 1 (без data path) или заглушка-диспетчер — выгрузки нет.
		return false
	}
	p.ingestStarted = true
	p.startIngestPool(ctx, d)
	return true
}

// startIngestPool запускает маршрутизатор close-уведомлений, коалисер и
// воркеров выгрузки (задачи 5.2–5.4). stop — снимок канала остановки
// (паттерн пулов: Stop закрывает канал и зануляет его под stopMu —
// захваченный закрытый канал читаем навсегда).
func (p *Provider) startIngestPool(ctx context.Context, d *cfapi.Dispatcher) {
	stop := p.stopChan()
	if stop == nil {
		return
	}
	db := newIngestDebouncer(p.ingestDelay, p.clock)
	logger.Debugf("ingest pool started: workers=%d delay=%s queue=%d",
		p.ingestWorkers, p.ingestDelay, d.QueueCapacity().CloseCompletion)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.ingestRouter(ctx, db, d, stop)
	}()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		db.run(ctx, stop)
	}()
	for i := 0; i < p.ingestWorkers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.ingestWorker(ctx, db.out, stop)
		}()
	}
	// Watcher ReadDirectoryChangesW (design.md U6, живая трассировка 27.11):
	// основной детектор user-изменений (фильтр не доставляет
	// NOTIFY_FILE_CLOSE_COMPLETION для plain-файлов); NOTIFY-канал остаётся
	// для плейсхолдер-операций. Оба источника сходятся в один коалисер.
	p.startFileWatcher(ctx, db, stop)
}
