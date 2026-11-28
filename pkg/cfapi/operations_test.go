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

// Тесты обёрток операций плейсхолдеров (fix-cfapi-followups A4, задача 4.1):
// негативный сценарий RevertPlaceholderByPath на обычном файле — сентинел
// «не плейсхолдер», файл остаётся цел (реальный cldapi.dll, без
// зарегистрированного sync root).

package cfapi

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRevertPlaceholderByPathPlainFile — revert обычного файла вне sync root
// обязан отказать с определённым ответом «не плейсхолдер» (сентинел
// ERROR_NOT_A_CLOUD_FILE) и оставить файл нетронутым (содержимое и метка).
func TestRevertPlaceholderByPathPlainFile(t *testing.T) {
	if err := Available(); err != nil {
		t.Fatalf("cldapi.dll is not available: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.txt")
	const content = "user data must survive a rejected revert"
	if err := os.WriteFile(path, []byte(content), 0o666); err != nil {
		t.Fatalf("write plain file: %v", err)
	}

	err := RevertPlaceholderByPath(path, RevertFlagNone)
	if err == nil {
		t.Fatal("revert обычного файла прошёл, ожидается отказ")
	}
	if !IsNotAPlaceholder(err) {
		t.Fatalf("ошибка %v не распознана как «не плейсхолдер» (ожидается сентинел ERROR_NOT_A_CLOUD_FILE)", err)
	}

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read back after rejected revert: %v", rerr)
	}
	if string(got) != content {
		t.Fatalf("содержимое изменилось после отказа revert: %q", got)
	}
}
