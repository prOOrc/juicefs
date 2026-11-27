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
// успешного Flush новый файл конвертируется в плейсхолдер с identity =
// inode backend БЕЗ дегидратации (данные остаются локально полными — спека
// «Новый файл»), изменённый плейсхолдер помечается in-sync
// (CfSetInSyncState; inSync=TrackNone при регистрации — состояние
// переключает провайдер).
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
	"encoding/binary"
	"errors"
	"io"
	"os"
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

// convertRetryDelaysDefault — паузы повторов конверсии после закрытия
// записи (живой ретест этапа 5: convert сразу после close отвергается
// фильтром с 0x8007017c, «на холодную» тот же convert проходит): первая
// попытка немедленно, затем повторы через 500 мс / 2 c / 5 c — суммарное
// окно ~7.5 c. Отказ последней попытки — Warn (содержимое выгружено;
// конверсию повторит следующий watcher-MODIFIED либо рестарт).
var convertRetryDelaysDefault = []time.Duration{500 * time.Millisecond, 2 * time.Second, 5 * time.Second}

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
// классификация состояния, пометка in-sync, конверсия нового файла.
// Реальная реализация — osStore; подменная — в тестах.
type ingestStore interface {
	// PlaceholderState — CF_PLACEHOLDER_STATE локального пути.
	PlaceholderState(path string) (uint32, error)
	// SetInSync — CfSetInSyncState(InSyncStateInSync) по пути.
	SetInSync(path string) error
	// ConvertFile — конверсия РЕАЛЬНОГО файла в плейсхолдер (файл, не
	// каталог; identity удерживает вызывающий до возврата).
	ConvertFile(path string, fileIdentity []byte, convertFlags uint32) error
}

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

// classifyNotifyClose — классификация NOTIFY_FILE_CLOSE_COMPLETION
// (задача 5.2, design.md D8 шаг 1): файл без плейсхолдера и плейсхолдер вне
// in-sync ставятся в очередь выгрузки; плейсхолдер in-sync, каталоги и пути
// вне sync root игнорируются. --read-only (спека «Режим без записи»)
// игнорирует всё с однократным логом.
func (p *Provider) classifyNotifyClose(job *cfapi.NotifyFileCloseCompletionJob) (ingestTask, bool) {
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
	local := localJoin(p.root, fsPath)
	if fi, err := os.Stat(local); err == nil && fi.IsDir() {
		// Каталоги — этап 6 (создание/переименование/удаление каталогов,
		// design.md D5): close-completion каталогов выгрузке не подлежит.
		return ingestTask{}, false
	}
	state, err := p.istore.PlaceholderState(local)
	if err != nil {
		// Только реальный отказ API (open/GetFileInformation — cfapi
		// трактует INVALID как NO_STATES): задание не ставится.
		logger.Warnf("close completion %s: placeholder state: %v; ignored", fsPath, err)
		return ingestTask{}, false
	}
	// Классификация по состоянию (design.md D8 шаг 1, живая проба этапа 5):
	// 0/NO_STATES — не плейсхолдер, новый файл пользователя → to-upload;
	// placeholder+in-sync — выгружать нечего (создан walk-ом/уже выгружен) →
	// ignore; placeholder без in-sync — изменённый → to-upload.
	switch {
	case state&cfapi.PlaceholderStatePlaceholder == 0:
		logger.Debugf("close completion %s: state=0x%x plain file -> upload", fsPath, state)
	case state&cfapi.PlaceholderStateInSync != 0:
		logger.Debugf("close completion %s: state=0x%x placeholder in sync -> ignore", fsPath, state)
		return ingestTask{}, false
	default:
		logger.Debugf("close completion %s: state=0x%x placeholder modified -> upload", fsPath, state)
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
			if task, ok := p.classifyNotifyClose(job); ok {
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
// успешного Flush — финализация плейсхолдера (finalizeIngest).
func (p *Provider) runIngest(ctx context.Context, task ingestTask) {
	started := time.Now()
	// Повторная классификация на момент обработки: за задержку коалисинга
	// состояние могло измениться (выгружено параллельным заданием, сверткой,
	// файл удалён/переименован пользователем).
	state, err := p.istore.PlaceholderState(task.local)
	if err != nil {
		logger.Warnf("ingest %s: placeholder state: %v; skipped", task.fsPath, err)
		return
	}
	if state&cfapi.PlaceholderStatePlaceholder != 0 && state&cfapi.PlaceholderStateInSync != 0 {
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
	// Локальный хендл закрывается ДО финализации: во время convert/update
	// у записи не остаётся других открытых хендлов serve (диагноз «файл
	// залочен, пока serve жив»).
	if err := src.Close(); err != nil {
		logger.Debugf("ingest %s: close local: %v", task.fsPath, err)
	}
	srcClosed = true
	p.finalizeIngest(ctx, task, state, f, size)
	logger.Debugf("ingest done: %s bytes=%d dur=%s", task.fsPath, size, time.Since(started).Truncate(time.Microsecond))
}

// finalizeIngest доводит локальную запись до конечного состояния (design.md
// D8 шаг 5) ПОСЛЕ подтверждённого Flush: изменённый плейсхолдер —
// CfSetInSyncState(InSyncStateInSync); новый файл — конверсия в плейсхолдер
// с identity = inode backend (U8) флагом MARK_IN_SYNC и БЕЗ DEHYDRATE —
// данные остаются локально полными (инвариант выгрузки: локальный файл
// не затирается). Конверсия — с повторами (см. convertRetryDelaysDefault):
// живой ретест этапа 5 показал, что фильтр короткое время после закрытия
// записи не принимает convert (0x8007017c), «на холодную» тот же convert
// проходит. Индекс актуализируется при изменении пути (D6.7).
func (p *Provider) finalizeIngest(ctx context.Context, task ingestTask, state uint32, f ingestFile, size int64) {
	ino := uint64(f.Inode())
	if state&cfapi.PlaceholderStatePlaceholder != 0 {
		if err := p.istore.SetInSync(task.local); err != nil {
			logger.Warnf("ingest %s: mark in sync: %v (content uploaded; state pending)", task.fsPath, err)
			return
		}
	} else {
		identity := make([]byte, 8)
		binary.LittleEndian.PutUint64(identity, ino)
		var cerr error
		for attempt := 0; ; attempt++ {
			cerr = p.istore.ConvertFile(task.local, identity, cfapi.ConvertFlagMarkInSync)
			if cerr == nil || attempt >= len(p.convertRetryDelays) {
				break
			}
			logger.Debugf("ingest %s: convert rejected (attempt %d): %v; retrying in %s", task.fsPath, attempt+1, cerr, p.convertRetryDelays[attempt])
			if !sleepCtx(ctx, p.stopChan(), p.convertRetryDelays[attempt]) {
				logger.Debugf("ingest %s: stopped between convert retries (content uploaded; conversion pending)", task.fsPath)
				return
			}
		}
		if cerr != nil {
			logger.Warnf("ingest %s: convert to placeholder: %v after %d attempts (content uploaded; conversion pending)", task.fsPath, cerr, len(p.convertRetryDelays)+1)
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
