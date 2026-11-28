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

// Идентичность STANDARD-записи (fix-cfapi-followups A1, U-F0): аксессор
// хвоста FileIdentity переменной длины и именованный предикат «не
// плейсхолдер» STANDARD-запроса. Идентичность провайдера — inode записи
// backend, 8 байт LE (design.md D6, createEntry/makeCreateInfo); FileId —
// платформенный идентификатор и для сверки не используется.
package cfapi

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hresultFromWin32 — HRESULT_FROM_WIN32 (winerror.h): Win32-код в облике
// HRESULT (0x80070000 | код).
func hresultFromWin32(code uint32) HRESULT {
	return HRESULT(int32(0x80070000 | code))
}

// errNotAPlaceholderCode — HRESULT_FROM_WIN32(ERROR_NOT_A_CLOUD_FILE):
// определённый ответ CfGetPlaceholderInfo о записи без плейсхолдерности
// (обычный файл пользователя). Сентинел классификации A1: у плейсхолдера
// STANDARD-запрос обязан проходить; этот код — единственный неуспех,
// трактуемый как ответ «не плейсхолдер», прочие — ошибка запроса.
var errNotAPlaceholderCode = hresultFromWin32(uint32(windows.ERROR_NOT_A_CLOUD_FILE))

// IsNotAPlaceholder — ошибка STANDARD-запроса означает «запись не является
// плейсхолдером» (CfGetPlaceholderInfo вернул ERROR_NOT_A_CLOUD_FILE).
// Прочие неуспехи (занятость записи, права, исчезновение пути) — false:
// определённого ответа нет.
func IsNotAPlaceholder(err error) bool {
	var hr *HRESULTError
	if errors.As(err, &hr) {
		return hr.Code == errNotAPlaceholderCode
	}
	return false
}

// FileIdentityBytes — провайдерская идентичность STANDARD-записи: хвост
// переменной длины за структурой (CF_PLACEHOLDER_STANDARD_INFO.FileIdentity,
// cfapi.h:1013-1014) — буфер ответа продолжается за FileIdentity[1]
// (PlaceholderStandardInfoByPath читает в один буфер с запасом 4KiB).
// Нулевая длина и длина сверх CF_PLACEHOLDER_MAX_FILE_IDENTITY_LENGTH —
// пустой срез: такое поле фильтр не пишет, чтение без проверки могло бы
// выйти за буфер.
func (i *CFPlaceholderStandardInfo) FileIdentityBytes() []byte {
	if i.FileIdentityLength == 0 || i.FileIdentityLength > PlaceholderMaxFileIdentityLen {
		return nil
	}
	return unsafe.Slice(&i.FileIdentity[0], int(i.FileIdentityLength))
}

// InodeIdentity — идентичность записи как inode (uint64 LE — та же
// сериализация, что в createEntry/makeCreateInfo, design.md D6): первые
// 8 байт FileIdentityBytes (декодер leUint64 — общий с fileIDOf).
// Идентичность короче 8 байт — 0.
func (i *CFPlaceholderStandardInfo) InodeIdentity() uint64 {
	b := i.FileIdentityBytes()
	if len(b) < 8 {
		return 0
	}
	return leUint64(&b[0])
}

// StandardInfoOfPath — CF_PLACEHOLDER_STANDARD_INFO по пути с определённым
// ответом «не плейсхолдер»: «не плейсхолдер» → (nil, false, nil) по
// сентинелу ERROR_NOT_A_CLOUD_FILE, прочие неуспехи — err. Директорийность
// открытия — по os.Lstat (метаданные, без гидрации): файлы — режимы
// openPlaceholderHandle, подтверждённые живой диагностикой (файлы БЕЗ
// FILE_FLAG_BACKUP_SEMANTICS — convert/update на файловом хендле с ним
// фильтр отвергает; каталоги — BACKUP_SEMANTICS обязателен).
func StandardInfoOfPath(path string) (*CFPlaceholderStandardInfo, bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, false, err
	}
	info, err := PlaceholderStandardInfoByPath(path, fi.IsDir())
	if err != nil {
		if IsNotAPlaceholder(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return info, true, nil
}
