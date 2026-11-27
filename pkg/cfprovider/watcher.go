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

// Watcher локальных изменений (design.md U6, живая трассировка 27.11):
// фильтр НЕ доставляет NOTIFY_FILE_CLOSE_COMPLETION для plain-файлов
// (thunk не вызван; NOTIFY стреляют только по плейсхолдер-операциям), поэтому
// основной детектор user-изменений — ReadDirectoryChangesW на корне sync
// root (запасной вариант U6 стал основным). Watcher — ТОЛЬКО источник
// событий: события сходятся в тот же классификатор и коалисер, что
// NOTIFY-джобы (единая точка — classifyNotifyClose: strip корня fsPathOf,
// игнор каталогов, игнор in-sync плейсхолдеров, --read-only), дальше —
// существующий конвейер выгрузки (коалисер 2 c гасит пачки событий от
// ReadDirectoryChangesW на каждую запись).
//
// Цикл — OVERLAPPED-паттерн (канонический для ReadDirectoryChangesW, как в
// fsnotify): handle с FILE_FLAG_OVERLAPPED, вызов возвращает сразу
// (I/O pending), завершение — manual-reset event через WaitForSingleObject,
// результат — GetOverlappedResult. Блокирующий режим (overlapped=nil) в
// горутине ненадёжен: первый вызов, выданный параллельно с записью в
// каталог, детерминированно падает ERROR_NOACCESS (998, бисекция 27.11),
// а отмена блокирующего вызова через CancelIoEx возвращает не
// OPERATION_ABORTED, а тот же 998. Отмена — CancelIoEx по ctx/stop
// (завершение OPERATION_ABORTED через GetOverlappedResult). Переполнение
// буфера (ERROR_NOTIFY_ENUM_DIR — события пачки не поместились) — Warn +
// продолжение (прототип: потерянные события восполняются следующей записью
// либо сверкой при старте, design.md D9). REMOVED и RENAMED_OLD_NAME не
// выгружаются — передача удаления/переименования в backend — этап 6
// (design.md D5), локальные записи не трогаются.
//
// Взаимодействие с walk: запись плейсхолдеров walk-ом тоже порождает
// события; к моменту срабатывания debounce (2 c) convert уже пометил их
// in-sync — повторная классификация в runIngest их отбрасывает.
//
// Этап 6: watcher — также источник операций распространения. REMOVED —
// отмена выгрузки при удалении локального файла (задача 6.5; распространение
// удаления известных плейсхолдеров — NOTIFY_* фильтра, U6: NOTIFY стреляют
// по плейсхолдер-операциям); dir-события (ADDED, RENAMED_OLD/NEW_NAME) —
// очереди Directory* диспетчера — запасной механизм U9 для точки D5
// «Создание каталога» (OS-callback'а нет) и переименований plain-каталогов.
//
// cldapi.dll не вызывается (инвариант «Движок проверяется без CfAPI»);
// ReadDirectoryChangesW — kernel32, тестируется на реальном каталоге.
package cfprovider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// watcherBufferSize — буфер ReadDirectoryChangesW (~64KB, design.md U6).
const watcherBufferSize = 64 << 10

// watcherCancelRetryInterval — пауза цикла CancelIoEx в горутине отмены:
// закрывает окно «I/O не outstanding» между итерациями watcher'а.
const watcherCancelRetryInterval = 50 * time.Millisecond

// watcherNotifyFilter — маска событий: создание/удаление/переименование
// имён, запись содержимого и изменение размера. DIR_NAME нужен, чтобы
// классификатор видел и игнорировал каталоги; подкаталоги обходит
// WatchSubtree — в очередь имён каталоги не ставятся.
const watcherNotifyFilter = windows.FILE_NOTIFY_CHANGE_FILE_NAME |
	windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_LAST_WRITE |
	windows.FILE_NOTIFY_CHANGE_SIZE

// watchUploadAction — действие к выгрузке (winnt.h FILE_ACTION_*): ADDED —
// новый файл; MODIFIED — изменены содержимое/метаданные; RENAMED_NEW_NAME —
// переименован в новое имя. REMOVED не выгружается: отмена выгрузки
// (задача 6.5) и распространение удаления — через notifyDeletePath
// (NOTIFY_* фильтра); RENAMED_OLD_NAME — сопоставление пары dir-событий
// (задача 6.1).
func watchUploadAction(action uint32) bool {
	switch action {
	case windows.FILE_ACTION_ADDED, windows.FILE_ACTION_MODIFIED, windows.FILE_ACTION_RENAMED_NEW_NAME:
		return true
	}
	return false
}

// fileNotifyInformation — заголовок FILE_NOTIFY_INFORMATION (winnt.h): 12
// байт, за ним FileName — UTF-16 без терминатора, FileNameLength — в БАЙТАХ.
type fileNotifyInformation struct {
	NextEntryOffset uint32
	Action          uint32
	FileNameLength  uint32
}

// fileNotifyInfoHeaderSize — размер заголовка FILE_NOTIFY_INFORMATION.
const fileNotifyInfoHeaderSize = int(unsafe.Sizeof(fileNotifyInformation{})) // 12

// watchEvent — событие FILE_NOTIFY_INFORMATION: имя относительно корня и
// действие (winnt.h FILE_ACTION_*).
type watchEvent struct {
	name   string
	action uint32
}

// parseNotifyEventsAll разбирает NextEntryOffset-цепочку
// FILE_NOTIFY_INFORMATION из буфера ReadDirectoryChangesW и возвращает ВСЕ
// события (задачи 6.1/6.5: REMOVED — отмена выгрузки; dir-события —
// маршрутизация в диспетчер). Битый хвост (обрезанная запись, некорректный
// offset) останавливает разбор — событие теряется, но цикл не зависает и не
// выходит за буфер.
func parseNotifyEventsAll(buf []byte) []watchEvent {
	var events []watchEvent
	for offset := 0; offset+fileNotifyInfoHeaderSize <= len(buf); {
		info := (*fileNotifyInformation)(unsafe.Pointer(&buf[offset]))
		nameStart := offset + fileNotifyInfoHeaderSize
		nameEnd := nameStart + int(info.FileNameLength)
		if nameEnd > len(buf) {
			break // обрезанная запись (частично заполненный буфер)
		}
		if info.FileNameLength >= 2 {
			n := int(info.FileNameLength) / 2
			events = append(events, watchEvent{
				name:   windows.UTF16ToString((*[1 << 16]uint16)(unsafe.Pointer(&buf[nameStart]))[:n:n]),
				action: info.Action,
			})
		}
		if info.NextEntryOffset == 0 {
			break // конец цепочки
		}
		if int(info.NextEntryOffset) < fileNotifyInfoHeaderSize {
			break // некорректный offset — защита от зацикливания
		}
		offset += int(info.NextEntryOffset)
	}
	return events
}

// parseNotifyEvents — имена записей, подлежащих выгрузке
// (watchUploadAction); фильтр поверх parseNotifyEventsAll.
func parseNotifyEvents(buf []byte) []string {
	var names []string
	for _, ev := range parseNotifyEventsAll(buf) {
		if watchUploadAction(ev.action) {
			names = append(names, ev.name)
		}
	}
	return names
}

// startFileWatcher запускает watcher'а (после маркера, рядом с пулом
// выгрузки — startIngestPool; один на корень: guard StartIngest).
// d — диспетчер для dir-событий (запасной механизм U9, задача 6.1).
func (p *Provider) startFileWatcher(ctx context.Context, db *ingestDebouncer, d *cfapi.Dispatcher, stop chan struct{}) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runFileWatcher(ctx, db, d, stop)
	}()
}

// noteDirRenameOld запоминает OLD_NAME-событие в FIFO-очередь ожидания.
// Пары OLD→NEW приходят ПОДРЯД в одном блоке FILE_NOTIFY_INFORMATION: OLD
// сохраняется, СЛЕДУЮЩИЙ NEW_NAME с тем же родителем образует пару
// (матч по равенству путей невозможен — old != new). Тип исчезнувшего пути
// до NEW неизвестен (stat по старому пути уже пуст) — FILE-OLD тоже
// попадает в очередь, но пары не собирает: NEW-файл снимает голову
// (dropDirRenameOld), файловые переименования ведутся NOTIFY-каналом
// фильтра и выгрузкой NEW-имени. Очередь ограничена: переименования за
// пределы корня NEW не получают — при переполнении сбрасывается (потерянная
// пара — создание в backend, восполняется сверкой).
func (p *Provider) noteDirRenameOld(full string) {
	p.dirRenameMu.Lock()
	defer p.dirRenameMu.Unlock()
	if len(p.dirRenamePending) >= p.dirRenameOldLimit {
		p.dirRenamePending = nil
		logger.Debugf("watcher %s: dir rename pending reset (limit %d)", p.root, p.dirRenameOldLimit)
	}
	p.dirRenamePending = append(p.dirRenamePending, full)
}

// takeDirRenameOld — спаривание по смежности: голова очереди OLD с тем же
// родителем, что у NEW-пути, — пара (переименование); голова с чужим
// родителем (несмежное событие — сброс очереди/гонка) отбрасывается, NEW
// трактуется как создание.
func (p *Provider) takeDirRenameOld(newFull string) (string, bool) {
	p.dirRenameMu.Lock()
	defer p.dirRenameMu.Unlock()
	if len(p.dirRenamePending) == 0 {
		return "", false // старт watcher'а посреди переименования — создание
	}
	old := p.dirRenamePending[0]
	p.dirRenamePending = p.dirRenamePending[1:]
	if filepath.Dir(old) != filepath.Dir(newFull) {
		return "", false
	}
	return old, true
}

// dropDirRenameOld снимает голову очереди ожидания: NEW-событие оказалось
// файлом — его OLD пару не собирает (иначе файловый OLD спарился бы со
// следующим dir-переименованием).
func (p *Provider) dropDirRenameOld() {
	p.dirRenameMu.Lock()
	if n := len(p.dirRenamePending); n > 0 {
		p.dirRenamePending = p.dirRenamePending[1:]
	}
	p.dirRenameMu.Unlock()
}

// routeWatcherDirEvent — dir-события watcher'а → очереди Directory*
// диспетчера (запасной механизм U9, задача 6.1): ADDED — создание каталога
// (Mkdir в backend, спека «Новый каталог»); RENAMED_NEW_NAME —
// переименование каталога (пара по смежности с OLD_NAME; неспаренный NEW —
// старт watcher'а посреди переименования/сброс очереди — трактуется как
// создание: notifyDirCreate усыновит существующую запись). Переполнение
// очереди — событие теряется с Debug (восполняется сверкой/рестартом).
func (p *Provider) routeWatcherDirEvent(d *cfapi.Dispatcher, action uint32, full string) {
	if d == nil {
		return
	}
	switch action {
	case windows.FILE_ACTION_ADDED:
		if err := d.OnNotifyDirectoryCreated(&cfapi.NotifyDirectoryCreatedJob{Path: full}); err != nil {
			logger.Debugf("watcher %s: dir create %s dropped (queue full): %v", p.root, full, err)
		}
	case windows.FILE_ACTION_RENAMED_NEW_NAME:
		if old, ok := p.takeDirRenameOld(full); ok {
			if err := d.OnNotifyDirectoryRenamed(&cfapi.NotifyDirectoryRenamedJob{SourcePath: old, TargetPath: full}); err != nil {
				logger.Debugf("watcher %s: dir rename %s dropped (queue full): %v", p.root, full, err)
			}
			return
		}
		logger.Debugf("watcher %s: dir rename old name missing for %s; routing as create", p.root, full)
		if err := d.OnNotifyDirectoryCreated(&cfapi.NotifyDirectoryCreatedJob{Path: full}); err != nil {
			logger.Debugf("watcher %s: dir create %s dropped (queue full): %v", p.root, full, err)
		}
	default:
		// MODIFIED каталогов не обрабатывается (каталоги не выгружаются).
	}
}

// runFileWatcher — цикл watcher'а: overlapped ReadDirectoryChangesW на корне
// sync root (WatchSubtree=true), события → маршрутизация: REMOVED — отмена
// выгрузки (задача 6.5); dir-события — очереди Directory* диспетчера (U9,
// задача 6.1); прочие файловые — классификация → коалисер.
// Отмена — CancelIoEx по ctx/stop: операция завершается OPERATION_ABORTED
// (GetOverlappedResult), event взводится, цикл выходит, handle закрывается.
// Все структуры I/O — в куче (Overlapped переживёт завершение операции).
func (p *Provider) runFileWatcher(ctx context.Context, db *ingestDebouncer, d *cfapi.Dispatcher, stop chan struct{}) {
	root16, err := windows.UTF16PtrFromString(p.root)
	if err != nil {
		logger.Warnf("watcher %s: path: %v; local changes are detected by close notifications only", p.root, err)
		return
	}
	// FILE_LIST_DIRECTORY + BACKUP_SEMANTICS + OVERLAPPED — хендл каталога
	// для асинхронного ReadDirectoryChangesW.
	h, err := windows.CreateFile(root16, windows.FILE_LIST_DIRECTORY, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		logger.Warnf("watcher %s: open: %v; local changes are detected by close notifications only", p.root, err)
		return
	}
	done := make(chan struct{})
	defer windows.CloseHandle(h) //nolint:errcheck
	defer close(done)
	event, err := windows.CreateEvent(nil, 1, 0, nil) // manual-reset
	if err != nil {
		logger.Warnf("watcher %s: create event: %v; local changes are detected by close notifications only", p.root, err)
		return
	}
	defer windows.CloseHandle(event) //nolint:errcheck
	ov := new(windows.Overlapped)
	ov.HEvent = windows.Handle(event)
	// Горутина отмены (ctx/stop): CancelIoEx ПОВТОРЯЕТСЯ до подтверждения
	// завершения watcher'а (done). Одиночный вызов ненадёжен: он может
	// попасть в окно «I/O не outstanding» (основная горутина разбирает
	// прошлые события / готовит следующий ReadDirectoryChanges) —
	// ERROR_NOT_FOUND игнорируется, горутина отмены выходит, и уже
	// выданный следующий ReadDirectoryChanges никто не отменяет →
	// WaitForSingleObject(INFINITE) навсегда (в serve — не завершиться
	// через wg.Wait; в тестах — редкий таймаут, живая диагностика 27.11).
	// Цикл с паузой 50 мс закрывает окно: как только ReadDirectoryChanges
	// становится outstanding, очередной CancelIoEx завершает его с
	// OPERATION_ABORTED. Завершение цикла — только по done (watcher сам
	// дошёл до выхода после успешной отмены либо по собственной ошибке).
	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
		case <-done:
			return
		}
		for {
			_ = windows.CancelIoEx(h, ov)
			select {
			case <-done:
				return
			case <-time.After(watcherCancelRetryInterval):
			}
		}
	}()
	buf := make([]byte, watcherBufferSize)
	logger.Debugf("watcher started: root=%s filter=0x%x buffer=%d", p.root, watcherNotifyFilter, watcherBufferSize)
	for {
		// Неблокирующая проверка отмены ПЕРЕД КАЖДЫМ ReadDirectoryChanges:
		// между итерациями разбора событий ctx/stop могли сработать — без
		// проверки следующий вызов outstanding-ом ушёл бы в ожидание до
		// очередного CancelIoEx цикла отмены (см. горутину выше).
		select {
		case <-ctx.Done():
			logger.Debugf("watcher %s: stopped (context canceled)", p.root)
			return
		case <-stop:
			logger.Debugf("watcher %s: stopped", p.root)
			return
		default:
		}
		if err := windows.ResetEvent(event); err != nil {
			logger.Warnf("watcher %s: reset event: %v; watcher stopped", p.root, err)
			return
		}
		// Вызов возвращается сразу (I/O pending); результат — по event.
		err := windows.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)), true, watcherNotifyFilter, nil, ov, 0)
		if err != nil {
			logger.Warnf("watcher %s: ReadDirectoryChanges: %v; watcher stopped", p.root, err)
			return
		}
		if s, werr := windows.WaitForSingleObject(event, windows.INFINITE); werr != nil || s != windows.WAIT_OBJECT_0 {
			logger.Warnf("watcher %s: wait: err=%v state=%d; watcher stopped", p.root, werr, s)
			return
		}
		var nbytes uint32
		if err := windows.GetOverlappedResult(h, ov, &nbytes, false); err != nil {
			switch {
			case errors.Is(err, windows.ERROR_OPERATION_ABORTED):
				logger.Debugf("watcher %s: stopped (I/O canceled)", p.root)
				return
			case errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR), errors.Is(err, windows.ERROR_MORE_DATA):
				// Пачка не поместилась в буфер — её события потеряны;
				// прототип (design.md U6): Warn + продолжение, следующие
				// записи и сверка при старте восполняют.
				logger.Warnf("watcher %s: notification buffer overflow, some events lost; continuing", p.root)
				continue
			default:
				var eno syscall.Errno
				if errors.As(err, &eno) {
					logger.Warnf("watcher %s: GetOverlappedResult: %v (errno=%d); watcher stopped", p.root, err, eno)
				} else {
					logger.Warnf("watcher %s: GetOverlappedResult: %v; watcher stopped", p.root, err)
				}
				return
			}
		}
		if nbytes == 0 || int(nbytes) > len(buf) {
			continue // пустой ответ либо некорректная длина — не разбираем
		}
		for _, ev := range parseNotifyEventsAll(buf[:nbytes]) {
			if ev.name == "" {
				continue
			}
			// Единая точка маршрутизации: полный локальный путь.
			full := filepath.Join(p.root, ev.name)
			switch {
			case ev.action == windows.FILE_ACTION_REMOVED:
				// Задача 6.5: отмена выгрузки при удалении локального файла
				// (удаление известных плейсхолдеров — NOTIFY_* фильтра).
				p.handleLocalRemoved(full)
				continue
			case ev.action == windows.FILE_ACTION_RENAMED_OLD_NAME:
				// Пара OLD→NEW приходит ПОДРЯД в одном блоке: OLD сохраняется
				// в очередь ожидания, спаривание — по СЛЕДУЮЩЕМУ NEW с тем же
				// родителем (тип исчезнувшего пути определит NEW: dir-NEW →
				// DirectoryRenamed, file-NEW → dropDirRenameOld).
				p.noteDirRenameOld(full)
				continue
			}
			if fi, err := os.Stat(full); err == nil && fi.IsDir() {
				p.routeWatcherDirEvent(d, ev.action, full)
				continue
			}
			if ev.action == windows.FILE_ACTION_RENAMED_NEW_NAME {
				// Файловое переименование: его OLD пары не собирает.
				p.dropDirRenameOld()
			}
			// Файлы: выгрузка (как в этапе 5; RENAMED_OLD_NAME отсечён выше).
			if !watchUploadAction(ev.action) {
				continue
			}
			// Единая точка с NOTIFY-джобами: classifyNotifyClose (strip корня
			// fsPathOf без учёта регистра, игнор in-sync плейсхолдеров,
			// --read-only). fromWatcher=true — супрессия собственных событий
			// финализации (remove+create U18-α) действует только здесь;
			// callback-путь шлёт только настоящие правки пользователя.
			job := &cfapi.NotifyFileCloseCompletionJob{Path: full}
			if task, ok := p.classifyNotifyClose(ctx, job, true); ok {
				logger.Debugf("watcher %s: %s -> upload queue", p.root, task.fsPath)
				db.submit(task)
			}
		}
	}
}
