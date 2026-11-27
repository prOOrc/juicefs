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

// Тесты сверки при старте (design.md D9, задача 6.4; спека «Сверка при
// старте», сценарий «Восстановление после сбоя»): плейсхолдеры вне in-sync
// и локальные файлы без записи backend → очередь выгрузки; записи индекса
// без локального файла в обработанных каталогах → очередь удаления backend;
// конфликты (локальные не-плейсхолдеры с именем hub) не трогаются;
// --read-only отключает сверку; walk не воскрешает известные удалённые
// записи (гард index-known). cldapi.dll не вызывается.
package cfprovider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// newReconcileProvider — провайдер сверки: журнал подключён, коалисера нет
// (queueUpload уходит в журнал — replayer'у), часы реальные не нужны.
func newReconcileProvider(t *testing.T) (*Provider, *fakeIngestBackend, *fakeIngestStore, *fakeStore, *Index, *Journal, *opLog) {
	t.Helper()
	p, backend, store, fstore, idx, log := newIngestTestProvider(t)
	j, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	p.journal = j
	return p, backend, store, fstore, idx, j, log
}

// TestReconcileOutOfSyncPlaceholderQueued — изменённый плейсхолдер вне
// in-sync (выгрузка не завершилась — сбой после close) ставится в очередь
// выгрузки; backend не трогается сверткой.
func TestReconcileOutOfSyncPlaceholderQueued(t *testing.T) {
	p, backend, store, _, _, j, _ := newReconcileProvider(t)
	local := filepath.Join(p.root, "doc.txt")
	if err := os.WriteFile(local, []byte("changed"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	if err := p.idx.Put(42, "/doc.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	store.setStandard(local, cfapi.InSyncStateNotInSync) // вне in-sync
	entries := []BackendEntry{{Name: "doc.txt", Inode: 42, Size: 7, MtimeSec: 1}}

	res := populateDirectory(context.Background(), p, "/", entries)

	if res.skipped != 1 || res.createdFiles != 0 {
		t.Fatalf("skipped=%d created=%d, ожидается 1/0 (existing)", res.skipped, res.createdFiles)
	}
	if n := backend.creates; n != 0 {
		t.Errorf("сверка тронула backend: creates=%d", n)
	}
	ops := j.Ops()
	if len(ops) != 1 || ops[0].Type != OpUpload || ops[0].Path != "/doc.txt" {
		t.Fatalf("очередь = %+v, ожидается upload /doc.txt", ops)
	}
}

// TestReconcileInSyncSkipped — плейсхолдер in-sync (норма) не ставится в
// очередь.
func TestReconcileInSyncSkipped(t *testing.T) {
	p, backend, store, _, _, j, _ := newReconcileProvider(t)
	local := filepath.Join(p.root, "synced.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	if err := p.idx.Put(9, "/synced.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	store.setStandard(local, cfapi.InSyncStateInSync)
	entries := []BackendEntry{{Name: "synced.txt", Inode: 9, Size: 1, MtimeSec: 1}}

	populateDirectory(context.Background(), p, "/", entries)

	if n := len(j.Ops()); n != 0 {
		t.Errorf("очередь = %+v, ожидается пусто (in sync)", j.Ops())
	}
	if n := backend.creates; n != 0 {
		t.Errorf("creates = %d, ожидается 0", n)
	}
}

// TestReconcileMissingIndexEntryDeleted — сценарий «Восстановление после
// сбоя»: удалённый плейсхолдер (индекс знает inode, локального файла нет)
// в наполненном каталоге: walk НЕ воскрешает запись (гард index-known),
// сверка ставит удаление backend; исполнение удаляет запись backend и
// снимает индекс.
func TestReconcileMissingIndexEntryDeleted(t *testing.T) {
	p, backend, _, fstore, idx, j, _ := newReconcileProvider(t)
	// Запись наша (индекс знает inode); hub-запись есть (сбой после
	// локального удаления, до backend-удаления).
	if err := idx.Put(7, "/gone.bin"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	entries := []BackendEntry{{Name: "gone.bin", Inode: 7, Size: 3, MtimeSec: 1}}

	res := populateDirectory(context.Background(), p, "/", entries)

	if res.skipped != 1 || res.createdFiles != 0 {
		t.Fatalf("skipped=%d created=%d, ожидается 1/0 (гард index-known: не воскрешать)", res.skipped, res.createdFiles)
	}
	if n := len(fstore.convertCalls()); n != 0 {
		t.Errorf("конверсий = %d, ожидается 0 (создание запрещено)", n)
	}
	ops := j.Ops()
	if len(ops) != 1 || ops[0].Type != OpDelete || ops[0].Path != "/gone.bin" || ops[0].Ino != 7 {
		t.Fatalf("очередь = %+v, ожидается delete /gone.bin ino=7", ops)
	}
	// Исполнение очереди: backend-запись удалена, индекс чист.
	backend.mu.Lock()
	backend.files["/gone.bin"] = []byte("abc")
	backend.inodes["/gone.bin"] = 7
	backend.mu.Unlock()
	if err := p.executeJournalOp(context.Background(), ops[0]); err != nil {
		t.Fatalf("executeJournalOp: %v", err)
	}
	if len(backend.content("/gone.bin")) != 0 {
		t.Error("backend-запись не удалена")
	}
	if _, ok := idx.Path(7); ok {
		t.Error("индекс не почищен")
	}
}

// TestReconcilePlainFileQueued — локальный файл без плейсхолдера и без
// записи backend (потерянные события watcher'а) ставится в очередь
// выгрузки (спека «Сверка при старте»: «локальные файлы без плейсхолдера»).
func TestReconcilePlainFileQueued(t *testing.T) {
	p, backend, _, _, _, j, _ := newReconcileProvider(t)
	local := filepath.Join(p.root, "missed.txt")
	if err := os.WriteFile(local, []byte("never uploaded"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}

	populateDirectory(context.Background(), p, "/", nil)

	ops := j.Ops()
	if len(ops) != 1 || ops[0].Type != OpUpload || ops[0].Path != "/missed.txt" {
		t.Fatalf("очередь = %+v, ожидается upload /missed.txt", ops)
	}
	if n := backend.creates; n != 0 {
		t.Errorf("сверка тронула backend: creates=%d", n)
	}
}

// TestReconcileConflictNotTouched — локальный не-плейсхолдер с именем
// записи hub (конфликт): не выгружается, не удаляется, индекс не получает
// запись (инвариант «локальный файл пользователя не перезаписывается и не
// удаляется»).
func TestReconcileConflictNotTouched(t *testing.T) {
	p, backend, _, fstore, idx, j, _ := newReconcileProvider(t)
	local := filepath.Join(p.root, "user.txt")
	data := []byte("user data")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	entries := []BackendEntry{{Name: "user.txt", Inode: 5, Size: 9, MtimeSec: 1}}

	res := populateDirectory(context.Background(), p, "/", entries)

	if res.conflicts != 1 {
		t.Fatalf("conflicts = %d, ожидается 1", res.conflicts)
	}
	if n := len(j.Ops()); n != 0 {
		t.Errorf("очередь = %+v, ожидается пусто (конфликт не трогается)", j.Ops())
	}
	if n := backend.creates; n != 0 {
		t.Errorf("creates = %d, ожидается 0", n)
	}
	if n := len(fstore.convertCalls()); n != 0 {
		t.Errorf("конверсий = %d, ожидается 0", n)
	}
	if _, ok := idx.Path(5); ok {
		t.Error("конфликт попал в индекс")
	}
	if got, err := os.ReadFile(local); err != nil || string(got) != string(data) {
		t.Errorf("локальный файл изменён: %q, %v", got, err)
	}
}

// TestReconcileReadOnlyNoOps — --read-only: сверка не ставит ни выгрузку,
// ни удаление (спека «Режим без записи»).
func TestReconcileReadOnlyNoOps(t *testing.T) {
	p, _, store, _, _, j, _ := newReconcileProvider(t)
	p.readOnly = true
	local := filepath.Join(p.root, "ro.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	if err := p.idx.Put(3, "/ro.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	store.setStandard(local, cfapi.InSyncStateNotInSync)
	entries := []BackendEntry{{Name: "ro.txt", Inode: 3, Size: 1, MtimeSec: 1}}

	populateDirectory(context.Background(), p, "/", entries)

	if n := len(j.Ops()); n != 0 {
		t.Errorf("очередь = %+v, ожидается пусто (read-only)", j.Ops())
	}
}

// TestReconcileIndexTombstoneReload — надгробия индекса переживают
// перезапуск: Delete снимает запись, загрузка журнала её не воскрешает.
func TestReconcileIndexTombstoneReload(t *testing.T) {
	dir := t.TempDir()
	ix, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if err := ix.Put(42, "/keep.txt"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := ix.Put(43, "/gone.txt"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := ix.Delete(43); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	entries := ix.PathsInDir("/")
	if len(entries) != 1 || entries[0].Ino != 42 {
		t.Fatalf("PathsInDir = %+v, ожидается только ino 42", entries)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ix2, err := NewIndex(dir)
	if err != nil {
		t.Fatalf("повторный NewIndex: %v", err)
	}
	defer ix2.Close()
	if _, ok := ix2.Path(43); ok {
		t.Error("надгробие не пережило перезагрузку (запись 43 воскресла)")
	}
	if _, ok := ix2.Path(42); !ok {
		t.Error("живая запись 42 потеряна после перезагрузки")
	}
	if _, ok := ix2.LookupPath("/gone.txt"); ok {
		t.Error("byPath не почищен надгробием")
	}
}

// TestReconcileIndexRenamePrefixAndPathsInDir — RenamePrefix переносит
// каталог с потомками (прямой эффект — пути, обратный — byPath), PathsInDir
// выдаёт только ПРЯМЫХ детей каталога (сверка не удаляет записи вложенных
// каталогов по листингу родителя).
func TestReconcileIndexRenamePrefixAndPathsInDir(t *testing.T) {
	ix, err := NewIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer ix.Close()
	for _, tc := range []struct {
		ino  uint64
		path string
	}{{1, "/odir"}, {2, "/odir/kid.txt"}, {3, "/odir/sub/grand"}, {4, "/other.txt"}, {5, "/odirx/lookalike"}} {
		if err := ix.Put(tc.ino, tc.path); err != nil {
			t.Fatalf("Put %d: %v", tc.ino, err)
		}
	}
	if n, err := ix.RenamePrefix("/odir", "/ndir"); err != nil || n != 3 {
		t.Fatalf("RenamePrefix = (%d, %v), ожидается (3, nil)", n, err)
	}
	if got, _ := ix.Path(2); got != "/ndir/kid.txt" {
		t.Errorf("Path(2) = %q, ожидается /ndir/kid.txt", got)
	}
	if _, ok := ix.LookupPath("/odir/sub/grand"); ok {
		t.Error("byPath старого пути не перенесён")
	}
	if _, ok := ix.LookupPath("/ndir/sub/grand"); !ok {
		t.Error("byPath нового пути отсутствует")
	}
	if got, _ := ix.Path(5); got != "/odirx/lookalike" {
		t.Error("RenamePrefix зацепил путь-двойника по префиксу")
	}
	// Прямые дети корня: 4 (other.txt), 1 (ndir) — но не 2/3 (внуки) и не 5.
	direct := ix.PathsInDir("/")
	inos := map[uint64]bool{}
	for _, e := range direct {
		inos[e.Ino] = true
	}
	if !inos[4] || !inos[1] || inos[2] || inos[3] || inos[5] {
		t.Errorf("PathsInDir(/) = %+v, ожидается {1, 4}", direct)
	}
	// Прямые дети каталога.
	direct = ix.PathsInDir("/ndir")
	if len(direct) != 1 || direct[0].Ino != 2 {
		t.Errorf("PathsInDir(/ndir) = %+v, ожидается только ino 2", direct)
	}
	// Мини-проверка DeletePrefix: потомки снимаются, чужие остаются.
	if _, err := ix.DeletePrefix("/ndir"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if _, ok := ix.Path(2); ok {
		t.Error("потомок не снят DeletePrefix")
	}
	if _, ok := ix.Path(5); !ok {
		t.Error("DeletePrefix снял чужую запись")
	}
}

// TestReconcileDeleteQueueReplayedAfterRestart — журнал с очередью удаления
// переживает перезапуск: NewJournal в том же каталоге состояния загружает
// операцию, исполнение (ветка replayer'а) доводит до backend.
func TestReconcileDeleteQueueReplayedAfterRestart(t *testing.T) {
	p, backend, _, _, _, j, _ := newReconcileProvider(t)
	// Первый прогон: сверка поставила удаление (индекс знает запись,
	// локально пусто).
	if err := p.idx.Put(8, "/lost.bin"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	populateDirectory(context.Background(), p, "/", nil)
	ops := j.Ops()
	if len(ops) != 1 || ops[0].Type != OpDelete {
		t.Fatalf("очередь = %+v, ожидается delete /lost.bin", ops)
	}
	backend.mu.Lock()
	backend.files["/lost.bin"] = []byte("z")
	backend.inodes["/lost.bin"] = 8
	backend.mu.Unlock()
	// «Перезапуск»: тот же каталог состояния, новый Journal (в тесте каталог
	// недоступен из newReconcileProvider — перезагрузку формата покрывает
	// TestJournalAddAckReload); здесь — исполнение загруженной операции.
	if err := p.executeJournalOp(context.Background(), ops[0]); err != nil {
		t.Fatalf("executeJournalOp: %v", err)
	}
	if len(backend.content("/lost.bin")) != 0 {
		t.Error("backend-запись не удалена")
	}
	if _, ok := p.idx.Path(8); ok {
		t.Error("индекс не почищен")
	}
}
