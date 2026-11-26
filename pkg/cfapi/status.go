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

import "syscall"

// NTSTATUS-коды для CompletionStatus в CF_OPERATION_PARAMETERS и для ответов
// провайдера. cfapi.h их не определяет (кроме CF_PROVIDER_STATUS_TERMINATED/
// ERROR выше); значения сверены с Windows SDK 10.0.26100.0,
// shared/ntstatus.h — той же версией SDK, что и эталонный cfapi.h
// (файл ntstatus.h в репозиторий не коммитится; см. design.md D1).

const (
	StatusSuccess               NTStatus = 0x00000000 // STATUS_SUCCESS
	StatusPending               NTStatus = 0x00000103 // STATUS_PENDING
	StatusEndOfFile             NTStatus = 0xC0000011 // STATUS_END_OF_FILE
	StatusAccessDenied          NTStatus = 0xC0000022 // STATUS_ACCESS_DENIED
	StatusObjectNameNotFound    NTStatus = 0xC0000034 // STATUS_OBJECT_NAME_NOT_FOUND
	StatusSharingViolation      NTStatus = 0xC0000043 // STATUS_SHARING_VIOLATION
	StatusDiskFull              NTStatus = 0xC000007F // STATUS_DISK_FULL
	StatusInsufficientResources NTStatus = 0xC000009A // STATUS_INSUFFICIENT_RESOURCES
	StatusCancelled             NTStatus = 0xC0000120 // STATUS_CANCELLED
	StatusInternalError         NTStatus = 0xC00000E5 // STATUS_INTERNAL_ERROR
	StatusIODeviceError         NTStatus = 0xC0000185 // STATUS_IO_DEVICE_ERROR

	// Диапазон Cloud Files (0xC000CF00..). Ответ на отменённую гидратацию —
	// STATUS_CLOUD_FILE_REQUEST_CANCELED; проба U13 (этап 3) уточнит семантику.
	StatusCloudFileProviderNotRunning    NTStatus = 0xC000CF01 // STATUS_CLOUD_FILE_PROVIDER_NOT_RUNNING
	StatusCloudFileNotInSync             NTStatus = 0xC000CF08 // STATUS_CLOUD_FILE_NOT_IN_SYNC
	StatusCloudFileInvalidRequest        NTStatus = 0xC000CF0B // STATUS_CLOUD_FILE_INVALID_REQUEST
	StatusCloudFileInsufficientResources NTStatus = 0xC000CF10 // STATUS_CLOUD_FILE_INSUFFICIENT_RESOURCES
	StatusCloudFileNetworkUnavailable    NTStatus = 0xC000CF11 // STATUS_CLOUD_FILE_NETWORK_UNAVAILABLE
	StatusCloudFileUnsuccessful          NTStatus = 0xC000CF12 // STATUS_CLOUD_FILE_UNSUCCESSFUL
	StatusCloudFileNotUnderSyncRoot      NTStatus = 0xC000CF13 // STATUS_CLOUD_FILE_NOT_UNDER_SYNC_ROOT
	StatusCloudFileInUse                 NTStatus = 0xC000CF14 // STATUS_CLOUD_FILE_IN_USE
	StatusCloudFilePinned                NTStatus = 0xC000CF15 // STATUS_CLOUD_FILE_PINNED
	StatusCloudFileRequestAborted        NTStatus = 0xC000CF16 // STATUS_CLOUD_FILE_REQUEST_ABORTED
	StatusCloudFileAccessDenied          NTStatus = 0xC000CF18 // STATUS_CLOUD_FILE_ACCESS_DENIED
	StatusCloudFileRequestCanceled       NTStatus = 0xC000CF1B // STATUS_CLOUD_FILE_REQUEST_CANCELED
	StatusCloudFileProviderTerminated    NTStatus = 0xC000CF1D // STATUS_CLOUD_FILE_PROVIDER_TERMINATED
)

// StatusFromErrno — грубое преобразование errno движка (pkg/fs, syscall.Errno)
// в NTSTATUS для ответа TRANSFER_DATA. Полная таблица не нужна: провайдер
// обязан ответить чем-то корректным по коду; узнаваемые errno переводятся,
// остальное — STATUS_INTERNAL_ERROR (design.md, шаг 3).
func StatusFromErrno(errno syscall.Errno) NTStatus {
	switch errno {
	case syscall.EACCES, syscall.EPERM:
		return StatusAccessDenied
	case syscall.ENOENT:
		return StatusObjectNameNotFound
	case syscall.EBUSY:
		return StatusSharingViolation
	case syscall.ENOSPC:
		return StatusDiskFull
	case syscall.ENOMEM:
		return StatusInsufficientResources
	case syscall.EINTR:
		return StatusCancelled
	case syscall.EIO:
		return StatusIODeviceError
	default:
		return StatusInternalError
	}
}
