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

// Тесты restart-сверки свежести (fix-cfapi-followups A2, задача 2.1):
// чистая compareVersion (таблица с границами допуска) и checkFreshness
// (equal без мутаций и STANDARD-запросов, stat-ошибка, каталог,
// несовпавший путь индекса — сверка не начинается).

package cfprovider

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// TestCompareVersion — таблица вердиктов сверки (design.md A2): размер
// строго, mtime с допуском freshMtimeSlack; границы допуска включительно;
// разошедшийся размер при |Δmtime| ≤ допуск — ambiguous.
func TestCompareVersion(t *testing.T) {
	backend := BackendEntry{Size: 100, MtimeSec: 1700000000, MtimeNsec: 0}
	bt := time.Unix(1700000000, 0)

	cases := []struct {
		name  string
		size  int64
		mtime time.Time
		want  freshVerdict
	}{
		{"exact match", 100, bt, freshEqual},
		{"within slack plus", 100, bt.Add(freshMtimeSlack - time.Second), freshEqual},
		{"within slack minus", 100, bt.Add(-freshMtimeSlack + time.Second), freshEqual},
		{"boundary at plus slack", 100, bt.Add(freshMtimeSlack), freshEqual},
		{"boundary at minus slack", 100, bt.Add(-freshMtimeSlack), freshEqual},
		{"local newer beyond slack", 100, bt.Add(freshMtimeSlack + time.Nanosecond), freshLocalNewer},
		{"backend newer beyond slack", 100, bt.Add(-freshMtimeSlack - time.Nanosecond), freshBackendNewer},
		{"local newer same size", 100, bt.Add(time.Hour), freshLocalNewer},
		{"backend newer same size", 100, bt.Add(-time.Hour), freshBackendNewer},
		{"size differs same mtime", 101, bt, freshAmbiguous},
		{"size differs within slack", 99, bt.Add(time.Second), freshAmbiguous},
		{"size differs local beyond slack", 101, bt.Add(freshMtimeSlack + time.Second), freshLocalNewer},
		{"size differs backend beyond slack", 99, bt.Add(-freshMtimeSlack - time.Second), freshBackendNewer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareVersion(tc.size, tc.mtime, backend); got != tc.want {
				t.Errorf("compareVersion(size=%d, mtime=+%s) = %v, ожидается %v",
					tc.size, tc.mtime.Sub(bt), got, tc.want)
			}
		})
	}
}

// equalStore — fakeStore с равными версиями записи (свежесть equal) и
// STANDARD-ответом, который НЕ ДОЛЖЕН запрашиваться при equal.
func equalStore(localPath string, e BackendEntry) *fakeStore {
	return &fakeStore{
		local:    map[string]bool{localPath: true},
		standard: map[string]*cfapi.CFPlaceholderStandardInfo{localPath: fakeStdInfoWithIdentity(e.Inode)},
		stats: map[string]fakeStat{
			localPath: {size: e.Size, mtime: time.Unix(e.MtimeSec, int64(e.MtimeNsec))},
		},
	}
}

// TestCheckFreshnessEqualNoMutations — спека «Рестарт без изменений»:
// равные версии — вердикт equal, ноль действий (никаких Update/convert),
// ноль STANDARD-запросов (идемпотентность тёплого пути) и ноль счётчиков.
func TestCheckFreshnessEqualNoMutations(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "same.bin", Inode: 400, Size: 7, MtimeSec: 10}
	localPath := root + "\\same.bin"
	store := equalStore(localPath, entry)
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)

	fc := p.checkFreshness(entry, "/same.bin", localPath)

	if fc.verdict != freshEqual {
		t.Errorf("verdict = %v, ожидается equal", fc.verdict)
	}
	if fc.counts.total() != 0 {
		t.Errorf("счётчики не пусты: %+v", fc.counts)
	}
	if n := len(store.updateCalls()); n != 0 {
		t.Errorf("equal вызвал Update: %d вызовов", n)
	}
	if n := store.standardOfCalls; n != 0 {
		t.Errorf("equal сделал STANDARD-запросов: %d, ожидается 0", n)
	}
}

// TestCheckFreshnessStatError — stat-ошибка: вердикта нет, действий и
// счётчиков нет (удаление обработает reconcileDir).
func TestCheckFreshnessStatError(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "gone.bin", Inode: 401, Size: 7, MtimeSec: 10}
	localPath := root + "\\gone.bin"
	// stats не задан — StatOf отказывает; STANDARD не должен запрашиваться.
	store := &fakeStore{local: map[string]bool{localPath: true}}
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)

	fc := p.checkFreshness(entry, "/gone.bin", localPath)

	if fc.statErr == nil {
		t.Error("statErr пуст, ожидается ошибка StatOf")
	}
	if fc.counts.total() != 0 {
		t.Errorf("счётчики не пусты: %+v", fc.counts)
	}
	if n := store.standardOfCalls; n != 0 {
		t.Errorf("stat-ошибка сделала STANDARD-запросов: %d, ожидается 0", n)
	}
}

// TestCheckFreshnessDirSkipped — каталоги исключены из сверки (NTFS двигает
// mtime каталога при любой смене записей): сверка не начинается, STANDARD
// не запрашивается, счётчики пусты.
func TestCheckFreshnessDirSkipped(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "dir", Inode: 402, IsDir: true, MtimeSec: 10}
	localPath := root + "\\dir"
	store := &fakeStore{
		local: map[string]bool{localPath: true},
		stats: map[string]fakeStat{localPath: {isDir: true, mtime: time.Unix(1, 0)}},
	}
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)

	fc := p.checkFreshness(entry, "/dir", localPath)

	if !fc.isDir || fc.verdict != freshEqual {
		t.Errorf("isDir=%v verdict=%v, ожидается каталог без вердикта", fc.isDir, fc.verdict)
	}
	if fc.counts.total() != 0 {
		t.Errorf("счётчики не пусты: %+v", fc.counts)
	}
	if n := store.standardOfCalls; n != 0 {
		t.Errorf("каталог сделал STANDARD-запросов: %d, ожидается 0", n)
	}
}

// TestCheckFreshnessPathMismatchSkipped — известная индексу запись, чей путь
// в индексе отличается от localPath (переименование), сверку не начинает:
// StatOf не вызывается вовсе (fix-cfapi-followups A2, известная ветка).
func TestCheckFreshnessPathMismatchSkipped(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "moved.bin", Inode: 403, Size: 7, MtimeSec: 10}
	localPath := root + "\\moved.bin"
	store := &fakeStore{local: map[string]bool{localPath: true}}
	p, idx := newTestProvider(t, root, &fakeBackend{}, store)
	if err := idx.Put(entry.Inode, "/other/moved.bin"); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	res := &dirPopulation{dir: "/"}
	examples := []string{}

	classifyOccupied(p, res, entry, "/moved.bin", localPath, true, &examples)

	if res.skipped != 1 {
		t.Errorf("skipped = %d, ожидается 1 (known-ветка)", res.skipped)
	}
	if n := store.statOfCalls; n != 0 {
		t.Errorf("несовпавший путь начал сверку: %d вызовов StatOf, ожидается 0", n)
	}
	if res.fresh.total() != 0 {
		t.Errorf("счётчики свежести не пусты: %+v", res.fresh)
	}
}

// divergedStore — fakeStore с разошедшейся версией: stat задаётся
// параметрами, STANDARD-ответ — withStd (identity + гейты pin/in-sync);
// по умолчанию запись in-sync и не pinned (штатный рестарт).
func divergedStore(localPath string, ino uint64, stat fakeStat, withStd func(*cfapi.CFPlaceholderStandardInfo)) *fakeStore {
	info := fakeStdInfoWithIdentity(ino)
	info.InSyncState = cfapi.InSyncStateInSync
	if withStd != nil {
		withStd(info)
	}
	return &fakeStore{
		local:    map[string]bool{localPath: true},
		standard: map[string]*cfapi.CFPlaceholderStandardInfo{localPath: info},
		stats:    map[string]fakeStat{localPath: stat},
	}
}

// TestCheckFreshnessLocalNewerQueuesUpload — спека «Локальная правка при
// выключенном serve выгружается»: локаль новее (mtime за допуском),
// in-sync → задание выгрузки тем же механизмом, что reconcileDir.
func TestCheckFreshnessLocalNewerQueuesUpload(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "edited.bin", Inode: 410, Size: 7, MtimeSec: 10}
	localPath := root + "\\edited.bin"
	store := divergedStore(localPath, entry.Inode, fakeStat{size: 7, mtime: time.Unix(13, 0)}, nil)
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)
	db := newIngestDebouncer(time.Millisecond, wallClock{})
	p.ingestDb = db // коалисер: queueUpload кладёт в reqs (воркер не запущен)

	fc := p.checkFreshness(entry, "/edited.bin", localPath)

	if fc.counts.queuedUpload != 1 || fc.counts.total() != 1 {
		t.Errorf("счётчики: %+v, ожидается queued-upload=1", fc.counts)
	}
	select {
	case got := <-db.reqs:
		if got.fsPath != "/edited.bin" || got.local != localPath {
			t.Errorf("задание выгрузки: %+v, ожидается /edited.bin", got)
		}
	default:
		t.Error("задание выгрузки не поставлено в очередь")
	}
}

// TestCheckFreshnessLocalNewerReadOnly — read-only: только Warn, очередь не
// затрагивается (спека «Режим без записи»).
func TestCheckFreshnessLocalNewerReadOnly(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "ro.bin", Inode: 411, Size: 7, MtimeSec: 10}
	localPath := root + "\\ro.bin"
	store := divergedStore(localPath, entry.Inode, fakeStat{size: 7, mtime: time.Unix(13, 0)}, nil)
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)
	p.readOnly = true
	db := newIngestDebouncer(time.Millisecond, wallClock{})
	p.ingestDb = db
	hook := &logCapture{}
	logger.AddHook(hook)

	fc := p.checkFreshness(entry, "/ro.bin", localPath)

	if fc.counts.readonly != 1 || fc.counts.total() != 1 {
		t.Errorf("счётчики: %+v, ожидается readonly=1", fc.counts)
	}
	select {
	case got := <-db.reqs:
		t.Errorf("read-only поставил выгрузку: %+v", got)
	default:
	}
	if n := hook.count("read-only mode, upload not queued"); n != 1 {
		t.Errorf("строк Warn read-only: %d, ожидается 1", n)
	}
}

// TestCheckFreshnessLocalNewerOutOfSyncLeftToReconcile — локаль новее, но
// запись вне in-sync: ветка остаётся за очередью выгрузки сверки
// (reconcileDir ставит её сам) — счётчик out-of-sync, без очереди и Warn.
func TestCheckFreshnessLocalNewerOutOfSyncLeftToReconcile(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "oo.bin", Inode: 412, Size: 7, MtimeSec: 10}
	localPath := root + "\\oo.bin"
	store := divergedStore(localPath, entry.Inode, fakeStat{size: 7, mtime: time.Unix(13, 0)}, func(info *cfapi.CFPlaceholderStandardInfo) {
		info.InSyncState = cfapi.InSyncStateNotInSync
	})
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)
	db := newIngestDebouncer(time.Millisecond, wallClock{})
	p.ingestDb = db

	fc := p.checkFreshness(entry, "/oo.bin", localPath)

	if fc.counts.outOfSync != 1 || fc.counts.total() != 1 {
		t.Errorf("счётчики: %+v, ожидается out-of-sync=1", fc.counts)
	}
	select {
	case got := <-db.reqs:
		t.Errorf("ветка out-of-sync поставила выгрузку: %+v", got)
	default:
	}
}

// TestCheckFreshnessBackendNewerRevalidates — спека «Правка с другой машины
// подтягивается на рестарте»: backend новее, in-sync, не pinned →
// markSelfEvent + Update(VerifyInSync|Dehydrate) с новыми size/mtime.
func TestCheckFreshnessBackendNewerRevalidates(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "remote.bin", Inode: 413, Size: 99, MtimeSec: 42, MtimeNsec: 5}
	localPath := root + "\\remote.bin"
	store := divergedStore(localPath, entry.Inode, fakeStat{size: 7, mtime: time.Unix(10, 0)}, nil)
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)

	fc := p.checkFreshness(entry, "/remote.bin", localPath)

	if fc.counts.revalidated != 1 || fc.counts.total() != 1 {
		t.Errorf("счётчики: %+v, ожидается revalidated=1", fc.counts)
	}
	// Супрессия собственного события перед Update (watcher-путь).
	if suppressed, _ := p.selfEventSuppressed("/remote.bin"); !suppressed {
		t.Error("markSelfEvent не поставил окно супрессии на /remote.bin")
	}
	updates := store.updateCalls()
	if len(updates) != 1 {
		t.Fatalf("Update вызовов = %d, ожидается 1", len(updates))
	}
	u := updates[0]
	if u.path != localPath || u.size != 99 {
		t.Errorf("Update: %+v, ожидается remote.bin size 99", u)
	}
	if u.flags != cfapi.UpdateFlagVerifyInSync|cfapi.UpdateFlagDehydrate {
		t.Errorf("Update flags = 0x%x, ожидается VerifyInSync|Dehydrate", u.flags)
	}
	ft := windows.NsecToFiletime(42*1e9 + 5)
	wantLastWrite := int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
	if u.lastWriteTime != wantLastWrite {
		t.Errorf("Update LastWriteTime = %d, ожидается из записи hub (%d)", u.lastWriteTime, wantLastWrite)
	}
}

// TestCheckFreshnessBackendNewerPinnedSkips — спека «Pinned устарел»:
// backend новее при pinned — пропуск с предупреждением, без мутаций.
func TestCheckFreshnessBackendNewerPinnedSkips(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "pinned.bin", Inode: 414, Size: 99, MtimeSec: 42}
	localPath := root + "\\pinned.bin"
	store := divergedStore(localPath, entry.Inode, fakeStat{size: 7, mtime: time.Unix(10, 0)}, func(info *cfapi.CFPlaceholderStandardInfo) {
		info.PinState = cfapi.PinStatePinned
	})
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)
	hook := &logCapture{}
	logger.AddHook(hook)

	fc := p.checkFreshness(entry, "/pinned.bin", localPath)

	if fc.counts.pinned != 1 || fc.counts.total() != 1 {
		t.Errorf("счётчики: %+v, ожидается pinned=1", fc.counts)
	}
	if n := len(store.updateCalls()); n != 0 {
		t.Errorf("pinned отревалидирован: %d вызовов Update", n)
	}
	if n := hook.count("pinned stale"); n != 1 {
		t.Errorf("строк Warn «pinned stale»: %d, ожидается 1", n)
	}
}

// TestCheckFreshnessBackendNewerOutOfSyncSkips — backend новее при записи
// вне in-sync — истинный конфликт обеих сторон: skip + Warn, без мутаций.
func TestCheckFreshnessBackendNewerOutOfSyncSkips(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "both.bin", Inode: 415, Size: 99, MtimeSec: 42}
	localPath := root + "\\both.bin"
	store := divergedStore(localPath, entry.Inode, fakeStat{size: 7, mtime: time.Unix(10, 0)}, func(info *cfapi.CFPlaceholderStandardInfo) {
		info.InSyncState = cfapi.InSyncStateNotInSync
	})
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)

	fc := p.checkFreshness(entry, "/both.bin", localPath)

	if fc.counts.outOfSync != 1 || fc.counts.total() != 1 {
		t.Errorf("счётчики: %+v, ожидается out-of-sync=1", fc.counts)
	}
	if n := len(store.updateCalls()); n != 0 {
		t.Errorf("конфликт отревалидирован: %d вызовов Update", n)
	}
}

// TestCheckFreshnessAmbiguousSkipsNoMutations — спека «Двусторонняя правка»:
// размер разошёлся при |Δmtime| ≤ допуск — пропуск с предупреждением, без
// мутаций и очередей.
func TestCheckFreshnessAmbiguousSkipsNoMutations(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "amb.bin", Inode: 416, Size: 99, MtimeSec: 10}
	localPath := root + "\\amb.bin"
	store := divergedStore(localPath, entry.Inode, fakeStat{size: 7, mtime: time.Unix(11, 0)}, nil)
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)
	db := newIngestDebouncer(time.Millisecond, wallClock{})
	p.ingestDb = db
	hook := &logCapture{}
	logger.AddHook(hook)

	fc := p.checkFreshness(entry, "/amb.bin", localPath)

	if fc.verdict != freshAmbiguous || fc.counts.ambiguous != 1 || fc.counts.total() != 1 {
		t.Errorf("verdict=%v счётчики: %+v, ожидается ambiguous=1", fc.verdict, fc.counts)
	}
	select {
	case got := <-db.reqs:
		t.Errorf("ambiguous поставил выгрузку: %+v", got)
	default:
	}
	if n := len(store.updateCalls()); n != 0 {
		t.Errorf("ambiguous отревалидирован: %d вызовов Update", n)
	}
	if n := hook.count("both sides diverged"); n != 1 {
		t.Errorf("строк Warn ambiguous: %d, ожидается 1", n)
	}
}

// TestCheckFreshnessStateUnavailableSkips — STANDARD-запрос не дал ответа на
// разошедшейся версии: консервативный skip без действий и счётчика (повтор
// на следующем walk).
func TestCheckFreshnessStateUnavailableSkips(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "noh.json", Inode: 417, Size: 99, MtimeSec: 42}
	localPath := root + "\\noh.json"
	store := divergedStore(localPath, entry.Inode, fakeStat{size: 7, mtime: time.Unix(10, 0)}, nil)
	store.standardOfErr = map[string]error{localPath: errFake("fake: sharing violation")}
	p, _ := newTestProvider(t, root, &fakeBackend{}, store)

	fc := p.checkFreshness(entry, "/noh.json", localPath)

	if fc.counts.total() != 0 {
		t.Errorf("счётчики не пусты: %+v", fc.counts)
	}
	if n := len(store.updateCalls()); n != 0 {
		t.Errorf("недоказанное состояние отревалидировано: %d вызовов Update", n)
	}
}

// TestCheckFreshnessAdoptDivergedSkipsNormalization — усыновление разошедшейся
// записи (identity своя, локальная версия новее): нормализация-дегидратация
// НЕ выполняется (равные версии — условие дегидратации), запись усыновляется
// (idx.Put), вердикт попадает в счётчики очереди выгрузки.
func TestCheckFreshnessAdoptDivergedSkipsNormalization(t *testing.T) {
	root := t.TempDir()
	entry := BackendEntry{Name: "adopt-newer.bin", Inode: 418, Size: 5, MtimeSec: 10}
	localPath := root + "\\adopt-newer.bin"
	store := divergedStore(localPath, entry.Inode, fakeStat{size: 5, mtime: time.Unix(15, 0)}, nil)
	p, idx := newTestProvider(t, root, &fakeBackend{}, store)
	db := newIngestDebouncer(time.Millisecond, wallClock{})
	p.ingestDb = db
	res := &dirPopulation{dir: "/"}
	examples := []string{}

	classifyOccupied(p, res, entry, "/adopt-newer.bin", localPath, true, &examples)

	if got, ok := idx.Path(entry.Inode); !ok || got != "/adopt-newer.bin" {
		t.Errorf("idx.Path(%d) = (%q, %v), ожидается усыновление", entry.Inode, got, ok)
	}
	if n := len(store.updateCalls()); n != 0 {
		t.Errorf("разошедшаяся запись дегидратирована при усыновлении: %d вызовов Update", n)
	}
	if res.fresh.queuedUpload != 1 {
		t.Errorf("счётчики: %+v, ожидается queued-upload=1", res.fresh)
	}
	if res.adopted != 1 {
		t.Errorf("adopted = %d, ожидается 1", res.adopted)
	}
}
