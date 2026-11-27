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

// Тесты watcher'а (design.md U6): разбор NextEntryOffset-цепочки
// FILE_NOTIFY_INFORMATION на синтетическом буфере (действия к выгрузке vs
// отфильтрованные, битые хвосты) и сквозной прогон на РЕАЛЬНОМ
// ReadDirectoryChangesW в t.TempDir (создание/запись/переименование файла →
// события → классификация → коалисер; kernel32, без cldapi.dll — инвариант
// «Движок проверяется без CfAPI»).
package cfprovider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// align4 — выравнивание размера записи FILE_NOTIFY_INFORMATION на 4 байта
// (NextEntryOffset в реальном фильтре выровнен).
func align4(n int) uint32 { return uint32((n + 3) &^ 3) }

// appendNotifyEntry добавляет запись FILE_NOTIFY_INFORMATION в буфер: запись
// выровнена на 4 (нулевой паддинг), nextOffset — NextEntryOffset записи
// (0 — конец цепочки), name — UTF-16 без терминатора (FileNameLength — в
// байтах, как у реального фильтра).
func appendNotifyEntry(buf []byte, action uint32, name string, nextOffset uint32) []byte {
	name16, err := windows.UTF16FromString(name)
	if err != nil {
		panic(err)
	}
	name16 = name16[:len(name16)-1] // FileNameLength — без терминатора
	size := align4(fileNotifyInfoHeaderSize + len(name16)*2)
	entry := make([]byte, size)
	*(*fileNotifyInformation)(unsafe.Pointer(&entry[0])) = fileNotifyInformation{
		NextEntryOffset: nextOffset,
		Action:          action,
		FileNameLength:  uint32(len(name16) * 2),
	}
	copy(entry[fileNotifyInfoHeaderSize:], (*(*[1 << 20]byte)(unsafe.Pointer(&name16[0])))[:len(name16)*2])
	return append(buf, entry...)
}

// TestParseNotifyEventsChain — цепочка из пяти записей: фильтр действий
// пропускает ADDED/MODIFIED/RENAMED_NEW_NAME, отсекает REMOVED и
// RENAMED_OLD_NAME; прошивка NextEntryOffset между записями, ноль — конец.
func TestParseNotifyEventsChain(t *testing.T) {
	var buf []byte
	buf = appendNotifyEntry(buf, windows.FILE_ACTION_ADDED, "new.txt", 0)
	buf = appendNotifyEntry(buf, windows.FILE_ACTION_REMOVED, "deleted.txt", 0)
	buf = appendNotifyEntry(buf, windows.FILE_ACTION_MODIFIED, "changed.txt", 0)
	buf = appendNotifyEntry(buf, windows.FILE_ACTION_RENAMED_OLD_NAME, "old.txt", 0)
	buf = appendNotifyEntry(buf, windows.FILE_ACTION_RENAMED_NEW_NAME, "renamed.txt", 0)
	// Прошить NextEntryOffset: каждая запись указывает на следующую
	// (выровненный собственный размер), последняя — 0 (уже 0).
	off := 0
	for i := 0; i < 4; i++ {
		info := (*fileNotifyInformation)(unsafe.Pointer(&buf[off]))
		info.NextEntryOffset = align4(fileNotifyInfoHeaderSize + int(info.FileNameLength))
		off += int(info.NextEntryOffset)
	}

	names := parseNotifyEvents(buf)
	if len(names) != 3 || names[0] != "new.txt" || names[1] != "changed.txt" || names[2] != "renamed.txt" {
		t.Fatalf("разобрано %v, ожидается [new.txt changed.txt renamed.txt] (REMOVED и RENAMED_OLD_NAME отфильтрованы)", names)
	}
}

// TestParseNotifyEventsBadTail — битые хвосты: обрезанная запись (имя за
// границей буфера) и некорректный NextEntryOffset останавливают разбор без
// зацикливания и без выхода за буфер.
func TestParseNotifyEventsBadTail(t *testing.T) {
	var buf []byte
	buf = appendNotifyEntry(buf, windows.FILE_ACTION_ADDED, "ok.txt", 0)
	(*fileNotifyInformation)(unsafe.Pointer(&buf[0])).NextEntryOffset = align4(fileNotifyInfoHeaderSize + 2*len("ok.txt"))

	// Обрезанная вторая запись: заголовок есть, имени нет (FileNameLength
	// указывает за границу буфера).
	truncated := append(append([]byte{}, buf...), make([]byte, fileNotifyInfoHeaderSize)...)
	*(*fileNotifyInformation)(unsafe.Pointer(&truncated[len(buf)])) = fileNotifyInformation{
		NextEntryOffset: 0,
		Action:          windows.FILE_ACTION_MODIFIED,
		FileNameLength:  8, // имени в буфере нет
	}
	names := parseNotifyEvents(truncated)
	if len(names) != 1 || names[0] != "ok.txt" {
		t.Fatalf("разобрано %v, ожидается [ok.txt] (обрезанный хвост остановил разбор)", names)
	}

	// Некорректный NextEntryOffset (< размера заголовка) — разбор
	// останавливается, зацикливания нет.
	bad := appendNotifyEntry(nil, windows.FILE_ACTION_ADDED, "a.txt", 4)
	names = parseNotifyEvents(bad)
	if len(names) != 1 || names[0] != "a.txt" {
		t.Fatalf("разобрано %v, ожидается [a.txt]", names)
	}
}

// TestWatcherRealDirectory — сквозной прогон на реальном ReadDirectoryChangesW
// в t.TempDir: создание+запись файла → события ADDED/MODIFIED → классификация
// (state=0 → to-upload) → коалисер → ОДНО задание; mkdir — каталог
// игнорируется; изменение файла → MODIFIED; переименование →
// RENAMED_NEW_NAME нового имени (OLD отфильтрован); остановка по cancel ctx
// завершает горутину watcher'а (CancelIoEx разблокирует чтение).
//
// События, случившиеся до первого ReadDirectoryChangesW, теряются (фильтр не
// буферизует их до запуска чтения) — первая запись повторяется в цикле, пока
// watcher не начнёт доставлять (следующие фазы идут при уже работающем
// чтении и детерминированы).
func TestWatcherRealDirectory(t *testing.T) {
	p, _, _, _, _, _ := newIngestTestProvider(t)
	db := newIngestDebouncer(50*time.Millisecond, wallClock{}) // debounce гасит пачку ADDED+MODIFIED
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	dbDone := make(chan struct{})
	go func() {
		defer close(dbDone)
		db.run(ctx, stop)
	}()
	p.startFileWatcher(ctx, db, stop)

	// Создание + запись (с повтором до старта чтения watcher'а).
	local := filepath.Join(p.root, "watched.txt")
	var got ingestTask
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := os.WriteFile(local, []byte("watch me"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		select {
		case got = <-db.out:
		case <-time.After(50 * time.Millisecond):
		}
		if got.fsPath != "" || time.Now().After(deadline) {
			break
		}
	}
	if got.fsPath != "/watched.txt" || got.local != local || got.key != "/watched.txt" {
		t.Fatalf("задание = %+v, ожидается /watched.txt", got)
	}

	// Каталог: DIR_NAME-событие → классификатор игнорирует каталоги
	// (WatchSubtree сам рекурсивный — в очередь имён каталог не ставится).
	if err := os.Mkdir(filepath.Join(p.root, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Изменение существующего файла: MODIFIED → задание (тот же путь).
	if err := os.WriteFile(local, []byte("watch me again"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got = recvTask(t, db.out)
	if got.fsPath != "/watched.txt" {
		t.Fatalf("задание на изменение = %+v, ожидается /watched.txt", got)
	}

	// Переименование: RENAMED_OLD_NAME фильтруется, NEW_NAME ставится в
	// очередь под новым именем.
	renamed := filepath.Join(p.root, "renamed.txt")
	if err := os.Rename(local, renamed); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got = recvTask(t, db.out)
	if got.fsPath != "/renamed.txt" || got.local != renamed {
		t.Fatalf("задание на переименование = %+v, ожидается /renamed.txt", got)
	}

	// Остановка: cancel ctx → CancelIoEx → горутина watcher'а (в p.wg)
	// завершается.
	cancel()
	watcherDone := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(watcherDone)
	}()
	select {
	case <-watcherDone:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher не завершился за 5 c после отмены")
	}
	<-dbDone
}
