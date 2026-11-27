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
// cldapi.dll не вызывается (инвариант «Движок проверяется без CfAPI»);
// ReadDirectoryChangesW — kernel32, тестируется на реальном каталоге.
package cfprovider

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// watcherBufferSize — буфер ReadDirectoryChangesW (~64KB, design.md U6).
const watcherBufferSize = 64 << 10

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
// переименован в новое имя. REMOVED и RENAMED_OLD_NAME не выгружаются:
// передача удаления/переименования в backend — этап 6 (design.md D5);
// локальная запись не трогается.
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

// parseNotifyEvents разбирает NextEntryOffset-цепочку FILE_NOTIFY_INFORMATION
// из буфера ReadDirectoryChangesW и возвращает относительные имена записей,
// подлежащих выгрузке (watchUploadAction). Битый хвост (обрезанная запись,
// некорректный offset) останавливает разбор — событие теряется, но цикл не
// зависает и не выходит за буфер.
func parseNotifyEvents(buf []byte) []string {
	var names []string
	for offset := 0; offset+fileNotifyInfoHeaderSize <= len(buf); {
		info := (*fileNotifyInformation)(unsafe.Pointer(&buf[offset]))
		nameStart := offset + fileNotifyInfoHeaderSize
		nameEnd := nameStart + int(info.FileNameLength)
		if nameEnd > len(buf) {
			break // обрезанная запись (частично заполненный буфер)
		}
		if watchUploadAction(info.Action) && info.FileNameLength >= 2 {
			n := int(info.FileNameLength) / 2
			names = append(names, windows.UTF16ToString((*[1 << 16]uint16)(unsafe.Pointer(&buf[nameStart]))[:n:n]))
		}
		if info.NextEntryOffset == 0 {
			break // конец цепочки
		}
		if int(info.NextEntryOffset) < fileNotifyInfoHeaderSize {
			break // некорректный offset — защита от зацикливания
		}
		offset += int(info.NextEntryOffset)
	}
	return names
}

// startFileWatcher запускает watcher'а (после маркера, рядом с пулом
// выгрузки — startIngestPool; один на корень: guard StartIngest).
func (p *Provider) startFileWatcher(ctx context.Context, db *ingestDebouncer, stop chan struct{}) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runFileWatcher(ctx, db, stop)
	}()
}

// runFileWatcher — цикл watcher'а: overlapped ReadDirectoryChangesW на корне
// sync root (WatchSubtree=true), события → классификация → коалисер.
// Отмена — CancelIoEx по ctx/stop: операция завершается OPERATION_ABORTED
// (GetOverlappedResult), event взводится, цикл выходит, handle закрывается.
// Все структуры I/O — в куче (Overlapped переживёт завершение операции).
func (p *Provider) runFileWatcher(ctx context.Context, db *ingestDebouncer, stop chan struct{}) {
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
	// Горутина отмены: ctx/stop → CancelIoEx — операция завершается
	// OPERATION_ABORTED, event взводится, цикл корректно выходит. done —
	// watcher завершился сам, отменять нечего.
	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
		case <-done:
			return
		}
		_ = windows.CancelIoEx(h, ov)
	}()
	buf := make([]byte, watcherBufferSize)
	logger.Debugf("watcher started: root=%s filter=0x%x buffer=%d", p.root, watcherNotifyFilter, watcherBufferSize)
	for {
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
		for _, name := range parseNotifyEvents(buf[:nbytes]) {
			if name == "" {
				continue
			}
			// Единая точка с NOTIFY-джобами: полный локальный путь →
			// classifyNotifyClose (strip корня fsPathOf без учёта регистра,
			// игнор каталогов и in-sync плейсхолдеров, --read-only).
			job := &cfapi.NotifyFileCloseCompletionJob{Path: filepath.Join(p.root, name)}
			if task, ok := p.classifyNotifyClose(job); ok {
				logger.Debugf("watcher %s: %s -> upload queue", p.root, task.fsPath)
				db.submit(task)
			}
		}
	}
}
