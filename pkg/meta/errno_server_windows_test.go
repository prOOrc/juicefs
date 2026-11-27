//go:build windows
// +build windows

package meta

import (
	"syscall"
	"testing"
)

// TestServerErrno — Linux-номера сервера мапятся в платформенные константы:
// живой кейс 27.11 (facility-маунт): серверный EEXIST(17) на Windows
// печатался как Win32 «different disk drive» и не совпадал с syscall.EEXIST
// (536870942) — повторная выгрузка существующего файла не завершалась.
func TestServerErrno(t *testing.T) {
	if serverErrno(17) != syscall.EEXIST {
		t.Fatalf("serverErrno(17) = %v, want syscall.EEXIST", serverErrno(17))
	}
	if serverErrno(5) != syscall.EIO {
		t.Fatalf("serverErrno(5) = %v, want syscall.EIO", serverErrno(5))
	}
	if serverErrno(13) != syscall.EACCES {
		t.Fatalf("serverErrno(13) = %v, want syscall.EACCES", serverErrno(13))
	}
	if serverErrno(39) != syscall.ENOTEMPTY {
		t.Fatalf("serverErrno(39) = %v, want syscall.ENOTEMPTY", serverErrno(39))
	}
	if serverErrno(2) != syscall.ENOENT {
		t.Fatalf("serverErrno(2) = %v, want syscall.ENOENT", serverErrno(2))
	}
	// Неизвестный номер проходит как есть; 0 — успех.
	if serverErrno(133) != syscall.Errno(133) {
		t.Fatal("unknown server errno must pass through unchanged")
	}
	if serverErrno(0) != 0 {
		t.Fatal("serverErrno(0) must be success")
	}
}
