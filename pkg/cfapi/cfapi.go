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

// Package cfapi — биндинг Windows Cloud Files API (cldapi.dll) без cgo
// (design.md D1): транскрипция структур cfapi.h, процедуры, thunk'и
// callback'ов, диспетчер заданий и пул страница-выровненных буферов.
//
// Движок провайдера (pkg/cfprovider) обращается к cldapi.dll только через
// этот пакет: интерфейс CallbackDispatcher и процедурные обёртки проверяются
// на подменных реализациях без зарегистрированного sync root.
package cfapi

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// cldapi.dll — user-mode API Cloud Files; загрузка строго из System32.
// Обёртки-процедуры паникуют, если DLL не найдена: до CfConnectSyncRoot
// вызывайте Available (Windows 10 1709+ содержит cldapi.dll всегда).
var (
	modcldapi = windows.NewLazySystemDLL("cldapi.dll")

	procCfRegisterSyncRoot                = modcldapi.NewProc("CfRegisterSyncRoot")
	procCfUnregisterSyncRoot              = modcldapi.NewProc("CfUnregisterSyncRoot")
	procCfConnectSyncRoot                 = modcldapi.NewProc("CfConnectSyncRoot")
	procCfDisconnectSyncRoot              = modcldapi.NewProc("CfDisconnectSyncRoot")
	procCfCreatePlaceholders              = modcldapi.NewProc("CfCreatePlaceholders")
	procCfExecute                         = modcldapi.NewProc("CfExecute")
	procCfReportProviderProgress          = modcldapi.NewProc("CfReportProviderProgress")
	procCfGetTransferKey                  = modcldapi.NewProc("CfGetTransferKey")
	procCfReleaseTransferKey              = modcldapi.NewProc("CfReleaseTransferKey")
	procCfHydratePlaceholder              = modcldapi.NewProc("CfHydratePlaceholder")
	procCfDehydratePlaceholder            = modcldapi.NewProc("CfDehydratePlaceholder")
	procCfSetPinState                     = modcldapi.NewProc("CfSetPinState")
	procCfSetInSyncState                  = modcldapi.NewProc("CfSetInSyncState")
	procCfConvertToPlaceholder            = modcldapi.NewProc("CfConvertToPlaceholder")
	procCfUpdatePlaceholder               = modcldapi.NewProc("CfUpdatePlaceholder")
	procCfGetPlaceholderInfo              = modcldapi.NewProc("CfGetPlaceholderInfo")
	procCfGetPlaceholderStateFromFileInfo = modcldapi.NewProc("CfGetPlaceholderStateFromFileInfo")
)

// HRESULTError — ошибка HRESULT, возвращённая процедурой cldapi.dll.
type HRESULTError struct {
	Call string
	Code HRESULT
}

func (e *HRESULTError) Error() string {
	return fmt.Sprintf("%s: HRESULT 0x%08x", e.Call, uint32(e.Code))
}

// hresult оборачивает код возврата процедуры в ошибку с контекстом вызова.
func hresult(r uintptr, call string) error {
	if hr := HRESULT(int32(r)); hr != 0 {
		return &HRESULTError{Call: call, Code: hr}
	}
	return nil
}

// Available проверяет, что cldapi.dll загрузилась.
func Available() error {
	return modcldapi.Load()
}

// bytesPtr копирует байты в новую память и возвращает указатель для
// полей-идентичностей (GC удерживает копию через поле структуры).
func bytesPtr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return unsafe.Pointer(&c[0])
}

// NewSyncRegistration заполняет CF_SYNC_REGISTRATION: StructSize, копии
// строк (UTF-16) и идентичностей. Структура владеет своей памятью —
// строки и идентичности копируются.
func NewSyncRegistration(providerName, providerVersion string, syncRootIdentity, fileIdentity []byte, providerID windows.GUID) (*CFSyncRegistration, error) {
	name, err := windows.UTF16PtrFromString(providerName)
	if err != nil {
		return nil, err
	}
	version, err := windows.UTF16PtrFromString(providerVersion)
	if err != nil {
		return nil, err
	}
	return &CFSyncRegistration{
		StructSize:             uint32(unsafe.Sizeof(CFSyncRegistration{})),
		ProviderName:           name,
		ProviderVersion:        version,
		SyncRootIdentity:       bytesPtr(syncRootIdentity),
		SyncRootIdentityLength: uint32(len(syncRootIdentity)),
		FileIdentity:           bytesPtr(fileIdentity),
		FileIdentityLength:     uint32(len(fileIdentity)),
		ProviderId:             providerID,
	}, nil
}

// NewSyncPolicies заполняет CF_SYNC_POLICIES: StructSize и политики.
func NewSyncPolicies(hydration CFHydrationPolicy, population CFPopulationPolicy, inSync, hardLink, placeholderManagement uint32) *CFSyncPolicies {
	return &CFSyncPolicies{
		StructSize:            uint32(unsafe.Sizeof(CFSyncPolicies{})),
		Hydration:             hydration,
		Population:            population,
		InSync:                inSync,
		HardLink:              hardLink,
		PlaceholderManagement: placeholderManagement,
	}
}

// CfRegisterSyncRoot регистрирует sync root (CF_REGISTER_FLAGS).
func CfRegisterSyncRoot(syncRootPath string, registration *CFSyncRegistration, policies *CFSyncPolicies, registerFlags uint32) error {
	path, err := windows.UTF16PtrFromString(syncRootPath)
	if err != nil {
		return err
	}
	r, _, _ := procCfRegisterSyncRoot.Call(
		uintptr(unsafe.Pointer(path)),
		uintptr(unsafe.Pointer(registration)),
		uintptr(unsafe.Pointer(policies)),
		uintptr(registerFlags),
	)
	return hresult(r, "CfRegisterSyncRoot")
}

// CfUnregisterSyncRoot снимает регистрацию sync root.
func CfUnregisterSyncRoot(syncRootPath string) error {
	path, err := windows.UTF16PtrFromString(syncRootPath)
	if err != nil {
		return err
	}
	r, _, _ := procCfUnregisterSyncRoot.Call(uintptr(unsafe.Pointer(path)))
	return hresult(r, "CfUnregisterSyncRoot")
}

// CfConnectSyncRoot подключает провайдер к sync root с таблицей callback'ов
// (см. CallbackTable в callbacks.go). CallbackContext возвращается фильтром
// в CF_CALLBACK_INFO.CallbackContext.
func CfConnectSyncRoot(syncRootPath string, callbackTable []CFCallbackRegistration, callbackContext uintptr, connectFlags uint32, connectionKey *ConnectionKey) error {
	path, err := windows.UTF16PtrFromString(syncRootPath)
	if err != nil {
		return err
	}
	var table uintptr
	if len(callbackTable) > 0 {
		table = uintptr(unsafe.Pointer(&callbackTable[0]))
	}
	r, _, _ := procCfConnectSyncRoot.Call(
		uintptr(unsafe.Pointer(path)),
		table,
		callbackContext,
		uintptr(connectFlags),
		uintptr(unsafe.Pointer(connectionKey)),
	)
	return hresult(r, "CfConnectSyncRoot")
}

// CfDisconnectSyncRoot разрывает соединение с sync root.
func CfDisconnectSyncRoot(connectionKey ConnectionKey) error {
	r, _, _ := procCfDisconnectSyncRoot.Call(uintptr(connectionKey.Internal))
	return hresult(r, "CfDisconnectSyncRoot")
}

// CfCreatePlaceholders создаёт плейсхолдеры в базовом каталоге; результаты
// записи каждой записи — в поле Result элементов массива.
func CfCreatePlaceholders(baseDirectoryPath string, placeholderArray []CFPlaceholderCreateInfo, createFlags uint32, entriesProcessed *uint32) error {
	path, err := windows.UTF16PtrFromString(baseDirectoryPath)
	if err != nil {
		return err
	}
	var array uintptr
	if len(placeholderArray) > 0 {
		array = uintptr(unsafe.Pointer(&placeholderArray[0]))
	}
	processed := uintptr(0)
	if entriesProcessed != nil {
		processed = uintptr(unsafe.Pointer(entriesProcessed))
	}
	r, _, _ := procCfCreatePlaceholders.Call(
		uintptr(unsafe.Pointer(path)),
		array,
		uintptr(len(placeholderArray)),
		uintptr(createFlags),
		processed,
	)
	return hresult(r, "CfCreatePlaceholders")
}

// CfExecute выполняет операцию провайдера (TRANSFER_DATA, TRANSFER_PLACEHOLDERS,
// ACK_*): единственный ответ на задание гидратации (design.md, инвариант).
func CfExecute(opInfo *CFOperationInfo, opParams *CFOperationParameters) error {
	r, _, _ := procCfExecute.Call(
		uintptr(unsafe.Pointer(opInfo)),
		uintptr(unsafe.Pointer(opParams)),
	)
	return hresult(r, "CfExecute")
}

// CfReportProviderProgress сообщает прогресс гидратации.
func CfReportProviderProgress(connectionKey ConnectionKey, transferKey TransferKey, progressTotal, progressCompleted int64) error {
	r, _, _ := procCfReportProviderProgress.Call(
		uintptr(connectionKey.Internal),
		uintptr(transferKey),
		uintptr(progressTotal),
		uintptr(progressCompleted),
	)
	return hresult(r, "CfReportProviderProgress")
}

// CfGetTransferKey получает CF_TRANSFER_KEY открытого файла — ключ для
// отмены (CANCEL-обработка, design.md D5).
func CfGetTransferKey(fileHandle syscall.Handle, transferKey *TransferKey) error {
	r, _, _ := procCfGetTransferKey.Call(
		uintptr(fileHandle),
		uintptr(unsafe.Pointer(transferKey)),
	)
	return hresult(r, "CfGetTransferKey")
}

// CfReleaseTransferKey освобождает ключ, полученный CfGetTransferKey (VOID).
func CfReleaseTransferKey(fileHandle syscall.Handle, transferKey *TransferKey) {
	procCfReleaseTransferKey.Call(
		uintptr(fileHandle),
		uintptr(unsafe.Pointer(transferKey)),
	)
}

// CfHydratePlaceholder запрашивает гидратацию диапазона; блокирует до
// завершения FETCH_DATA (разрешён только из пула выгрузки — design.md D3).
func CfHydratePlaceholder(fileHandle syscall.Handle, startingOffset, length int64, hydrateFlags uint32, overlapped uintptr) error {
	r, _, _ := procCfHydratePlaceholder.Call(
		uintptr(fileHandle),
		uintptr(startingOffset),
		uintptr(length),
		uintptr(hydrateFlags),
		overlapped,
	)
	return hresult(r, "CfHydratePlaceholder")
}

// CfDehydratePlaceholder дегидратирует диапазон плейсхолдера.
func CfDehydratePlaceholder(fileHandle syscall.Handle, startingOffset, length int64, dehydrateFlags uint32, overlapped uintptr) error {
	r, _, _ := procCfDehydratePlaceholder.Call(
		uintptr(fileHandle),
		uintptr(startingOffset),
		uintptr(length),
		uintptr(dehydrateFlags),
		overlapped,
	)
	return hresult(r, "CfDehydratePlaceholder")
}

// CfSetPinState устанавливает состояние pin (CF_SET_PIN_FLAGS).
func CfSetPinState(fileHandle syscall.Handle, pinState, pinFlags uint32, overlapped uintptr) error {
	r, _, _ := procCfSetPinState.Call(
		uintptr(fileHandle),
		uintptr(pinState),
		uintptr(pinFlags),
		overlapped,
	)
	return hresult(r, "CfSetPinState")
}

// CfSetInSyncState устанавливает состояние синхронизации плейсхолдера.
func CfSetInSyncState(fileHandle syscall.Handle, inSyncState, inSyncFlags uint32, inSyncUsn *int64) error {
	var usn uintptr
	if inSyncUsn != nil {
		usn = uintptr(unsafe.Pointer(inSyncUsn))
	}
	r, _, _ := procCfSetInSyncState.Call(
		uintptr(fileHandle),
		uintptr(inSyncState),
		uintptr(inSyncFlags),
		usn,
	)
	return hresult(r, "CfSetInSyncState")
}

// CfConvertToPlaceholder преобразует обычный файл в плейсхолдер (U8).
func CfConvertToPlaceholder(fileHandle syscall.Handle, fileIdentity unsafe.Pointer, fileIdentityLength, convertFlags uint32, convertUsn *int64, overlapped uintptr) error {
	var usn uintptr
	if convertUsn != nil {
		usn = uintptr(unsafe.Pointer(convertUsn))
	}
	var identity uintptr
	if fileIdentity != nil {
		identity = uintptr(fileIdentity)
	}
	r, _, _ := procCfConvertToPlaceholder.Call(
		uintptr(fileHandle),
		identity,
		uintptr(fileIdentityLength),
		uintptr(convertFlags),
		usn,
		overlapped,
	)
	return hresult(r, "CfConvertToPlaceholder")
}

// CfUpdatePlaceholder обновляет метаданные/идентичность плейсхолдера
// (CF_UPDATE_FLAGS); диапазоны дегидратации — CF_FILE_RANGE.
func CfUpdatePlaceholder(fileHandle syscall.Handle, fsMetadata *CFFileMetadata, fileIdentity unsafe.Pointer, fileIdentityLength uint32, dehydrateRanges *CFFileRange, dehydrateRangeCount, updateFlags uint32, updateUsn *int64, overlapped uintptr) error {
	var metadata, ranges, usn uintptr
	if fsMetadata != nil {
		metadata = uintptr(unsafe.Pointer(fsMetadata))
	}
	if dehydrateRanges != nil {
		ranges = uintptr(unsafe.Pointer(dehydrateRanges))
	}
	if updateUsn != nil {
		usn = uintptr(unsafe.Pointer(updateUsn))
	}
	var identity uintptr
	if fileIdentity != nil {
		identity = uintptr(fileIdentity)
	}
	r, _, _ := procCfUpdatePlaceholder.Call(
		uintptr(fileHandle),
		metadata,
		identity,
		uintptr(fileIdentityLength),
		ranges,
		uintptr(dehydrateRangeCount),
		uintptr(updateFlags),
		usn,
		overlapped,
	)
	return hresult(r, "CfUpdatePlaceholder")
}

// CfGetPlaceholderInfo читает информацию о плейсхолдере; минимум —
// CF_PLACEHOLDER_BASIC_INFO (pin/in-sync, design.md D1).
func CfGetPlaceholderInfo(fileHandle syscall.Handle, infoClass uint32, infoBuffer []byte, returnedLength *uint32) error {
	var buf, ret uintptr
	if len(infoBuffer) > 0 {
		buf = uintptr(unsafe.Pointer(&infoBuffer[0]))
	}
	if returnedLength != nil {
		ret = uintptr(unsafe.Pointer(returnedLength))
	}
	r, _, _ := procCfGetPlaceholderInfo.Call(
		uintptr(fileHandle),
		uintptr(infoClass),
		buf,
		uintptr(len(infoBuffer)),
		ret,
	)
	return hresult(r, "CfGetPlaceholderInfo")
}

// CfGetPlaceholderStateFromFileInfo возвращает CF_PLACEHOLDER_STATE из буфера
// GetFileInformationByHandleEx (ошибка — PlaceholderStateInvalid).
func CfGetPlaceholderStateFromFileInfo(infoBuffer []byte, infoClass uint32) uint32 {
	var buf uintptr
	if len(infoBuffer) > 0 {
		buf = uintptr(unsafe.Pointer(&infoBuffer[0]))
	}
	state, _, _ := procCfGetPlaceholderStateFromFileInfo.Call(buf, uintptr(infoClass))
	return uint32(state)
}
