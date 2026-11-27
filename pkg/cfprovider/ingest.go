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
// Распространение пользовательских операций (этап 6, задачи 6.1–6.2,
// design.md D5/D9): удаление, переименование и создание каталогов передаются
// в backend ТОЛЬКО для записей, известных провайдеру как плейсхолдеры
// (inode в индексе по FileID/пути); неизвестные и локальные plain-файлы
// пользователя — никогда; удаление sync root — никогда (однократный Warn).
// Источники: очереди NOTIFY_FILE_RENAMED/NOTIFY_FILE_DELETED диспетчера
// (файл/каталог по *_IS_DIRECTORY-флагам — thunk'и) и запасной механизм U9
// для точки «Создание каталога» (watcher подаёт dir-события в очереди
// Directory*). Индекс ведёт rename-семантику: пере-резолв inode через
// backend Stat нового пути, каталог — RenamePrefix потомков, удаление —
// надгробия журнала. Неуспешные операции (недоступный hub) — в журнал
// ops.journal (задача 6.3, journal.go): replayer повторяет с нарастающей
// паузой 1с→30с до успеха, в том числе после перезапуска serve.
//
// Отмена выгрузки при удалении локального файла (задача 6.5, design.md D9):
// REMOVED watcher'а → handleLocalRemoved — активной выгрузке ставится флаг
// отмены: до подтверждения Flush задание снимается (версии в backend нет);
// после подтверждения — в журнал ставится удаление по inode. Реальное окно
// удаления ограничено шарингом os.Open (без FILE_SHARE_DELETE удаление
// блокируется, пока провайдер читает) — флаг закрывает гонку доставки
// события и окна debounce/финализации; файл, исчезнувший до удаления на
// финализации, тоже даёт удаление по inode.
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
// повторится следующим close-уведомлением, журналом либо сверкой при старте
// (D9).
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
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
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
// нужная выгрузке (этап 5) и распространению пользовательских операций
// (этап 6: удаление/переименование/создание каталога). Реальная реализация —
// fsIngestBackend; подменная — в тестах (инвариант «Движок проверяется без
// CfAPI»).
type ingestBackend interface {
	// Create создаёт новую запись backend (O_EXCL; занятое имя — EEXIST).
	Create(ctx meta.Context, path string) (ingestFile, syscall.Errno)
	// OpenWrite открывает существующую запись backend на запись.
	OpenWrite(ctx meta.Context, path string) (ingestFile, syscall.Errno)
	// Rename переименовывает запись backend (этап 6: rename-семантика meta
	// сохраняет inode — индекс сверяется через Stat нового пути).
	Rename(ctx meta.Context, oldpath, newpath string) syscall.Errno
	// Delete удаляет файл backend (fs.Unlink).
	Delete(ctx meta.Context, path string) syscall.Errno
	// Rmdir удаляет пустой каталог backend.
	Rmdir(ctx meta.Context, path string) syscall.Errno
	// Mkdir создаёт каталог backend.
	Mkdir(ctx meta.Context, path string) syscall.Errno
	// Stat возвращает inode и тип записи backend (пере-резолв inode после
	// rename/mkdir — точки D5 этапа 6).
	Stat(ctx meta.Context, path string) (ino uint64, isDir bool, errno syscall.Errno)
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

func (b fsIngestBackend) Rename(ctx meta.Context, oldpath, newpath string) syscall.Errno {
	return b.fsys.Rename(ctx, oldpath, newpath, 0)
}

func (b fsIngestBackend) Delete(ctx meta.Context, path string) syscall.Errno {
	return b.fsys.Unlink(ctx, path)
}

func (b fsIngestBackend) Rmdir(ctx meta.Context, path string) syscall.Errno {
	return b.fsys.Rmdir(ctx, path)
}

func (b fsIngestBackend) Mkdir(ctx meta.Context, path string) syscall.Errno {
	return b.fsys.Mkdir(ctx, path, 0o755, 0)
}

func (b fsIngestBackend) Stat(ctx meta.Context, path string) (uint64, bool, syscall.Errno) {
	fi, errno := b.fsys.Stat(ctx, path)
	if errno != 0 {
		return 0, false, errno
	}
	return uint64(fi.Inode()), fi.IsDir(), 0
}

// ingestStore — CfAPI-операции плейсхолдеров write-пути (design.md D8/U8):
// классификация состояния, пометка in-sync изменённого плейсхолдера и
// ответы на blocked-уведомления (U19: ACK_DELETE/ACK_RENAME — без них rm/mv
// пользователя висят). Пересоздание нового файла — createEntry-примитив
// через placeholderStore (U18-α). Реальная реализация — osStore; подменная —
// в тестах.
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
	// AnswerDelete — ACK_DELETE на уведомление NOTIFY_DELETE (U19; каталоги
	// — тот же колбэк с флагом IS_DIRECTORY): ровно один ответ на уведомление,
	// любой исход обработки.
	AnswerDelete(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, flags uint32, status cfapi.NTStatus) error
	// AnswerRename — ACK_RENAME на уведомление NOTIFY_RENAME (U19).
	AnswerRename(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, flags uint32, status cfapi.NTStatus) error
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

// uploadHandle — ручка активной выгрузки для отмены (задача 6.5, design.md
// D9 «Удаление локального файла с незавершённой выгрузкой»): watcher-событие
// REMOVED ставит флаг; runIngest проверяет его между порциями записи и сразу
// после подтверждённого Flush (версия уже в backend — журнал: удаление по
// inode).
type uploadHandle struct {
	cancel atomic.Bool
}

// uploadLocks — сериализация выгрузок одного пути: источников заданий два
// (watcher/NOTIFY и replayer журнала), параллельный runIngest одного пути
// писал бы двумя хендлами чересполосно в одну запись backend; второй запуск
// после первого переклассифицируется (in-sync — skip).
func (p *Provider) uploadPathLock(key string) func() {
	v, _ := p.uploadLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// trackUpload регистрирует активную выгрузку по ключу пути.
func (p *Provider) trackUpload(key string, h *uploadHandle) {
	p.activeMu.Lock()
	p.activeUploads[key] = h
	p.activeMu.Unlock()
}

// untrackUpload снимает выгрузку с учёта (идентичность ручки: повторная
// выгрузка того же пути не удаляет чужую регистрацию).
func (p *Provider) untrackUpload(key string, h *uploadHandle) {
	p.activeMu.Lock()
	if p.activeUploads[key] == h {
		delete(p.activeUploads, key)
	}
	p.activeMu.Unlock()
}

// handleLocalRemoved — событие REMOVED watcher'а (задача 6.5):
//   - выгрузка пути в полёте — флаг отмены: до Flush задание просто
//     снимается (версия не подтверждена); после Flush runIngest ставит в
//     журнал удаление по inode (design.md D9);
//   - активной выгрузки нет, но в журнале ждёт выгрузка этого пути
//     (задание снято занятостью файла или остановкой serve) — локальный
//     файл удалён, ожидающая выгрузка заменяется удалением: подтверждённая
//     версия подлежит удалению, отсутствие записи в backend исполняется
//     как ENOENT-успех.
//
// Собственные события финализации (replace-after-upload: remove+recreate,
// U18-α) отсекаются окном супрессии (markSelfEvent ставит его ДО remove).
func (p *Provider) handleLocalRemoved(localPath string) {
	fsPath := p.fsPathOf(localPath)
	if fsPath == "" || fsPath == "/" {
		return
	}
	key := strings.ToLower(fsPath)
	if suppressed, _ := p.selfEventSuppressed(key); suppressed {
		logger.Debugf("watcher %s: removed during recreate window (self event), ignored", fsPath)
		return
	}
	p.activeMu.Lock()
	h, active := p.activeUploads[key]
	p.activeMu.Unlock()
	if active {
		h.cancel.Store(true)
		logger.Debugf("watcher %s: removed during upload; cancellation requested", fsPath)
		return
	}
	if p.readOnly || p.journal == nil {
		return
	}
	if p.journal.HasPending(OpUpload, fsPath) {
		// inode — из индекса, если запись известна (исполнение удаления
		// почистит индекс; не известна — 0, индекс не трогается, fix-1.4).
		var ino uint64
		if p.idx != nil {
			if found, ok := p.idx.LookupPath(fsPath); ok {
				ino = found
			}
		}
		if err := p.journal.ReplaceUploadWithDelete(fsPath, ino); err != nil {
			logger.Warnf("watcher %s: journal replace upload with delete: %v", fsPath, err)
		} else {
			logger.Debugf("watcher %s: removed with pending upload; delete queued instead", fsPath)
		}
	}
}

// journalAdd — nil-безопасная запись операции в журнал провайдера (журнал
// может отсутствовать: режим без каталога состояния, часть тестов).
func (p *Provider) journalAdd(op JournalOp) {
	if p.journal == nil {
		return
	}
	if err := p.journal.Add(op); err != nil {
		logger.Warnf("journal %s %s: %v", op.Type, op.Path, err)
	}
}

// queueUpload ставит задание выгрузки в коалисер пула; пул ещё не поднят
// (верхний уровень до маркера) либо очередь полна — задание в журнал
// (replayer исполнит его с пулом; дедупликация по ключу).
func (p *Provider) queueUpload(task ingestTask) {
	p.ingestMu.Lock()
	db := p.ingestDb
	p.ingestMu.Unlock()
	if db != nil {
		select {
		case db.reqs <- task:
			return
		default:
		}
	}
	p.journalAdd(JournalOp{Type: OpUpload, Path: task.fsPath})
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
// (lower fsPath); until — дедлайн (диагностика тестов). Истекшая запись
// удаляется лениво (при обращении) — карта не растёт сверх числа
// выгрузок в окне.
func (p *Provider) selfEventSuppressed(key string) (suppressed bool, until time.Time) {
	p.selfEventMu.Lock()
	defer p.selfEventMu.Unlock()
	until, ok := p.selfEventUntil[key]
	if !ok {
		return false, time.Time{}
	}
	if p.clock.Now().Before(until) {
		return true, until
	}
	delete(p.selfEventUntil, key)
	return false, until
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

// stateQueryTimeout — жёсткий потолок STANDARD-запроса (U19, живая
// трассировка 27.11): запрос по записи в pending-delete (rm в полёте до
// нашего ACK_DELETE) блокируется в CreateFile БЕСКОНЕЧНО и без потолка
// убивал горутину-роутер — все уведомления и выгрузка стояли навсегда.
const stateQueryTimeout = 3 * time.Second

// errStateQueryTimeout — STANDARD-запрос не уложился в потолок: состояние
// недоступно (pending-delete/зависший CreateFile) — классификация трактуется
// как plain → to-upload; runIngest снимет задание по несчитываемому
// локальному файлу, ack уйдёт, пользователь увидит результат своего rm.
var errStateQueryTimeout = errors.New("cfprovider: state query timed out")

// boundedSyncState — placeholderSyncState с потолком времени: запрос идёт в
// отдельной горутине (таймаут отменяет её retry-цикл; одиночный вызов,
// зависший в CreateFile syscall'а, вернуть нельзя — горутина освобождается
// при ближайшем возврате/отмене ctx, прототип это принимает). Таймаут —
// errStateQueryTimeout.
func (p *Provider) boundedSyncState(ctx context.Context, local string) (isPlaceholder, inSync bool, err error) {
	timeout := p.stateQueryTimeout
	type stateResult struct {
		isPh, inSync bool
		err          error
	}
	ch := make(chan stateResult, 1)
	qctx, cancel := context.WithCancel(ctx)
	go func() {
		isPh, inSync, qerr := p.placeholderSyncState(qctx, local)
		ch <- stateResult{isPh: isPh, inSync: inSync, err: qerr}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		cancel()
		return r.isPh, r.inSync, r.err
	case <-timer.C:
		cancel()
		logger.Debugf("state query %s: timed out after %s; treating as plain file", local, timeout)
		return false, false, errStateQueryTimeout
	case <-ctx.Done():
		cancel()
		return false, false, ctx.Err()
	}
}

// classifyNotifyClose — классификация NOTIFY_FILE_CLOSE_COMPLETION и
// watcher-событий (единая точка; задача 5.2, design.md D8 шаг 1): файл без
// плейсхолдера и плейсхолдер вне in-sync ставятся в очередь выгрузки;
// плейсхолдер in-sync, каталоги и пути вне sync root игнорируются.
// --read-only (спека «Режим без записи») игнорирует всё с однократным логом.
//
// fromWatcher различает источник: окно супрессии собственных событий
// (markSelfEvent, replace-after-upload) отсекает только события watcher —
// он видит remove+create самой финализации. Фильтр не шлёт CLOSE_COMPLETION
// на провайдерские CfCreatePlaceholders с MARK_IN_SYNC (запись создаётся
// уже in-sync, dirty-состояния нет), поэтому callback-путь — ВСЕГДА
// настоящая правка пользователя: супрессия здесь съедала бы реальные правки
// в 10-секундном окне после выгрузки (живой кейс 27.11, AgioDrive8: правка
// через 4с после replace не выгружалась до следующего события/рестарта).
func (p *Provider) classifyNotifyClose(ctx context.Context, job *cfapi.NotifyFileCloseCompletionJob, fromWatcher bool) (ingestTask, bool) {
	if p.readOnly {
		p.readOnlyNotice()
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
	if fromWatcher {
		if suppressed, _ := p.selfEventSuppressed(strings.ToLower(fsPath)); suppressed {
			logger.Debugf("watcher close %s: self event suppressed (recreate window)", fsPath)
			return ingestTask{}, false
		}
	}
	local := localJoin(p.root, fsPath)
	if fi, err := os.Stat(local); err == nil && fi.IsDir() {
		// Каталоги — этап 6 (создание/переименование/удаление каталогов,
		// design.md D5): close-completion каталогов выгрузке не подлежит.
		return ingestTask{}, false
	}
	// Классификация по честному STANDARD-состоянию (см. boundedSyncState:
	// потолок времени — запрос по pending-delete блокируется навсегда, U19):
	// отказ запроса — plain-файл, новый файл пользователя → to-upload;
	// placeholder+in-sync — выгружать нечего (создан walk-ом/уже выгружен) →
	// ignore; placeholder без in-sync — изменённый → to-upload.
	isPlaceholder, inSync, err := p.boundedSyncState(ctx, local)
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

// ingestRouter — маршрутизатор NOTIFY-очередей диспетчера (design.md D5/D8):
// CloseCompletion — классификация выгрузки (этап 5); переименования и
// удаления (файлы и каталоги по *_IS_DIRECTORY-флагам) и создание каталогов
// (запасной механизм U9 — watcher) — распространение в backend ТОЛЬКО
// известных плейсхолдеров (этап 6, задачи 6.1–6.2). NOTIFY-задания не
// требуют ответа CfExecute — Complete не нужен.
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
			// Фильтр не шлёт CLOSE_COMPLETION на провайдерские
			// MARK_IN_SYNC-создания: callback-путь — всегда правка
			// пользователя, супрессия не применяется (fromWatcher=false).
			if task, ok := p.classifyNotifyClose(ctx, job, false); ok {
				db.submit(task)
			}
		case job := <-d.FileRenamed():
			if job == nil {
				continue
			}
			logger.Debugf("NOTIFY_FILE_RENAMED: %q -> %q", job.SourcePath, job.TargetPath)
			p.notifyRenamePaths(ctx, job.SourcePath, job.TargetPath, job.FileID, false, job.ConnectionKey, job.TransferKey, job.RequestKey)
		case job := <-d.FileDeleted():
			if job == nil {
				continue
			}
			logger.Debugf("NOTIFY_FILE_DELETED: path=%q", job.Path)
			p.notifyDeletePath(job.Path, job.FileID, false, job.ConnectionKey, job.TransferKey, job.RequestKey)
		case job := <-d.DirectoryCreated():
			if job == nil {
				continue
			}
			logger.Debugf("NOTIFY_DIRECTORY_CREATED: path=%q", job.Path)
			p.notifyDirCreate(ctx, job.Path)
		case job := <-d.DirectoryRenamed():
			if job == nil {
				continue
			}
			logger.Debugf("NOTIFY_DIRECTORY_RENAMED: %q -> %q", job.SourcePath, job.TargetPath)
			p.notifyRenamePaths(ctx, job.SourcePath, job.TargetPath, 0, true, job.ConnectionKey, job.TransferKey, job.RequestKey)
		case job := <-d.DirectoryDeleted():
			if job == nil {
				continue
			}
			logger.Debugf("NOTIFY_DIRECTORY_DELETED: path=%q", job.Path)
			p.notifyDeletePath(job.Path, 0, true, job.ConnectionKey, job.TransferKey, job.RequestKey)
		}
	}
}

// readOnlyNotice — однократный лог режима без записи для notify-операций
// (спека «Режим без записи»: локальные изменения не передаются в backend).
func (p *Provider) readOnlyNotice() {
	p.readOnlyOnce.Do(func() {
		logger.Info("read-only mode: local changes are not uploaded to the volume (--read-only)")
	})
}

// knownPlaceholder — известен ли провайдеру путь как плейсхолдер (задача
// 6.2, спека «Распространение удалений...»: передача только для записей,
// известных как плейсхолдеры): inode в индексе (по FileID уведомления либо
// по пути). Неизвестные записи и локальные plain-файлы пользователя в
// backend не передаются никогда.
func (p *Provider) knownPlaceholder(fileID uint64, fsPath string) (uint64, bool) {
	if fileID != 0 {
		if _, ok := p.idx.Path(fileID); ok {
			return fileID, true
		}
	}
	return p.idx.LookupPath(fsPath)
}

// backendHasEntry — существует ли запись backend по пути (проверка ложного
// ENOENT при rename: цель есть — запись валидна).
func (p *Provider) backendHasEntry(mctx meta.Context, fsPath string) bool {
	_, _, err := p.ibackend.Stat(mctx, fsPath)
	return err == 0
}

// deleteOpName — имя backend-операции удаления для следа трассировки.
func deleteOpName(isDir bool) string {
	if isDir {
		return "Rmdir"
	}
	return "Unlink"
}

// propagateDelete — удаление известного плейсхолдера в backend (задачи
// 6.1–6.2): fs.Unlink (файл) / fs.Rmdir (каталог); ENOENT — записи уже нет
// (исполнено). Успех — индекс чистится (каталог — с потомками).
func (p *Provider) propagateDelete(mctx meta.Context, fsPath string, ino uint64, isDir bool) syscall.Errno {
	// Трассировка U19: backend-хоп до/после (fs.Unlink/Rmdir → meta).
	logger.Debugf("delete %s: backend %s...", fsPath, deleteOpName(isDir))
	var eno syscall.Errno
	if isDir {
		eno = p.ibackend.Rmdir(mctx, fsPath)
	} else {
		eno = p.ibackend.Delete(mctx, fsPath)
	}
	logger.Debugf("delete %s: backend done, errno=%v", fsPath, eno)
	if eno != 0 && eno != syscall.ENOENT {
		return eno
	}
	if err := p.idx.Delete(ino); err != nil {
		logger.Warnf("delete %s: index: %v", fsPath, err)
	}
	if isDir {
		if _, err := p.idx.DeletePrefix(fsPath); err != nil {
			logger.Warnf("delete %s: index prefix: %v", fsPath, err)
		}
	}
	return 0
}

// notifyDeletePath — NOTIFY_FILE_DELETED / NOTIFY_DIRECTORY_DELETED (задачи
// 6.1–6.2; U19, живая проба 27.11: удаление blocked-style — rm пользователя
// не завершается, пока провайдер не ответит ACK_DELETE; каталоги — тот же
// OS-колбэк с флагом *_IS_DIRECTORY, отдельной ack-операции для них нет).
// Известный плейсхолдер → удаление в backend; sync root — никогда
// (однократный Warn); неизвестное/локальное — игнор. ЛЮБОЙ исход завершается
// РОВНО ОДНИМ AnswerDelete (defer — инвариант): backend-успех (и ENOENT —
// записи уже нет) → StatusSuccess, локальное удаление завершается; отказ
// backend → код отказа — фильтр завершает rm ошибкой, локальная запись
// остаётся, состояние консистентно, повтор — rm пользователя после
// восстановления (спека «Удаление офлайн»: «после повтора» ls не содержит
// запись); unknown/plain/sync-root/read-only — StatusSuccess (не блокировать
// пользователя; удаление plain-файла — локальное, backend не трогается).
func (p *Provider) notifyDeletePath(localPath string, fileID uint64, isDir bool, conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey) {
	// Трассировка U19: вход задания и исход ack — по ним ретест отличает
	// «фильтр не вызвал callback», «задание не дошло до движка» и «hang
	// внутри обработки».
	logger.Debugf("notify delete: path=%q fileID=%d isDir=%v", localPath, fileID, isDir)
	status := cfapi.StatusSuccess
	defer func() {
		logger.Debugf("notify delete %q: handled; ACK_DELETE status=0x%x", localPath, status)
		if err := p.istore.AnswerDelete(conn, transferKey, requestKey, cfapi.OperationAckDeleteFlagNone, status); err != nil {
			logger.Warnf("ack delete %q: %v", localPath, err)
		}
	}()
	if p.readOnly {
		p.readOnlyNotice()
		return
	}
	if p.ibackend == nil || p.idx == nil {
		return // режим этапа 1 (без data path) — распространять нечем
	}
	fsPath := p.fsPathOf(localPath)
	if fsPath == "" {
		logger.Debugf("delete %q: outside sync root, ignored", localPath)
		return
	}
	if fsPath == "/" {
		// Инвариант design.md: удаление sync root никогда не передаётся в
		// backend — serve при следующем старте пересоздаёт каталог.
		p.syncRootWarnOnce.Do(func() {
			logger.Warnf("sync root %s deletion is never propagated to the backend", p.root)
		})
		return
	}
	ino, known := p.knownPlaceholder(fileID, fsPath)
	if !known {
		logger.Debugf("delete %s: not a known placeholder; ignored (local entries are never propagated)", fsPath)
		return
	}
	mctx := meta.WrapWithCancel(context.Background(), uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
	defer mctx.Cancel()
	eno := p.propagateDelete(mctx, fsPath, ino, isDir)
	switch {
	case eno == 0:
		logger.Debugf("delete %s: removed from backend", fsPath)
	case permanentJournalErrno(eno):
		// fix-1.5 (data-safe): индекс НЕ снимать; с ack-отказом rm
		// завершается ошибкой и локальная запись остаётся — расхождения нет.
		logger.Warnf("delete %s: %v (delete denied; entry kept in backend and index)", fsPath, eno)
		status = cfapi.StatusFromErrno(eno)
	default:
		logger.Warnf("delete %s: %v; acknowledged with error (retry via user rm or next reconcile)", fsPath, eno)
		status = cfapi.StatusFromErrno(eno)
	}
}

// propagateRename — переименование известного плейсхолдера в backend и
// проводка индекса: inode нового пути пере-резолвится через backend Stat
// нового пути (rename в meta сохраняет inode — сверяется); каталог — пути
// потомков (RenamePrefix). ENOENT старой записи — ошибка перманентная
// (запись снята другим клиентом/журналом), индекс чистится.
func (p *Provider) propagateRename(mctx meta.Context, oldFsPath, newFsPath string, ino uint64, isDir bool) syscall.Errno {
	// Трассировка U19: backend-хоп до/после (fs.Rename → meta.Rename).
	logger.Debugf("rename %s -> %s: backend Rename...", oldFsPath, newFsPath)
	eno := p.ibackend.Rename(mctx, oldFsPath, newFsPath)
	logger.Debugf("rename %s -> %s: backend done, errno=%v", oldFsPath, newFsPath, eno)
	if eno != 0 {
		return eno
	}
	if err := p.indexAfterBackendRename(mctx, oldFsPath, newFsPath, ino, isDir); err != nil {
		logger.Warnf("rename %s -> %s: index: %v", oldFsPath, newFsPath, err)
	}
	return 0
}

// notifyRenamePaths — NOTIFY_FILE_RENAMED / NOTIFY_DIRECTORY_RENAMED (задачи
// 6.1–6.2; U19: blocked-style — mv пользователя не завершается без
// ACK_RENAME; каталоги — тот же OS-колбэк с флагом *_IS_DIRECTORY).
// Известный плейсхолдер → переименование в backend, индекс — на новый путь
// (inode сохраняется/пере-резолвится); sync root и неизвестные записи — игнор
// (plain-файл пользователя по новому пути выгрузит классификация
// watcher-события RENAMED_NEW_NAME). ЛЮБОЙ исход — РОВНО ОДИН AnswerRename
// (defer): backend-успех и ложный ENOENT (гонка — цель уже существует,
// fix-1.8) → StatusSuccess; отказ backend → код отказа — фильтр завершает mv
// ошибкой, запись остаётся на старом имени (локально и в backend),
// состояние консистентно, повтор — mv пользователя после восстановления.
// Изменённый плейсхолдер вне in-sync ПОСЛЕ успешного переименования
// (модификация в окне коалисинга — задание по старому пути снимется)
// ставится в выгрузку по новому пути.
func (p *Provider) notifyRenamePaths(ctx context.Context, sourcePath, targetPath string, fileID uint64, isDir bool, conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey) {
	// Трассировка U19 — см. notifyDeletePath.
	logger.Debugf("notify rename: %q -> %q fileID=%d isDir=%v", sourcePath, targetPath, fileID, isDir)
	status := cfapi.StatusSuccess
	defer func() {
		logger.Debugf("notify rename %q -> %q: handled; ACK_RENAME status=0x%x", sourcePath, targetPath, status)
		if err := p.istore.AnswerRename(conn, transferKey, requestKey, cfapi.OperationAckRenameFlagNone, status); err != nil {
			logger.Warnf("ack rename %q -> %q: %v", sourcePath, targetPath, err)
		}
	}()
	if p.readOnly {
		p.readOnlyNotice()
		return
	}
	if p.ibackend == nil || p.idx == nil {
		return
	}
	oldFsPath := p.fsPathOf(sourcePath)
	newFsPath := p.fsPathOf(targetPath)
	if oldFsPath == "" || newFsPath == "" {
		logger.Debugf("rename %q -> %q: outside sync root, ignored", sourcePath, targetPath)
		return
	}
	if oldFsPath == "/" || newFsPath == "/" {
		p.syncRootWarnOnce.Do(func() {
			logger.Warnf("sync root %s operations are never propagated to the backend", p.root)
		})
		return
	}
	if oldFsPath == newFsPath {
		return // no-op переименование (потерянная пара dir-событий)
	}
	ino, known := p.knownPlaceholder(fileID, oldFsPath)
	if !known {
		logger.Debugf("rename %s -> %s: not a known placeholder; ignored", oldFsPath, newFsPath)
		return
	}
	mctx := meta.WrapWithCancel(ctx, uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
	defer mctx.Cancel()
	eno := p.propagateRename(mctx, oldFsPath, newFsPath, ino, isDir)
	switch {
	case eno == 0:
		logger.Debugf("rename %s -> %s: renamed in backend", oldFsPath, newFsPath)
	case eno == syscall.ENOENT && p.backendHasEntry(mctx, newFsPath):
		// Ложный ENOENT (гонка: переименование успел исполнить другой
		// источник/клиент) — цель существует: индекс переносим, валидную
		// запись не сносим (fix-1.8); исход для пользователя успешный.
		logger.Debugf("rename %s -> %s: ENOENT raced, target exists; keeping entry", oldFsPath, newFsPath)
		if err := p.indexAfterBackendRename(mctx, oldFsPath, newFsPath, ino, isDir); err != nil {
			logger.Warnf("rename %s -> %s: index: %v", oldFsPath, newFsPath, err)
		}
	case eno == syscall.ENOENT:
		logger.Warnf("rename %s -> %s: backend entry is gone; dropped", oldFsPath, newFsPath)
		if err := p.idx.Delete(ino); err != nil {
			logger.Warnf("rename %s: index: %v", oldFsPath, err)
		}
		if isDir {
			if _, err := p.idx.DeletePrefix(oldFsPath); err != nil {
				logger.Warnf("rename %s: index prefix: %v", oldFsPath, err)
			}
		}
		status = cfapi.StatusObjectNameNotFound
	case permanentJournalErrno(eno) || eno == syscall.ENOTEMPTY || eno == syscall.EEXIST:
		logger.Warnf("rename %s -> %s: %v (permanent; dropped)", oldFsPath, newFsPath, eno)
		status = cfapi.StatusFromErrno(eno)
	default:
		logger.Warnf("rename %s -> %s: %v; acknowledged with error (retry via user mv)", oldFsPath, newFsPath, eno)
		status = cfapi.StatusFromErrno(eno)
	}
	if eno != 0 || isDir {
		return // отказ — локальный mv будет откачен фильтром; каталоги не выгружаются
	}
	// Изменённый плейсхолдер, переименованный в окне коалисинга: задание по
	// старому пути снимется (файла нет) — выгрузка по новому пути
	// (callback-семантика: супрессия не применяется).
	newLocal := localJoin(p.root, newFsPath)
	if info, err := p.istore.StandardInfo(newLocal); err == nil && info.InSyncState != cfapi.InSyncStateInSync {
		if task, ok := p.classifyNotifyClose(ctx, &cfapi.NotifyFileCloseCompletionJob{Path: targetPath}, false); ok {
			p.queueUpload(task)
		}
	}
}

// propagateDirCreate — распространение создания каталога в backend (спека
// «Новый каталог»): Mkdir (EEXIST — запись уже есть, не ошибка), inode —
// через backend Stat, локальный каталог конвертируется в плейсхолдер
// (MARK_IN_SYNC — как в create-примитиве walk, design.md D6.4) и попадает
// в индекс.
func (p *Provider) propagateDirCreate(ctx context.Context, fsPath string) error {
	local := localJoin(p.root, fsPath)
	fi, err := os.Stat(local)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return permanentOpErrorf("%s is not a directory", local)
	}
	mctx := meta.WrapWithCancel(ctx, uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
	defer mctx.Cancel()
	eno := p.ibackend.Mkdir(mctx, fsPath)
	if eno != 0 && eno != syscall.EEXIST {
		return eno
	}
	ino, _, serr := p.ibackend.Stat(mctx, fsPath)
	if serr != 0 {
		return serr
	}
	// Наш плейсхолдер уже на диске (гонка с walk: его конверсия могла
	// отстать от индекса) — усыновление без повторной конверсии.
	if exists, isPh, serr := p.store.StateOf(local); serr == nil && exists && isPh {
		if err := p.idx.Put(ino, fsPath); err != nil {
			logger.Warnf("mkdir %s: index: %v", fsPath, err)
		}
		return nil
	}
	if _, known := p.idx.Path(ino); known {
		// Конверсия — от walk (createEntry), inode уже его: не конвертируем
		// повторно (повторная конверсия плейсхолдера отвергается фильтром).
		if err := p.idx.Put(ino, fsPath); err != nil {
			logger.Warnf("mkdir %s: index: %v", fsPath, err)
		}
		return nil
	}
	identity := make([]byte, 8)
	binary.LittleEndian.PutUint64(identity, ino)
	if cerr := p.store.Convert(local, true, identity, cfapi.ConvertFlagMarkInSync); cerr != nil {
		return fmt.Errorf("convert %s: %w", fsPath, cerr)
	}
	if err := p.idx.Put(ino, fsPath); err != nil {
		logger.Warnf("mkdir %s: index: %v", fsPath, err)
	}
	return nil
}

// notifyDirCreate — создание каталога в sync root (задача 6.1, точка D5
// «Создание каталога»; источник — запасной механизм U9: watcher подаёт
// dir-события ADDED в очередь DirectoryCreated диспетчера): walk-созданные
// каталоги (индекс) и каталоги в повторе журнала — skip; новый каталог →
// backend Mkdir + convert + индекс; недоступный hub → журнал (replayer).
func (p *Provider) notifyDirCreate(ctx context.Context, localPath string) {
	// Трассировка U19 — см. notifyDeletePath (исходы логируются в ветках ниже).
	logger.Debugf("notify dir create: path=%q", localPath)
	if p.readOnly {
		p.readOnlyNotice()
		return
	}
	if p.ibackend == nil || p.idx == nil {
		return
	}
	fsPath := p.fsPathOf(localPath)
	if fsPath == "" || fsPath == "/" {
		logger.Debugf("dir create %q: outside sync root, ignored", localPath)
		return
	}
	if _, known := p.idx.LookupPath(fsPath); known {
		logger.Debugf("dir create %s: known placeholder (walk), skipped", fsPath)
		return
	}
	if p.journal != nil && p.journal.HasPending(OpMkdir, fsPath) {
		return // повтор уже ведёт replayer
	}
	err := p.propagateDirCreate(ctx, fsPath)
	switch {
	case err == nil:
		logger.Infof("ingest %s: directory created, in sync", fsPath)
	case errors.Is(err, os.ErrNotExist), isPermanentOpError(err):
		logger.Debugf("dir create %s: %v; dropped", fsPath, err)
	case permanentJournalErrno(errToErrno(err)):
		logger.Warnf("dir create %s: %v (permanent; dropped)", fsPath, err)
	default:
		logger.Warnf("dir create %s: %v; queued for retry", fsPath, err)
		p.journalAdd(JournalOp{Type: OpMkdir, Path: fsPath})
	}
}

// errToErrno — errno из цепочки ошибки (для классификации перманентности).
func errToErrno(err error) syscall.Errno {
	var eno syscall.Errno
	if errors.As(err, &eno) {
		return eno
	}
	return 0
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
//
// Неуспешные операции пишутся в журнал (задача 6.3, design.md D9): отказ
// записи/Truncate/Flush (недоступный hub — спека «Отложенные операции при
// недоступном hub»), занятость файла на replace-after-upload — повтор
// replayer'ом с нарастающей паузой, в том числе после перезапуска.
// Возврат ошибки — для replayer'а (журнала): nil — исполнено/исполнение
// не требуется; opPermanentError — снять с учёта; прочее — повторить.
// Воркеры пула результат игнорируют (повтор — следующий close/журнал).
//
// Отмена при удалении локального файла в окне выгрузки (задача 6.5): флаг
// uploadHandle проверяется между порциями (версия не подтверждена —
// задание просто снимается) и сразу после Flush (версия подтверждена,
// локальная запись удалена пользователем — журнал: удаление по inode).
func (p *Provider) runIngest(ctx context.Context, task ingestTask) error {
	started := time.Now()
	unlock := p.uploadPathLock(task.key)
	defer unlock()
	h := &uploadHandle{}
	p.trackUpload(task.key, h)
	defer p.untrackUpload(task.key, h)
	// Повторная классификация на момент обработки: за задержку коалисинга
	// состояние могло измениться (выгружено параллельным заданием, сверткой,
	// файл удалён/переименован пользователем) — тот же честный
	// STANDARD-запрос под потолком времени (см. boundedSyncState, U19).
	isPlaceholder, inSync, serr := p.boundedSyncState(ctx, task.local)
	if serr != nil {
		if ctx.Err() != nil {
			// Остановка во время повтора занятой записи — задание снимается
			// (недоведённое повторится следующим событием/рестартом).
			return nil
		}
		// Отказ запроса — у записи нет плейсхолдерности (plain-файл):
		// выгружаем как новый (семантика классификации).
		logger.Debugf("ingest %s: no placeholder record (%v) -> uploading as new file", task.fsPath, serr)
		isPlaceholder, inSync = false, false
	}
	if isPlaceholder && inSync {
		logger.Debugf("ingest %s: already in sync, skipped", task.fsPath)
		return nil
	}
	src, fi, err := p.openLocalSharedRead(ctx, task.local)
	if err != nil {
		// Файл удалён до выгрузки (или нечитаем): задание отменяется —
		// задание не ставится в журнал (выгружать нечего); защита
		// подтверждённых версий — REMOVED/ReplaceUploadWithDelete (6.5).
		if ctx.Err() == nil {
			logger.Warnf("ingest %s: open local: %v; skipped", task.fsPath, err)
			return permanentOpErrorf("local file %s is gone", task.fsPath)
		}
		return nil
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
		return nil
	}
	size := fi.Size()
	if h.cancel.Load() {
		// Удаление в окне коалисинга/повтора занятости (задача 6.5):
		// версия не подтверждена — просто снимаем задание.
		logger.Debugf("ingest %s: canceled (local file removed), version not confirmed", task.fsPath)
		return nil
	}

	mctx := meta.WrapWithCancel(ctx, uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
	defer mctx.Cancel()

	f, errno := p.ibackend.Create(mctx, task.fsPath)
	if errno != 0 {
		if errno != syscall.EEXIST {
			logger.Warnf("ingest %s: create backend entry: %v; skipped", task.fsPath, errno)
			p.journalAdd(JournalOp{Type: OpUpload, Path: task.fsPath})
			return errno
		}
		// Имя занято в backend (гонка с другим клиентом либо наш плейсхолдер
		// без индексной записи — утерянный журнал): содержимое пишется
		// поверх существующей записи; финализация — по локальному состоянию.
		if f, errno = p.ibackend.OpenWrite(mctx, task.fsPath); errno != 0 {
			logger.Warnf("ingest %s: open backend entry: %v; skipped", task.fsPath, errno)
			p.journalAdd(JournalOp{Type: OpUpload, Path: task.fsPath})
			return errno
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
			return nil
		}
		if h.cancel.Load() {
			// Удаление в окне выгрузки до Flush (задача 6.5): версия не
			// подтверждена — задание снимается без журнала; удаление
			// существовавшей версии донесёт NOTIFY_FILE_DELETED (плейсхолдер).
			logger.Infof("ingest %s: canceled (local file removed before flush)", task.fsPath)
			return nil
		}
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			if _, werrno := f.Pwrite(mctx, buf[:n], off); werrno != 0 {
				logger.Warnf("ingest %s: write backend: %v; skipped", task.fsPath, werrno)
				p.journalAdd(JournalOp{Type: OpUpload, Path: task.fsPath})
				return werrno
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
		return rerr
	}
	if h.cancel.Load() {
		// Удаление в окне выгрузки до подтверждения (задача 6.5): версия не
		// подтверждена — задание снимается без журнала.
		logger.Infof("ingest %s: canceled (local file removed before flush)", task.fsPath)
		return nil
	}
	if errno := f.Truncate(mctx, uint64(size)); errno != 0 {
		logger.Warnf("ingest %s: truncate backend to %d: %v; skipped", task.fsPath, size, errno)
		p.journalAdd(JournalOp{Type: OpUpload, Path: task.fsPath})
		return errno
	}
	// Атомарная фиксация (design.md D9, спека «Видимость версии»): версия
	// файла видна в backend ровно после Flush; финализация ниже — только
	// при успехе (write-back выключен, задача 5.5).
	if errno := f.Flush(mctx); errno != 0 {
		logger.Warnf("ingest %s: flush: %v (version not confirmed; local file untouched)", task.fsPath, errno)
		p.journalAdd(JournalOp{Type: OpUpload, Path: task.fsPath})
		return errno
	}
	// Версия подтверждена (flushed): далее удаление локальной записи — уже
	// не «отмена», а удаление подтверждённой версии (журнал по inode).
	if h.cancel.Load() {
		ino := uint64(f.Inode())
		logger.Infof("ingest %s: upload confirmed, local file deleted by user; delete queued by inode %d", task.fsPath, ino)
		p.journalAdd(JournalOp{Type: OpDelete, Path: task.fsPath, Ino: ino})
		return nil
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
	return nil
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
		if !p.removeUploadedLocal(ctx, task, ino) {
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
			// повторного создания нет. Контент выгружен. ПОЛНЫЙ entry
			// (size/mtime): усыновление гидратированного своего
			// плейсхолдера вызывает ensureDehydrated → Update с
			// метаданными записи — урезанный entry дегидратировал бы
			// в нулевой размер/эпоху.
			logger.Warnf("ingest %s: recreated name occupied after upload; reclassifying", task.fsPath)
			var examples []string
			classifyOccupied(p, &dirPopulation{dir: path.Dir(task.fsPath)}, uploadedEntry(task, ino, size, mtime), task.fsPath, task.local, false, &examples)
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
// исчерпание — Warn, задание снимается, в журнал ставится выгрузка
// (задача 6.3/D9: replayer повторит и завершит замену; контент выгружен).
// Файл, исчезнувший до удаления (пользователь удалил подтверждённую
// версию, задача 6.5), — журнал: удаление по inode; пересоздавать нечего.
func (p *Provider) removeUploadedLocal(ctx context.Context, task ingestTask, ino uint64) bool {
	delay := p.ingestRetryDelay
	for attempt := 1; ; attempt++ {
		err := p.remover.Remove(task.local)
		if err == nil {
			return true
		}
		if os.IsNotExist(err) {
			logger.Debugf("ingest %s: local file already gone after upload (deleted by user); delete queued by inode %d", task.fsPath, ino)
			p.journalAdd(JournalOp{Type: OpDelete, Path: task.fsPath, Ino: ino})
			return false
		}
		if attempt >= ingestRemoveAttempts || !isSharingViolation(err) {
			logger.Warnf("ingest %s: pending (file busy), content uploaded, will retry on next close/restart: %v", task.fsPath, err)
			p.journalAdd(JournalOp{Type: OpUpload, Path: task.fsPath})
			return false
		}
		logger.Debugf("ingest %s: local file busy after upload (attempt %d): %v; retrying in %s", task.fsPath, attempt, err, delay)
		if !sleepCtx(ctx, p.stopChan(), delay) {
			logger.Infof("ingest %s: stopped while local file busy after upload (content in backend; will retry on next close/restart)", task.fsPath)
			p.journalAdd(JournalOp{Type: OpUpload, Path: task.fsPath})
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
	return createEntry(p, path.Dir(task.fsPath), uploadedEntry(task, ino, size, mtime))
}

// uploadedEntry — полная BackendEntry выгруженной записи (имя, inode,
// размер и mtime локальной записи): один источник для recreateUploaded и
// EEXIST-реклассификации — урезанный entry в classifyOccupied дегидратировал
// бы усыновлённый плейсхолдер в нулевой размер/эпоху (ensureDehydrated →
// Update(makeFsMetadata(e))).
func uploadedEntry(task ingestTask, ino uint64, size int64, mtime time.Time) BackendEntry {
	return BackendEntry{
		Name:      path.Base(task.fsPath),
		Inode:     ino,
		Size:      size,
		MtimeSec:  mtime.Unix(),
		MtimeNsec: uint32(mtime.Nanosecond()),
		IsDir:     false,
	}
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

// startIngestPool запускает маршрутизатор NOTIFY-очередей, коалисер, воркеров
// выгрузки (задачи 5.2–5.4), watcher (U6) и replayer журнала операций
// (задача 6.3). stop — снимок канала остановки (паттерн пулов: Stop закрывает
// канал и зануляет его под stopMu — захваченный закрытый канал читаем
// навсегда).
func (p *Provider) startIngestPool(ctx context.Context, d *cfapi.Dispatcher) {
	stop := p.stopChan()
	if stop == nil {
		return
	}
	db := newIngestDebouncer(p.ingestDelay, p.clock)
	p.ingestDbMu.Lock()
	p.ingestDb = db
	p.ingestDbMu.Unlock()
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
	// для плейсхолдер-операций. Оба источника сходятся в один коалисер;
	// dir-события watcher подаёт в очереди Directory* диспетчера (запасной
	// механизм U9, задача 6.1), REMOVED — в отмену выгрузки (задача 6.5).
	p.startFileWatcher(ctx, db, d, stop)
	// Replayer журнала операций (задача 6.3): повтор невыполненных операций
	// с нарастающей паузой, в том числе накопленных до запуска.
	p.startJournalReplay(ctx, stop)
}
