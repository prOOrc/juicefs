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
	"testing"
	"unsafe"
)

// Эталонные размеры и смещения структур CF_* из cfapi.h — gold-значения
// одноразовой C-утилиты layout_dump (mingw-w64 gcc x86_64 по cfapi.h NuGet
// Microsoft.Windows.SDK.CPP 10.0.26100.7175; design.md D1, задача 1.2).
// Утилита и header в репозиторий не входят — здесь только числа.
// Расхождение любой константы с эталоном валит тест до первого вызова
// cldapi.dll (спека «Сверка layout структур биндинга»).
const (
	goldSizeofPointer = 8
	goldSizeofDWORD   = 4
	goldSizeofGUID    = 16

	goldSizeofFileBasicInfo            = 40
	goldOffFileBasicInfoCreationTime   = 0
	goldOffFileBasicInfoLastAccessTime = 8
	goldOffFileBasicInfoLastWriteTime  = 16
	goldOffFileBasicInfoChangeTime     = 24
	goldOffFileBasicInfoFileAttributes = 32

	goldSizeofFileMetadata       = 48
	goldOffFileMetadataBasicInfo = 0
	goldOffFileMetadataFileSize  = 40

	goldSizeofPlaceholderCreateInfo              = 88
	goldOffPlaceholderCreateInfoRelativeFileName = 0
	goldOffPlaceholderCreateInfoFsMetadata       = 8
	goldOffPlaceholderCreateInfoFileIdentity     = 56
	goldOffPlaceholderCreateInfoIdentityLength   = 64
	goldOffPlaceholderCreateInfoFlags            = 68
	goldOffPlaceholderCreateInfoResult           = 72
	goldOffPlaceholderCreateInfoCreateUsn        = 80

	goldSizeofProcessInfo           = 48
	goldOffProcessInfoStructSize    = 0
	goldOffProcessInfoProcessId     = 4
	goldOffProcessInfoImagePath     = 8
	goldOffProcessInfoPackageName   = 16
	goldOffProcessInfoApplicationId = 24
	goldOffProcessInfoCommandLine   = 32
	goldOffProcessInfoSessionId     = 40

	goldSizeofHydrationPolicy  = 4
	goldSizeofPopulationPolicy = 4

	goldSizeofSyncPolicies                   = 24
	goldOffSyncPoliciesStructSize            = 0
	goldOffSyncPoliciesHydration             = 4
	goldOffSyncPoliciesPopulation            = 8
	goldOffSyncPoliciesInSync                = 12
	goldOffSyncPoliciesHardLink              = 16
	goldOffSyncPoliciesPlaceholderManagement = 20

	goldSizeofSyncRegistration                    = 72
	goldOffSyncRegistrationStructSize             = 0
	goldOffSyncRegistrationProviderName           = 8
	goldOffSyncRegistrationProviderVersion        = 16
	goldOffSyncRegistrationSyncRootIdentity       = 24
	goldOffSyncRegistrationSyncRootIdentityLength = 32
	goldOffSyncRegistrationFileIdentity           = 40
	goldOffSyncRegistrationFileIdentityLength     = 48
	goldOffSyncRegistrationProviderId             = 52 // GUID выровнен на 4, не на 8

	goldSizeofCallbackInfo                    = 152
	goldOffCallbackInfoStructSize             = 0
	goldOffCallbackInfoConnectionKey          = 8
	goldOffCallbackInfoCallbackContext        = 16
	goldOffCallbackInfoVolumeGuidName         = 24
	goldOffCallbackInfoVolumeDosName          = 32
	goldOffCallbackInfoVolumeSerialNumber     = 40
	goldOffCallbackInfoSyncRootFileId         = 48
	goldOffCallbackInfoSyncRootIdentity       = 56
	goldOffCallbackInfoSyncRootIdentityLength = 64
	goldOffCallbackInfoFileId                 = 72
	goldOffCallbackInfoFileSize               = 80
	goldOffCallbackInfoFileIdentity           = 88
	goldOffCallbackInfoFileIdentityLength     = 96
	goldOffCallbackInfoNormalizedPath         = 104
	goldOffCallbackInfoTransferKey            = 112
	goldOffCallbackInfoPriorityHint           = 120
	goldOffCallbackInfoCorrelationVector      = 128
	goldOffCallbackInfoProcessInfo            = 136
	goldOffCallbackInfoRequestKey             = 144

	goldSizeofCallbackParameters       = 64
	goldOffCallbackParametersParamSize = 0
	goldOffCallbackParametersUnion     = 8
	goldOffCallbackParametersUnionSize = 56 // max веток — FetchData

	goldSizeofCbCancel        = 24
	goldOffCbCancelFlags      = 0
	goldOffCbCancelFileOffset = 8
	goldOffCbCancelLength     = 16

	goldSizeofCbFetchData                   = 56
	goldOffCbFetchDataFlags                 = 0
	goldOffCbFetchDataRequiredFileOffset    = 8
	goldOffCbFetchDataRequiredLength        = 16
	goldOffCbFetchDataOptionalFileOffset    = 24
	goldOffCbFetchDataOptionalLength        = 32
	goldOffCbFetchDataLastDehydrationTime   = 40
	goldOffCbFetchDataLastDehydrationReason = 48

	goldSizeofCbValidateData                = 24
	goldOffCbValidateDataFlags              = 0
	goldOffCbValidateDataRequiredFileOffset = 8
	goldOffCbValidateDataRequiredLength     = 16

	goldSizeofCbFetchPlaceholders     = 16
	goldOffCbFetchPlaceholdersFlags   = 0
	goldOffCbFetchPlaceholdersPattern = 8

	goldSizeofCbOpenCompletion          = 4
	goldSizeofCbCloseCompletion         = 4
	goldSizeofCbDehydrate               = 8
	goldOffCbDehydrateFlags             = 0
	goldOffCbDehydrateReason            = 4
	goldSizeofCbDehydrateCompletion     = 8
	goldOffCbDehydrateCompletionFlags   = 0
	goldOffCbDehydrateCompletionReason  = 4
	goldSizeofCbDelete                  = 4
	goldSizeofCbDeleteCompletion        = 4
	goldSizeofCbRename                  = 16
	goldOffCbRenameFlags                = 0
	goldOffCbRenameTargetPath           = 8
	goldSizeofCbRenameCompletion        = 16
	goldOffCbRenameCompletionFlags      = 0
	goldOffCbRenameCompletionSourcePath = 8

	goldSizeofCallbackRegistration      = 16
	goldOffCallbackRegistrationType     = 0
	goldOffCallbackRegistrationCallback = 8

	goldSizeofSyncStatus               = 24
	goldOffSyncStatusStructSize        = 0
	goldOffSyncStatusCode              = 4
	goldOffSyncStatusDescriptionOffset = 8
	goldOffSyncStatusDescriptionLength = 12
	goldOffSyncStatusDeviceIdOffset    = 16
	goldOffSyncStatusDeviceIdLength    = 20

	goldSizeofOperationInfo               = 48
	goldOffOperationInfoStructSize        = 0
	goldOffOperationInfoType              = 4
	goldOffOperationInfoConnectionKey     = 8
	goldOffOperationInfoTransferKey       = 16
	goldOffOperationInfoCorrelationVector = 24
	goldOffOperationInfoSyncStatus        = 32
	goldOffOperationInfoRequestKey        = 40

	goldSizeofOperationParameters       = 48
	goldOffOperationParametersParamSize = 0
	goldOffOperationParametersUnion     = 8
	goldOffOperationParametersUnionSize = 40 // max веток — RetrieveData

	goldSizeofOpTransferData              = 32
	goldOffOpTransferDataFlags            = 0
	goldOffOpTransferDataCompletionStatus = 4
	goldOffOpTransferDataBuffer           = 8
	goldOffOpTransferDataOffset           = 16
	goldOffOpTransferDataLength           = 24

	goldSizeofOpRetrieveData            = 40
	goldOffOpRetrieveDataFlags          = 0
	goldOffOpRetrieveDataBuffer         = 8
	goldOffOpRetrieveDataOffset         = 16
	goldOffOpRetrieveDataLength         = 24
	goldOffOpRetrieveDataReturnedLength = 32

	goldSizeofOpAckData              = 24
	goldOffOpAckDataFlags            = 0
	goldOffOpAckDataCompletionStatus = 4
	goldOffOpAckDataOffset           = 8
	goldOffOpAckDataLength           = 16

	goldSizeofOpRestartHydration                = 32
	goldOffOpRestartHydrationFlags              = 0
	goldOffOpRestartHydrationFsMetadata         = 8
	goldOffOpRestartHydrationFileIdentity       = 16
	goldOffOpRestartHydrationFileIdentityLength = 24

	goldSizeofOpTransferPlaceholders              = 32
	goldOffOpTransferPlaceholdersFlags            = 0
	goldOffOpTransferPlaceholdersCompletionStatus = 4
	goldOffOpTransferPlaceholdersTotalCount       = 8
	goldOffOpTransferPlaceholdersArray            = 16
	goldOffOpTransferPlaceholdersCount            = 24
	goldOffOpTransferPlaceholdersProcessed        = 28

	goldSizeofOpAckDehydrate                = 24
	goldOffOpAckDehydrateFlags              = 0
	goldOffOpAckDehydrateCompletionStatus   = 4
	goldOffOpAckDehydrateFileIdentity       = 8
	goldOffOpAckDehydrateFileIdentityLength = 16

	goldSizeofOpAckRename              = 8
	goldOffOpAckRenameFlags            = 0
	goldOffOpAckRenameCompletionStatus = 4
	goldSizeofOpAckDelete              = 8
	goldOffOpAckDeleteFlags            = 0
	goldOffOpAckDeleteCompletionStatus = 4

	goldSizeofFileRange    = 16
	goldOffFileRangeStart  = 0
	goldOffFileRangeLength = 8

	goldSizeofPlaceholderBasicInfo                = 32
	goldOffPlaceholderBasicInfoPinState           = 0
	goldOffPlaceholderBasicInfoInSyncState        = 4
	goldOffPlaceholderBasicInfoFileId             = 8
	goldOffPlaceholderBasicInfoSyncRootFileId     = 16
	goldOffPlaceholderBasicInfoFileIdentityLength = 24
	goldOffPlaceholderBasicInfoFileIdentity       = 28

	goldSizeofPlaceholderStandardInfo                = 64
	goldOffPlaceholderStandardInfoOnDiskDataSize     = 0
	goldOffPlaceholderStandardInfoValidatedDataSize  = 8
	goldOffPlaceholderStandardInfoModifiedDataSize   = 16
	goldOffPlaceholderStandardInfoPropertiesSize     = 24
	goldOffPlaceholderStandardInfoPinState           = 32
	goldOffPlaceholderStandardInfoInSyncState        = 36
	goldOffPlaceholderStandardInfoFileId             = 40
	goldOffPlaceholderStandardInfoSyncRootFileId     = 48
	goldOffPlaceholderStandardInfoFileIdentityLength = 56
	goldOffPlaceholderStandardInfoFileIdentity       = 60
)

// checkS/checkO сверяют размер и смещение с эталоном.
func checkS(t *testing.T, name string, got, want uintptr) {
	t.Helper()
	if got != want {
		t.Errorf("sizeof %s = %d, эталон %d", name, got, want)
	}
}

func checkO(t *testing.T, name string, got, want uintptr) {
	t.Helper()
	if got != want {
		t.Errorf("offset %s = %d, эталон %d", name, got, want)
	}
}

func TestLayout(t *testing.T) {
	// Примитивы платформы: Go windows/amd64 и mingw-w64 x86_64 должны совпадать.
	checkS(t, "unsafe.Pointer", unsafe.Sizeof((*byte)(nil)), goldSizeofPointer)
	checkS(t, "uint32(DWORD)", unsafe.Sizeof(uint32(0)), goldSizeofDWORD)
	checkS(t, "windows.GUID", unsafe.Sizeof(windowsGUID{}), goldSizeofGUID)
	checkS(t, "ConnectionKey", unsafe.Sizeof(ConnectionKey{}), 8)
	checkS(t, "TransferKey", unsafe.Sizeof(TransferKey(0)), 8)
	checkS(t, "RequestKey", unsafe.Sizeof(RequestKey(0)), 8)

	// CF_FS_METADATA / FILE_BASIC_INFO
	checkS(t, "FileBasicInfo", unsafe.Sizeof(FileBasicInfo{}), goldSizeofFileBasicInfo)
	checkO(t, "FileBasicInfo.CreationTime", unsafe.Offsetof(FileBasicInfo{}.CreationTime), goldOffFileBasicInfoCreationTime)
	checkO(t, "FileBasicInfo.LastAccessTime", unsafe.Offsetof(FileBasicInfo{}.LastAccessTime), goldOffFileBasicInfoLastAccessTime)
	checkO(t, "FileBasicInfo.LastWriteTime", unsafe.Offsetof(FileBasicInfo{}.LastWriteTime), goldOffFileBasicInfoLastWriteTime)
	checkO(t, "FileBasicInfo.ChangeTime", unsafe.Offsetof(FileBasicInfo{}.ChangeTime), goldOffFileBasicInfoChangeTime)
	checkO(t, "FileBasicInfo.FileAttributes", unsafe.Offsetof(FileBasicInfo{}.FileAttributes), goldOffFileBasicInfoFileAttributes)

	checkS(t, "CFFileMetadata", unsafe.Sizeof(CFFileMetadata{}), goldSizeofFileMetadata)
	checkO(t, "CFFileMetadata.BasicInfo", unsafe.Offsetof(CFFileMetadata{}.BasicInfo), goldOffFileMetadataBasicInfo)
	checkO(t, "CFFileMetadata.FileSize", unsafe.Offsetof(CFFileMetadata{}.FileSize), goldOffFileMetadataFileSize)

	// CF_PLACEHOLDER_CREATE_INFO
	checkS(t, "CFPlaceholderCreateInfo", unsafe.Sizeof(CFPlaceholderCreateInfo{}), goldSizeofPlaceholderCreateInfo)
	checkO(t, "CFPlaceholderCreateInfo.RelativeFileName", unsafe.Offsetof(CFPlaceholderCreateInfo{}.RelativeFileName), goldOffPlaceholderCreateInfoRelativeFileName)
	checkO(t, "CFPlaceholderCreateInfo.FsMetadata", unsafe.Offsetof(CFPlaceholderCreateInfo{}.FsMetadata), goldOffPlaceholderCreateInfoFsMetadata)
	checkO(t, "CFPlaceholderCreateInfo.FileIdentity", unsafe.Offsetof(CFPlaceholderCreateInfo{}.FileIdentity), goldOffPlaceholderCreateInfoFileIdentity)
	checkO(t, "CFPlaceholderCreateInfo.FileIdentityLength", unsafe.Offsetof(CFPlaceholderCreateInfo{}.FileIdentityLength), goldOffPlaceholderCreateInfoIdentityLength)
	checkO(t, "CFPlaceholderCreateInfo.Flags", unsafe.Offsetof(CFPlaceholderCreateInfo{}.Flags), goldOffPlaceholderCreateInfoFlags)
	checkO(t, "CFPlaceholderCreateInfo.Result", unsafe.Offsetof(CFPlaceholderCreateInfo{}.Result), goldOffPlaceholderCreateInfoResult)
	checkO(t, "CFPlaceholderCreateInfo.CreateUsn", unsafe.Offsetof(CFPlaceholderCreateInfo{}.CreateUsn), goldOffPlaceholderCreateInfoCreateUsn)

	// CF_PROCESS_INFO
	checkS(t, "CFProcessInfo", unsafe.Sizeof(CFProcessInfo{}), goldSizeofProcessInfo)
	checkO(t, "CFProcessInfo.StructSize", unsafe.Offsetof(CFProcessInfo{}.StructSize), goldOffProcessInfoStructSize)
	checkO(t, "CFProcessInfo.ProcessId", unsafe.Offsetof(CFProcessInfo{}.ProcessId), goldOffProcessInfoProcessId)
	checkO(t, "CFProcessInfo.ImagePath", unsafe.Offsetof(CFProcessInfo{}.ImagePath), goldOffProcessInfoImagePath)
	checkO(t, "CFProcessInfo.PackageName", unsafe.Offsetof(CFProcessInfo{}.PackageName), goldOffProcessInfoPackageName)
	checkO(t, "CFProcessInfo.ApplicationId", unsafe.Offsetof(CFProcessInfo{}.ApplicationId), goldOffProcessInfoApplicationId)
	checkO(t, "CFProcessInfo.CommandLine", unsafe.Offsetof(CFProcessInfo{}.CommandLine), goldOffProcessInfoCommandLine)
	checkO(t, "CFProcessInfo.SessionId", unsafe.Offsetof(CFProcessInfo{}.SessionId), goldOffProcessInfoSessionId)

	// CF_SYNC_POLICIES и вложенные политики
	checkS(t, "CFHydrationPolicy", unsafe.Sizeof(CFHydrationPolicy{}), goldSizeofHydrationPolicy)
	checkS(t, "CFPopulationPolicy", unsafe.Sizeof(CFPopulationPolicy{}), goldSizeofPopulationPolicy)
	checkS(t, "CFSyncPolicies", unsafe.Sizeof(CFSyncPolicies{}), goldSizeofSyncPolicies)
	checkO(t, "CFSyncPolicies.StructSize", unsafe.Offsetof(CFSyncPolicies{}.StructSize), goldOffSyncPoliciesStructSize)
	checkO(t, "CFSyncPolicies.Hydration", unsafe.Offsetof(CFSyncPolicies{}.Hydration), goldOffSyncPoliciesHydration)
	checkO(t, "CFSyncPolicies.Population", unsafe.Offsetof(CFSyncPolicies{}.Population), goldOffSyncPoliciesPopulation)
	checkO(t, "CFSyncPolicies.InSync", unsafe.Offsetof(CFSyncPolicies{}.InSync), goldOffSyncPoliciesInSync)
	checkO(t, "CFSyncPolicies.HardLink", unsafe.Offsetof(CFSyncPolicies{}.HardLink), goldOffSyncPoliciesHardLink)
	checkO(t, "CFSyncPolicies.PlaceholderManagement", unsafe.Offsetof(CFSyncPolicies{}.PlaceholderManagement), goldOffSyncPoliciesPlaceholderManagement)

	// CF_SYNC_REGISTRATION
	checkS(t, "CFSyncRegistration", unsafe.Sizeof(CFSyncRegistration{}), goldSizeofSyncRegistration)
	checkO(t, "CFSyncRegistration.StructSize", unsafe.Offsetof(CFSyncRegistration{}.StructSize), goldOffSyncRegistrationStructSize)
	checkO(t, "CFSyncRegistration.ProviderName", unsafe.Offsetof(CFSyncRegistration{}.ProviderName), goldOffSyncRegistrationProviderName)
	checkO(t, "CFSyncRegistration.ProviderVersion", unsafe.Offsetof(CFSyncRegistration{}.ProviderVersion), goldOffSyncRegistrationProviderVersion)
	checkO(t, "CFSyncRegistration.SyncRootIdentity", unsafe.Offsetof(CFSyncRegistration{}.SyncRootIdentity), goldOffSyncRegistrationSyncRootIdentity)
	checkO(t, "CFSyncRegistration.SyncRootIdentityLength", unsafe.Offsetof(CFSyncRegistration{}.SyncRootIdentityLength), goldOffSyncRegistrationSyncRootIdentityLength)
	checkO(t, "CFSyncRegistration.FileIdentity", unsafe.Offsetof(CFSyncRegistration{}.FileIdentity), goldOffSyncRegistrationFileIdentity)
	checkO(t, "CFSyncRegistration.FileIdentityLength", unsafe.Offsetof(CFSyncRegistration{}.FileIdentityLength), goldOffSyncRegistrationFileIdentityLength)
	checkO(t, "CFSyncRegistration.ProviderId", unsafe.Offsetof(CFSyncRegistration{}.ProviderId), goldOffSyncRegistrationProviderId)

	// CF_CALLBACK_INFO
	checkS(t, "CFCallbackInfo", unsafe.Sizeof(CFCallbackInfo{}), goldSizeofCallbackInfo)
	checkO(t, "CFCallbackInfo.StructSize", unsafe.Offsetof(CFCallbackInfo{}.StructSize), goldOffCallbackInfoStructSize)
	checkO(t, "CFCallbackInfo.ConnectionKey", unsafe.Offsetof(CFCallbackInfo{}.ConnectionKey), goldOffCallbackInfoConnectionKey)
	checkO(t, "CFCallbackInfo.CallbackContext", unsafe.Offsetof(CFCallbackInfo{}.CallbackContext), goldOffCallbackInfoCallbackContext)
	checkO(t, "CFCallbackInfo.VolumeGuidName", unsafe.Offsetof(CFCallbackInfo{}.VolumeGuidName), goldOffCallbackInfoVolumeGuidName)
	checkO(t, "CFCallbackInfo.VolumeDosName", unsafe.Offsetof(CFCallbackInfo{}.VolumeDosName), goldOffCallbackInfoVolumeDosName)
	checkO(t, "CFCallbackInfo.VolumeSerialNumber", unsafe.Offsetof(CFCallbackInfo{}.VolumeSerialNumber), goldOffCallbackInfoVolumeSerialNumber)
	checkO(t, "CFCallbackInfo.SyncRootFileId", unsafe.Offsetof(CFCallbackInfo{}.SyncRootFileId), goldOffCallbackInfoSyncRootFileId)
	checkO(t, "CFCallbackInfo.SyncRootIdentity", unsafe.Offsetof(CFCallbackInfo{}.SyncRootIdentity), goldOffCallbackInfoSyncRootIdentity)
	checkO(t, "CFCallbackInfo.SyncRootIdentityLength", unsafe.Offsetof(CFCallbackInfo{}.SyncRootIdentityLength), goldOffCallbackInfoSyncRootIdentityLength)
	checkO(t, "CFCallbackInfo.FileId", unsafe.Offsetof(CFCallbackInfo{}.FileId), goldOffCallbackInfoFileId)
	checkO(t, "CFCallbackInfo.FileSize", unsafe.Offsetof(CFCallbackInfo{}.FileSize), goldOffCallbackInfoFileSize)
	checkO(t, "CFCallbackInfo.FileIdentity", unsafe.Offsetof(CFCallbackInfo{}.FileIdentity), goldOffCallbackInfoFileIdentity)
	checkO(t, "CFCallbackInfo.FileIdentityLength", unsafe.Offsetof(CFCallbackInfo{}.FileIdentityLength), goldOffCallbackInfoFileIdentityLength)
	checkO(t, "CFCallbackInfo.NormalizedPath", unsafe.Offsetof(CFCallbackInfo{}.NormalizedPath), goldOffCallbackInfoNormalizedPath)
	checkO(t, "CFCallbackInfo.TransferKey", unsafe.Offsetof(CFCallbackInfo{}.TransferKey), goldOffCallbackInfoTransferKey)
	checkO(t, "CFCallbackInfo.PriorityHint", unsafe.Offsetof(CFCallbackInfo{}.PriorityHint), goldOffCallbackInfoPriorityHint)
	checkO(t, "CFCallbackInfo.CorrelationVector", unsafe.Offsetof(CFCallbackInfo{}.CorrelationVector), goldOffCallbackInfoCorrelationVector)
	checkO(t, "CFCallbackInfo.ProcessInfo", unsafe.Offsetof(CFCallbackInfo{}.ProcessInfo), goldOffCallbackInfoProcessInfo)
	checkO(t, "CFCallbackInfo.RequestKey", unsafe.Offsetof(CFCallbackInfo{}.RequestKey), goldOffCallbackInfoRequestKey)

	// CF_CALLBACK_PARAMETERS: внешняя структура и union на смещении 8
	checkS(t, "CFCallbackParameters", unsafe.Sizeof(CFCallbackParameters{}), goldSizeofCallbackParameters)
	checkO(t, "CFCallbackParameters.ParamSize", unsafe.Offsetof(CFCallbackParameters{}.ParamSize), goldOffCallbackParametersParamSize)
	checkO(t, "CFCallbackParameters.Union", unsafe.Offsetof(CFCallbackParameters{}.Union), goldOffCallbackParametersUnion)
	checkS(t, "CFCallbackParameters.Union", unsafe.Sizeof(CFCallbackParameters{}.Union), goldOffCallbackParametersUnionSize)

	// Ветки union'а CF_CALLBACK_PARAMETERS (относительные смещения = эталон - 8)
	checkS(t, "callbackCancelParams", unsafe.Sizeof(callbackCancelParams{}), goldSizeofCbCancel)
	checkO(t, "callbackCancelParams.Flags", unsafe.Offsetof(callbackCancelParams{}.Flags), goldOffCbCancelFlags)
	checkO(t, "callbackCancelParams.FileOffset", unsafe.Offsetof(callbackCancelParams{}.FileOffset), goldOffCbCancelFileOffset)
	checkO(t, "callbackCancelParams.Length", unsafe.Offsetof(callbackCancelParams{}.Length), goldOffCbCancelLength)

	checkS(t, "callbackFetchDataParams", unsafe.Sizeof(callbackFetchDataParams{}), goldSizeofCbFetchData)
	checkO(t, "callbackFetchDataParams.Flags", unsafe.Offsetof(callbackFetchDataParams{}.Flags), goldOffCbFetchDataFlags)
	checkO(t, "callbackFetchDataParams.RequiredFileOffset", unsafe.Offsetof(callbackFetchDataParams{}.RequiredFileOffset), goldOffCbFetchDataRequiredFileOffset)
	checkO(t, "callbackFetchDataParams.RequiredLength", unsafe.Offsetof(callbackFetchDataParams{}.RequiredLength), goldOffCbFetchDataRequiredLength)
	checkO(t, "callbackFetchDataParams.OptionalFileOffset", unsafe.Offsetof(callbackFetchDataParams{}.OptionalFileOffset), goldOffCbFetchDataOptionalFileOffset)
	checkO(t, "callbackFetchDataParams.OptionalLength", unsafe.Offsetof(callbackFetchDataParams{}.OptionalLength), goldOffCbFetchDataOptionalLength)
	checkO(t, "callbackFetchDataParams.LastDehydrationTime", unsafe.Offsetof(callbackFetchDataParams{}.LastDehydrationTime), goldOffCbFetchDataLastDehydrationTime)
	checkO(t, "callbackFetchDataParams.LastDehydrationReason", unsafe.Offsetof(callbackFetchDataParams{}.LastDehydrationReason), goldOffCbFetchDataLastDehydrationReason)

	checkS(t, "callbackValidateDataParams", unsafe.Sizeof(callbackValidateDataParams{}), goldSizeofCbValidateData)
	checkO(t, "callbackValidateDataParams.Flags", unsafe.Offsetof(callbackValidateDataParams{}.Flags), goldOffCbValidateDataFlags)
	checkO(t, "callbackValidateDataParams.RequiredFileOffset", unsafe.Offsetof(callbackValidateDataParams{}.RequiredFileOffset), goldOffCbValidateDataRequiredFileOffset)
	checkO(t, "callbackValidateDataParams.RequiredLength", unsafe.Offsetof(callbackValidateDataParams{}.RequiredLength), goldOffCbValidateDataRequiredLength)

	checkS(t, "callbackFetchPlaceholdersParams", unsafe.Sizeof(callbackFetchPlaceholdersParams{}), goldSizeofCbFetchPlaceholders)
	checkO(t, "callbackFetchPlaceholdersParams.Flags", unsafe.Offsetof(callbackFetchPlaceholdersParams{}.Flags), goldOffCbFetchPlaceholdersFlags)
	checkO(t, "callbackFetchPlaceholdersParams.Pattern", unsafe.Offsetof(callbackFetchPlaceholdersParams{}.Pattern), goldOffCbFetchPlaceholdersPattern)

	checkS(t, "callbackOpenCompletionParams", unsafe.Sizeof(callbackOpenCompletionParams{}), goldSizeofCbOpenCompletion)
	checkS(t, "callbackCloseCompletionParams", unsafe.Sizeof(callbackCloseCompletionParams{}), goldSizeofCbCloseCompletion)
	checkS(t, "callbackDehydrateParams", unsafe.Sizeof(callbackDehydrateParams{}), goldSizeofCbDehydrate)
	checkO(t, "callbackDehydrateParams.Flags", unsafe.Offsetof(callbackDehydrateParams{}.Flags), goldOffCbDehydrateFlags)
	checkO(t, "callbackDehydrateParams.Reason", unsafe.Offsetof(callbackDehydrateParams{}.Reason), goldOffCbDehydrateReason)
	checkS(t, "callbackDehydrateCompletionParams", unsafe.Sizeof(callbackDehydrateCompletionParams{}), goldSizeofCbDehydrateCompletion)
	checkO(t, "callbackDehydrateCompletionParams.Flags", unsafe.Offsetof(callbackDehydrateCompletionParams{}.Flags), goldOffCbDehydrateCompletionFlags)
	checkO(t, "callbackDehydrateCompletionParams.Reason", unsafe.Offsetof(callbackDehydrateCompletionParams{}.Reason), goldOffCbDehydrateCompletionReason)
	checkS(t, "callbackDeleteParams", unsafe.Sizeof(callbackDeleteParams{}), goldSizeofCbDelete)
	checkS(t, "callbackDeleteCompletionParams", unsafe.Sizeof(callbackDeleteCompletionParams{}), goldSizeofCbDeleteCompletion)
	checkS(t, "callbackRenameParams", unsafe.Sizeof(callbackRenameParams{}), goldSizeofCbRename)
	checkO(t, "callbackRenameParams.Flags", unsafe.Offsetof(callbackRenameParams{}.Flags), goldOffCbRenameFlags)
	checkO(t, "callbackRenameParams.TargetPath", unsafe.Offsetof(callbackRenameParams{}.TargetPath), goldOffCbRenameTargetPath)
	checkS(t, "callbackRenameCompletionParams", unsafe.Sizeof(callbackRenameCompletionParams{}), goldSizeofCbRenameCompletion)
	checkO(t, "callbackRenameCompletionParams.Flags", unsafe.Offsetof(callbackRenameCompletionParams{}.Flags), goldOffCbRenameCompletionFlags)
	checkO(t, "callbackRenameCompletionParams.SourcePath", unsafe.Offsetof(callbackRenameCompletionParams{}.SourcePath), goldOffCbRenameCompletionSourcePath)

	// CF_CALLBACK_REGISTRATION
	checkS(t, "CFCallbackRegistration", unsafe.Sizeof(CFCallbackRegistration{}), goldSizeofCallbackRegistration)
	checkO(t, "CFCallbackRegistration.Type", unsafe.Offsetof(CFCallbackRegistration{}.Type), goldOffCallbackRegistrationType)
	checkO(t, "CFCallbackRegistration.Callback", unsafe.Offsetof(CFCallbackRegistration{}.Callback), goldOffCallbackRegistrationCallback)

	// CF_SYNC_STATUS
	checkS(t, "CFSyncStatus", unsafe.Sizeof(CFSyncStatus{}), goldSizeofSyncStatus)
	checkO(t, "CFSyncStatus.StructSize", unsafe.Offsetof(CFSyncStatus{}.StructSize), goldOffSyncStatusStructSize)
	checkO(t, "CFSyncStatus.Code", unsafe.Offsetof(CFSyncStatus{}.Code), goldOffSyncStatusCode)
	checkO(t, "CFSyncStatus.DescriptionOffset", unsafe.Offsetof(CFSyncStatus{}.DescriptionOffset), goldOffSyncStatusDescriptionOffset)
	checkO(t, "CFSyncStatus.DescriptionLength", unsafe.Offsetof(CFSyncStatus{}.DescriptionLength), goldOffSyncStatusDescriptionLength)
	checkO(t, "CFSyncStatus.DeviceIdOffset", unsafe.Offsetof(CFSyncStatus{}.DeviceIdOffset), goldOffSyncStatusDeviceIdOffset)
	checkO(t, "CFSyncStatus.DeviceIdLength", unsafe.Offsetof(CFSyncStatus{}.DeviceIdLength), goldOffSyncStatusDeviceIdLength)

	// CF_OPERATION_INFO
	checkS(t, "CFOperationInfo", unsafe.Sizeof(CFOperationInfo{}), goldSizeofOperationInfo)
	checkO(t, "CFOperationInfo.StructSize", unsafe.Offsetof(CFOperationInfo{}.StructSize), goldOffOperationInfoStructSize)
	checkO(t, "CFOperationInfo.Type", unsafe.Offsetof(CFOperationInfo{}.Type), goldOffOperationInfoType)
	checkO(t, "CFOperationInfo.ConnectionKey", unsafe.Offsetof(CFOperationInfo{}.ConnectionKey), goldOffOperationInfoConnectionKey)
	checkO(t, "CFOperationInfo.TransferKey", unsafe.Offsetof(CFOperationInfo{}.TransferKey), goldOffOperationInfoTransferKey)
	checkO(t, "CFOperationInfo.CorrelationVector", unsafe.Offsetof(CFOperationInfo{}.CorrelationVector), goldOffOperationInfoCorrelationVector)
	checkO(t, "CFOperationInfo.SyncStatus", unsafe.Offsetof(CFOperationInfo{}.SyncStatus), goldOffOperationInfoSyncStatus)
	checkO(t, "CFOperationInfo.RequestKey", unsafe.Offsetof(CFOperationInfo{}.RequestKey), goldOffOperationInfoRequestKey)

	// CF_OPERATION_PARAMETERS: внешняя структура и union на смещении 8
	checkS(t, "CFOperationParameters", unsafe.Sizeof(CFOperationParameters{}), goldSizeofOperationParameters)
	checkO(t, "CFOperationParameters.ParamSize", unsafe.Offsetof(CFOperationParameters{}.ParamSize), goldOffOperationParametersParamSize)
	checkO(t, "CFOperationParameters.Union", unsafe.Offsetof(CFOperationParameters{}.Union), goldOffOperationParametersUnion)
	checkS(t, "CFOperationParameters.Union", unsafe.Sizeof(CFOperationParameters{}.Union), goldOffOperationParametersUnionSize)

	// Ветки union'а CF_OPERATION_PARAMETERS
	checkS(t, "operationTransferDataParams", unsafe.Sizeof(operationTransferDataParams{}), goldSizeofOpTransferData)
	checkO(t, "operationTransferDataParams.Flags", unsafe.Offsetof(operationTransferDataParams{}.Flags), goldOffOpTransferDataFlags)
	checkO(t, "operationTransferDataParams.CompletionStatus", unsafe.Offsetof(operationTransferDataParams{}.CompletionStatus), goldOffOpTransferDataCompletionStatus)
	checkO(t, "operationTransferDataParams.Buffer", unsafe.Offsetof(operationTransferDataParams{}.Buffer), goldOffOpTransferDataBuffer)
	checkO(t, "operationTransferDataParams.Offset", unsafe.Offsetof(operationTransferDataParams{}.Offset), goldOffOpTransferDataOffset)
	checkO(t, "operationTransferDataParams.Length", unsafe.Offsetof(operationTransferDataParams{}.Length), goldOffOpTransferDataLength)

	checkS(t, "operationRetrieveDataParams", unsafe.Sizeof(operationRetrieveDataParams{}), goldSizeofOpRetrieveData)
	checkO(t, "operationRetrieveDataParams.Flags", unsafe.Offsetof(operationRetrieveDataParams{}.Flags), goldOffOpRetrieveDataFlags)
	checkO(t, "operationRetrieveDataParams.Buffer", unsafe.Offsetof(operationRetrieveDataParams{}.Buffer), goldOffOpRetrieveDataBuffer)
	checkO(t, "operationRetrieveDataParams.Offset", unsafe.Offsetof(operationRetrieveDataParams{}.Offset), goldOffOpRetrieveDataOffset)
	checkO(t, "operationRetrieveDataParams.Length", unsafe.Offsetof(operationRetrieveDataParams{}.Length), goldOffOpRetrieveDataLength)
	checkO(t, "operationRetrieveDataParams.ReturnedLength", unsafe.Offsetof(operationRetrieveDataParams{}.ReturnedLength), goldOffOpRetrieveDataReturnedLength)

	checkS(t, "operationAckDataParams", unsafe.Sizeof(operationAckDataParams{}), goldSizeofOpAckData)
	checkO(t, "operationAckDataParams.Flags", unsafe.Offsetof(operationAckDataParams{}.Flags), goldOffOpAckDataFlags)
	checkO(t, "operationAckDataParams.CompletionStatus", unsafe.Offsetof(operationAckDataParams{}.CompletionStatus), goldOffOpAckDataCompletionStatus)
	checkO(t, "operationAckDataParams.Offset", unsafe.Offsetof(operationAckDataParams{}.Offset), goldOffOpAckDataOffset)
	checkO(t, "operationAckDataParams.Length", unsafe.Offsetof(operationAckDataParams{}.Length), goldOffOpAckDataLength)

	checkS(t, "operationRestartHydrationParams", unsafe.Sizeof(operationRestartHydrationParams{}), goldSizeofOpRestartHydration)
	checkO(t, "operationRestartHydrationParams.Flags", unsafe.Offsetof(operationRestartHydrationParams{}.Flags), goldOffOpRestartHydrationFlags)
	checkO(t, "operationRestartHydrationParams.FsMetadata", unsafe.Offsetof(operationRestartHydrationParams{}.FsMetadata), goldOffOpRestartHydrationFsMetadata)
	checkO(t, "operationRestartHydrationParams.FileIdentity", unsafe.Offsetof(operationRestartHydrationParams{}.FileIdentity), goldOffOpRestartHydrationFileIdentity)
	checkO(t, "operationRestartHydrationParams.FileIdentityLength", unsafe.Offsetof(operationRestartHydrationParams{}.FileIdentityLength), goldOffOpRestartHydrationFileIdentityLength)

	checkS(t, "operationTransferPlaceholdersParams", unsafe.Sizeof(operationTransferPlaceholdersParams{}), goldSizeofOpTransferPlaceholders)
	checkO(t, "operationTransferPlaceholdersParams.Flags", unsafe.Offsetof(operationTransferPlaceholdersParams{}.Flags), goldOffOpTransferPlaceholdersFlags)
	checkO(t, "operationTransferPlaceholdersParams.CompletionStatus", unsafe.Offsetof(operationTransferPlaceholdersParams{}.CompletionStatus), goldOffOpTransferPlaceholdersCompletionStatus)
	checkO(t, "operationTransferPlaceholdersParams.PlaceholderTotalCount", unsafe.Offsetof(operationTransferPlaceholdersParams{}.PlaceholderTotalCount), goldOffOpTransferPlaceholdersTotalCount)
	checkO(t, "operationTransferPlaceholdersParams.PlaceholderArray", unsafe.Offsetof(operationTransferPlaceholdersParams{}.PlaceholderArray), goldOffOpTransferPlaceholdersArray)
	checkO(t, "operationTransferPlaceholdersParams.PlaceholderCount", unsafe.Offsetof(operationTransferPlaceholdersParams{}.PlaceholderCount), goldOffOpTransferPlaceholdersCount)
	checkO(t, "operationTransferPlaceholdersParams.EntriesProcessed", unsafe.Offsetof(operationTransferPlaceholdersParams{}.EntriesProcessed), goldOffOpTransferPlaceholdersProcessed)

	checkS(t, "operationAckDehydrateParams", unsafe.Sizeof(operationAckDehydrateParams{}), goldSizeofOpAckDehydrate)
	checkO(t, "operationAckDehydrateParams.Flags", unsafe.Offsetof(operationAckDehydrateParams{}.Flags), goldOffOpAckDehydrateFlags)
	checkO(t, "operationAckDehydrateParams.CompletionStatus", unsafe.Offsetof(operationAckDehydrateParams{}.CompletionStatus), goldOffOpAckDehydrateCompletionStatus)
	checkO(t, "operationAckDehydrateParams.FileIdentity", unsafe.Offsetof(operationAckDehydrateParams{}.FileIdentity), goldOffOpAckDehydrateFileIdentity)
	checkO(t, "operationAckDehydrateParams.FileIdentityLength", unsafe.Offsetof(operationAckDehydrateParams{}.FileIdentityLength), goldOffOpAckDehydrateFileIdentityLength)

	checkS(t, "operationAckRenameParams", unsafe.Sizeof(operationAckRenameParams{}), goldSizeofOpAckRename)
	checkO(t, "operationAckRenameParams.Flags", unsafe.Offsetof(operationAckRenameParams{}.Flags), goldOffOpAckRenameFlags)
	checkO(t, "operationAckRenameParams.CompletionStatus", unsafe.Offsetof(operationAckRenameParams{}.CompletionStatus), goldOffOpAckRenameCompletionStatus)
	checkS(t, "operationAckDeleteParams", unsafe.Sizeof(operationAckDeleteParams{}), goldSizeofOpAckDelete)
	checkO(t, "operationAckDeleteParams.Flags", unsafe.Offsetof(operationAckDeleteParams{}.Flags), goldOffOpAckDeleteFlags)
	checkO(t, "operationAckDeleteParams.CompletionStatus", unsafe.Offsetof(operationAckDeleteParams{}.CompletionStatus), goldOffOpAckDeleteCompletionStatus)

	// CF_FILE_RANGE
	checkS(t, "CFFileRange", unsafe.Sizeof(CFFileRange{}), goldSizeofFileRange)
	checkO(t, "CFFileRange.StartingOffset", unsafe.Offsetof(CFFileRange{}.StartingOffset), goldOffFileRangeStart)
	checkO(t, "CFFileRange.Length", unsafe.Offsetof(CFFileRange{}.Length), goldOffFileRangeLength)

	// CF_PLACEHOLDER_BASIC_INFO / CF_PLACEHOLDER_STANDARD_INFO
	checkS(t, "CFPlaceholderBasicInfo", unsafe.Sizeof(CFPlaceholderBasicInfo{}), goldSizeofPlaceholderBasicInfo)
	checkO(t, "CFPlaceholderBasicInfo.PinState", unsafe.Offsetof(CFPlaceholderBasicInfo{}.PinState), goldOffPlaceholderBasicInfoPinState)
	checkO(t, "CFPlaceholderBasicInfo.InSyncState", unsafe.Offsetof(CFPlaceholderBasicInfo{}.InSyncState), goldOffPlaceholderBasicInfoInSyncState)
	checkO(t, "CFPlaceholderBasicInfo.FileId", unsafe.Offsetof(CFPlaceholderBasicInfo{}.FileId), goldOffPlaceholderBasicInfoFileId)
	checkO(t, "CFPlaceholderBasicInfo.SyncRootFileId", unsafe.Offsetof(CFPlaceholderBasicInfo{}.SyncRootFileId), goldOffPlaceholderBasicInfoSyncRootFileId)
	checkO(t, "CFPlaceholderBasicInfo.FileIdentityLength", unsafe.Offsetof(CFPlaceholderBasicInfo{}.FileIdentityLength), goldOffPlaceholderBasicInfoFileIdentityLength)
	checkO(t, "CFPlaceholderBasicInfo.FileIdentity", unsafe.Offsetof(CFPlaceholderBasicInfo{}.FileIdentity), goldOffPlaceholderBasicInfoFileIdentity)

	checkS(t, "CFPlaceholderStandardInfo", unsafe.Sizeof(CFPlaceholderStandardInfo{}), goldSizeofPlaceholderStandardInfo)
	checkO(t, "CFPlaceholderStandardInfo.OnDiskDataSize", unsafe.Offsetof(CFPlaceholderStandardInfo{}.OnDiskDataSize), goldOffPlaceholderStandardInfoOnDiskDataSize)
	checkO(t, "CFPlaceholderStandardInfo.ValidatedDataSize", unsafe.Offsetof(CFPlaceholderStandardInfo{}.ValidatedDataSize), goldOffPlaceholderStandardInfoValidatedDataSize)
	checkO(t, "CFPlaceholderStandardInfo.ModifiedDataSize", unsafe.Offsetof(CFPlaceholderStandardInfo{}.ModifiedDataSize), goldOffPlaceholderStandardInfoModifiedDataSize)
	checkO(t, "CFPlaceholderStandardInfo.PropertiesSize", unsafe.Offsetof(CFPlaceholderStandardInfo{}.PropertiesSize), goldOffPlaceholderStandardInfoPropertiesSize)
	checkO(t, "CFPlaceholderStandardInfo.PinState", unsafe.Offsetof(CFPlaceholderStandardInfo{}.PinState), goldOffPlaceholderStandardInfoPinState)
	checkO(t, "CFPlaceholderStandardInfo.InSyncState", unsafe.Offsetof(CFPlaceholderStandardInfo{}.InSyncState), goldOffPlaceholderStandardInfoInSyncState)
	checkO(t, "CFPlaceholderStandardInfo.FileId", unsafe.Offsetof(CFPlaceholderStandardInfo{}.FileId), goldOffPlaceholderStandardInfoFileId)
	checkO(t, "CFPlaceholderStandardInfo.SyncRootFileId", unsafe.Offsetof(CFPlaceholderStandardInfo{}.SyncRootFileId), goldOffPlaceholderStandardInfoSyncRootFileId)
	checkO(t, "CFPlaceholderStandardInfo.FileIdentityLength", unsafe.Offsetof(CFPlaceholderStandardInfo{}.FileIdentityLength), goldOffPlaceholderStandardInfoFileIdentityLength)
	checkO(t, "CFPlaceholderStandardInfo.FileIdentity", unsafe.Offsetof(CFPlaceholderStandardInfo{}.FileIdentity), goldOffPlaceholderStandardInfoFileIdentity)
}

// windowsGUID — псевдоним для проверки размера GUID из x/sys (16 байт,
// выравнивание 4 — как C GUID в CF_SYNC_REGISTRATION).
type windowsGUID = struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// TestCallbackTypeConstants сверяет CF_CALLBACK_TYPE с эталоном (позиционные
// значения из cfapi.h, layout_dump).
func TestCallbackTypeConstants(t *testing.T) {
	cases := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"FETCH_DATA", CallbackTypeFetchData, 0},
		{"VALIDATE_DATA", CallbackTypeValidateData, 1},
		{"CANCEL_FETCH_DATA", CallbackTypeCancelFetchData, 2},
		{"FETCH_PLACEHOLDERS", CallbackTypeFetchPlaceholders, 3},
		{"CANCEL_FETCH_PLACEHOLDERS", CallbackTypeCancelFetchPlaceholders, 4},
		{"NOTIFY_FILE_OPEN_COMPLETION", CallbackTypeNotifyFileOpenCompletion, 5},
		{"NOTIFY_FILE_CLOSE_COMPLETION", CallbackTypeNotifyFileCloseCompletion, 6},
		{"NOTIFY_DEHYDRATE", CallbackTypeNotifyDehydrate, 7},
		{"NOTIFY_DEHYDRATE_COMPLETION", CallbackTypeNotifyDehydrateCompletion, 8},
		{"NOTIFY_DELETE", CallbackTypeNotifyDelete, 9},
		{"NOTIFY_DELETE_COMPLETION", CallbackTypeNotifyDeleteCompletion, 10},
		{"NOTIFY_RENAME", CallbackTypeNotifyRename, 11},
		{"NOTIFY_RENAME_COMPLETION", CallbackTypeNotifyRenameCompletion, 12},
		{"NONE", CallbackTypeNone, 0xffffffff},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("CallbackType %s = %d, эталон %d", c.name, c.got, c.want)
		}
	}
}

// TestOperationTypeConstants сверяет CF_OPERATION_TYPE.
func TestOperationTypeConstants(t *testing.T) {
	cases := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"TRANSFER_DATA", OperationTypeTransferData, 0},
		{"RETRIEVE_DATA", OperationTypeRetrieveData, 1},
		{"ACK_DATA", OperationTypeAckData, 2},
		{"RESTART_HYDRATION", OperationTypeRestartHydration, 3},
		{"TRANSFER_PLACEHOLDERS", OperationTypeTransferPlaceholders, 4},
		{"ACK_DEHYDRATE", OperationTypeAckDehydrate, 5},
		{"ACK_DELETE", OperationTypeAckDelete, 6},
		{"ACK_RENAME", OperationTypeAckRename, 7},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("OperationType %s = %d, эталон %d", c.name, c.got, c.want)
		}
	}
}

// TestFlagConstants сверяет флаги и перечисления с эталоном layout_dump.
func TestFlagConstants(t *testing.T) {
	cases := []struct {
		name string
		got  uint32
		want uint32
	}{
		{"CF_REGISTER_FLAG_UPDATE", RegisterFlagUpdate, 1},
		{"CF_REGISTER_FLAG_DISABLE_ON_DEMAND_POPULATION_ON_ROOT", RegisterFlagDisableOnDemandPopulationOnRoot, 2},
		{"CF_REGISTER_FLAG_MARK_IN_SYNC_ON_ROOT", RegisterFlagMarkInSyncOnRoot, 4},
		{"CF_CONNECT_FLAG_REQUIRE_PROCESS_INFO", ConnectFlagRequireProcessInfo, 2},
		{"CF_CONNECT_FLAG_REQUIRE_FULL_FILE_PATH", ConnectFlagRequireFullFilePath, 4},
		{"CF_CONNECT_FLAG_BLOCK_SELF_IMPLICIT_HYDRATION", ConnectFlagBlockSelfImplicitHydration, 8},
		{"CF_CALLBACK_CANCEL_FLAG_IO_TIMEOUT", CallbackCancelFlagIOTimeout, 1},
		{"CF_CALLBACK_CANCEL_FLAG_IO_ABORTED", CallbackCancelFlagIOAborted, 2},
		{"CF_CALLBACK_FETCH_DATA_FLAG_RECOVERY", CallbackFetchDataFlagRecovery, 1},
		{"CF_CALLBACK_FETCH_DATA_FLAG_EXPLICIT_HYDRATION", CallbackFetchDataFlagExplicitHydration, 2},
		{"CF_CALLBACK_VALIDATE_DATA_FLAG_EXPLICIT_HYDRATION", CallbackValidateDataFlagExplicitHydration, 2},
		{"CF_CALLBACK_CLOSE_COMPLETION_FLAG_DELETED", CallbackCloseCompletionFlagDeleted, 1},
		{"CF_CALLBACK_DEHYDRATE_FLAG_BACKGROUND", CallbackDehydrateFlagBackground, 1},
		{"CF_CALLBACK_RENAME_FLAG_IS_DIRECTORY", CallbackRenameFlagIsDirectory, 1},
		{"CF_CALLBACK_RENAME_FLAG_SOURCE_IN_SCOPE", CallbackRenameFlagSourceInScope, 2},
		{"CF_CALLBACK_RENAME_FLAG_TARGET_IN_SCOPE", CallbackRenameFlagTargetInScope, 4},
		{"CF_CALLBACK_DELETE_FLAG_IS_DIRECTORY", CallbackDeleteFlagIsDirectory, 1},
		{"CF_CALLBACK_DELETE_FLAG_IS_UNDELETE", CallbackDeleteFlagIsUndelete, 2},
		{"CF_CALLBACK_DEHYDRATION_REASON_USER_MANUAL", CallbackDehydrationReasonUserManual, 1},
		{"CF_PLACEHOLDER_CREATE_FLAG_DISABLE_ON_DEMAND_POPULATION", PlaceholderCreateFlagDisableOnDemandPopulation, 1},
		{"CF_PLACEHOLDER_CREATE_FLAG_MARK_IN_SYNC", PlaceholderCreateFlagMarkInSync, 2},
		{"CF_PLACEHOLDER_CREATE_FLAG_SUPERSEDE", PlaceholderCreateFlagSupersede, 4},
		{"CF_PLACEHOLDER_CREATE_FLAG_ALWAYS_FULL", PlaceholderCreateFlagAlwaysFull, 8},
		{"CF_CREATE_FLAG_STOP_ON_ERROR", CreateFlagStopOnError, 1},
		{"CF_OPEN_FILE_FLAG_EXCLUSIVE", OpenFileFlagExclusive, 1},
		{"CF_OPEN_FILE_FLAG_WRITE_ACCESS", OpenFileFlagWriteAccess, 2},
		{"CF_OPEN_FILE_FLAG_DELETE_ACCESS", OpenFileFlagDeleteAccess, 4},
		{"CF_OPEN_FILE_FLAG_FOREGROUND", OpenFileFlagForeground, 8},
		{"CF_CONVERT_FLAG_MARK_IN_SYNC", ConvertFlagMarkInSync, 1},
		{"CF_CONVERT_FLAG_DEHYDRATE", ConvertFlagDehydrate, 2},
		{"CF_CONVERT_FLAG_ENABLE_ON_DEMAND_POPULATION", ConvertFlagEnableOnDemandPopulation, 4},
		{"CF_CONVERT_FLAG_ALWAYS_FULL", ConvertFlagAlwaysFull, 8},
		{"CF_CONVERT_FLAG_FORCE_CONVERT_TO_CLOUD_FILE", ConvertFlagForceConvertToCloudFile, 16},
		{"CF_UPDATE_FLAG_VERIFY_IN_SYNC", UpdateFlagVerifyInSync, 1},
		{"CF_UPDATE_FLAG_MARK_IN_SYNC", UpdateFlagMarkInSync, 2},
		{"CF_UPDATE_FLAG_DEHYDRATE", UpdateFlagDehydrate, 4},
		{"CF_UPDATE_FLAG_ENABLE_ON_DEMAND_POPULATION", UpdateFlagEnableOnDemandPopulation, 8},
		{"CF_UPDATE_FLAG_DISABLE_ON_DEMAND_POPULATION", UpdateFlagDisableOnDemandPopulation, 16},
		{"CF_UPDATE_FLAG_REMOVE_FILE_IDENTITY", UpdateFlagRemoveFileIdentity, 32},
		{"CF_UPDATE_FLAG_CLEAR_IN_SYNC", UpdateFlagClearInSync, 64},
		{"CF_UPDATE_FLAG_REMOVE_PROPERTY", UpdateFlagRemoveProperty, 128},
		{"CF_UPDATE_FLAG_PASSTHROUGH_FS_METADATA", UpdateFlagPassthroughFsMetadata, 256},
		{"CF_UPDATE_FLAG_ALWAYS_FULL", UpdateFlagAlwaysFull, 512},
		{"CF_UPDATE_FLAG_ALLOW_PARTIAL", UpdateFlagAllowPartial, 1024},
		{"CF_DEHYDRATE_FLAG_BACKGROUND", DehydrateFlagBackground, 1},
		{"CF_SET_PIN_FLAG_RECURSE", SetPinFlagRecurse, 1},
		{"CF_SET_PIN_FLAG_RECURSE_ONLY", SetPinFlagRecurseOnly, 2},
		{"CF_SET_PIN_FLAG_RECURSE_STOP_ON_ERROR", SetPinFlagRecurseStopOnError, 4},
		{"CF_PIN_STATE_PINNED", PinStatePinned, 1},
		{"CF_PIN_STATE_UNPINNED", PinStateUnpinned, 2},
		{"CF_PIN_STATE_EXCLUDED", PinStateExcluded, 3},
		{"CF_PIN_STATE_INHERIT", PinStateInherit, 4},
		{"CF_IN_SYNC_STATE_IN_SYNC", InSyncStateInSync, 1},
		{"CF_PLACEHOLDER_STATE_PLACEHOLDER", PlaceholderStatePlaceholder, 1},
		{"CF_PLACEHOLDER_STATE_SYNC_ROOT", PlaceholderStateSyncRoot, 2},
		{"CF_PLACEHOLDER_STATE_ESSENTIAL_PROP_PRESENT", PlaceholderStateEssentialPropPresent, 4},
		{"CF_PLACEHOLDER_STATE_IN_SYNC", PlaceholderStateInSync, 8},
		{"CF_PLACEHOLDER_STATE_PARTIAL", PlaceholderStatePartial, 16},
		{"CF_PLACEHOLDER_STATE_PARTIALLY_ON_DISK", PlaceholderStatePartiallyOnDisk, 32},
		{"CF_PLACEHOLDER_STATE_INVALID", PlaceholderStateInvalid, 0xffffffff},
		{"CF_INSYNC_POLICY_TRACK_ALL", InSyncPolicyTrackAll, 0x00ffffff},
		{"CF_INSYNC_POLICY_PRESERVE_INSYNC_FOR_SYNC_ENGINE", InSyncPolicyPreserveInSyncForSyncEngine, 0x80000000},
		{"CF_HARDLINK_POLICY_ALLOWED", HardlinkPolicyAllowed, 1},
		{"CF_PLACEHOLDER_MANAGEMENT_POLICY_CREATE_UNRESTRICTED", PlaceholderManagementPolicyCreateUnrestricted, 1},
		{"CF_PLACEHOLDER_MANAGEMENT_POLICY_CONVERT_TO_UNRESTRICTED", PlaceholderManagementPolicyConvertToUnrestricted, 2},
		{"CF_PLACEHOLDER_MANAGEMENT_POLICY_UPDATE_UNRESTRICTED", PlaceholderManagementPolicyUpdateUnrestricted, 4},
		{"CF_PROVIDER_STATUS_TERMINATED", ProviderStatusTerminated, 0xC0000001},
		{"CF_PROVIDER_STATUS_ERROR", ProviderStatusError, 0xC0000002},
		{"CF_OPERATION_RESTART_HYDRATION_FLAG_MARK_IN_SYNC", OperationRestartHydrationFlagMarkInSync, 1},
		{"CF_OPERATION_TRANSFER_PLACEHOLDERS_FLAG_STOP_ON_ERROR", OperationTransferPlaceholdersFlagStopOnError, 1},
		{"CF_OPERATION_TRANSFER_PLACEHOLDERS_FLAG_DISABLE_ON_DEMAND_POPULATION", OperationTransferPlaceholdersFlagDisableOnDemandPopulation, 2},
		{"CF_PLACEHOLDER_INFO_STANDARD", PlaceholderInfoStandard, 1},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, эталон %#x", c.name, c.got, c.want)
		}
	}
}
