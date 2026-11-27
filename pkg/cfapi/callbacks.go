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
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/utils"
)

// logger — следы цепочки NOTIFY (диагностика живой пробы этапа 5): в thunk
// close-completion логируется факт вызова фильтром (путь), уровень Debug —
// serve с --verbose/--log-level debug. Уровень Info по умолчанию следы
// скрывает — живой ретест проводить с --verbose.
var logger = utils.GetLogger("cfapi")

// Thunk'и callback'ов (design.md D5). Ограничения syscall.NewCallback и
// инвариант «thunk без ввода-вывода»: функция уровня пакета, без замыканий;
// на стеке callback'а только копирование полей в задание и постановка в
// очередь диспетчера; паника недопустима — она завершает процесс (nil-диспетчер
// и любые редкие ветки обрабатываются возвратом, а не паникой).
//
// CF_CALLBACK в cfapi.h — VOID-прототип: возврат фильтром игнорируется,
// thunk всегда возвращает 0 (возврат ошибки из callback'а — проба U13).
//
// Десять точек диспетчеризации по таблице D5. CF_CALLBACK_TYPE не различает
// файл/каталог: NOTIFY_RENAME и NOTIFY_DELETE несут флаг *_IS_DIRECTORY, по
// нему thunk маршрутизирует в файловый или каталоговый метод. Отдельного
// callback-типа создания каталога в cfapi.h нет (13 типов, U9): метод
// OnNotifyDirectoryCreated существует в интерфейсе, но OS-обратного вызова
// для него нет — источник задания определит проба U9/запасной механизм (D6).

// Задания — копии параметров callback'ов; живут дольше callback'а, поэтому
// содержат только скопированные значения, никаких указателей в память Windows.

// FetchDataJob — FETCH_DATA: задание гидратации (design.md D7).
type FetchDataJob struct {
	ConnectionKey  ConnectionKey
	TransferKey    TransferKey
	RequestKey     RequestKey
	FileID         uint64 // первые 8 байт FileIdentity — inode (LE), design.md D6
	Flags          uint32 // CF_CALLBACK_FETCH_DATA_FLAGS
	RequiredOffset int64
	RequiredLength int64
	OptionalOffset int64
	OptionalLength int64
	PriorityHint   uint8
	Path           string      // NormalizedPath (инструментирование пробы U3); пуст — фильтр не прислал
	Cancel         atomic.Bool // ставится OnCancelFetchData (реестр активных заданий)
}

// CancelFetchDataJob — CANCEL_FETCH_DATA.
type CancelFetchDataJob struct {
	ConnectionKey ConnectionKey
	TransferKey   TransferKey
	RequestKey    RequestKey
	Flags         uint32 // CF_CALLBACK_CANCEL_FLAGS
	FileOffset    int64
	Length        int64
}

// FetchPlaceholdersJob — FETCH_PLACEHOLDERS: наполнение каталога (design.md D6).
type FetchPlaceholdersJob struct {
	ConnectionKey ConnectionKey
	TransferKey   TransferKey
	RequestKey    RequestKey
	Path          string // NormalizedPath каталога (REQUIRE_FULL_FILE_PATH)
	FileID        uint64 // первые 8 байт FileIdentity каталога — inode (LE), design.md D6
	Pattern       string // шаблон FindFirstFileW; пустой — все записи
	Cancel        atomic.Bool
}

// CancelFetchPlaceholdersJob — CANCEL_FETCH_PLACEHOLDERS.
type CancelFetchPlaceholdersJob struct {
	ConnectionKey ConnectionKey
	TransferKey   TransferKey
	RequestKey    RequestKey
	Flags         uint32
	FileOffset    int64
	Length        int64
}

// NotifyFileCloseCompletionJob — NOTIFY_FILE_CLOSE_COMPLETION: классификация
// файла, постановка выгрузки (design.md D8).
type NotifyFileCloseCompletionJob struct {
	ConnectionKey ConnectionKey
	TransferKey   TransferKey
	RequestKey    RequestKey
	Flags         uint32 // CF_CALLBACK_CLOSE_COMPLETION_FLAGS
	Path          string // NormalizedPath, скопирован внутри callback'а
	FileID        uint64
	FileSize      int64
}

// NotifyFileRenamedJob — NOTIFY_RENAME без CF_CALLBACK_RENAME_FLAG_IS_DIRECTORY.
type NotifyFileRenamedJob struct {
	ConnectionKey ConnectionKey
	TransferKey   TransferKey
	RequestKey    RequestKey
	Flags         uint32 // CF_CALLBACK_RENAME_FLAGS
	SourcePath    string
	TargetPath    string
	FileID        uint64
}

// NotifyFileDeletedJob — NOTIFY_DELETE без CF_CALLBACK_DELETE_FLAG_IS_DIRECTORY.
type NotifyFileDeletedJob struct {
	ConnectionKey ConnectionKey
	TransferKey   TransferKey
	RequestKey    RequestKey
	Flags         uint32 // CF_CALLBACK_DELETE_FLAGS
	Path          string
	FileID        uint64
}

// NotifyDirectoryCreatedJob — создание каталога (design.md D5, шаг 6).
// OS-обратного вызова нет (см. комментарий выше); источник заданий —
// запасной механизм U9, доставка через Dispatcher.OnNotifyDirectoryCreated.
type NotifyDirectoryCreatedJob struct {
	ConnectionKey ConnectionKey
	TransferKey   TransferKey
	RequestKey    RequestKey
	Path          string
	FileID        uint64
}

// NotifyDirectoryRenamedJob — NOTIFY_RENAME с CF_CALLBACK_RENAME_FLAG_IS_DIRECTORY.
type NotifyDirectoryRenamedJob struct {
	ConnectionKey ConnectionKey
	TransferKey   TransferKey
	RequestKey    RequestKey
	Flags         uint32
	SourcePath    string
	TargetPath    string
}

// NotifyDirectoryDeletedJob — NOTIFY_DELETE с CF_CALLBACK_DELETE_FLAG_IS_DIRECTORY.
type NotifyDirectoryDeletedJob struct {
	ConnectionKey ConnectionKey
	TransferKey   TransferKey
	RequestKey    RequestKey
	Flags         uint32
	Path          string
}

// Приёмник заданий; устанавливается до CfConnectSyncRoot (SetDispatcher).
var currentDispatcher atomic.Value // хранит dispatcherHolder

type dispatcherHolder struct {
	d CallbackDispatcher
}

// SetDispatcher устанавливает приёмник заданий callback'ов. nil возвращает
// поведение «задание отброшено без паники» (для тестов и остановки).
func SetDispatcher(d CallbackDispatcher) {
	currentDispatcher.Store(dispatcherHolder{d: d})
}

// dispatch возвращает текущий приёмник или nil (thunk тогда молча отбрасывает
// задание — паника недопустима).
func dispatch() CallbackDispatcher {
	if h, ok := currentDispatcher.Load().(dispatcherHolder); ok {
		return h.d
	}
	return nil
}

// utf16ToString копирует UTF-16 строку из памяти Windows в Go string —
// только чтение памяти внутри callback'а, без I/O.
func utf16ToString(p *uint16) string {
	if p == nil {
		return ""
	}
	return windows.UTF16PtrToString(p)
}

// Thunk'и — функции уровня пакета; адреса фиксируются в переменных уровня
// пакета (syscall.NewCallback не принимает замыкания).

var (
	fetchDataThunk                 = syscall.NewCallback(fetchDataCallback)
	cancelFetchDataThunk           = syscall.NewCallback(cancelFetchDataCallback)
	fetchPlaceholdersThunk         = syscall.NewCallback(fetchPlaceholdersCallback)
	cancelFetchPlaceholdersThunk   = syscall.NewCallback(cancelFetchPlaceholdersCallback)
	notifyFileCloseCompletionThunk = syscall.NewCallback(notifyFileCloseCompletionCallback)
	notifyRenameThunk              = syscall.NewCallback(notifyRenameCallback) // файл + каталог по флагу
	notifyDeleteThunk              = syscall.NewCallback(notifyDeleteCallback) // файл + каталог по флагу
)

// CallbackTable возвращает таблицу регистрации callback'ов для
// CfConnectSyncRoot: семь OS-типов покрывают десять точек диспетчеризации D5
// (переименование/удаление файла и каталога различаются флагом); конец —
// CF_CALLBACK_REGISTRATION_END.
func CallbackTable() []CFCallbackRegistration {
	return []CFCallbackRegistration{
		{Type: CallbackTypeFetchData, Callback: fetchDataThunk},
		{Type: CallbackTypeCancelFetchData, Callback: cancelFetchDataThunk},
		{Type: CallbackTypeFetchPlaceholders, Callback: fetchPlaceholdersThunk},
		{Type: CallbackTypeCancelFetchPlaceholders, Callback: cancelFetchPlaceholdersThunk},
		{Type: CallbackTypeNotifyFileCloseCompletion, Callback: notifyFileCloseCompletionThunk},
		{Type: CallbackTypeNotifyRename, Callback: notifyRenameThunk},
		{Type: CallbackTypeNotifyDelete, Callback: notifyDeleteThunk},
		{Type: CallbackTypeNone, Callback: 0},
	}
}

// fetchDataCallback — thunk FETCH_DATA: marshal → dispatch, без I/O.
func fetchDataCallback(info *CFCallbackInfo, params *CFCallbackParameters) uintptr {
	p := params.FetchDataParams()
	job := &FetchDataJob{
		ConnectionKey:  info.ConnectionKey,
		TransferKey:    info.TransferKey,
		RequestKey:     info.RequestKey,
		FileID:         fileIDOf(info.FileIdentity, info.FileIdentityLength),
		Flags:          p.Flags,
		RequiredOffset: p.RequiredFileOffset,
		RequiredLength: p.RequiredLength,
		OptionalOffset: p.OptionalFileOffset,
		OptionalLength: p.OptionalLength,
		PriorityHint:   info.PriorityHint,
		Path:           utf16ToString(info.NormalizedPath), // только копирование памяти
	}
	if d := dispatch(); d != nil {
		// Очередь переполнена: диспетчер посчитал drop; возврат из VOID-callback
		// ничего не меняет (U13 — отдельная проба этапа 3).
		_ = d.OnFetchData(job)
	}
	return 0
}

// cancelFetchDataCallback — thunk CANCEL_FETCH_DATA: marshal → dispatch.
func cancelFetchDataCallback(info *CFCallbackInfo, params *CFCallbackParameters) uintptr {
	p := params.CancelParams()
	job := &CancelFetchDataJob{
		ConnectionKey: info.ConnectionKey,
		TransferKey:   info.TransferKey,
		RequestKey:    info.RequestKey,
		Flags:         p.Flags,
		FileOffset:    p.FileOffset,
		Length:        p.Length,
	}
	if d := dispatch(); d != nil {
		_ = d.OnCancelFetchData(job)
	}
	return 0
}

// fetchPlaceholdersCallback — thunk FETCH_PLACEHOLDERS.
func fetchPlaceholdersCallback(info *CFCallbackInfo, params *CFCallbackParameters) uintptr {
	p := params.FetchPlaceholdersParams()
	job := &FetchPlaceholdersJob{
		ConnectionKey: info.ConnectionKey,
		TransferKey:   info.TransferKey,
		RequestKey:    info.RequestKey,
		Path:          utf16ToString(info.NormalizedPath),
		FileID:        fileIDOf(info.FileIdentity, info.FileIdentityLength),
		Pattern:       utf16ToString(p.Pattern),
	}
	if d := dispatch(); d != nil {
		_ = d.OnFetchPlaceholders(job)
	}
	return 0
}

// cancelFetchPlaceholdersCallback — thunk CANCEL_FETCH_PLACEHOLDERS.
func cancelFetchPlaceholdersCallback(info *CFCallbackInfo, params *CFCallbackParameters) uintptr {
	p := params.CancelParams()
	job := &CancelFetchPlaceholdersJob{
		ConnectionKey: info.ConnectionKey,
		TransferKey:   info.TransferKey,
		RequestKey:    info.RequestKey,
		Flags:         p.Flags,
		FileOffset:    p.FileOffset,
		Length:        p.Length,
	}
	if d := dispatch(); d != nil {
		_ = d.OnCancelFetchPlaceholders(job)
	}
	return 0
}

// notifyFileCloseCompletionCallback — thunk NOTIFY_FILE_CLOSE_COMPLETION.
// След вызова фильтром (живая проба этапа 5): Debugf до постановки в
// очередь — это первая точка, по которой ретест отличает «фильтр не
// вызвал callback» от «задание потерялось дальше»; форматирование —
// только копированные поля, I/O на стеке callback'а по-прежнему нет.
func notifyFileCloseCompletionCallback(info *CFCallbackInfo, params *CFCallbackParameters) uintptr {
	p := params.CloseCompletionParams()
	job := &NotifyFileCloseCompletionJob{
		ConnectionKey: info.ConnectionKey,
		TransferKey:   info.TransferKey,
		RequestKey:    info.RequestKey,
		Flags:         p.Flags,
		Path:          utf16ToString(info.NormalizedPath),
		FileID:        fileIDOf(info.FileIdentity, info.FileIdentityLength),
		FileSize:      info.FileSize,
	}
	logger.Debugf("NOTIFY_FILE_CLOSE_COMPLETION callback: path=%q fileID=%d size=%d flags=%#x", job.Path, job.FileID, job.FileSize, job.Flags)
	if d := dispatch(); d != nil {
		_ = d.OnNotifyFileCloseCompletion(job)
	}
	return 0
}

// notifyRenameCallback — thunk NOTIFY_RENAME: маршрутизация файл/каталог по
// CF_CALLBACK_RENAME_FLAG_IS_DIRECTORY. След вызова фильтром (трассировка
// U19: без него «нет NOTIFY_RENAME в логе» ничего не доказывает — ретест
// отличает «фильтр не вызвал callback» от «задание потерялось дальше»;
// документированное отступление D5 — как в close-completion, только копии
// полей, I/O на стеке callback'а нет).
func notifyRenameCallback(info *CFCallbackInfo, params *CFCallbackParameters) uintptr {
	p := params.RenameParams()
	source := utf16ToString(info.NormalizedPath)
	target := utf16ToString(p.TargetPath)
	logger.Debugf("NOTIFY_RENAME callback: %q -> %q isDir=%v flags=%#x", source, target, p.Flags&CallbackRenameFlagIsDirectory != 0, p.Flags)
	if d := dispatch(); d != nil {
		if p.Flags&CallbackRenameFlagIsDirectory != 0 {
			_ = d.OnNotifyDirectoryRenamed(&NotifyDirectoryRenamedJob{
				ConnectionKey: info.ConnectionKey,
				TransferKey:   info.TransferKey,
				RequestKey:    info.RequestKey,
				Flags:         p.Flags,
				SourcePath:    source,
				TargetPath:    target,
			})
		} else {
			_ = d.OnNotifyFileRenamed(&NotifyFileRenamedJob{
				ConnectionKey: info.ConnectionKey,
				TransferKey:   info.TransferKey,
				RequestKey:    info.RequestKey,
				Flags:         p.Flags,
				SourcePath:    source,
				TargetPath:    target,
				FileID:        fileIDOf(info.FileIdentity, info.FileIdentityLength),
			})
		}
	}
	return 0
}

// notifyDeleteCallback — thunk NOTIFY_DELETE: маршрутизация файл/каталог по
// CF_CALLBACK_DELETE_FLAG_IS_DIRECTORY. След вызова фильтром (трассировка
// U19 — см. notifyRenameCallback).
func notifyDeleteCallback(info *CFCallbackInfo, params *CFCallbackParameters) uintptr {
	p := params.DeleteParams()
	path := utf16ToString(info.NormalizedPath)
	logger.Debugf("NOTIFY_DELETE callback: path=%q isDir=%v flags=%#x", path, p.Flags&CallbackDeleteFlagIsDirectory != 0, p.Flags)
	if d := dispatch(); d != nil {
		if p.Flags&CallbackDeleteFlagIsDirectory != 0 {
			_ = d.OnNotifyDirectoryDeleted(&NotifyDirectoryDeletedJob{
				ConnectionKey: info.ConnectionKey,
				TransferKey:   info.TransferKey,
				RequestKey:    info.RequestKey,
				Flags:         p.Flags,
				Path:          path,
			})
		} else {
			_ = d.OnNotifyFileDeleted(&NotifyFileDeletedJob{
				ConnectionKey: info.ConnectionKey,
				TransferKey:   info.TransferKey,
				RequestKey:    info.RequestKey,
				Flags:         p.Flags,
				Path:          path,
				FileID:        fileIDOf(info.FileIdentity, info.FileIdentityLength),
			})
		}
	}
	return 0
}
