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

// Константы и перечисления Windows Cloud Files API — транскрипция cfapi.h
// (NuGet Microsoft.Windows.SDK.CPP 10.0.26100.7175, SHA-256 c50fb112...,
// см. design.md D1). Значения сверены одноразовой C-утилитой layout_dump
// (mingw-w64 gcc, вне репозитория) в layout_test.go.

// Именованные константы cfapi.h.
const (
	EOF                           int64 = -1   // CF_EOF
	RequestKeyDefault             int64 = 0    // CF_REQUEST_KEY_DEFAULT
	PlaceholderMaxFileIdentityLen       = 4096 // CF_PLACEHOLDER_MAX_FILE_IDENTITY_LENGTH
	MaxPriorityHint                     = 15   // CF_MAX_PRIORITY_HINT
	MaxProviderNameLength               = 255  // CF_MAX_PROVIDER_NAME_LENGTH
	MaxProviderVersionLength            = 255  // CF_MAX_PROVIDER_VERSION_LENGTH
)

// CF_CALLBACK_TYPE.
const (
	CallbackTypeFetchData                 uint32 = 0
	CallbackTypeValidateData              uint32 = 1
	CallbackTypeCancelFetchData           uint32 = 2
	CallbackTypeFetchPlaceholders         uint32 = 3
	CallbackTypeCancelFetchPlaceholders   uint32 = 4
	CallbackTypeNotifyFileOpenCompletion  uint32 = 5
	CallbackTypeNotifyFileCloseCompletion uint32 = 6
	CallbackTypeNotifyDehydrate           uint32 = 7
	CallbackTypeNotifyDehydrateCompletion uint32 = 8
	CallbackTypeNotifyDelete              uint32 = 9
	CallbackTypeNotifyDeleteCompletion    uint32 = 10
	CallbackTypeNotifyRename              uint32 = 11
	CallbackTypeNotifyRenameCompletion    uint32 = 12
	CallbackTypeNone                      uint32 = 0xffffffff
)

// CF_CALLBACK_CANCEL_FLAGS.
const (
	CallbackCancelFlagNone      uint32 = 0x00000000
	CallbackCancelFlagIOTimeout uint32 = 0x00000001
	CallbackCancelFlagIOAborted uint32 = 0x00000002
)

// CF_CALLBACK_FETCH_DATA_FLAGS.
const (
	CallbackFetchDataFlagNone              uint32 = 0x00000000
	CallbackFetchDataFlagRecovery          uint32 = 0x00000001
	CallbackFetchDataFlagExplicitHydration uint32 = 0x00000002
)

// CF_CALLBACK_VALIDATE_DATA_FLAGS.
const (
	CallbackValidateDataFlagNone              uint32 = 0x00000000
	CallbackValidateDataFlagExplicitHydration uint32 = 0x00000002
)

// CF_CALLBACK_FETCH_PLACEHOLDERS_FLAGS.
const CallbackFetchPlaceholdersFlagNone uint32 = 0x00000000

// CF_CALLBACK_OPEN_COMPLETION_FLAGS.
const (
	CallbackOpenCompletionFlagNone                   uint32 = 0x00000000
	CallbackOpenCompletionFlagPlaceholderUnknown     uint32 = 0x00000001
	CallbackOpenCompletionFlagPlaceholderUnsupported uint32 = 0x00000002
)

// CF_CALLBACK_CLOSE_COMPLETION_FLAGS.
const (
	CallbackCloseCompletionFlagNone    uint32 = 0x00000000
	CallbackCloseCompletionFlagDeleted uint32 = 0x00000001
)

// CF_CALLBACK_DEHYDRATE_FLAGS.
const (
	CallbackDehydrateFlagNone       uint32 = 0x00000000
	CallbackDehydrateFlagBackground uint32 = 0x00000001
)

// CF_CALLBACK_DEHYDRATE_COMPLETION_FLAGS.
const (
	CallbackDehydrateCompletionFlagNone       uint32 = 0x00000000
	CallbackDehydrateCompletionFlagBackground uint32 = 0x00000001
	CallbackDehydrateCompletionFlagDehydrated uint32 = 0x00000002
)

// CF_CALLBACK_DEHYDRATION_REASON.
const (
	CallbackDehydrationReasonNone             uint32 = 0
	CallbackDehydrationReasonUserManual       uint32 = 1
	CallbackDehydrationReasonSystemLowSpace   uint32 = 2
	CallbackDehydrationReasonSystemInactivity uint32 = 3
	CallbackDehydrationReasonSystemOSUpgrade  uint32 = 4
)

// CF_CALLBACK_DELETE_FLAGS.
const (
	CallbackDeleteFlagNone        uint32 = 0x00000000
	CallbackDeleteFlagIsDirectory uint32 = 0x00000001
	CallbackDeleteFlagIsUndelete  uint32 = 0x00000002
)

// CF_CALLBACK_DELETE_COMPLETION_FLAGS.
const CallbackDeleteCompletionFlagNone uint32 = 0x00000000

// CF_CALLBACK_RENAME_FLAGS.
const (
	CallbackRenameFlagNone          uint32 = 0x00000000
	CallbackRenameFlagIsDirectory   uint32 = 0x00000001
	CallbackRenameFlagSourceInScope uint32 = 0x00000002
	CallbackRenameFlagTargetInScope uint32 = 0x00000004
)

// CF_CALLBACK_RENAME_COMPLETION_FLAGS.
const CallbackRenameCompletionFlagNone uint32 = 0x00000000

// CF_CONNECT_FLAGS.
const (
	ConnectFlagNone                       uint32 = 0x00000000
	ConnectFlagRequireProcessInfo         uint32 = 0x00000002
	ConnectFlagRequireFullFilePath        uint32 = 0x00000004
	ConnectFlagBlockSelfImplicitHydration uint32 = 0x00000008
)

// CF_REGISTER_FLAGS.
const (
	RegisterFlagNone                            uint32 = 0x00000000
	RegisterFlagUpdate                          uint32 = 0x00000001
	RegisterFlagDisableOnDemandPopulationOnRoot uint32 = 0x00000002
	RegisterFlagMarkInSyncOnRoot                uint32 = 0x00000004
)

// CF_HYDRATION_POLICY_PRIMARY.
const (
	HydrationPolicyPartial     uint16 = 0
	HydrationPolicyProgressive uint16 = 1
	HydrationPolicyFull        uint16 = 2
	HydrationPolicyAlwaysFull  uint16 = 3
)

// CF_HYDRATION_POLICY_MODIFIER.
const (
	HydrationPolicyModifierNone                      uint16 = 0x0000
	HydrationPolicyModifierValidationRequired        uint16 = 0x0001
	HydrationPolicyModifierStreamingAllowed          uint16 = 0x0002
	HydrationPolicyModifierAutoDehydrationAllowed    uint16 = 0x0004
	HydrationPolicyModifierAllowFullRestartHydration uint16 = 0x0008
)

// CF_POPULATION_POLICY_PRIMARY.
const (
	PopulationPolicyPartial    uint16 = 0
	PopulationPolicyFull       uint16 = 2
	PopulationPolicyAlwaysFull uint16 = 3
)

// CF_POPULATION_POLICY_MODIFIER.
const PopulationPolicyModifierNone uint16 = 0x0000

// CF_INSYNC_POLICY.
const (
	InSyncPolicyTrackNone                       uint32 = 0x00000000
	InSyncPolicyTrackFileCreationTime           uint32 = 0x00000001
	InSyncPolicyTrackFileReadonlyAttribute      uint32 = 0x00000002
	InSyncPolicyTrackFileHiddenAttribute        uint32 = 0x00000004
	InSyncPolicyTrackFileSystemAttribute        uint32 = 0x00000008
	InSyncPolicyTrackDirectoryCreationTime      uint32 = 0x00000010
	InSyncPolicyTrackDirectoryReadonlyAttribute uint32 = 0x00000020
	InSyncPolicyTrackDirectoryHiddenAttribute   uint32 = 0x00000040
	InSyncPolicyTrackDirectorySystemAttribute   uint32 = 0x00000080
	InSyncPolicyTrackFileLastWriteTime          uint32 = 0x00000100
	InSyncPolicyTrackDirectoryLastWriteTime     uint32 = 0x00000200
	InSyncPolicyTrackFileAll                    uint32 = 0x0055550f
	InSyncPolicyTrackDirectoryAll               uint32 = 0x00aaaaf0
	InSyncPolicyTrackAll                        uint32 = 0x00ffffff
	InSyncPolicyPreserveInSyncForSyncEngine     uint32 = 0x80000000
)

// CF_HARDLINK_POLICY.
const (
	HardlinkPolicyNone    uint32 = 0x00000000
	HardlinkPolicyAllowed uint32 = 0x00000001
)

// CF_PLACEHOLDER_MANAGEMENT_POLICY — кто делает placeholder-операции.
// DEFAULT (0x0) означает «провайдер делает всё»: фильтр блокирует
// пользовательские операции с плейсхолдерами. Unrestricted-биты открывают
// их пользователю: CREATE (0x1 — новые файлы в sync root), CONVERT (0x2 —
// конверсия full-файла в плейсхолдер, например после выгрузки), UPDATE
// (0x4 — правки содержимого плейсхолдеров). Живая проба 27.11: DEFAULT на
// register → ЛЮБОЙ create в sync root падает с ERROR_CANT_RESOLVE_FILENAME
// (Win32 1921 / HRESULT 0x80070781) — для write-пути (В3) нужны все три
// бита (маска 0x7).
const (
	PlaceholderManagementPolicyDefault               uint32 = 0x00000000
	PlaceholderManagementPolicyCreateUnrestricted    uint32 = 0x00000001
	PlaceholderManagementPolicyConvertToUnrestricted uint32 = 0x00000002
	PlaceholderManagementPolicyUpdateUnrestricted    uint32 = 0x00000004
)

// CF_PLACEHOLDER_CREATE_FLAGS.
const (
	PlaceholderCreateFlagNone                      uint32 = 0x00000000
	PlaceholderCreateFlagDisableOnDemandPopulation uint32 = 0x00000001
	PlaceholderCreateFlagMarkInSync                uint32 = 0x00000002
	PlaceholderCreateFlagSupersede                 uint32 = 0x00000004
	PlaceholderCreateFlagAlwaysFull                uint32 = 0x00000008
)

// CF_OPERATION_TYPE.
const (
	OperationTypeTransferData         uint32 = 0
	OperationTypeRetrieveData         uint32 = 1
	OperationTypeAckData              uint32 = 2
	OperationTypeRestartHydration     uint32 = 3
	OperationTypeTransferPlaceholders uint32 = 4
	OperationTypeAckDehydrate         uint32 = 5
	OperationTypeAckDelete            uint32 = 6
	OperationTypeAckRename            uint32 = 7
)

// CF_OPERATION_*_FLAGS.
const (
	OperationTransferDataFlagNone                              uint32 = 0x00000000
	OperationRetrieveDataFlagNone                              uint32 = 0x00000000
	OperationAckDataFlagNone                                   uint32 = 0x00000000
	OperationRestartHydrationFlagNone                          uint32 = 0x00000000
	OperationRestartHydrationFlagMarkInSync                    uint32 = 0x00000001
	OperationTransferPlaceholdersFlagNone                      uint32 = 0x00000000
	OperationTransferPlaceholdersFlagStopOnError               uint32 = 0x00000001
	OperationTransferPlaceholdersFlagDisableOnDemandPopulation uint32 = 0x00000002
	OperationAckDehydrateFlagNone                              uint32 = 0x00000000
	OperationAckRenameFlagNone                                 uint32 = 0x00000000
	OperationAckDeleteFlagNone                                 uint32 = 0x00000000
)

// CF_SYNC_PROVIDER_STATUS.
const (
	ProviderStatusDisconnected      uint32 = 0x00000000
	ProviderStatusIdle              uint32 = 0x00000001
	ProviderStatusPopulateNamespace uint32 = 0x00000002
	ProviderStatusPopulateMetadata  uint32 = 0x00000004
	ProviderStatusPopulateContent   uint32 = 0x00000008
	ProviderStatusSyncIncremental   uint32 = 0x00000010
	ProviderStatusSyncFull          uint32 = 0x00000020
	ProviderStatusConnectivityLost  uint32 = 0x00000040
	ProviderStatusClearFlags        uint32 = 0x80000000
	ProviderStatusTerminated        uint32 = 0xC0000001
	ProviderStatusError             uint32 = 0xC0000002
)

// CF_PIN_STATE.
const (
	PinStateUnspecified uint32 = 0
	PinStatePinned      uint32 = 1
	PinStateUnpinned    uint32 = 2
	PinStateExcluded    uint32 = 3
	PinStateInherit     uint32 = 4 // только set
)

// CF_SET_PIN_FLAGS.
const (
	SetPinFlagNone               uint32 = 0x00000000
	SetPinFlagRecurse            uint32 = 0x00000001
	SetPinFlagRecurseOnly        uint32 = 0x00000002
	SetPinFlagRecurseStopOnError uint32 = 0x00000004
)

// CF_IN_SYNC_STATE.
const (
	InSyncStateNotInSync uint32 = 0
	InSyncStateInSync    uint32 = 1
)

// CF_SET_IN_SYNC_FLAGS.
const SetInSyncFlagNone uint32 = 0x00000000

// CF_HYDRATE_FLAGS / CF_DEHYDRATE_FLAGS.
const (
	HydrateFlagNone         uint32 = 0x00000000
	DehydrateFlagNone       uint32 = 0x00000000
	DehydrateFlagBackground uint32 = 0x00000001
)

// CF_CREATE_FLAGS.
const (
	CreateFlagNone        uint32 = 0x00000000
	CreateFlagStopOnError uint32 = 0x00000001
)

// CF_OPEN_FILE_FLAGS.
const (
	OpenFileFlagNone         uint32 = 0x00000000
	OpenFileFlagExclusive    uint32 = 0x00000001
	OpenFileFlagWriteAccess  uint32 = 0x00000002
	OpenFileFlagDeleteAccess uint32 = 0x00000004
	OpenFileFlagForeground   uint32 = 0x00000008
)

// CF_CONVERT_FLAGS.
const (
	ConvertFlagNone                     uint32 = 0x00000000
	ConvertFlagMarkInSync               uint32 = 0x00000001
	ConvertFlagDehydrate                uint32 = 0x00000002
	ConvertFlagEnableOnDemandPopulation uint32 = 0x00000004
	ConvertFlagAlwaysFull               uint32 = 0x00000008
	ConvertFlagForceConvertToCloudFile  uint32 = 0x00000010
)

// CF_UPDATE_FLAGS.
const (
	UpdateFlagNone                      uint32 = 0x00000000
	UpdateFlagVerifyInSync              uint32 = 0x00000001
	UpdateFlagMarkInSync                uint32 = 0x00000002
	UpdateFlagDehydrate                 uint32 = 0x00000004
	UpdateFlagEnableOnDemandPopulation  uint32 = 0x00000008
	UpdateFlagDisableOnDemandPopulation uint32 = 0x00000010
	UpdateFlagRemoveFileIdentity        uint32 = 0x00000020
	UpdateFlagClearInSync               uint32 = 0x00000040
	UpdateFlagRemoveProperty            uint32 = 0x00000080
	UpdateFlagPassthroughFsMetadata     uint32 = 0x00000100
	UpdateFlagAlwaysFull                uint32 = 0x00000200
	UpdateFlagAllowPartial              uint32 = 0x00000400
)

// CF_REVERT_FLAGS.
const RevertFlagNone uint32 = 0x00000000

// CF_PLACEHOLDER_STATE.
const (
	PlaceholderStateNoStates             uint32 = 0x00000000
	PlaceholderStatePlaceholder          uint32 = 0x00000001
	PlaceholderStateSyncRoot             uint32 = 0x00000002
	PlaceholderStateEssentialPropPresent uint32 = 0x00000004
	PlaceholderStateInSync               uint32 = 0x00000008
	PlaceholderStatePartial              uint32 = 0x00000010
	PlaceholderStatePartiallyOnDisk      uint32 = 0x00000020
	PlaceholderStateInvalid              uint32 = 0xffffffff
)

// CF_PLACEHOLDER_INFO_CLASS.
const (
	PlaceholderInfoBasic    uint32 = 0
	PlaceholderInfoStandard uint32 = 1
)

// CF_SYNC_ROOT_INFO_CLASS.
const (
	SyncRootInfoBasic    uint32 = 0
	SyncRootInfoStandard uint32 = 1
	SyncRootInfoProvider uint32 = 2
)

// FILE_INFO_BY_HANDLE_CLASS — только используемые значения (fileapi.h).
const (
	FileInfoBasicInfo    uint32 = 0 // FileBasicInfo
	FileInfoStandardInfo uint32 = 1 // FileStandardInfo
)
