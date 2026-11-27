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

// Обёртки CfExecute — операции провайдера (план задач: файл operations.go
// наполняется этапами 3+ по потребности; на этапе 2 — ответ на
// FETCH_PLACEHOLDERS, design.md D5/D6; на этапе 3 — ответ на FETCH_DATA;
// на этапе 4 — pin/unpin и отложенные биндинги CfOpenFileWithOplock/
// CfRevertPlaceholder, задача 4.1).
package cfapi

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// AnswerFetchData отвечает на задание FETCH_DATA операцией TRANSFER_DATA
// (design.md D7): данные диапазона [offset, offset+length) плейсхолдера
// передаются фильтру за один CfExecute. Семантика по cfapi.h (union
// CF_OPERATION_PARAMETERS, ветка TransferData) и C-паттерну MS-сэмпла
// CloudMirror (Utilities.cpp, ApplyTransferStateToFile):
//
//	opInfo.Type = CF_OPERATION_TYPE_TRANSFER_DATA (0)
//	TransferData.Flags = NONE; CompletionStatus = status;
//	TransferData.Buffer = LPCVOID buf (страница-выровненный VirtualAlloc
//	из BufferPool — U4; nil допустим для ответа без данных);
//	TransferData.Offset/Length — LARGE_INTEGER запрошенного диапазона.
//	ParamSize = sizeof(CF_OPERATION_PARAMETERS) (union целиком — как в
//	AnswerFetchPlaceholders, живые пробы U2 27.11).
//
// status — CompletionStatus: StatusSuccess с данными либо код отказа
// (отмена — StatusCloudFileRequestCanceled, ошибки чтения — StatusFromErrno);
// при неуспешном статусе буфер игнорируется фильтром — buf nil, length 0.
// buf обязан начинаться на странице-выровненном адресе (BufferPool.Get).
//
// Ответ обязателен на каждое задание ровно один раз — иначе чтение
// вызывающего процесса остаётся подвешенным до таймаута фильтра.
func AnswerFetchData(conn ConnectionKey, transferKey TransferKey, requestKey RequestKey, status NTStatus, buf []byte, offset, length int64) error {
	opInfo := CFOperationInfo{
		StructSize:    uint32(unsafe.Sizeof(CFOperationInfo{})),
		Type:          OperationTypeTransferData,
		ConnectionKey: conn,
		TransferKey:   transferKey,
		RequestKey:    requestKey,
	}
	opParams := CFOperationParameters{
		// ParamSize — размер union'а CF_OPERATION_PARAMETERS целиком
		// (см. AnswerFetchPlaceholders: пробы U2 27.11; ветка TransferData
		// = 32 байта, union — 40).
		ParamSize: uint32(unsafe.Sizeof(CFOperationParameters{})),
	}
	p := opParams.OperationTransferData()
	p.Flags = OperationTransferDataFlagNone
	p.CompletionStatus = status
	if len(buf) > 0 {
		p.Buffer = unsafe.Pointer(&buf[0])
	}
	p.Offset = offset
	p.Length = length
	return CfExecute(&opInfo, &opParams)
}

// AnswerFetchPlaceholders отвечает на задание FETCH_PLACEHOLDERS операцией
// TRANSFER_PLACEHOLDERS: записи создаются фильтром относительно каталога,
// с которым связан TransferKey (RelativeFileName каждой записи — имя внутри
// этого каталога, без разделителей пути).
//
// status — CompletionStatus ответа (StatusSuccess, при отмене — код отказа);
// placeholders — записи плейсхолдеров (nil допустим, например при отмене);
// total — PlaceholderTotalCount: полное число записей каталога (не EOF, см.
// провайдера); entriesProcessed — EntriesProcessed: кумулятивный счётчик
// обработанных провайдером записей. Живая проба 27.11: фильтр завершает
// population по processed >= total — single-shot ответ должен нести
// processed = полное число записей каталога (== count, когда каталог
// передаётся целиком), иначе create пендится вечно.
//
// Ответ обязателен на каждое задание ровно один раз — иначе перечисление
// каталога в Explorer остаётся подвешенным до таймаута фильтра.
func AnswerFetchPlaceholders(conn ConnectionKey, transferKey TransferKey, requestKey RequestKey, status NTStatus, placeholders []CFPlaceholderCreateInfo, total int64, entriesProcessed uint32) error {
	opInfo := CFOperationInfo{
		StructSize:    uint32(unsafe.Sizeof(CFOperationInfo{})),
		Type:          OperationTypeTransferPlaceholders,
		ConnectionKey: conn,
		TransferKey:   transferKey,
		RequestKey:    requestKey,
	}
	opParams := CFOperationParameters{
		// ParamSize — размер ВСЕЙ структуры параметров (union целиком).
		// cfapi.h ParamSize не документирует; в MS-сэмпле CloudMirror
		// встречается и размер активной ветки (sizeof(opParams.TransferData)).
		// Живые пробы U2 (27.11): (1) ParamSize=32 (размер ветки
		// TransferPlaceholders) при count=0/NULL-массиве — E_INVALIDARG;
		// (2) с union-размером count=0+SUCCESS принят фильтром — легальный
		// ответ «населено, можно создавать» (на provider'е: пустой каталог
		// или каталог, всё содержимое которого уже на диске). Если union
		// целиком когда-нибудь отвергается — вернуть размер ветки.
		ParamSize: uint32(unsafe.Sizeof(CFOperationParameters{})),
	}
	p := opParams.OperationTransferPlaceholders()
	p.Flags = OperationTransferPlaceholdersFlagNone
	p.CompletionStatus = status
	p.PlaceholderTotalCount = total
	if len(placeholders) > 0 {
		p.PlaceholderArray = &placeholders[0]
	}
	p.PlaceholderCount = uint32(len(placeholders))
	p.EntriesProcessed = entriesProcessed
	return CfExecute(&opInfo, &opParams)
}

// CfOpenFileWithOplock открывает запись sync root хендлом, пригодным для
// Cf-операций плейсхолдеров (cfapi.h:765): protected-хендл держит oplock
// фильтра и переживает дегидратацию/гидрацию записи. Флаги —
// CF_OPEN_FILE_FLAGS (OpenFileFlag*).
func CfOpenFileWithOplock(filePath string, flags uint32, protectedHandle *syscall.Handle) error {
	path, err := windows.UTF16PtrFromString(filePath)
	if err != nil {
		return err
	}
	r, _, _ := procCfOpenFileWithOplock.Call(
		uintptr(unsafe.Pointer(path)),
		uintptr(flags),
		uintptr(unsafe.Pointer(protectedHandle)),
	)
	return hresult(r, "CfOpenFileWithOplock")
}

// CfRevertPlaceholder откатывает запись в обычный (не-плейсхолдер) файл
// (cfapi.h:854): содержимое становится локальным, связь с sync engine
// разрывается. Отложенный биндинг этапа 6 (проба U15 — осиротевшие
// плейсхолдеры блокируют удаление); к командам прототипа не подключён.
func CfRevertPlaceholder(fileHandle syscall.Handle, revertFlags uint32, overlapped uintptr) error {
	r, _, _ := procCfRevertPlaceholder.Call(
		uintptr(fileHandle),
		uintptr(revertFlags),
		overlapped,
	)
	return hresult(r, "CfRevertPlaceholder")
}

// PlaceholderStateByPath — CF_PLACEHOLDER_STATE записи sync root по пути
// (cfapi.h CfGetPlaceholderStateFromFileInfo): атрибуты FileBasicInfo
// (GetFileInformationByHandleEx) скармливаются функции напрямую, live-хендл
// после запроса закрывается. Используется ТОЛЬКО `status <путь>` (5.7) —
// классификация close-уведомлений/watcher-событий переведена на честный
// CF_PLACEHOLDER_STANDARD_INFO (после convert-DEHYDRATE фильтр чистит
// basic-атрибуты, и эта эвристика возвращает 0 для реального плейсхолдера —
// живой ретест 27.11). Семантика результата (живая проба этапа 5,
// 2026-11-27): NO_STATES=0 — файл не плейсхолдер; INVALID=0xffffffff
// функция вернула для свежесозданного plain-файла вместо NO_STATES —
// трактуется так же, как NO_STATES (0, nil), не отказ. Ошибкой остаётся
// только реальный отказ API — открытие или GetFileInformationByHandleEx.
//
// Открытие GENERIC_READ и запрос атрибутов данных не читают — FETCH_DATA
// не вызывается; шаринг read+write+delete не мешает параллельной работе
// пользователя.
func PlaceholderStateByPath(path string) (uint32, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return cfapiPlaceholderStateInvalid, err
	}
	h, err := windows.CreateFile(path16, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return cfapiPlaceholderStateInvalid, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		_ = windows.CloseHandle(h)
	}()
	var bi FileBasicInfo // FILE_BASIC_INFO — тот же layout, что CF_FS_METADATA.BasicInfo (structs.go)
	buf := (*[unsafe.Sizeof(bi)]byte)(unsafe.Pointer(&bi))[:]
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, &buf[0], uint32(len(buf))); err != nil {
		return cfapiPlaceholderStateInvalid, fmt.Errorf("GetFileInformationByHandleEx(%s): %w", path, err)
	}
	state := CfGetPlaceholderStateFromFileInfo(buf, FileInfoBasicInfo)
	if state == cfapiPlaceholderStateInvalid {
		// INVALID — «не удалось определить состояние» (cfapi.h); живая проба
		// этапа 5 показала его для plain-файла, где ожидается NO_STATES.
		// Для выгрузки это класс «новый файл» — возвращаем NO_STATES как
		// валидный результат (семантика MS: 0 = не плейсхолдер), не отказ.
		logger.Debugf("PlaceholderStateByPath(%s): INVALID from CfGetPlaceholderStateFromFileInfo; treating as plain file (NO_STATES)", path)
		return PlaceholderStateNoStates, nil
	}
	return state, nil
}

// cfapiPlaceholderStateInvalid — CF_PLACEHOLDER_INVALID из cfapi.h (все
// единицы): ошибка CfGetPlaceholderStateFromFileInfo.
const cfapiPlaceholderStateInvalid = uint32(0xffffffff)

// SetInSyncStateByPath — CfSetInSyncState по пути (write-путь выгрузки,
// design.md D8: изменённый плейсхолдер после подтверждённого Flush
// помечается синхронизированным; inSync=TrackNone при регистрации сам
// состояние не переключает). inSyncState — CF_IN_SYNC_STATE
// (InSyncStateInSync/InSyncStateNotInSync).
func SetInSyncStateByPath(path string, inSyncState uint32) error {
	var h syscall.Handle
	if err := CfOpenFileWithOplock(path, OpenFileFlagNone, &h); err != nil {
		return fmt.Errorf("CfOpenFileWithOplock(%s): %w", path, err)
	}
	defer func() {
		_ = windows.CloseHandle(windows.Handle(h))
	}()
	if err := CfSetInSyncState(h, inSyncState, SetInSyncFlagNone, nil); err != nil {
		return fmt.Errorf("CfSetInSyncState(%s, %d): %w", path, inSyncState, err)
	}
	return nil
}

// SetPinStateByPath — pin/unpin по пути (задача 4.1, cfapi.h:909):
// CfOpenFileWithOplock → CfSetPinState → закрытие хендла. pinState —
// CF_PIN_STATE (PinStatePinned/PinStateUnpinned/...), pinFlags —
// CF_SET_PIN_FLAGS (SetPinFlag*; RECURSE — флаг --recursive команды).
//
// Вызов безопасен из ЛЮБОГО процесса с доступом к записи — в том числе из
// CLI-процесса `juicefs clouddrive pin` (design.md D3 ограничивает только
// пул воркеров провайдера: воркер гидратации не должен вызывать
// CfSetPinState/CfHydratePlaceholder, чтобы не блокировать себя на
// собственную очередь). Состояние pin фильтр хранит персистентно
// (переживает перезапуск провайдера); гидрация — по FETCH_DATA к живому
// соединению, поэтому скачивание содержимого требует работающего serve.
//
// Проба U5 (этап 4): рекурсивный pin каталога фильтр выполняет сам —
// FETCH_DATA приходит в serve пачкой на все файлы поддерева и обслуживается
// пулом гидратации (--hydration-workers); CLI-процесс не участвует.
// Unpin (PinStateUnpinned): фильтр дешидратирует запись; callback
// NOTIFY_DEHYDRATE не зарегистрирован (D5 — не среди 9 OS-точек), поэтому
// уведомление протекает без ack — поведение по умолчанию допустимо.
func SetPinStateByPath(path string, pinState, pinFlags uint32) error {
	var h syscall.Handle
	if err := CfOpenFileWithOplock(path, OpenFileFlagNone, &h); err != nil {
		return fmt.Errorf("CfOpenFileWithOplock(%s): %w", path, err)
	}
	defer func() {
		_ = windows.CloseHandle(windows.Handle(h))
	}()
	if err := CfSetPinState(h, pinState, pinFlags, 0); err != nil {
		return fmt.Errorf("CfSetPinState(%s): %w", path, err)
	}
	return nil
}
