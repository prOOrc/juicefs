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

package cfprovider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIndexPutPathReload — базовый контракт: Put/Path, сохранение в журнал и
// загрузка при повторном открытии (спека «Перезапуск без дублей» опирается
// на индекс, переживший перезапуск).
func TestIndexPutPathReload(t *testing.T) {
	dir := t.TempDir()
	ix, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if err := ix.Put(42, "/docs/report.pdf"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := ix.Put(7, "/"); err != nil {
		t.Fatalf("Put root: %v", err)
	}
	if p, ok := ix.Path(42); !ok || p != "/docs/report.pdf" {
		t.Errorf("Path(42) = (%q, %v), ожидается /docs/report.pdf", p, ok)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Повторное открытие — записи восстановлены из журнала.
	ix2, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("повторный NewIndex: %v", err)
	}
	defer ix2.Close()
	for _, tc := range []struct {
		ino  uint64
		want string
	}{{42, "/docs/report.pdf"}, {7, "/"}} {
		if p, ok := ix2.Path(tc.ino); !ok || p != tc.want {
			t.Errorf("после перезагрузки Path(%d) = (%q, %v), ожидается %q", tc.ino, p, ok, tc.want)
		}
	}
	// Новая запись дописывается в существующий журнал без затирания.
	if err := ix2.Put(99, "/a/b"); err != nil {
		t.Fatalf("Put после перезагрузки: %v", err)
	}
	ix3, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("третий NewIndex: %v", err)
	}
	defer ix3.Close()
	if _, ok := ix3.Path(42); !ok {
		t.Error("третий NewIndex потерял запись 42")
	}
	if _, ok := ix3.Path(99); !ok {
		t.Error("третий NewIndex потерял запись 99")
	}
}

// TestIndexLoadSkipsCorruptTail — журнал после падения может содержать
// обрезанную последнюю строку: загрузка пропускает её и восстанавливает
// предыдущие записи.
func TestIndexLoadSkipsCorruptTail(t *testing.T) {
	dir := t.TempDir()
	jPath := filepath.Join(dir, indexJournalName)
	content := `{"ino":10,"path":"/a"}
{"ino":11,"path":"/b/c"
{"ino":12,"path":"/d"}
not-json
`
	if err := os.WriteFile(jPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ix, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex с битым журналом: %v", err)
	}
	defer ix.Close()
	if p, ok := ix.Path(10); !ok || p != "/a" {
		t.Errorf("Path(10) = (%q, %v), ожидается /a", p, ok)
	}
	if _, ok := ix.Path(11); ok {
		t.Error("обрезанная строка не должна была загрузиться (ino 11)")
	}
	if p, ok := ix.Path(12); !ok || p != "/d" {
		t.Errorf("Path(12) = (%q, %v), ожидается /d", p, ok)
	}
	if _, ok := ix.Path(13); ok {
		t.Error("мусорная строка не должна была загрузиться")
	}
}

// TestIndexResolveInoFallback — ResolveIno: карта → resolver (подъём по
// родителям, U12); найденное кешируется и попадает в журнал.
func TestIndexResolveInoFallback(t *testing.T) {
	dir := t.TempDir()
	ix, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer ix.Close()

	// Без resolver'а промах — не успех.
	if _, ok := ix.ResolveIno(context.Background(), 5); ok {
		t.Error("ResolveIno без resolver не должен находить неизвестный inode")
	}

	calls := 0
	ix.SetResolver(func(ctx context.Context, ino uint64) (string, bool) {
		calls++
		if ino == 5 {
			return "/deep/nested/entry", true
		}
		return "", false
	})
	p, ok := ix.ResolveIno(context.Background(), 5)
	if !ok || p != "/deep/nested/entry" {
		t.Fatalf("ResolveIno(5) = (%q, %v), ожидается /deep/nested/entry", p, ok)
	}
	if calls != 1 {
		t.Errorf("resolver вызван %d раз, ожидается 1", calls)
	}
	// Повтор — из карты, resolver не вызывается.
	if _, ok := ix.ResolveIno(context.Background(), 5); !ok {
		t.Fatal("закешированный промах")
	}
	if calls != 1 {
		t.Errorf("повторный ResolveIno снова пошёл в resolver: %d вызовов", calls)
	}
	// Закешированный путь переживает перезагрузку журнала.
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ix2, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("перезагрузка: %v", err)
	}
	defer ix2.Close()
	if p, ok := ix2.Path(5); !ok || p != "/deep/nested/entry" {
		t.Errorf("после перезагрузки Path(5) = (%q, %v)", p, ok)
	}
}

// TestIndexJournalFormat — журнал JSONL: каждая строка самостоятельная
// запись с полями ino/path.
func TestIndexJournalFormat(t *testing.T) {
	dir := t.TempDir()
	ix, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if err := ix.Put(1, "/x"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, indexJournalName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("в журнале %d строк, ожидается 1: %q", len(lines), string(b))
	}
	if !strings.Contains(lines[0], `"ino":1`) || !strings.Contains(lines[0], `"path":"/x"`) {
		t.Errorf("строка журнала не содержит поля ino/path: %q", lines[0])
	}
}
