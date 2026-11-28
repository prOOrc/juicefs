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

// Тесты аксессора идентичности STANDARD-записи и предиката «не плейсхолдер»
// (fix-cfapi-followups A1, задача 1.1) — без cldapi.dll.

package cfapi

import (
	"encoding/binary"
	"errors"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stdInfoWithIdentity — STANDARD-запись с хвостом identity в одном буфере
// (модель ответа CfGetPlaceholderInfo: структура и хвост — одна память).
func stdInfoWithIdentity(identity []byte) *CFPlaceholderStandardInfo {
	buf := make([]byte, unsafe.Sizeof(CFPlaceholderStandardInfo{})+uintptr(len(identity)))
	info := (*CFPlaceholderStandardInfo)(unsafe.Pointer(&buf[0]))
	info.FileIdentityLength = uint32(len(identity))
	tail := unsafe.Slice(&info.FileIdentity[0], len(identity))
	copy(tail, identity)
	return info
}

// TestFileIdentityBytes — аксессор отдаёт ровно FileIdentityLength байт
// хвоста; нулевая и сверхмаксимальная длина — пустой срез (чтение без
// проверки вышло бы за буфер).
func TestFileIdentityBytes(t *testing.T) {
	ino := uint64(0x1122334455667788)
	identity := make([]byte, 8)
	binary.LittleEndian.PutUint64(identity, ino)
	info := stdInfoWithIdentity(identity)
	if got := info.FileIdentityBytes(); len(got) != 8 || binary.LittleEndian.Uint64(got) != ino {
		t.Errorf("FileIdentityBytes = %v (len %d), ожидается 8 байт inode %#x", got, len(got), ino)
	}
	if got := info.InodeIdentity(); got != ino {
		t.Errorf("InodeIdentity = %#x, ожидается %#x", got, ino)
	}

	// Короткая идентичность (≠8) — bytes отдаются, inode — 0 (foreign).
	short := stdInfoWithIdentity([]byte{1, 2, 3, 4})
	if got := short.FileIdentityBytes(); len(got) != 4 {
		t.Errorf("короткая идентичность: len %d, ожидается 4", len(got))
	}
	if got := short.InodeIdentity(); got != 0 {
		t.Errorf("InodeIdentity короткой = %#x, ожидается 0", got)
	}

	// Нулевая длина — пустой срез.
	empty := stdInfoWithIdentity(nil)
	if got := empty.FileIdentityBytes(); len(got) != 0 {
		t.Errorf("нулевая длина: len %d, ожидается 0", len(got))
	}
	// Длина сверх CF_PLACEHOLDER_MAX_FILE_IDENTITY_LENGTH — пустой срез.
	huge := &CFPlaceholderStandardInfo{FileIdentityLength: PlaceholderMaxFileIdentityLen + 1}
	if got := huge.FileIdentityBytes(); got != nil {
		t.Errorf("сверхмаксимальная длина: %d байт, ожидается nil", len(got))
	}
}

// TestIsNotAPlaceholder — предикат распознаёт только сентинел
// ERROR_NOT_A_CLOUD_FILE (HRESULT_FROM_WIN32); прочие HRESULT и обычные
// ошибки — false.
func TestIsNotAPlaceholder(t *testing.T) {
	if got := uint32(errNotAPlaceholderCode); got != uint32(hresultFromWin32(uint32(windows.ERROR_NOT_A_CLOUD_FILE))) || got != 0x80070178 {
		t.Errorf("сентинел = 0x%08x, ожидается HRESULT_FROM_WIN32(ERROR_NOT_A_CLOUD_FILE) = 0x80070178", got)
	}
	if !IsNotAPlaceholder(&HRESULTError{Call: "CfGetPlaceholderInfo", Code: errNotAPlaceholderCode}) {
		t.Error("сентинел не распознан как «не плейсхолдер»")
	}
	if IsNotAPlaceholder(&HRESULTError{Call: "CfGetPlaceholderInfo", Code: hresultFromWin32(uint32(windows.ERROR_SHARING_VIOLATION))}) {
		t.Error("sharing violation распознан как «не плейсхолдер»")
	}
	if IsNotAPlaceholder(syscall.ENOENT) {
		t.Error("syscall-ошибка распознана как «не плейсхолдер»")
	}
	if IsNotAPlaceholder(errors.New("plain")) {
		t.Error("обычная ошибка распознана как «не плейсхолдер»")
	}
	if IsNotAPlaceholder(nil) {
		t.Error("nil распознан как «не плейсхолдер»")
	}
}
