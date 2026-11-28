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

// Тесты движка cleanup (fix-cfapi-followups A4, задача 4.3) — целиком на
// подменных реализациях CleanupFS/CleanupDetector/CleanupReverter/
// CleanupRegistrar/CleanupMutexes: реальный диск и cldapi.dll не нужны
// (инвариант «Движок проверяется без CfAPI»). Классификация L/P/N/U,
// dry-run без мутаций, apply удаляет только подтверждённо-reverted,
// каталоги снизу вверх и kept, отказы (mutex/NTFS/нет корня/корень тома/
// вложенный живой корень/зарегистрирован без --unregister), --unregister до
// revert'ов, идемпотентность, state-dir не трогается.

package cfprovider

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

const cleanupTestRoot = `C:\cleanup-test`

// fakeNode — запись фейкового дерева.
type fakeNode struct {
	isDir  bool
	isLink bool
	attrs  uint32 // FILE_ATTRIBUTE_* для fallback-классификации
	size   int64
}

// fakeDirEntry — fs.DirEntry над fakeNode.
type fakeDirEntry struct {
	node *fakeNode
	name string
}

func (e fakeDirEntry) Name() string { return e.name }

func (e fakeDirEntry) IsDir() bool { return e.node.isDir && !e.node.isLink }

func (e fakeDirEntry) Type() fs.FileMode {
	if e.node.isLink {
		return fs.ModeSymlink
	}
	if e.node.isDir {
		return fs.ModeDir
	}
	return 0
}

func (e fakeDirEntry) Info() (fs.FileInfo, error) {
	return fakeFileInfo{node: e.node, name: e.name}, nil
}

// fakeFileInfo — fs.FileInfo над fakeNode; Sys отдаёт атрибуты Windows.
type fakeFileInfo struct {
	node *fakeNode
	name string
}

func (i fakeFileInfo) Name() string { return i.name }
func (i fakeFileInfo) Size() int64  { return i.node.size }
func (i fakeFileInfo) Mode() fs.FileMode {
	if i.node.isLink {
		return fs.ModeSymlink
	}
	if i.node.isDir {
		return fs.ModeDir
	}
	return 0
}
func (i fakeFileInfo) ModTime() time.Time { return time.Unix(1700000000, 0) }
func (i fakeFileInfo) IsDir() bool        { return i.node.isDir && !i.node.isLink }
func (i fakeFileInfo) Sys() any {
	return &syscall.Win32FileAttributeData{FileAttributes: i.node.attrs}
}

// fakeCleanupFS — CleanupFS над деревом в памяти.
type fakeCleanupFS struct {
	entries     map[string]*fakeNode
	fsName      string
	removeTries []string // все попытки Remove
	removed     []string // успешные Remove
}

func (s *fakeCleanupFS) Stat(name string) (fs.FileInfo, error) {
	n, ok := s.entries[filepath.Clean(name)]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: syscall.ENOENT}
	}
	return fakeFileInfo{node: n, name: filepath.Base(name)}, nil
}

func (s *fakeCleanupFS) FileSystem(path string) (string, error) {
	return s.fsName, nil
}

func (s *fakeCleanupFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	return s.walk(filepath.Clean(root), fn)
}

func (s *fakeCleanupFS) walk(dir string, fn fs.WalkDirFunc) error {
	n, ok := s.entries[dir]
	if !ok {
		return fn(dir, nil, &fs.PathError{Op: "open", Path: dir, Err: syscall.ENOENT})
	}
	base := filepath.Base(dir)
	if dir == filepath.Clean(dir) && strings.Count(dir, `\`) == 1 {
		base = dir // корень тома — имя с разделителем (как у filepath.WalkDir)
	}
	if err := fn(dir, fakeDirEntry{node: n, name: base}, nil); err != nil {
		if err == fs.SkipDir && n.isDir {
			return nil
		}
		return err
	}
	if !n.isDir || n.isLink {
		return nil
	}
	prefix := dir + `\`
	var children []string
	for p := range s.entries {
		if strings.HasPrefix(p, prefix) && !strings.Contains(strings.TrimPrefix(p, prefix), `\`) {
			children = append(children, p)
		}
	}
	sort.Strings(children)
	for _, c := range children {
		if err := s.walk(c, fn); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeCleanupFS) Remove(name string) error {
	name = filepath.Clean(name)
	s.removeTries = append(s.removeTries, name)
	n, ok := s.entries[name]
	if !ok {
		return &fs.PathError{Op: "remove", Path: name, Err: syscall.ENOENT}
	}
	if n.isDir {
		prefix := name + `\`
		for p := range s.entries {
			if strings.HasPrefix(p, prefix) {
				return &fs.PathError{Op: "remove", Path: name, Err: syscall.Errno(windows.ERROR_DIR_NOT_EMPTY)}
			}
		}
	}
	delete(s.entries, name)
	s.removed = append(s.removed, name)
	return nil
}

// fakeCleanupDetector — STANDARD-классификация: карта плейсхолдеров и карта
// «жёстких» ошибок (иных, чем сентинел «не плейсхолдер»). Отсутствие в
// обеих — не плейсхолдер. sticky — записи, остающиеся плейсхолдерами после
// revert (пост-проверка обязана их поймать).
type fakeCleanupDetector struct {
	ph      map[string]*cfapi.CFPlaceholderStandardInfo
	hardErr map[string]error
	sticky  map[string]bool
}

func (d *fakeCleanupDetector) StandardInfoOf(path string) (*cfapi.CFPlaceholderStandardInfo, bool, error) {
	path = filepath.Clean(path)
	if e, ok := d.hardErr[path]; ok {
		return nil, false, e
	}
	if d.sticky[path] {
		return &cfapi.CFPlaceholderStandardInfo{}, true, nil
	}
	info, ok := d.ph[path]
	if !ok {
		return nil, false, nil
	}
	return info, true, nil
}

// fakeCleanupReverter — revert: удаляет запись из карты плейсхолдеров
// (модель «стал plain»); отказы задаются картой fail (failAll — мёртвый
// провайдер: отказ для всех записей, HRESULT 0x80070194); журнал вызовов —
// общий с регистром unregister для проверки порядка.
type fakeCleanupReverter struct {
	det     *fakeCleanupDetector
	fail    map[string]error
	failAll bool
	log     *[]string
}

func (r *fakeCleanupReverter) RevertPlaceholder(path string, flags uint32) error {
	path = filepath.Clean(path)
	*r.log = append(*r.log, "revert "+path)
	if r.failAll {
		// Мёртвый провайдер: фильтр отвечает 0x80070194 на любую запись.
		return &cfapi.HRESULTError{Call: "CfRevertPlaceholder", Code: cfapi.HRESULT(-2147024492)} // 0x80070194
	}
	if err, ok := r.fail[path]; ok {
		return err
	}
	if _, ok := r.det.ph[path]; !ok {
		return &cfapi.HRESULTError{Call: "CfRevertPlaceholder", Code: cfapi.HRESULT(-2147024520)} // 0x80070178
	}
	delete(r.det.ph, path)
	return nil
}

// fakeCleanupRegistrar — регистрация и unregister с журналом порядка.
type fakeCleanupRegistrar struct {
	registered      bool
	unregisterErr   error
	unregisterCalls int
	log             *[]string
}

func (r *fakeCleanupRegistrar) Registered(root string) bool { return r.registered }

func (r *fakeCleanupRegistrar) Unregister(root string) error {
	r.unregisterCalls++
	*r.log = append(*r.log, "unregister "+root)
	return r.unregisterErr
}

// fakeCleanupMutexes — R2: live — корни с живым провайдером (Hold отказывает
// и Probe отвечает «живой»), held — удержание движка.
type fakeCleanupMutexes struct {
	live map[string]bool
	held string
}

func (m *fakeCleanupMutexes) Hold(root string) error {
	root = filepath.Clean(root)
	if m.live[root] || m.held == root {
		return fmt.Errorf("another provider instance for %s is already running (mutex)", root)
	}
	m.held = root
	return nil
}

func (m *fakeCleanupMutexes) Probe(root string) error {
	root = filepath.Clean(root)
	if m.live[root] || m.held == root {
		return fmt.Errorf("provider mutex exists (a live provider for %s)", root)
	}
	return nil
}

func (m *fakeCleanupMutexes) Release() { m.held = "" }

// fakeStdInfo — STANDARD-запись для карты плейсхолдеров.
func fakeStdInfo(fileID int64, inSync, pin uint32, onDisk int64) *cfapi.CFPlaceholderStandardInfo {
	return &cfapi.CFPlaceholderStandardInfo{
		FileId:         fileID,
		InSyncState:    inSync,
		PinState:       pin,
		OnDiskDataSize: onDisk,
	}
}

// cleanupTestEnv — стандартное дерево тестов (под корнем cleanupTestRoot):
// plain-файлы и каталог, ссылки, плейсхолдеры (дегидратированные и
// гидратированный), каталоги-плейсхолдеры (удаляемый и с plain-ребёнком),
// запись с «жёсткой» ошибкой STANDARD (unknown) и каталог состояния.
type cleanupTestEnv struct {
	fs   *fakeCleanupFS
	det  *fakeCleanupDetector
	rev  *fakeCleanupReverter
	reg  *fakeCleanupRegistrar
	mut  *fakeCleanupMutexes
	deps *CleanupDeps
	log  *[]string
}

func newCleanupTestEnv(t *testing.T) *cleanupTestEnv {
	t.Helper()
	e := &cleanupTestEnv{log: &[]string{}}
	e.det = &fakeCleanupDetector{
		ph: map[string]*cfapi.CFPlaceholderStandardInfo{
			filePath(`ph-dehyd.txt`):   fakeStdInfo(11, cfapi.InSyncStateInSync, cfapi.PinStateUnpinned, 0),
			filePath(`ph-hydr.txt`):    fakeStdInfo(12, cfapi.InSyncStateInSync, cfapi.PinStateUnpinned, 100),
			filePath(`sub\ph.txt`):     fakeStdInfo(13, cfapi.InSyncStateInSync, cfapi.PinStateUnpinned, 0),
			filePath(`phdir\deep.txt`): fakeStdInfo(14, cfapi.InSyncStateNotInSync, cfapi.PinStateUnpinned, 0),
			filePath(`phdir`):          fakeStdInfo(21, cfapi.InSyncStateInSync, cfapi.PinStateUnpinned, 0),
			filePath(`phdir-kept`):     fakeStdInfo(22, cfapi.InSyncStateInSync, cfapi.PinStateUnpinned, 0),
		},
		hardErr: map[string]error{
			filePath(`unknown.txt`): errors.New("sharing violation"),
		},
		sticky: map[string]bool{},
	}
	e.rev = &fakeCleanupReverter{det: e.det, fail: map[string]error{}, log: e.log}
	e.reg = &fakeCleanupRegistrar{log: e.log}
	e.mut = &fakeCleanupMutexes{live: map[string]bool{}}
	e.fs = &fakeCleanupFS{
		entries: map[string]*fakeNode{
			cleanupTestRoot:                  {isDir: true, attrs: windows.FILE_ATTRIBUTE_DIRECTORY},
			filePath(`keep.txt`):             {attrs: windows.FILE_ATTRIBUTE_NORMAL, size: 5},
			filePath(`ph-dehyd.txt`):         {attrs: windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS, size: 100},
			filePath(`ph-hydr.txt`):          {attrs: windows.FILE_ATTRIBUTE_NORMAL, size: 100},
			filePath(`unknown.txt`):          {attrs: windows.FILE_ATTRIBUTE_NORMAL, size: 1},
			filePath(`lnk.txt`):              {isLink: true, attrs: windows.FILE_ATTRIBUTE_REPARSE_POINT},
			filePath(`sub`):                  {isDir: true, attrs: windows.FILE_ATTRIBUTE_DIRECTORY},
			filePath(`sub\ph.txt`):           {attrs: windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS, size: 50},
			filePath(`sub\keep2.txt`):        {attrs: windows.FILE_ATTRIBUTE_NORMAL, size: 3},
			filePath(`phdir`):                {isDir: true, attrs: windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_RECALL_ON_OPEN},
			filePath(`phdir\deep.txt`):       {attrs: windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS, size: 50},
			filePath(`phdir-kept`):           {isDir: true, attrs: windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_RECALL_ON_OPEN},
			filePath(`phdir-kept\keep3.txt`): {attrs: windows.FILE_ATTRIBUTE_NORMAL, size: 2},
			filePath(`st`):                   {isDir: true, attrs: windows.FILE_ATTRIBUTE_DIRECTORY},
			filePath(`st\state.bin`):         {attrs: windows.FILE_ATTRIBUTE_NORMAL, size: 9},
		},
		fsName: "NTFS",
	}
	e.deps = &CleanupDeps{FS: e.fs, Detector: e.det, Reverter: e.rev, Registrar: e.reg, Mutexes: e.mut}
	return e
}

// filePath — путь записи в тестовом дереве.
func filePath(rel string) string {
	return cleanupTestRoot + `\` + rel
}

// runCleanup — запуск движка с перехватом отчёта.
func runCleanup(t *testing.T, e *cleanupTestEnv, mutate func(*CleanupOptions)) (*CleanupReport, string, error) {
	t.Helper()
	var buf bytes.Buffer
	opts := CleanupOptions{Root: cleanupTestRoot, StateDir: filePath(`st`), Out: &buf}
	if mutate != nil {
		mutate(&opts)
	}
	rep, err := RunCleanup(e.deps, opts)
	return rep, buf.String(), err
}

// TestCleanupClassifyAndDryRun — классификация L/P/N/U по дереву и dry-run
// без единой мутации (спека «Dry-run без мутаций»).
func TestCleanupClassifyAndDryRun(t *testing.T) {
	e := newCleanupTestEnv(t)
	rep, out, err := runCleanup(t, e, nil)
	if err != nil {
		t.Fatalf("dry-run завершился ошибкой: %v", err)
	}
	if rep.Placeholders != 6 {
		t.Errorf("Placeholders = %d, ожидается 6", rep.Placeholders)
	}
	if rep.Dehydrated != 3 || rep.Hydrated != 1 {
		t.Errorf("Dehydrated/Hydrated = %d/%d, ожидается 3/1", rep.Dehydrated, rep.Hydrated)
	}
	if rep.Plain != 4 {
		t.Errorf("Plain = %d, ожидается 4 (keep.txt, keep2.txt, keep3.txt, каталог sub)", rep.Plain)
	}
	if rep.Unknown != 1 {
		t.Errorf("Unknown = %d, ожидается 1 (жёсткая ошибка STANDARD без RECALL-атрибутов)", rep.Unknown)
	}
	if rep.Links != 1 {
		t.Errorf("Links = %d, ожидается 1", rep.Links)
	}
	if rep.Files != 8 || rep.Dirs != 3 {
		t.Errorf("Files/Dirs = %d/%d, ожидается 8/3", rep.Files, rep.Dirs)
	}
	if rep.PlanRevertFiles != 4 || rep.PlanRevertDirs != 2 || rep.PlanDeleteFiles != 0 || rep.PlanRemoveDirs != 0 {
		t.Errorf("план files=%d/%d dirs=%d/%d, ожидается revert 4/2, delete/remove 0/0",
			rep.PlanRevertFiles, rep.PlanDeleteFiles, rep.PlanRevertDirs, rep.PlanRemoveDirs)
	}
	if !rep.MutexHeld {
		t.Error("mutex не удержан в dry-run")
	}
	// Ноль мутаций: реестр не тронут, revert'ов нет, записи на месте.
	if rep.Unregistered || e.reg.unregisterCalls != 0 {
		t.Error("dry-run снял регистрацию")
	}
	if len(*e.rev.log) != 0 {
		t.Errorf("dry-run вызвал revert: %v", e.rev.log)
	}
	if len(e.fs.removeTries) != 0 {
		t.Errorf("dry-run вызвал Remove: %v", e.fs.removeTries)
	}
	if len(e.det.ph) != 6 {
		t.Errorf("dry-run изменил классификацию: осталось %d плейсхолдеров, ожидается 6", len(e.det.ph))
	}
	if !strings.Contains(out, "dry-run") || !strings.Contains(out, "NTFS") {
		t.Errorf("отчёт без заголовка режима/ФС:\n%s", out)
	}
}

// TestCleanupApplyRevertNoDelete — --apply revert'ит все плейсхолдеры и не
// удаляет ни одной записи; plain/ссылки не трогаются (спека «Уровни
// применения», уровень --apply).
func TestCleanupApplyRevertNoDelete(t *testing.T) {
	e := newCleanupTestEnv(t)
	rep, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
	})
	if err != nil {
		t.Fatalf("apply завершился ошибкой: %v", err)
	}
	if rep.RevertedFiles != 4 || rep.RevertedDirs != 2 {
		t.Errorf("RevertedFiles/Dirs = %d/%d, ожидается 4/2", rep.RevertedFiles, rep.RevertedDirs)
	}
	if rep.RemovedFiles != 0 || rep.RemovedDirs != 0 || len(e.fs.removeTries) != 0 {
		t.Errorf("без --delete-files есть удаления: files=%d dirs=%d tries=%v",
			rep.RemovedFiles, rep.RemovedDirs, e.fs.removeTries)
	}
	for _, p := range []string{`ph-dehyd.txt`, `ph-hydr.txt`, `sub\ph.txt`, `phdir\deep.txt`, `phdir`, `phdir-kept`} {
		if _, ok := e.det.ph[filePath(p)]; ok {
			t.Errorf("плейсхолдер %s не reverted", p)
		}
	}
	for _, p := range []string{`keep.txt`, `sub\keep2.txt`, `phdir-kept\keep3.txt`, `lnk.txt`, `unknown.txt`} {
		if _, ok := e.fs.entries[filePath(p)]; !ok {
			t.Errorf("нетронутая запись %s удалена", p)
		}
	}
	if e.reg.unregisterCalls != 0 {
		t.Error("unregister вызван без --unregister")
	}
}

// TestCleanupApplyDeleteFiles — --apply --delete-files: удаление только
// подтверждённо-reverted; каталоги снизу вверх; каталог с plain-ребёнком —
// kept (спека «Revert и удаление»).
func TestCleanupApplyDeleteFiles(t *testing.T) {
	e := newCleanupTestEnv(t)
	rep, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
		o.DeleteFiles = true
	})
	if err != nil {
		t.Fatalf("apply --delete-files завершился ошибкой: %v", err)
	}
	if rep.RemovedFiles != 4 {
		t.Errorf("RemovedFiles = %d, ожидается 4 (только reverted)", rep.RemovedFiles)
	}
	if rep.RemovedDirs != 1 || rep.KeptDirs != 1 {
		t.Errorf("RemovedDirs/KeptDirs = %d/%d, ожидается 1/1 (phdir удалён, phdir-kept с plain-ребёнком оставлен)",
			rep.RemovedDirs, rep.KeptDirs)
	}
	for _, p := range []string{`ph-dehyd.txt`, `ph-hydr.txt`, `sub\ph.txt`, `phdir\deep.txt`, `phdir`} {
		if _, ok := e.fs.entries[filePath(p)]; ok {
			t.Errorf("reverted-запись %s не удалена", p)
		}
	}
	if _, ok := e.fs.entries[filePath(`phdir-kept`)]; !ok {
		t.Error("непустой каталог phdir-kept удалён, ожидается kept")
	}
	if _, ok := e.fs.entries[filePath(`phdir-kept\keep3.txt`)]; !ok {
		t.Error("plain-ребёнок kept-каталога удалён")
	}
	// Порядок «снизу вверх»: дочерний плейсхолдер phdir\deep.txt удалён до
	// самого phdir (иначе Remove каталога отверг бы удаление).
	deepIdx := indexOf(e.fs.removed, filePath(`phdir\deep.txt`))
	dirIdx := indexOf(e.fs.removed, filePath(`phdir`))
	if deepIdx == -1 || dirIdx == -1 || deepIdx > dirIdx {
		t.Errorf("порядок удаления снизу вверх нарушен: removed=%v", e.fs.removed)
	}
}

// TestCleanupApplyDirRevertBlocked — отказ revert'а hydrated-потомка
// блокирует revert каталога: каталог остаётся плейсхолдером и не удаляется.
// (Потомок hydrated: отказ revert'а сохраняет запись — dehydrated-потомок
// при --delete-files был бы удалён fallback'ом прямого remove, см.
// TestCleanupDehydratedFallbackDirectRemove.)
func TestCleanupApplyDirRevertBlocked(t *testing.T) {
	e := newCleanupTestEnv(t)
	e.det.ph[filePath(`phdir\deep.txt`)] = fakeStdInfo(14, cfapi.InSyncStateNotInSync, cfapi.PinStateUnpinned, 50) // hydrated
	e.rev.fail[filePath(`phdir\deep.txt`)] = errors.New("HRESULT 0x8007017c")
	rep, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
		o.DeleteFiles = true
	})
	if err == nil {
		t.Fatal("apply с отказом revert'а обязан вернуть ошибку")
	}
	if rep.RevertedDirs != 1 {
		t.Errorf("RevertedDirs = %d, ожидается 1 (только phdir-kept; phdir заблокирован потомком)", rep.RevertedDirs)
	}
	if _, ok := e.det.ph[filePath(`phdir`)]; !ok {
		t.Error("каталог phdir reverted несмотря на неот reverтированного потомка")
	}
	if _, ok := e.fs.entries[filePath(`phdir`)]; !ok {
		t.Error("заблокированный каталог удалён")
	}
	if _, ok := e.det.ph[filePath(`phdir\deep.txt`)]; !ok {
		t.Error("отказавший потомок числится reverted")
	}
	if _, ok := e.fs.entries[filePath(`phdir\deep.txt`)]; !ok {
		t.Error("hydrated-потомок с отказавшим revert'ом удалён")
	}
}

// TestCleanupDehydratedFallbackDirectRemove — revert всегда отказывает
// (мёртвый провайдер, HRESULT 0x80070194 — orphan-сценарий, репроба U-C4):
// доказанно dehydrated-сироты с --delete-files удаляются прямым os.Remove
// (removed-direct), опустевший каталог-плейсхолдер — тоже; hydrated-сирота
// не трогается, отказавший непустой каталог — kept.
func TestCleanupDehydratedFallbackDirectRemove(t *testing.T) {
	e := newCleanupTestEnv(t)
	e.rev.failAll = true
	rep, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
		o.DeleteFiles = true
	})
	if err == nil {
		t.Fatal("apply с мёртвым провайдером обязан вернуть ошибку (hydrated-сирота остаётся)")
	}
	// Dehydrated-файлы (ph-dehyd.txt, sub\ph.txt, phdir\deep.txt) удалены
	// напрямую, пустой каталог phdir — тоже (потомок обработан).
	if rep.RemovedDirect != 4 {
		t.Errorf("RemovedDirect = %d, ожидается 4 (3 dehydrated-файла + пустой каталог phdir)", rep.RemovedDirect)
	}
	if rep.RevertedFiles != 0 || rep.RevertedDirs != 0 || rep.RemovedFiles != 0 || rep.RemovedDirs != 0 {
		t.Errorf("revert'ы не должны были пройти: reverted files=%d dirs=%d removed files=%d dirs=%d",
			rep.RevertedFiles, rep.RevertedDirs, rep.RemovedFiles, rep.RemovedDirs)
	}
	for _, p := range []string{`ph-dehyd.txt`, `sub\ph.txt`, `phdir\deep.txt`, `phdir`} {
		if _, ok := e.fs.entries[filePath(p)]; ok {
			t.Errorf("dehydrated-сирота %s не удалена прямым remove", p)
		}
	}
	// hydrated-сирота не тронута (может нести локальный контент).
	if _, ok := e.fs.entries[filePath(`ph-hydr.txt`)]; !ok {
		t.Error("hydrated-сирота удалена, ожидается сохранение")
	}
	if _, ok := e.det.ph[filePath(`ph-hydr.txt`)]; !ok {
		t.Error("hydrated-сирота числится reverted")
	}
	// phdir-kept: revert отказал, прямой remove непустого → kept + отказ.
	if _, ok := e.fs.entries[filePath(`phdir-kept`)]; !ok {
		t.Error("непустой каталог phdir-kept удалён")
	}
	if _, ok := e.fs.entries[filePath(`phdir-kept\keep3.txt`)]; !ok {
		t.Error("plain-ребёнок kept-каталога удалён")
	}
	if rep.KeptDirs != 1 {
		t.Errorf("KeptDirs = %d, ожидается 1 (phdir-kept)", rep.KeptDirs)
	}
	if rep.Errors == 0 || len(rep.ApplyErrors) == 0 {
		t.Errorf("отказы (hydrated-сирота, kept-каталог) не посчитаны: errors=%d hist=%v",
			rep.Errors, rep.ApplyErrors)
	}
}

// TestCleanupHydratedRevertFailureKeepsError — отказ revert'а hydrated-сироты
// сохраняется с rescue-указанием; запись не тронута (может нести локальный
// контент, которого нет в backend).
func TestCleanupHydratedRevertFailureKeepsError(t *testing.T) {
	e := newCleanupTestEnv(t)
	e.rev.fail[filePath(`ph-hydr.txt`)] = errors.New("HRESULT 0x80070194")
	rep, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
		o.DeleteFiles = true
	})
	if err == nil {
		t.Fatal("apply с отказавшим revert'ом hydrated-сироты обязан вернуть ошибку")
	}
	if _, ok := e.fs.entries[filePath(`ph-hydr.txt`)]; !ok {
		t.Error("hydrated-сирота тронута, ожидается сохранение")
	}
	if _, ok := e.det.ph[filePath(`ph-hydr.txt`)]; !ok {
		t.Error("hydrated-сирота числится reverted")
	}
	found := false
	for msg := range rep.ApplyErrors {
		if strings.Contains(msg, "hydrated: local-only content") {
			found = true
		}
	}
	if !found {
		t.Errorf("гистограмма без rescue-указания: %v", rep.ApplyErrors)
	}
	if rep.RemovedDirect != 0 {
		t.Errorf("RemovedDirect = %d, ожидается 0 (hydrated без fallback)", rep.RemovedDirect)
	}
}

// TestCleanupPostCheckKeepsFile — revert без пост-подтверждения «стал plain»
// (R4): файл не удаляется даже с --delete-files.
func TestCleanupPostCheckKeepsFile(t *testing.T) {
	e := newCleanupTestEnv(t)
	e.det.sticky[filePath(`ph-dehyd.txt`)] = true
	rep, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
		o.DeleteFiles = true
	})
	if err == nil {
		t.Fatal("apply с непройденной пост-проверкой обязан вернуть ошибку")
	}
	if rep.RevertedFiles != 3 {
		t.Errorf("RevertedFiles = %d, ожидается 3 (sticky-запись не подтверждена)", rep.RevertedFiles)
	}
	if _, ok := e.fs.entries[filePath(`ph-dehyd.txt`)]; !ok {
		t.Error("неподтверждённая запись удалена")
	}
}

// TestCleanupByAttributesFallback — при жёсткой ошибке STANDARD записи с
// RECALL-атрибутами классифицируются P by-attributes (RECALL_ON_DATA_ACCESS
// у файла, RECALL_ON_OPEN у каталога) и попадают в план.
func TestCleanupByAttributesFallback(t *testing.T) {
	e := newCleanupTestEnv(t)
	// Дегидратированный плейсхолдер несёт RECALL_ON_DATA_ACCESS; каталог-
	// плейсхолдер — RECALL_ON_OPEN: при жёсткой ошибке STANDARD оба
	// классифицируются P by-attributes. Гидратированный плейсхолдер
	// (атрибуты чистые) в ту же ошибку ушёл бы в unknown — сравнение ниже.
	e.det.hardErr[filePath(`ph-dehyd.txt`)] = errors.New("sharing violation")
	e.det.hardErr[filePath(`phdir`)] = errors.New("sharing violation")
	e.det.hardErr[filePath(`ph-hydr.txt`)] = errors.New("sharing violation")
	rep, _, err := runCleanup(t, e, nil)
	if err != nil {
		t.Fatalf("dry-run завершился ошибкой: %v", err)
	}
	if rep.ByAttributes != 2 {
		t.Errorf("ByAttributes = %d, ожидается 2 (файл RECALL_ON_DATA_ACCESS + каталог RECALL_ON_OPEN)", rep.ByAttributes)
	}
	// P = 5: два by-attributes (ph-dehyd.txt, phdir) + три подтверждённых
	// (sub\ph.txt, phdir\deep.txt, phdir-kept); ph-hydr.txt ушёл в unknown.
	if rep.Placeholders != 5 {
		t.Errorf("Placeholders = %d, ожидается 5 (by-attributes входят в P)", rep.Placeholders)
	}
	if rep.Unknown != 2 {
		t.Errorf("Unknown = %d, ожидается 2 (unknown.txt и гидратированный без RECALL-атрибутов)", rep.Unknown)
	}
}

// TestCleanupDryRunNestedLiveRootSkips — вложенный живой корень: dry-run
// пропускает поддерево и перечисляет его в отчёте, мутаций нет.
func TestCleanupDryRunNestedLiveRootSkips(t *testing.T) {
	e := newCleanupTestEnv(t)
	e.mut.live[filePath(`sub`)] = true
	rep, out, err := runCleanup(t, e, nil)
	if err != nil {
		t.Fatalf("dry-run с вложенным живым корнем не должен отказывать: %v", err)
	}
	if len(rep.NestedRoots) != 1 || rep.NestedRoots[0] != filePath(`sub`) {
		t.Errorf("NestedRoots = %v, ожидается [%s]", rep.NestedRoots, filePath(`sub`))
	}
	// Поддерево sub пропущено: его плейсхолдер и plain-ребёнок не классифицированы.
	if rep.Placeholders != 5 {
		t.Errorf("Placeholders = %d, ожидается 5 (sub пропущен)", rep.Placeholders)
	}
	if rep.Plain != 2 {
		t.Errorf("Plain = %d, ожидается 2 (sub и его plain-ребёнок keep2.txt пропущены)", rep.Plain)
	}
	if !strings.Contains(out, "Nested live sync roots: 1") {
		t.Errorf("отчёт не перечисляет вложенный живой корень:\n%s", out)
	}
}

// TestCleanupApplyNestedLiveRootRefuses — apply при вложенном живом корне —
// отказ до первой мутации.
func TestCleanupApplyNestedLiveRootRefuses(t *testing.T) {
	e := newCleanupTestEnv(t)
	e.mut.live[filePath(`sub`)] = true
	_, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
	})
	if err == nil {
		t.Fatal("apply с вложенным живым корнем обязан отказать")
	}
	if len(*e.rev.log) != 0 || len(e.fs.removeTries) != 0 || e.reg.unregisterCalls != 0 {
		t.Error("apply с вложенным живым корнем мутировал")
	}
}

// TestCleanupRefuseMutexHeld — живой провайдер на корне: отказ в обоих
// режимах до мутаций (спека «Провайдер работает»).
func TestCleanupRefuseMutexHeld(t *testing.T) {
	for _, apply := range []bool{false, true} {
		e := newCleanupTestEnv(t)
		e.mut.live[cleanupTestRoot] = true
		rep, _, err := runCleanup(t, e, func(o *CleanupOptions) {
			o.Apply = apply
		})
		if err == nil {
			t.Fatalf("apply=%v: отказ при живом провайдере не получен", apply)
		}
		if !strings.Contains(err.Error(), "mutex") {
			t.Errorf("apply=%v: ошибка без упоминания mutex: %v", apply, err)
		}
		if rep.MutexHeld {
			t.Errorf("apply=%v: mutex числится удержанным", apply)
		}
		if len(*e.rev.log) != 0 || len(e.fs.removeTries) != 0 {
			t.Errorf("apply=%v: мутации при живом провайдере", apply)
		}
	}
}

// TestCleanupRefusePreconditions — отказы статических предусловий: не NTFS,
// корень отсутствует, корень — корень тома.
func TestCleanupRefusePreconditions(t *testing.T) {
	e := newCleanupTestEnv(t)
	e.fs.fsName = "FAT32"
	if _, _, err := runCleanup(t, e, nil); err == nil || !strings.Contains(err.Error(), "NTFS") {
		t.Errorf("не-NTFS не отвергнут: %v", err)
	}

	e = newCleanupTestEnv(t)
	delete(e.fs.entries, cleanupTestRoot)
	if _, _, err := runCleanup(t, e, nil); err == nil {
		t.Error("несуществующий корень не отвергнут")
	}

	e = newCleanupTestEnv(t)
	// Корень тома существует (предусловие существования идёт раньше) и
	// отвергается следующим предусловием.
	e.fs.entries[`C:\`] = &fakeNode{isDir: true, attrs: windows.FILE_ATTRIBUTE_DIRECTORY}
	if _, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Root = `C:\`
	}); err == nil || !strings.Contains(err.Error(), "volume root") {
		t.Errorf("корень тома не отвергнут: %v", err)
	}
}

// TestCleanupRegisteredRequiresUnregister — R7: apply на зарегистрированном
// корне без --unregister — отказ; с --unregister — снятие регистрации и
// применение.
func TestCleanupRegisteredRequiresUnregister(t *testing.T) {
	e := newCleanupTestEnv(t)
	e.reg.registered = true
	_, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
	})
	if err == nil || !strings.Contains(err.Error(), "--unregister") {
		t.Fatalf("R7 не сработал: %v", err)
	}
	if e.reg.unregisterCalls != 0 || len(*e.rev.log) != 0 {
		t.Error("отказ по R7 мутировал")
	}

	e = newCleanupTestEnv(t)
	e.reg.registered = true
	rep, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
		o.Unregister = true
	})
	if err != nil {
		t.Fatalf("apply с --unregister завершился ошибкой: %v", err)
	}
	if e.reg.unregisterCalls != 1 || !rep.Unregistered {
		t.Errorf("unregister не выполнен: calls=%d report=%v", e.reg.unregisterCalls, rep.Unregistered)
	}
	if rep.RevertedFiles != 4 {
		t.Errorf("RevertedFiles = %d, ожидается 4", rep.RevertedFiles)
	}
}

// TestCleanupUnregisterAfterRevert — --unregister выполняется ПОСЛЕДНИМ
// шагом apply (живая проба U-C1 28.11: CfUnregisterSyncRoot удаляет
// дегидратированные плейсхолдер-скелеты и материализует гидратированные —
// revert обязан идти на ещё зарегистрированном корне): unregister в журнале
// после последнего revert'а и выполняется, даже если часть revert'ов
// отказала.
func TestCleanupUnregisterAfterRevert(t *testing.T) {
	e := newCleanupTestEnv(t)
	e.reg.registered = true
	e.rev.fail[filePath(`ph-dehyd.txt`)] = errors.New("HRESULT 0x80071899")
	rep, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
		o.Unregister = true
	})
	if err == nil {
		t.Fatal("apply с отказами revert'ов обязан вернуть ошибку")
	}
	if e.reg.unregisterCalls != 1 {
		t.Fatalf("unregister не выполнен: calls=%d", e.reg.unregisterCalls)
	}
	unregIdx := indexOf(*e.log, "unregister "+cleanupTestRoot)
	firstRevert := -1
	lastRevert := -1
	for i, s := range *e.log {
		if strings.HasPrefix(s, "revert ") {
			if firstRevert == -1 {
				firstRevert = i
			}
			lastRevert = i
		}
	}
	if unregIdx == -1 || firstRevert == -1 || unregIdx < lastRevert {
		t.Errorf("unregister не последний шаг apply (после revert'ов): log=%v", *e.log)
	}
	if rep.Errors == 0 {
		t.Error("отказы revert'ов не посчитаны")
	}
	if len(rep.ApplyErrors) == 0 {
		t.Error("гистограмма отказов пуста")
	}
}

// TestCleanupIdempotentSecondRun — идемпотентность (спека «Идемпотентность»):
// после успешного apply --delete-files повторный dry-run показывает ноль
// плейсхолдеров и пустой план.
func TestCleanupIdempotentSecondRun(t *testing.T) {
	e := newCleanupTestEnv(t)
	if _, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
		o.DeleteFiles = true
	}); err != nil {
		t.Fatalf("первый apply завершился ошибкой: %v", err)
	}
	rep, _, err := runCleanup(t, e, nil)
	if err != nil {
		t.Fatalf("второй (dry-run) прогон завершился ошибкой: %v", err)
	}
	if rep.Placeholders != 0 || rep.PlanRevertFiles != 0 || rep.PlanRevertDirs != 0 {
		t.Errorf("повторный dry-run: placeholders=%d plan=%d/%d, ожидается 0/0/0",
			rep.Placeholders, rep.PlanRevertFiles, rep.PlanRevertDirs)
	}
}

// TestCleanupStateDirUntouched — каталог состояния не трогается: исключён из
// обхода и классификации (правило state-dir, design.md A4).
func TestCleanupStateDirUntouched(t *testing.T) {
	e := newCleanupTestEnv(t)
	rep, _, err := runCleanup(t, e, func(o *CleanupOptions) {
		o.Apply = true
		o.DeleteFiles = true
	})
	if err != nil {
		t.Fatalf("apply завершился ошибкой: %v", err)
	}
	if _, ok := e.fs.entries[filePath(`st\state.bin`)]; !ok {
		t.Error("запись state-dir удалена")
	}
	if _, ok := e.fs.entries[filePath(`st`)]; !ok {
		t.Error("каталог state-dir удалён")
	}
	for _, p := range e.fs.removed {
		if p == filePath(`st`) || p == filePath(`st\state.bin`) {
			t.Errorf("Remove вызван для state-dir: %s", p)
		}
	}
	if _, ok := e.det.ph[filePath(`st\state.bin`)]; ok {
		t.Error("state.bin классифицирован как плейсхолдер")
	}
	_ = rep
}

// indexOf — первый индекс значения в срезе (-1 — нет).
func indexOf(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return -1
}
