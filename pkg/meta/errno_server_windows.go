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

package meta

import "syscall"

// serverErrno переводит номер errno СЕРВЕРА (Linux-нумерация по проводу:
// EEXIST=17, EIO=5, EACCES=13, ENOTEMPTY=39...) в константу syscall этой
// платформы. На Windows syscall.EEXIST/EIO/EACCES/... — большие
// APPLICATION_ERROR-значения (5368709xx), а Errno(17) печатается как Win32
// ERROR_NOT_SAME_DEVICE («different disk drive»): сравнения «в лоб» с
// syscall.EEXIST не совпадали никогда — живой кейс 27.11 (facility-маунт
// AgioDrive9): EEXIST сервера на повторном Create уходил в журнал как
// транзиентная «экзотика», выгрузка существующего файла не завершалась.
// ENOENT(2) совпадал численно случайно; Errno(5) (серверный EIO) печатался
// как «Access is denied» (Win32 5). ВинФСП-мост (pkg/winfsp errorconv) и все
// потребители пакета получают теперь честные константы. На других ОС —
// identity (errno_server_other.go).
func serverErrno(no uint32) syscall.Errno {
	switch syscall.Errno(no) {
	case 1:
		return syscall.EPERM
	case 2:
		return syscall.ENOENT
	case 5:
		return syscall.EIO
	case 9:
		return syscall.EBADF
	case 12:
		return syscall.ENOMEM
	case 13:
		return syscall.EACCES
	case 16:
		return syscall.EBUSY
	case 17:
		return syscall.EEXIST
	case 18:
		return syscall.EXDEV
	case 20:
		return syscall.ENOTDIR
	case 21:
		return syscall.EISDIR
	case 22:
		return syscall.EINVAL
	case 28:
		return syscall.ENOSPC
	case 30:
		return syscall.EROFS
	case 31:
		return syscall.EMLINK
	case 36:
		return syscall.ENAMETOOLONG
	case 39:
		return syscall.ENOTEMPTY
	case 122:
		return syscall.EDQUOT
	}
	return syscall.Errno(no)
}
