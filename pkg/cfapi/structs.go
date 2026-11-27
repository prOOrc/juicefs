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
	"unsafe"

	"golang.org/x/sys/windows"
)

// Транскрипция структур CF_* из cfapi.h (design.md D1). Раскладка памяти —
// Windows x64, natural alignment; размеры и смещения сверяются с эталонными
// значениями (layout_dump, mingw-w64 gcc x86_64) в layout_test.go.
//
// Правила транскрипции:
//   - DWORD/ULONG/HRESULT/NTSTATUS/enum -> uint32/int32;
//   - LARGE_INTEGER/USN                  -> int64 (TransferKey/RequestKey);
//   - LPCWSTR/PCWSTR                     -> *uint16 (GC видит указатель);
//   - LPCVOID, LPVOID                    -> unsafe.Pointer (GC видит указатель
//     и удерживает Go-память при вызове в cldapi.dll);
//   - CF_TRANSFER_KEY.Buffer и другие адреса не-Go памяти (VirtualAlloc) ->
//     uintptr/unsafe.Pointer без опасности для GC (память вне Go-кучи);
//   - Union Go не имеет: активная ветка выбирается типом callback'а/операции;
//     union транскрибирован массивом фиксированного размера + ветвями-структурами,
//     которые накладываются на него по одному и тому же смещению.

// HRESULT — код возврата STDAPI-функций cldapi.dll (0 — успех).
type HRESULT int32

// NTStatus — код завершения операций (CompletionStatus); бит-паттерн C
// NTSTATUS (LONG), беззнаковое представление — коды 0xC0000000+.
type NTStatus uint32

// ConnectionKey — CF_CONNECTION_KEY: непрозрачный ключ соединения с sync root.
type ConnectionKey struct {
	Internal int64
}

// TransferKey — CF_TRANSFER_KEY: LARGE_INTEGER, идентификатор операции гидратации.
type TransferKey int64

// RequestKey — CF_REQUEST_KEY: LARGE_INTEGER, идентификатор отдельного запроса.
type RequestKey int64

// FileBasicInfo — FILE_BASIC_INFO (fileapi.h), поле CF_FS_METADATA.
type FileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              [4]byte // выравнивание структуры до 8
}

// CFFileMetadata — CF_FS_METADATA: метаданные создаваемого плейсхолдера.
type CFFileMetadata struct {
	BasicInfo FileBasicInfo
	FileSize  int64
}

// CFPlaceholderCreateInfo — CF_PLACEHOLDER_CREATE_INFO: элемент массива
// CfCreatePlaceholders/TRANSFER_PLACEHOLDERS. Результат записи cldapi
// возвращает в поле Result.
type CFPlaceholderCreateInfo struct {
	RelativeFileName   *uint16 // LPCWSTR, путь относительно базового каталога
	FsMetadata         CFFileMetadata
	FileIdentity       unsafe.Pointer // LPCVOID (у провайдера — inode, 8 байт LE)
	FileIdentityLength uint32
	Flags              uint32 // CF_PLACEHOLDER_CREATE_FLAGS
	Result             HRESULT
	_                  [4]byte
	CreateUsn          int64 // USN
}

// CFProcessInfo — CF_PROCESS_INFO: сведения о процессе, вызвавшем callback
// (заполняется фильтром при CF_CONNECT_FLAG_REQUIRE_PROCESS_INFO).
type CFProcessInfo struct {
	StructSize    uint32
	ProcessId     uint32
	ImagePath     *uint16
	PackageName   *uint16
	ApplicationId *uint16
	CommandLine   *uint16
	SessionId     uint32
	_             [4]byte
}

// CFHydrationPolicy — CF_HYDRATION_POLICY: PRIMARY_USHORT + MODIFIER_USHORT.
type CFHydrationPolicy struct {
	Primary  uint16 // CF_HYDRATION_POLICY_PRIMARY
	Modifier uint16 // CF_HYDRATION_POLICY_MODIFIER
}

// CFPopulationPolicy — CF_POPULATION_POLICY.
type CFPopulationPolicy struct {
	Primary  uint16 // CF_POPULATION_POLICY_PRIMARY
	Modifier uint16 // CF_POPULATION_POLICY_MODIFIER
}

// CFSyncPolicies — CF_SYNC_POLICIES: политики регистрации sync root.
type CFSyncPolicies struct {
	StructSize            uint32
	Hydration             CFHydrationPolicy
	Population            CFPopulationPolicy
	InSync                uint32 // CF_INSYNC_POLICY
	HardLink              uint32 // CF_HARDLINK_POLICY
	PlaceholderManagement uint32 // CF_PLACEHOLDER_MANAGEMENT_POLICY
}

// CFSyncRegistration — CF_SYNC_REGISTRATION: описание провайдера при
// CfRegisterSyncRoot. Идентичности — байты произвольного размера; у
// провайдера SyncRootIdentity/FileIdentity — inode, 8 байт LE (design.md D6).
type CFSyncRegistration struct {
	StructSize             uint32
	_                      [4]byte
	ProviderName           *uint16
	ProviderVersion        *uint16
	SyncRootIdentity       unsafe.Pointer
	SyncRootIdentityLength uint32
	_                      [4]byte
	FileIdentity           unsafe.Pointer
	FileIdentityLength     uint32
	// Без паддинга перед ProviderId: GUID выровнен на 4 и ложится на 52
	// (эталон layout_dump).
	ProviderId windows.GUID
}

// CFCallbackInfo — CF_CALLBACK_INFO: контекст callback'а, заполняется
// фильтром cldflt.sys. Структура только читается; строки (UTF-16) и
// CFProcessInfo принадлежат Windows и действуют лишь внутри callback'а.
type CFCallbackInfo struct {
	StructSize             uint32
	_                      [4]byte
	ConnectionKey          ConnectionKey
	CallbackContext        unsafe.Pointer // CallbackContext из CfConnectSyncRoot
	VolumeGuidName         *uint16
	VolumeDosName          *uint16
	VolumeSerialNumber     uint32
	_                      [4]byte
	SyncRootFileId         int64
	SyncRootIdentity       unsafe.Pointer
	SyncRootIdentityLength uint32
	_                      [4]byte
	FileId                 int64
	FileSize               int64
	FileIdentity           unsafe.Pointer
	FileIdentityLength     uint32
	_                      [4]byte
	NormalizedPath         *uint16 // полный путь (CF_CONNECT_FLAG_REQUIRE_FULL_FILE_PATH)
	TransferKey            TransferKey
	PriorityHint           uint8
	_                      [7]byte
	CorrelationVector      *byte // PCORRELATION_VECTOR — ANSI-строка
	ProcessInfo            *CFProcessInfo
	RequestKey             RequestKey
}

// Размер union'а CF_CALLBACK_PARAMETERS — максимум веток, FetchData (56 байт);
// union начинается на смещении 8 (эталоны — layout_dump, layout_test.go).
const callbackUnionSize = 56

// CFCallbackParameters — CF_CALLBACK_PARAMETERS. Активная ветка union'а
// определяется типом callback'а (ParamSize фильтр ставит равным размеру
// активной ветки). Чтение веток — методами-видами из callbacks.go.
type CFCallbackParameters struct {
	ParamSize uint32
	_         [4]byte
	Union     [callbackUnionSize]byte
}

// Ветки union'а CF_CALLBACK_PARAMETERS; смещения — относительно начала
// union'а (абсолютные = +8), сверяются в layout_test.go.

// callbackCancelParams — ветка Cancel (CANCEL_FETCH_DATA,
// CANCEL_FETCH_PLACEHOLDERS): Flags + вложенный union FetchData.
type callbackCancelParams struct {
	Flags      uint32 // CF_CALLBACK_CANCEL_FLAGS
	_          [4]byte
	FileOffset int64 // Cancel.FetchData.FileOffset
	Length     int64 // Cancel.FetchData.Length
}

// callbackFetchDataParams — ветка FetchData (FETCH_DATA).
type callbackFetchDataParams struct {
	Flags                 uint32 // CF_CALLBACK_FETCH_DATA_FLAGS
	_                     [4]byte
	RequiredFileOffset    int64
	RequiredLength        int64
	OptionalFileOffset    int64
	OptionalLength        int64
	LastDehydrationTime   int64
	LastDehydrationReason uint32 // CF_CALLBACK_DEHYDRATION_REASON
}

// callbackValidateDataParams — ветка ValidateData (VALIDATE_DATA).
type callbackValidateDataParams struct {
	Flags              uint32 // CF_CALLBACK_VALIDATE_DATA_FLAGS
	_                  [4]byte
	RequiredFileOffset int64
	RequiredLength     int64
}

// callbackFetchPlaceholdersParams — ветка FetchPlaceholders (FETCH_PLACEHOLDERS).
type callbackFetchPlaceholdersParams struct {
	Flags   uint32 // CF_CALLBACK_FETCH_PLACEHOLDERS_FLAGS
	_       [4]byte
	Pattern *uint16 // шаблон FindFirstFileW
}

// callbackOpenCompletionParams — ветка OpenCompletion (NOTIFY_FILE_OPEN_COMPLETION).
type callbackOpenCompletionParams struct {
	Flags uint32 // CF_CALLBACK_OPEN_COMPLETION_FLAGS
}

// callbackCloseCompletionParams — ветка CloseCompletion (NOTIFY_FILE_CLOSE_COMPLETION).
type callbackCloseCompletionParams struct {
	Flags uint32 // CF_CALLBACK_CLOSE_COMPLETION_FLAGS
}

// callbackDehydrateParams — ветка Dehydrate (NOTIFY_DEHYDRATE).
type callbackDehydrateParams struct {
	Flags  uint32 // CF_CALLBACK_DEHYDRATE_FLAGS
	Reason uint32 // CF_CALLBACK_DEHYDRATION_REASON
}

// callbackDehydrateCompletionParams — ветка DehydrateCompletion
// (NOTIFY_DEHYDRATE_COMPLETION).
type callbackDehydrateCompletionParams struct {
	Flags  uint32 // CF_CALLBACK_DEHYDRATE_COMPLETION_FLAGS
	Reason uint32 // CF_CALLBACK_DEHYDRATION_REASON
}

// callbackDeleteParams — ветка Delete (NOTIFY_DELETE).
type callbackDeleteParams struct {
	Flags uint32 // CF_CALLBACK_DELETE_FLAGS
}

// callbackDeleteCompletionParams — ветка DeleteCompletion (NOTIFY_DELETE_COMPLETION).
type callbackDeleteCompletionParams struct {
	Flags uint32 // CF_CALLBACK_DELETE_COMPLETION_FLAGS
}

// callbackRenameParams — ветка Rename (NOTIFY_RENAME).
type callbackRenameParams struct {
	Flags      uint32 // CF_CALLBACK_RENAME_FLAGS
	_          [4]byte
	TargetPath *uint16
}

// callbackRenameCompletionParams — ветка RenameCompletion (NOTIFY_RENAME_COMPLETION).
type callbackRenameCompletionParams struct {
	Flags      uint32 // CF_CALLBACK_RENAME_COMPLETION_FLAGS
	_          [4]byte
	SourcePath *uint16
}

// Все ветки union'а начинаются с одного смещения (Union[0] выровнен на 8 —
// выравнивания полей-указателей достаточно).

// CancelParams — вид ветки Cancel.
func (p *CFCallbackParameters) CancelParams() *callbackCancelParams {
	return (*callbackCancelParams)(unsafe.Pointer(&p.Union[0]))
}

// FetchDataParams — вид ветки FetchData.
func (p *CFCallbackParameters) FetchDataParams() *callbackFetchDataParams {
	return (*callbackFetchDataParams)(unsafe.Pointer(&p.Union[0]))
}

// ValidateDataParams — вид ветки ValidateData.
func (p *CFCallbackParameters) ValidateDataParams() *callbackValidateDataParams {
	return (*callbackValidateDataParams)(unsafe.Pointer(&p.Union[0]))
}

// FetchPlaceholdersParams — вид ветки FetchPlaceholders.
func (p *CFCallbackParameters) FetchPlaceholdersParams() *callbackFetchPlaceholdersParams {
	return (*callbackFetchPlaceholdersParams)(unsafe.Pointer(&p.Union[0]))
}

// OpenCompletionParams — вид ветки OpenCompletion.
func (p *CFCallbackParameters) OpenCompletionParams() *callbackOpenCompletionParams {
	return (*callbackOpenCompletionParams)(unsafe.Pointer(&p.Union[0]))
}

// CloseCompletionParams — вид ветки CloseCompletion.
func (p *CFCallbackParameters) CloseCompletionParams() *callbackCloseCompletionParams {
	return (*callbackCloseCompletionParams)(unsafe.Pointer(&p.Union[0]))
}

// DehydrateParams — вид ветки Dehydrate.
func (p *CFCallbackParameters) DehydrateParams() *callbackDehydrateParams {
	return (*callbackDehydrateParams)(unsafe.Pointer(&p.Union[0]))
}

// DehydrateCompletionParams — вид ветки DehydrateCompletion.
func (p *CFCallbackParameters) DehydrateCompletionParams() *callbackDehydrateCompletionParams {
	return (*callbackDehydrateCompletionParams)(unsafe.Pointer(&p.Union[0]))
}

// DeleteParams — вид ветки Delete.
func (p *CFCallbackParameters) DeleteParams() *callbackDeleteParams {
	return (*callbackDeleteParams)(unsafe.Pointer(&p.Union[0]))
}

// DeleteCompletionParams — вид ветки DeleteCompletion.
func (p *CFCallbackParameters) DeleteCompletionParams() *callbackDeleteCompletionParams {
	return (*callbackDeleteCompletionParams)(unsafe.Pointer(&p.Union[0]))
}

// RenameParams — вид ветки Rename.
func (p *CFCallbackParameters) RenameParams() *callbackRenameParams {
	return (*callbackRenameParams)(unsafe.Pointer(&p.Union[0]))
}

// RenameCompletionParams — вид ветки RenameCompletion.
func (p *CFCallbackParameters) RenameCompletionParams() *callbackRenameCompletionParams {
	return (*callbackRenameCompletionParams)(unsafe.Pointer(&p.Union[0]))
}

// CFCallbackRegistration — CF_CALLBACK_REGISTRATION; конец таблицы —
// {CallbackTypeNone, 0} (CF_CALLBACK_REGISTRATION_END).
type CFCallbackRegistration struct {
	Type     uint32 // CF_CALLBACK_TYPE
	_        [4]byte
	Callback uintptr // адрес syscall.NewCallback (VOID-прототип: возврат игнорируется)
}

// CFSyncStatus — CF_SYNC_STATUS: расширенный статус провайдера
// (CfReportSyncStatus); описание/устройство — блобы после структуры.
type CFSyncStatus struct {
	StructSize        uint32
	Code              uint32
	DescriptionOffset uint32
	DescriptionLength uint32
	DeviceIdOffset    uint32
	DeviceIdLength    uint32
}

// CFOperationInfo — CF_OPERATION_INFO: заголовок операции CfExecute.
// StructSize заполняет вызывающий код (или хелперы пакета).
type CFOperationInfo struct {
	StructSize        uint32
	Type              uint32 // CF_OPERATION_TYPE
	ConnectionKey     ConnectionKey
	TransferKey       TransferKey
	CorrelationVector *byte // PCORRELATION_VECTOR
	SyncStatus        *CFSyncStatus
	RequestKey        RequestKey
}

// Размер union'а CF_OPERATION_PARAMETERS (максимум веток — RetrieveData,
// 40 байт); эталон — layout_dump.
const operationUnionSize = 40

// CFOperationParameters — CF_OPERATION_PARAMETERS. ParamSize ставится
// равным размеру активной ветки (как в примерах Microsoft). Адрес буфера
// TransferData — страница-выровненный VirtualAlloc из BufferPool (design.md D7).
type CFOperationParameters struct {
	ParamSize uint32
	_         [4]byte
	Union     [operationUnionSize]byte
}

// Ветки union'а CF_OPERATION_PARAMETERS (смещения относительно начала union'а).

// operationTransferDataParams — ветка TransferData: ответ на FETCH_DATA.
type operationTransferDataParams struct {
	Flags            uint32 // CF_OPERATION_TRANSFER_DATA_FLAGS
	CompletionStatus NTStatus
	Buffer           unsafe.Pointer // LPCVOID, Length байт; память вне Go-кучи (BufferPool)
	Offset           int64
	Length           int64
}

// operationRetrieveDataParams — ветка RetrieveData.
type operationRetrieveDataParams struct {
	Flags          uint32 // CF_OPERATION_RETRIEVE_DATA_FLAGS
	_              [4]byte
	Buffer         unsafe.Pointer // LPVOID
	Offset         int64
	Length         int64
	ReturnedLength int64
}

// operationAckDataParams — ветка AckData.
type operationAckDataParams struct {
	Flags            uint32 // CF_OPERATION_ACK_DATA_FLAGS
	CompletionStatus NTStatus
	Offset           int64
	Length           int64
}

// operationRestartHydrationParams — ветка RestartHydration.
type operationRestartHydrationParams struct {
	Flags              uint32 // CF_OPERATION_RESTART_HYDRATION_FLAGS
	_                  [4]byte
	FsMetadata         *CFFileMetadata
	FileIdentity       unsafe.Pointer
	FileIdentityLength uint32
	_                  [4]byte
}

// operationTransferPlaceholdersParams — ветка TransferPlaceholders: ответ
// на FETCH_PLACEHOLDERS.
type operationTransferPlaceholdersParams struct {
	Flags                 uint32 // CF_OPERATION_TRANSFER_PLACEHOLDERS_FLAGS
	CompletionStatus      NTStatus
	PlaceholderTotalCount int64
	PlaceholderArray      *CFPlaceholderCreateInfo
	PlaceholderCount      uint32
	EntriesProcessed      uint32
}

// operationAckDehydrateParams — ветка AckDehydrate.
type operationAckDehydrateParams struct {
	Flags              uint32 // CF_OPERATION_ACK_DEHYDRATE_FLAGS
	CompletionStatus   NTStatus
	FileIdentity       unsafe.Pointer
	FileIdentityLength uint32
	_                  [4]byte
}

// operationAckRenameParams — ветка AckRename.
type operationAckRenameParams struct {
	Flags            uint32 // CF_OPERATION_ACK_RENAME_FLAGS
	CompletionStatus NTStatus
}

// operationAckDeleteParams — ветка AckDelete.
type operationAckDeleteParams struct {
	Flags            uint32 // CF_OPERATION_ACK_DELETE_FLAGS
	CompletionStatus NTStatus
}

// OperationTransferData возвращает вид ветки TransferData.
func (p *CFOperationParameters) OperationTransferData() *operationTransferDataParams {
	return (*operationTransferDataParams)(unsafe.Pointer(&p.Union[0]))
}

// OperationRetrieveData возвращает вид ветки RetrieveData.
func (p *CFOperationParameters) OperationRetrieveData() *operationRetrieveDataParams {
	return (*operationRetrieveDataParams)(unsafe.Pointer(&p.Union[0]))
}

// OperationAckData возвращает вид ветки AckData.
func (p *CFOperationParameters) OperationAckData() *operationAckDataParams {
	return (*operationAckDataParams)(unsafe.Pointer(&p.Union[0]))
}

// OperationRestartHydration возвращает вид ветки RestartHydration.
func (p *CFOperationParameters) OperationRestartHydration() *operationRestartHydrationParams {
	return (*operationRestartHydrationParams)(unsafe.Pointer(&p.Union[0]))
}

// OperationTransferPlaceholders возвращает вид ветки TransferPlaceholders.
func (p *CFOperationParameters) OperationTransferPlaceholders() *operationTransferPlaceholdersParams {
	return (*operationTransferPlaceholdersParams)(unsafe.Pointer(&p.Union[0]))
}

// OperationAckDehydrate возвращает вид ветки AckDehydrate.
func (p *CFOperationParameters) OperationAckDehydrate() *operationAckDehydrateParams {
	return (*operationAckDehydrateParams)(unsafe.Pointer(&p.Union[0]))
}

// OperationAckRename возвращает вид ветки AckRename.
func (p *CFOperationParameters) OperationAckRename() *operationAckRenameParams {
	return (*operationAckRenameParams)(unsafe.Pointer(&p.Union[0]))
}

// OperationAckDelete возвращает вид ветки AckDelete.
func (p *CFOperationParameters) OperationAckDelete() *operationAckDeleteParams {
	return (*operationAckDeleteParams)(unsafe.Pointer(&p.Union[0]))
}

// CFFileRange — CF_FILE_RANGE: диапазон для дегидратации (CfUpdatePlaceholder).
type CFFileRange struct {
	StartingOffset int64
	Length         int64
}

// CFPlaceholderBasicInfo — CF_PLACEHOLDER_BASIC_INFO: ответ
// CfGetPlaceholderInfo(CF_PLACEHOLDER_INFO_BASIC). FileIdentity —
// переменная длина, продолжается за структурой.
type CFPlaceholderBasicInfo struct {
	PinState           uint32 // CF_PIN_STATE
	InSyncState        uint32 // CF_IN_SYNC_STATE
	FileId             int64
	SyncRootFileId     int64
	FileIdentityLength uint32
	FileIdentity       [1]byte // BYTE FileIdentity[1]: хвост переменной длины
}

// CFSyncRootBasicInfo — CF_SYNC_ROOT_BASIC_INFO: ответ
// CfGetSyncRootInfoByPath(CF_SYNC_ROOT_INFO_BASIC). Успешный запрос по пути —
// признак зарегистрированного sync root (design.md D4).
type CFSyncRootBasicInfo struct {
	SyncRootFileId int64
}

// CFSyncRootStandardInfo — CF_SYNC_ROOT_STANDARD_INFO (cfapi.h:1067): ответ
// CfGetSyncRootInfoByPath(CF_SYNC_ROOT_INFO_STANDARD) — фактические политики
// зарегистрированного sync root (гидрация, наполнение, in-sync, hardlink),
// статус провайдера и описание. Отдельного класса POLICIES в CfAPI нет:
// CF_SYNC_ROOT_INFO_CLASS эталонного header (cfapi.h:1027) содержит только
// BASIC/STANDARD/PROVIDER, поэтому STANDARD — единственный readback политик;
// PlaceholderManagementPolicy фильтр не отдаёт (задаётся только при
// регистрации, cfapi.h:237 CF_SYNC_POLICIES) — фактическое значение видно
// лишь поведением фильтра (матрица проб).
type CFSyncRootStandardInfo struct {
	SyncRootFileId         int64
	HydrationPolicy        CFHydrationPolicy
	PopulationPolicy       CFPopulationPolicy
	InSyncPolicy           uint32      // CF_INSYNC_POLICY
	HardLinkPolicy         uint32      // CF_HARDLINK_POLICY
	ProviderStatus         uint32      // CF_SYNC_PROVIDER_STATUS
	ProviderName           [256]uint16 // WCHAR[CF_MAX_PROVIDER_NAME_LENGTH + 1]
	ProviderVersion        [256]uint16
	SyncRootIdentityLength uint32
	SyncRootIdentity       [1]byte // BYTE[1]: хвост переменной длины
}

// CFPlaceholderStandardInfo — CF_PLACEHOLDER_STANDARD_INFO.
type CFPlaceholderStandardInfo struct {
	OnDiskDataSize     int64
	ValidatedDataSize  int64
	ModifiedDataSize   int64
	PropertiesSize     int64
	PinState           uint32
	InSyncState        uint32
	FileId             int64
	SyncRootFileId     int64
	FileIdentityLength uint32
	FileIdentity       [1]byte // BYTE FileIdentity[1]: хвост переменной длины
}

// fileIDOf читает inode из CF_CALLBACK_INFO.FileIdentity: первые 8 байт —
// inode (LE), design.md D6. Идентичность короче 8 байт считается отсутствующей.
func fileIDOf(identity unsafe.Pointer, length uint32) uint64 {
	if identity == nil || length < 8 {
		return 0
	}
	return leUint64((*byte)(identity))
}

// leUint64 читает little-endian uint64 из памяти по указателю без требований
// к выравниванию (используется на памяти Windows).
func leUint64(p *byte) uint64 {
	b := (*[8]byte)(unsafe.Pointer(p))
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}
