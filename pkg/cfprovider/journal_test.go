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

// Тесты журнала операций (задача 6.3, design.md D9): формат JSONL,
// дедупликация по ключу, порядок FIFO, compaction при Ack, загрузка при
// перезапуске, замена выгрузки удалением (задача 6.5), повторы replayer'а
// с нарастающей паузой на injectable-часах (спеки «Создание офлайн»,
// «Удаление офлайн»), запрет удаления sync root (задача 6.2).
// cldapi.dll не вызывается — backend/store подменные.
package cfprovider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/cfapi"
	"github.com/juicedata/juicefs/pkg/meta"
)

// TestJournalAddAckReload — базовый контракт журнала: Add с дедупликацией
// по ключу (повтор выгрузки того же пути заменяет запись), FIFO-порядок
// исполнения, формат строки JSONL, Ack снимает операцию и переуплотняет
// файл, перезагрузка при повторном открытии (перезапуск serve).
func TestJournalAddAckReload(t *testing.T) {
	dir := t.TempDir()
	j, err := NewJournal(dir)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	if err := j.Add(JournalOp{Type: OpDelete, Path: "/a.txt", Ino: 42}); err != nil {
		t.Fatalf("Add delete: %v", err)
	}
	if err := j.Add(JournalOp{Type: OpUpload, Path: "/b.txt"}); err != nil {
		t.Fatalf("Add upload: %v", err)
	}
	// Повтор того же ключа — замена (дедупликация), не вторая запись.
	if err := j.Add(JournalOp{Type: OpUpload, Path: "/b.txt"}); err != nil {
		t.Fatalf("Add upload again: %v", err)
	}
	if err := j.Add(JournalOp{Type: OpMkdir, Path: "/d"}); err != nil {
		t.Fatalf("Add mkdir: %v", err)
	}
	if err := j.Add(JournalOp{Type: OpRename, Path: "/c", NewPath: "/c2", Ino: 7, IsDir: true}); err != nil {
		t.Fatalf("Add rename: %v", err)
	}
	if got := j.Len(); got != 4 {
		t.Fatalf("Len = %d, ожидается 4 (дедупликация)", got)
	}
	ops := j.Ops()
	wantOrder := []string{"delete:/a.txt", "upload:/b.txt", "mkdir:/d", "rename:/c"}
	for i, w := range wantOrder {
		if ops[i].Key() != w {
			t.Errorf("Ops[%d].Key = %q, ожидается %q (FIFO)", i, ops[i].Key(), w)
		}
	}
	// Формат строки: JSONL с полями type/path (+ts).
	b, err := os.ReadFile(filepath.Join(dir, opsJournalName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 5 { // дедупликация — append всё равно пишет новую строку
		t.Fatalf("строк в файле = %d, ожидается 5 (append до compaction)", len(lines))
	}
	if !strings.Contains(lines[0], `"type":"delete"`) || !strings.Contains(lines[0], `"path":"/a.txt"`) || !strings.Contains(lines[0], `"ino":42`) {
		t.Errorf("первая строка без type/path/ino: %q", lines[0])
	}
	if !strings.Contains(lines[4], `"new_path":"/c2"`) || !strings.Contains(lines[4], `"is_dir":true`) {
		t.Errorf("строка rename без new_path/is_dir: %q", lines[4])
	}

	// Ack: операция снята, файл переуплотнён (3 строк вместо 5).
	opsSnapshot := j.Ops()
	if err := j.Ack(opsSnapshot[0]); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if got := j.Len(); got != 3 {
		t.Fatalf("Len после Ack = %d, ожидается 3", got)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b, err = os.ReadFile(filepath.Join(dir, opsJournalName))
	if err != nil {
		t.Fatalf("ReadFile после Ack: %v", err)
	}
	if n := strings.Count(strings.TrimRight(string(b), "\n"), "\n") + 1; n != 3 {
		t.Errorf("строк после compaction = %d, ожидается 3: %q", n, string(b))
	}

	// Перезагрузка: порядок и содержимое сохранены.
	j2, err := NewJournal(dir)
	if err != nil {
		t.Fatalf("повторный NewJournal: %v", err)
	}
	defer j2.Close()
	if got := j2.Len(); got != 3 {
		t.Fatalf("Len после перезагрузки = %d, ожидается 3", got)
	}
	ops = j2.Ops()
	for i, w := range wantOrder[1:] {
		if ops[i].Key() != w {
			t.Errorf("после перезагрузки Ops[%d].Key = %q, ожидается %q", i, ops[i].Key(), w)
		}
	}
	if !j2.HasPending(OpUpload, "/b.txt") || j2.HasPending(OpDelete, "/a.txt") {
		t.Error("HasPending после перезагрузки неверен")
	}
}

// TestJournalReplaceUploadWithDelete — задача 6.5: ожидающая выгрузка
// заменяется удалением; замена без ожидающей выгрузки — no-op.
func TestJournalReplaceUploadWithDelete(t *testing.T) {
	j, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	defer j.Close()
	if err := j.Add(JournalOp{Type: OpUpload, Path: "/X.txt"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := j.ReplaceUploadWithDelete("/x.txt", 0); err != nil {
		t.Fatalf("ReplaceUploadWithDelete: %v", err)
	}
	if got := j.Len(); got != 1 {
		t.Fatalf("Len = %d, ожидается 1 (замена, не добавление)", got)
	}
	ops := j.Ops()
	if ops[0].Type != OpDelete || ops[0].Path != "/X.txt" {
		t.Fatalf("op = %+v, ожидается delete /X.txt", ops[0])
	}
	// Повторной выгрузки нет — замена no-op.
	if err := j.ReplaceUploadWithDelete("/x.txt", 0); err != nil {
		t.Fatalf("повторная ReplaceUploadWithDelete: %v", err)
	}
	if got := j.Len(); got != 1 {
		t.Fatalf("Len после повторной замены = %d, ожидается 1", got)
	}
}

// newJournalTestProvider — провайдер выгрузки + журнал + injectable часы
// (повторы replayer'а двигает тест).
func newJournalTestProvider(t *testing.T) (*Provider, *fakeIngestBackend, *fakeIngestStore, *fakeStore, *Index, *Journal, *opLog, *fakeClock) {
	t.Helper()
	p, backend, store, fstore, idx, log := newIngestTestProvider(t)
	j, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	p.journal = j
	clock := newFakeClock(time.Unix(0, 0))
	p.clock = clock
	p.journalRetryBase = time.Second
	p.journalRetryMax = 30 * time.Second
	return p, backend, store, fstore, idx, j, log, clock
}

// startReplay поднимает replayer (без DLL и диспетчера — как в Start, но
// только журнальная горутина).
func startReplay(t *testing.T, p *Provider) context.CancelFunc {
	t.Helper()
	p.stopMu.Lock()
	p.stop = make(chan struct{})
	p.stopMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	p.startJournalReplay(ctx, p.stopChan())
	return cancel
}

// TestJournalNotifyDeleteOfflineAcksError — U19 (blocked-style) + спека
// «Удаление офлайн»: rm известного плейсхолдера при недоступном hub —
// ровно один ACK_DELETE с кодом отказа (rm пользователя завершается
// ошибкой, локальная запись остаётся), backend/индекс не тронуты, журнал
// пуст (повтор — rm пользователя после восстановления; journal-delete
// остаётся для reconcile-(б) и ReplaceUploadWithDelete — краш-окно).
func TestJournalNotifyDeleteOfflineAcksError(t *testing.T) {
	p, backend, store, _, idx, j, _, _ := newJournalTestProvider(t)
	cancel := startReplay(t, p)
	defer cancel()

	if err := idx.Put(42, "/a.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	backend.mu.Lock()
	backend.files["/a.txt"] = []byte("data")
	backend.inodes["/a.txt"] = 42
	backend.deleteErr = syscall.EIO // hub недоступен
	backend.mu.Unlock()

	p.notifyDeletePath(filepath.Join(p.root, "a.txt"), 42, false, testConn, testTransfer, testRequest)

	if n := store.deleteAckCount(); n != 1 {
		t.Fatalf("ack'ов = %d, ожидается ровно 1", n)
	}
	if ack := store.lastDeleteAck(); ack.status != cfapi.StatusFromErrno(syscall.EIO) {
		t.Errorf("ack status = 0x%x, ожидается StatusFromErrno(EIO)", ack.status)
	}
	if len(backend.content("/a.txt")) == 0 {
		t.Fatal("backend-запись удалена при недоступном hub")
	}
	if _, ok := idx.Path(42); !ok {
		t.Fatal("индекс потерял запись")
	}
	if got := j.Len(); got != 0 {
		t.Fatalf("Len журнала = %d, ожидается 0 (локальный rm не удался — распространять нечего)", got)
	}
}

// TestJournalReplayUploadOffline — спека «Создание офлайн»: выгрузка при
// недоступном hub (отказ Flush — версия не подтверждена) уходит в журнал;
// после восстановления replayer выгружает, финализирует (replace-after-
// upload) и снимает операцию.
func TestJournalReplayUploadOffline(t *testing.T) {
	p, backend, _, fstore, idx, j, _, clock := newJournalTestProvider(t)
	cancel := startReplay(t, p)
	defer cancel()

	local := filepath.Join(p.root, "new.txt")
	data := patternBytes(100)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}
	backend.mu.Lock()
	backend.flushErr = syscall.EIO // hub недоступен: Flush отказывает
	backend.mu.Unlock()

	p.runIngest(context.Background(), ingestTask{fsPath: "/new.txt", local: local, key: "/new.txt"})

	// Операция в журнале, финализации не было (write-back выключен).
	if got := j.Len(); got != 1 {
		t.Fatalf("Len журнала = %d, ожидается 1", got)
	}
	if op := j.Ops()[0]; op.Type != OpUpload || op.Path != "/new.txt" {
		t.Fatalf("op = %+v, ожидается upload /new.txt", op)
	}
	if n := len(fstore.convertCalls()); n != 0 {
		t.Fatalf("финализация при отказавшем Flush: %d конверсий", n)
	}

	// Восстановление: повтор — выгрузка, replace-after-upload, Ack.
	backend.mu.Lock()
	backend.flushErr = 0
	backend.mu.Unlock()
	clock.advance(time.Second)
	waitFor(t, func() bool { return j.Len() == 0 })
	waitFor(t, func() bool {
		return backend.content("/new.txt") != nil && len(backend.content("/new.txt")) == len(data)
	})
	ino := backend.inodeOf("/new.txt")
	waitFor(t, func() bool {
		got, ok := idx.Path(ino)
		return ok && got == "/new.txt"
	})
	if n := len(fstore.convertCalls()); n != 1 {
		t.Errorf("конверсий после восстановления = %d, ожидается 1 (replace-after-upload)", n)
	}
}

// TestJournalSyncRootNeverExecuted — инвариант (задача 6.2): удаление sync
// root никогда не исполняется из журнала (гард против испорченного журнала;
// основной — гард notifyDeletePath).
func TestJournalSyncRootNeverExecuted(t *testing.T) {
	p, backend, _, _, _, _, _, _ := newJournalTestProvider(t)
	err := p.executeDeleteOp(context.Background(), &JournalOp{Type: OpDelete, Path: "/", Ino: 1})
	if !isPermanentOpError(err) {
		t.Fatalf("executeDeleteOp(/) = %v, ожидается перманентный отказ", err)
	}
	if backend.deletes != 0 || backend.rmdirs != 0 {
		t.Errorf("backend затронут: deletes=%d rmdirs=%d, ожидается 0/0", backend.deletes, backend.rmdirs)
	}
	// Подстраховка диспетчера: неизвестный тип — перманентное снятие.
	err = p.executeJournalOp(context.Background(), &JournalOp{Type: "bogus", Path: "/x"})
	if !isPermanentOpError(err) {
		t.Fatalf("executeJournalOp(bogus) = %v, ожидается перманентный отказ", err)
	}
}

// TestJournalDeleteENOENTAsSuccess — повтор удаления отсутствующей записи
// (ENOENT) считается исполненным: индекс чистится, ошибка nil.
func TestJournalDeleteENOENTAsSuccess(t *testing.T) {
	p, backend, _, _, idx, _, _, _ := newJournalTestProvider(t)
	if err := idx.Put(15, "/ghost.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	err := p.executeDeleteOp(context.Background(), &JournalOp{Type: OpDelete, Path: "/ghost.txt", Ino: 15})
	if err != nil {
		t.Fatalf("executeDeleteOp(ENOENT) = %v, ожидается nil", err)
	}
	if backend.deletes != 1 {
		t.Errorf("deletes = %d, ожидается 1 (попытка удаления была)", backend.deletes)
	}
	if _, ok := idx.Path(15); ok {
		t.Error("индекс не почищен после ENOENT-удаления")
	}
}

// TestJournalReplayRenameAndMkdir — повторы переименования и создания
// каталога: rename переносит записи индекса (каталог — с потомками),
// mkdir доводит локальный каталог до плейсхолдера.
func TestJournalReplayRenameAndMkdir(t *testing.T) {
	p, backend, _, fstore, idx, _, _, _ := newJournalTestProvider(t)
	ctx := context.Background()

	// rename каталога: индекс — сам каталог и потомки.
	backend.mu.Lock()
	backend.dirs["/odir"] = true
	backend.inodes["/odir"] = 5
	backend.dirs["/ndir"] = true
	backend.inodes["/ndir"] = 5 // rename в meta сохраняет inode
	backend.mu.Unlock()
	if err := idx.Put(5, "/odir"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	if err := idx.Put(6, "/odir/kid.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	err := p.executeRenameOp(ctx, &JournalOp{Type: OpRename, Path: "/odir", NewPath: "/ndir", Ino: 5, IsDir: true})
	if err != nil {
		t.Fatalf("executeRenameOp: %v", err)
	}
	if got, ok := idx.Path(5); !ok || got != "/ndir" {
		t.Errorf("idx.Path(5) = (%q, %v), ожидается /ndir", got, ok)
	}
	if got, ok := idx.Path(6); !ok || got != "/ndir/kid.txt" {
		t.Errorf("idx.Path(6) = (%q, %v), ожидается /ndir/kid.txt (RenamePrefix)", got, ok)
	}

	// rename файла: пере-резолв inode через backend Stat нового пути
	// (rename в meta сохраняет inode — как в подмене; индекс переносится).
	backend.mu.Lock()
	backend.files["/r2"] = []byte("x")
	backend.inodes["/r2"] = 11
	backend.mu.Unlock()
	if err := idx.Put(11, "/r2"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	if err := p.executeRenameOp(ctx, &JournalOp{Type: OpRename, Path: "/r2", NewPath: "/r3", Ino: 11}); err != nil {
		t.Fatalf("executeRenameOp file: %v", err)
	}
	if got, ok := idx.Path(11); !ok || got != "/r3" {
		t.Errorf("idx.Path(11) = (%q, %v), ожидается /r3", got, ok)
	}
	if _, ok := idx.LookupPath("/r2"); ok {
		t.Error("byPath старого пути файла не перенесён")
	}

	// mkdir: backend-каталог создан, локальный каталог сконвертирован.
	localDir := filepath.Join(p.root, "fresh")
	if err := os.Mkdir(localDir, 0o755); err != nil {
		t.Fatalf("mkdir local: %v", err)
	}
	if err := p.executeMkdirOp(ctx, &JournalOp{Type: OpMkdir, Path: "/fresh"}); err != nil {
		t.Fatalf("executeMkdirOp: %v", err)
	}
	if !backend.hasDir("/fresh") {
		t.Error("backend-каталог не создан")
	}
	converts := fstore.convertCalls()
	if len(converts) != 1 || !converts[0].isDir {
		t.Fatalf("конверсии = %+v, ожидается одна конверсия каталога", converts)
	}
	if converts[0].flags != cfapi.ConvertFlagMarkInSync {
		t.Errorf("флаги конверсии = 0x%x, ожидается MARK_IN_SYNC", converts[0].flags)
	}
	ino, _, _ := backend.Stat(noopCtx(), "/fresh")
	if _, ok := idx.Path(ino); !ok {
		t.Error("mkdir не попал в индекс")
	}
}

// --- Фикс-итерация 1 (ревью этапа 6) ---

// journalOpPaths — пути операций для сообщений об ошибках (Ops() возвращает
// указатели — %+v печатает адреса).
func journalOpPaths(ops []*JournalOp) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Type+":"+op.Path)
	}
	return out
}

// TestJournalDecomposeDirDelete — fix-1.2: удаление непустого каталога
// (ENOTEMPTY) декомпозируется — прямые потомки (по индексу) становятся
// ГОЛОВОЙ очереди, родитель переносится в хвост; после исполнения детей
// родитель удаляется. Живелока «голова блокирует очередь» нет.
func TestJournalDecomposeDirDelete(t *testing.T) {
	p, backend, _, _, idx, j, _ := notifyJournalTestProvider(t)
	ctx := context.Background()
	// backend: каталог с двумя файлами; индекс знает родителя и детей.
	backend.mu.Lock()
	backend.dirs["/big"] = true
	backend.inodes["/big"] = 30
	backend.files["/big/a.txt"] = []byte("1")
	backend.inodes["/big/a.txt"] = 31
	backend.files["/big/b.txt"] = []byte("2")
	backend.inodes["/big/b.txt"] = 32
	backend.mu.Unlock()
	for _, tc := range []struct {
		ino  uint64
		path string
	}{{30, "/big"}, {31, "/big/a.txt"}, {32, "/big/b.txt"}} {
		if err := idx.Put(tc.ino, tc.path); err != nil {
			t.Fatalf("idx.Put(%d): %v", tc.ino, err)
		}
	}
	if err := j.Add(JournalOp{Type: OpDelete, Path: "/big", Ino: 30, IsDir: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := p.executeJournalOp(ctx, j.Ops()[0])
	if !errors.Is(err, errNeedsDecompose) {
		t.Fatalf("executeJournalOp = %v, ожидается errNeedsDecompose", err)
	}
	// Дети в голове (детерминированный порядок по пути), родитель в хвосту;
	// backend не тронут.
	ops := j.Ops()
	if len(ops) != 3 || ops[0].Path != "/big/a.txt" || ops[1].Path != "/big/b.txt" || ops[2].Path != "/big" {
		t.Fatalf("очередь = %v, ожидается [/big/a.txt /big/b.txt /big]", journalOpPaths(ops))
	}
	if !backend.hasDir("/big") {
		t.Error("родитель удалён до детей")
	}
	// Дети исполняются, затем родитель.
	for _, op := range ops {
		if err := p.executeJournalOp(ctx, op); err != nil {
			t.Fatalf("executeJournalOp(%s %s): %v", op.Type, op.Path, err)
		}
		if err := j.Ack(op); err != nil {
			t.Fatalf("Ack(%s): %v", op.Key(), err)
		}
	}
	if backend.hasDir("/big") {
		t.Error("каталог backend не удалён после декомпозиции")
	}
	if len(backend.content("/big/a.txt")) != 0 || len(backend.content("/big/b.txt")) != 0 {
		t.Error("потомки не удалены")
	}
	if _, ok := idx.Path(30); ok {
		t.Error("индекс родителя не почищен")
	}
	if j.Len() != 0 {
		t.Errorf("журнал = %+v, ожидается пуст", j.Ops())
	}
}

// TestJournalRotationNonBlocking — fix-1.2 (анти-livelock): отказавший оп
// переносится в хвост, остальные исполняются в том же проходе; восстановление
// доводит отказавший до исполнения (лимит ротаций на цикл не даёт ему
// блокировать остальных).
func TestJournalRotationNonBlocking(t *testing.T) {
	p, backend, _, _, idx, j, _ := notifyJournalTestProvider(t)
	clock := newFakeClock(time.Unix(0, 0))
	p.clock = clock
	p.journalRetryBase = time.Second
	cancel := startReplay(t, p)
	defer cancel()

	backend.mu.Lock()
	backend.files["/rot1.txt"] = []byte("a")
	backend.inodes["/rot1.txt"] = 50
	backend.files["/rot2.txt"] = []byte("b")
	backend.inodes["/rot2.txt"] = 51
	// Отказ удаления только rot1 (глобальный hub поднят — rot2 исполняется).
	backend.deleteErrPaths = map[string]syscall.Errno{"/rot1.txt": syscall.EIO}
	backend.mu.Unlock()
	if err := idx.Put(50, "/rot1.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	if err := idx.Put(51, "/rot2.txt"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	if err := j.Add(JournalOp{Type: OpDelete, Path: "/rot1.txt", Ino: 50}); err != nil {
		t.Fatalf("Add rot1: %v", err)
	}
	if err := j.Add(JournalOp{Type: OpDelete, Path: "/rot2.txt", Ino: 51}); err != nil {
		t.Fatalf("Add rot2: %v", err)
	}
	// Первый проход: rot1 отказал и ушёл в хвост — rot2 исполнилась сразу
	// (не заблокирована головой).
	clock.advance(time.Second)
	waitFor(t, func() bool { return len(backend.content("/rot2.txt")) == 0 })
	if len(backend.content("/rot1.txt")) == 0 {
		t.Fatal("rot1 исполнена при недоступном hub")
	}
	if got := j.Len(); got != 1 {
		t.Fatalf("Len журнала = %d, ожидается 1 (rot2 ack, rot1 ждёт)", got)
	}
	// Восстановление: rot1 доводится (пара тактов — пауза повтора injectable).
	backend.mu.Lock()
	delete(backend.deleteErrPaths, "/rot1.txt")
	backend.mu.Unlock()
	for i := 0; i < 5; i++ {
		clock.advance(time.Second)
	}
	waitFor(t, func() bool { return j.Len() == 0 })
	waitFor(t, func() bool { return len(backend.content("/rot1.txt")) == 0 })
}

// TestJournalAckTsGuard — гвард Ack по ts: заменённая за время исполнения
// операция (повторный отказ обновил ts) не снимается; исполняется снимается
// только своя версия снапшота.
func TestJournalAckTsGuard(t *testing.T) {
	j, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	defer j.Close()
	if err := j.Add(JournalOp{Type: OpUpload, Path: "/same.txt"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	snapshot := j.Ops()[0]
	// Замена за время исполнения (тот же ключ, новый ts).
	if err := j.Add(JournalOp{Type: OpUpload, Path: "/same.txt"}); err != nil {
		t.Fatalf("Add replace: %v", err)
	}
	if err := j.Ack(snapshot); err != nil {
		t.Fatalf("Ack snapshot: %v", err)
	}
	if got := j.Len(); got != 1 {
		t.Fatalf("Len = %d, ожидается 1 (заменённая запись не снята гвардом ts)", got)
	}
	if j.Ops()[0].Ts == snapshot.Ts {
		t.Error("Ops[0] — старая версия (замена не сохранилась)")
	}
	// Свежая версия снимается своей записью.
	fresh := j.Ops()[0]
	if err := j.Ack(fresh); err != nil {
		t.Fatalf("Ack fresh: %v", err)
	}
	if got := j.Len(); got != 0 {
		t.Fatalf("Len = %d, ожидается 0", got)
	}
}

// eisdirBackend — стаб: Unlink отказывает EISDIR (тип записи по ошибке
// Unlink — IndexPathEntry не несёт IsDir), Rmdir работает (fix-1.6).
type eisdirBackend struct {
	*fakeIngestBackend
}

func (b *eisdirBackend) Delete(ctx meta.Context, path string) syscall.Errno {
	return syscall.EISDIR
}

// TestJournalDeleteEISDIRFallback — fix-1.6: op без IsDir, а запись —
// каталог: Unlink отказывает EISDIR → fallback Rmdir доводит удаление,
// индекс чистится.
func TestJournalDeleteEISDIRFallback(t *testing.T) {
	p, backend, _, _, idx, _, _ := notifyJournalTestProvider(t)
	p.ibackend = &eisdirBackend{fakeIngestBackend: backend}
	backend.mu.Lock()
	backend.dirs["/stubdir"] = true
	backend.inodes["/stubdir"] = 60
	backend.mu.Unlock()
	if err := idx.Put(60, "/stubdir"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}

	op := &JournalOp{Type: OpDelete, Path: "/stubdir", Ino: 60} // IsDir не известен
	if err := p.executeDeleteOp(context.Background(), op); err != nil {
		t.Fatalf("executeDeleteOp = %v, ожидается nil (fallback Rmdir)", err)
	}
	if backend.hasDir("/stubdir") {
		t.Error("каталог не удалён fallback'ом")
	}
	if _, ok := idx.Path(60); ok {
		t.Error("индекс не почищен")
	}
}

// TestJournalDecomposeForeignChildrenDropped — ре-чек (hot-loop): каталог
// непуст, но известных индексу потомков нет (чужой клиент создал запись
// внутри, U17) — декомпозировать нечего: перманентное снятие (Warn, запись
// сохраняется), НЕ errNeedsDecompose — иначе replayer крутился бы без
// бэкофа и без счётчиков ротаций (счётчики сбрасывались «чистыми» циклами).
func TestJournalDecomposeForeignChildrenDropped(t *testing.T) {
	p, backend, _, _, idx, j, _ := notifyJournalTestProvider(t)
	backend.mu.Lock()
	backend.dirs["/foreign"] = true
	backend.inodes["/foreign"] = 40
	backend.files["/foreign/other.txt"] = []byte("x") // чужая запись — в индексе НЕ известна
	backend.mu.Unlock()
	if err := idx.Put(40, "/foreign"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	op := &JournalOp{Type: OpDelete, Path: "/foreign", Ino: 40, IsDir: true}

	err := p.executeDeleteOp(context.Background(), op)
	if !isPermanentOpError(err) {
		t.Fatalf("executeDeleteOp = %v, ожидается перманентное снятие (no known children)", err)
	}
	if !backend.hasDir("/foreign") {
		t.Error("каталог backend удалён (запись должна сохраняться, fix-1.5)")
	}
	if _, ok := idx.Path(40); !ok {
		t.Error("индекс снят при перманентном отказе (воскрешение)")
	}
	if len(j.Ops()) != 0 {
		t.Errorf("декомпозиция поставила потомков: %+v, ожидается пусто", journalOpPaths(j.Ops()))
	}
}

// TestPermanentJournalErrnoExdev — EXDEV (cross-company отказ платформы,
// «different disk drive») перманентен: повтор невозможен (живой кейс 27.11,
// AgioDrive9/facility: watcher-событие walk-каталога чужой компании →
// mkdir → EXDEV → вечный retry журнала до фикса).
func TestPermanentJournalErrnoExdev(t *testing.T) {
	if !permanentJournalErrno(syscall.EXDEV) {
		t.Fatal("EXDEV классифицирован транзиентным — cross-company отказ будет ретраиться вечно")
	}
	for _, eno := range []syscall.Errno{syscall.EACCES, syscall.EPERM, syscall.ENOTEMPTY} {
		if got := permanentJournalErrno(eno); got != (eno != syscall.ENOTEMPTY) {
			t.Fatalf("permanentJournalErrno(%v) = %v", eno, got)
		}
	}
	if permanentJournalErrno(syscall.EIO) {
		t.Fatal("EIO классифицирован перманентным — недоступный hub должен ретраиться")
	}
}
