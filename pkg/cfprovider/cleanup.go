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

// Cleanup-режим осиротевших плейсхолдеров (fix-cfapi-followups A4,
// design.md A4): plan-then-act над локальным корнем БЕЗ подключённого
// провайдера. Классификация каждой записи — STANDARD-запрос
// (cfapi.StandardInfoOfPath, тот же сентинел «не плейсхолдер», что у
// классификации A1): L — symlink/junction (не трогается), P — плейсхолдер,
// N — обычная запись, U — прочее (только отчёт; при иной ошибке STANDARD —
// fallback по атрибутам DirEntry.Info(): RECALL_ON_DATA_ACCESS у файла /
// RECALL_ON_OPEN у каталога → P by-attributes).
//
// Уровни: dry-run (по умолчанию) — отчёт без мутаций; --apply — revert
// плейсхолдеров (RevertPlaceholderByPath RevertFlagNone) с пост-проверкой
// «стал plain»; --apply --delete-files — + удаление только
// подтверждённо-reverted записей и пустых reverted-каталогов (снизу вверх,
// os.Remove — рекурсивное удаление запрещено); --apply --delete-files
// --unregister — + снятие регистрации sync root и чистка реестра Explorer
// (хелпер из cmd) — ПОСЛЕДНИЙ шаг apply, после всех мутаций: живая проба
// U-C1 (28.11) показала, что CfUnregisterSyncRoot на зарегистрированном
// корне удаляет дегидратированные плейсхолдер-скелеты с диска и
// материализует гидратированные в plain — revert обязан выполняться на ещё
// зарегистрированном корне.
//
// Живой факт репробы U-C4 (28.11): revert — provider-mediated операция, на
// мёртвом провайдере фильтр отвечает HRESULT 0x80070194, а R2 запрещает
// cleanup при живом провайдере — в orphan-сценарии revert succeed'нуться не
// может. Fallback (только с --delete-files): доказанно dehydrated-сирота
// (STANDARD: dehydrated или пустой логический размер — контент записи в
// backend, прямое os.Remove безопасно) — прямое os.Remove с пост-проверкой
// отсутствия (счётчик removed-direct); hydrated — ошибка с rescue-указанием
// (может нести локальный контент, которого нет в backend); by-attributes
// записи (без STANDARD-доказательства) — без fallback. Каталоги: прямой
// os.Remove пустого каталога разрешён тому же правилу (потомки-плейсхолдеры
// подтверждённо обработаны), непустой/недоказанный — kept.
//
// Правила безопасности: R1 — мутации только с --apply; R2 — живой
// провайдер (mutex Local\juicefs-clouddrive-<san>, provider.go) → отказ в
// обоих режимах; mutex создаётся и держится до конца команды (закрывает
// гонку «serve стартовал между проверкой и revert»), вложенные живые корни
// — probe по каталогам; R3 — plain/unknown не трогаются; R4 — удаление
// только после подтверждённого revert (пост-проверка); R5 — сам корень не
// удаляется и не revert'ится; R7 — apply на зарегистрированном корне
// требует --unregister; каталог state-dir не трогается.
//
// Обращения к диску, cldapi.dll, mutex'ам и регистрации — только через
// интерфейсы CleanupFS/CleanupDetector/CleanupReverter/CleanupRegistrar/
// CleanupMutexes: движок проверяется на подменных реализациях без
// зарегистрированного sync root (инвариант «Движок проверяется без CfAPI»).
package cfprovider

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// cleanupKind — класс локальной записи (design.md A4, классификация).
type cleanupKind int

const (
	cleanupLink        cleanupKind = iota // L — symlink/junction: не трогается
	cleanupPlaceholder                    // P — плейсхолдер (STANDARD или атрибуты)
	cleanupPlain                          // N — обычная запись пользователя
	cleanupUnknown                        // U — недоказанное состояние: только отчёт
)

// String — имя класса для отчёта.
func (k cleanupKind) String() string {
	switch k {
	case cleanupLink:
		return "link"
	case cleanupPlaceholder:
		return "placeholder"
	case cleanupPlain:
		return "plain"
	default:
		return "unknown"
	}
}

// cleanupExamplesPerClass — предел примеров на класс в отчёте (design.md A4).
const cleanupExamplesPerClass = 20

// CleanupOptions — параметры запуска cleanup (флаги clouddrive cleanup).
type CleanupOptions struct {
	Root        string    // локальный путь корня
	Apply       bool      // --apply: мутации разрешены
	DeleteFiles bool      // --delete-files: удаление подтверждённо-reverted
	Unregister  bool      // --unregister: снятие регистрации перед revert'ами
	StateDir    string    // каталог состояния — не трогается (исключение из обхода)
	Out         io.Writer // отчёт (stdout команды; тесты — bytes.Buffer)
}

// CleanupReport — результаты прогона: заголовок, счётчики, план, итоги apply.
type CleanupReport struct {
	Root                string
	Apply               bool
	DeleteFiles         bool
	Unregister          bool // запрошен --unregister
	MutexName           string
	MutexHeld           bool
	FileSystem          string
	Registered          bool
	Unregistered        bool // регистрация снята в этом прогоне
	Files               int  // N/P/U файлы
	Dirs                int  // N/P/U каталоги
	Links               int  // L — symlink/junction
	Placeholders        int  // P всего (подтверждённые + by-attributes)
	Dehydrated          int  // из P: OnDiskDataSize == 0 при ненулевом логическом размере
	Hydrated            int  // из P: содержимое локально
	ByAttributes        int  // из P: классификация по атрибутам (STANDARD не дал ответа)
	Plain               int  // N
	Unknown             int  // U
	Errors              int  // ошибки обхода/классификации/apply
	NestedRoots         []string
	PlanRevertFiles     int
	PlanDeleteFiles     int
	PlanRevertDirs      int
	PlanRemoveDirs      int
	RevertedFiles       int
	RemovedFiles        int
	RevertedDirs        int
	RemovedDirs         int
	RemovedDirect       int // прямое os.Remove dehydrated-сирот при отказе revert'а (fallback, U-C4)
	KeptDirs            int // каталоги-кандидаты на удаление, оставленные (не пустые/занятые)
	ExamplesPlaceholder []string
	ExamplesPlain       []string
	ExamplesUnknown     []string
	ExamplesLink        []string
	ApplyErrors         map[string]int // гистограмма отказов apply: сообщение → счёт
}

// cleanupEntry — запись плана: классификация одной записи корня.
type cleanupEntry struct {
	path   string // полный локальный путь
	rel    string // относительно корня (отчёт)
	kind   cleanupKind
	isDir  bool
	size   int64 // логический размер (для hydration в отчёте)
	byAttr bool  // P по атрибутам (STANDARD не дал определённого ответа)
	info   *cfapi.CFPlaceholderStandardInfo
}

// CleanupFS — файловая система для движка cleanup (реально — os/*;
// подменная — тесты).
type CleanupFS interface {
	// Stat — os.Stat корня (существование/директорийность).
	Stat(name string) (fs.FileInfo, error)
	// FileSystem — имя ФС тома пути (NTFS-предусловие).
	FileSystem(path string) (string, error)
	// WalkDir — обход корня (filepath.WalkDir; корень первый, дети после
	// родителя — descending-сортировка плана каталогов идёт снизу вверх).
	WalkDir(root string, fn fs.WalkDirFunc) error
	// Remove — os.Remove (файлы и ПУСТЫЕ каталоги; рекурсивного удаления нет).
	Remove(name string) error
}

// CleanupDetector — STANDARD-классификация записи (семантика
// cfapi.StandardInfoOfPath: «не плейсхолдер» → (nil, false, nil)).
type CleanupDetector interface {
	StandardInfoOf(path string) (*cfapi.CFPlaceholderStandardInfo, bool, error)
}

// CleanupReverter — revert плейсхолдера по пути (cfapi.RevertPlaceholderByPath).
type CleanupReverter interface {
	RevertPlaceholder(path string, flags uint32) error
}

// CleanupRegistrar — регистрация sync root для движка cleanup.
type CleanupRegistrar interface {
	// Registered — зарегистрирован ли корень (CfGetSyncRootInfoByPath,
	// как isSyncRootRegistered при старте провайдера).
	Registered(root string) bool
	// Unregister — снятие регистрации и чистка ключа реестра Explorer
	// (хелпер из cmd; незарегистрированный корень — WARN у вызывающего).
	Unregister(root string) error
}

// CleanupMutexes — именованные mutex провайдера (правило R2): Hold — отказ
// при живом serve и удержание до конца команды; Probe — живой ли корень во
// вложении.
type CleanupMutexes interface {
	// Hold — создать mutex корня (уже существует → отказ) и держать до Release.
	Hold(root string) error
	// Probe — существует ли mutex корня (живой провайдер).
	Probe(root string) error
	// Release — отпустить удержание (идемпотентно).
	Release()
}

// CleanupDeps — окружение движка cleanup; все поля обязательны.
type CleanupDeps struct {
	FS        CleanupFS
	Detector  CleanupDetector
	Reverter  CleanupReverter
	Registrar CleanupRegistrar
	Mutexes   CleanupMutexes
}

// NewCleanupDeps — реальное окружение: локальный диск, cldapi.dll через
// pkg/cfapi, именованные mutex'ы провайдера, регистрация по фильтру.
// unregister — хелпер снятия регистрации с чисткой реестра Explorer
// (cmd/clouddrive.go; в тестах — подмена или nil: Unregister вернёт ошибку).
func NewCleanupDeps(unregister func(root string) error) *CleanupDeps {
	return &CleanupDeps{
		FS:        osCleanupFS{},
		Detector:  osCleanupDetector{},
		Reverter:  osCleanupReverter{},
		Registrar: cfCleanupRegistrar{unregister: unregister},
		Mutexes:   &osCleanupMutexes{},
	}
}

// osCleanupFS — CleanupFS на реальном диске.
type osCleanupFS struct{}

func (osCleanupFS) Stat(name string) (fs.FileInfo, error) {
	return os.Stat(name)
}

func (osCleanupFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	return filepath.WalkDir(root, fn)
}

func (osCleanupFS) Remove(name string) error {
	return os.Remove(name)
}

func (osCleanupFS) FileSystem(path string) (string, error) {
	return volumeFileSystemOf(path)
}

// volumeFileSystemOf — имя файловой системы тома, содержащего путь
// (GetVolumeInformation по корню тома).
func volumeFileSystemOf(path string) (string, error) {
	vol := filepath.VolumeName(path)
	if vol == "" {
		return "", fmt.Errorf("path %s has no volume", path)
	}
	volU16, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return "", err
	}
	var fsName [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeInformation(volU16, nil, 0, nil, nil, nil, &fsName[0], uint32(len(fsName))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(fsName[:]), nil
}

// osCleanupDetector — STANDARD-классификация через cfapi.StandardInfoOfPath
// (тот же сентинел «не плейсхолдер», что у классификации A1).
type osCleanupDetector struct{}

func (osCleanupDetector) StandardInfoOf(path string) (*cfapi.CFPlaceholderStandardInfo, bool, error) {
	return cfapi.StandardInfoOfPath(path)
}

// osCleanupReverter — revert через cfapi.RevertPlaceholderByPath.
type osCleanupReverter struct{}

func (osCleanupReverter) RevertPlaceholder(path string, flags uint32) error {
	return cfapi.RevertPlaceholderByPath(path, flags)
}

// cfCleanupRegistrar — регистрация по фильтру (isSyncRootRegistered,
// provider.go) и хелпер снятия регистрации, переданный из cmd.
type cfCleanupRegistrar struct {
	unregister func(root string) error
}

func (r cfCleanupRegistrar) Registered(root string) bool {
	return isSyncRootRegistered(cfapi.OS, root)
}

func (r cfCleanupRegistrar) Unregister(root string) error {
	if r.unregister == nil {
		return fmt.Errorf("unregister helper is not configured")
	}
	return r.unregister(root)
}

// osCleanupMutexes — R2: Hold создаёт mutex единственного экземпляра
// (acquireInstanceMutex: ERROR_ALREADY_EXISTS → отказ) и держит хендл до
// Release; Probe отвечает «живой ли провайдер на корне» (mutex существует).
type osCleanupMutexes struct {
	held windows.Handle
}

func (m *osCleanupMutexes) Hold(root string) error {
	h, err := acquireInstanceMutex(root)
	if err != nil {
		return err
	}
	m.held = h
	return nil
}

func (m *osCleanupMutexes) Probe(root string) error {
	name := mutexName(root)
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("cfprovider: mutex name %q: %w", name, err)
	}
	h, err := windows.CreateMutex(nil, false, namePtr)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			// Хендл валиден даже при ERROR_ALREADY_EXISTS — закрываем его.
			_ = windows.CloseHandle(h)
			return fmt.Errorf("provider mutex %s exists (a live provider for %s)", name, root)
		}
		return fmt.Errorf("cfprovider: CreateMutex(%s): %w", name, err)
	}
	_ = windows.CloseHandle(h)
	return nil
}

func (m *osCleanupMutexes) Release() {
	if m.held != 0 {
		if err := windows.CloseHandle(m.held); err != nil {
			logger.Warnf("CloseHandle(cleanup instance mutex): %v", err)
		}
		m.held = 0
	}
}

// RunCleanup — движок cleanup (design.md A4): статические предусловия →
// mutex (R2) → WalkDir + классификация → предусловия плана (вложенные живые
// корни, R7) → при --apply: revert P-файлов → пост-проверка →
// (--delete-files) удаление reverted-файлов → revert P-каталогов снизу
// вверх → удаление пустых reverted-каталогов → (--unregister) снятие
// регистрации ПОСЛЕДНИМ шагом → отчёт. Без --apply мутаций нет. Отчёт
// печатается в opts.Out при любом исходе; ошибка — любой отказ (предусловия;
// при apply — также любые неудачи операций).
func RunCleanup(deps *CleanupDeps, opts CleanupOptions) (*CleanupReport, error) {
	rep := &CleanupReport{
		Apply:       opts.Apply,
		DeleteFiles: opts.DeleteFiles,
		Unregister:  opts.Unregister,
		ApplyErrors: make(map[string]int),
	}
	if opts.Out != nil {
		// Отчёт печатается при любом исходе — включая отказы предусловий.
		defer func() {
			PrintCleanupReport(opts.Out, rep)
		}()
	}
	if deps == nil || deps.FS == nil || deps.Detector == nil || deps.Reverter == nil || deps.Registrar == nil || deps.Mutexes == nil {
		return rep, fmt.Errorf("cleanup: dependencies are not fully configured")
	}

	// (1) Статические предусловия: корень существует/каталог/не корень
	// тома/NTFS (design.md A4; NTFS-хелпер register).
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return rep, fmt.Errorf("cleanup: resolve root %s: %w", opts.Root, err)
	}
	root = filepath.Clean(root)
	rep.Root = root
	fi, err := deps.FS.Stat(root)
	if err != nil {
		return rep, fmt.Errorf("cleanup: root %s: %w", root, err)
	}
	if !fi.IsDir() {
		return rep, fmt.Errorf("cleanup: root %s is not a directory", root)
	}
	if vol := filepath.VolumeName(root); vol != "" && strings.EqualFold(root, vol+`\`) {
		return rep, fmt.Errorf("cleanup: root %s is a volume root", root)
	}
	fsName, err := deps.FS.FileSystem(root)
	if err != nil {
		return rep, fmt.Errorf("cleanup: file system of %s: %w", root, err)
	}
	rep.FileSystem = fsName
	if !strings.EqualFold(fsName, "NTFS") {
		return rep, fmt.Errorf("cleanup: root %s is on %s; the Cloud Files API requires NTFS", root, fsName)
	}

	// (2) R2 — живой провайдер → отказ в обоих режимах; mutex держится до
	// конца команды (закрывает гонку со стартом serve).
	rep.MutexName = mutexName(root)
	if err := deps.Mutexes.Hold(root); err != nil {
		return rep, fmt.Errorf("cleanup: %w", err)
	}
	rep.MutexHeld = true
	defer deps.Mutexes.Release()

	// Регистрация — заголовок отчёта и предусловие R7.
	rep.Registered = deps.Registrar.Registered(root)

	// (3) WalkDir + классификация L/P/N/U (вложенные живые корни — Probe
	// по каталогам, поддерево пропускается).
	entries := classifyCleanupTree(deps, opts, root, rep)

	// (4) План: P-файлы и P-каталоги.
	var planFiles, planDirs []*cleanupEntry
	for _, e := range entries {
		if e.kind != cleanupPlaceholder {
			continue
		}
		if e.isDir {
			planDirs = append(planDirs, e)
		} else {
			planFiles = append(planFiles, e)
		}
	}
	rep.PlanRevertFiles = len(planFiles)
	rep.PlanRevertDirs = len(planDirs)
	if opts.DeleteFiles {
		rep.PlanDeleteFiles = len(planFiles)
		rep.PlanRemoveDirs = len(planDirs)
	}

	if !opts.Apply {
		// Dry-run: отчёт без мутаций (R1).
		return rep, nil
	}

	// Предусловия apply до первой мутации: вложенный живой корень и R7.
	if len(rep.NestedRoots) > 0 {
		return rep, fmt.Errorf("cleanup: %d live sync root(s) nested under %s: %s; aborting before mutations",
			len(rep.NestedRoots), root, strings.Join(rep.NestedRoots, ", "))
	}
	if rep.Registered && !opts.Unregister {
		return rep, fmt.Errorf("cleanup: sync root %s is registered; pass --unregister to allow cleanup", root)
	}

	// Состояние записей в apply: подтверждённый revert (пост-проверка R4) и
	// записи, удалённые с диска напрямую (fallback dehydrated-сирот).
	reverted := make(map[string]bool)
	gone := make(map[string]bool)

	// (5) Revert P-файлов: RevertPlaceholderByPath(RevertFlagNone) →
	// пост-проверка STANDARD «стал plain» (на ещё зарегистрированном корне —
	// CfUnregisterSyncRoot удаляет дегидратированные скелеты, живая проба
	// U-C1 28.11). Отказ revert'а: hydrated — ошибка с rescue-указанием;
	// доказанно dehydrated-сирота с --delete-files — fallback прямого
	// os.Remove (мёртвый провайдер отвечает 0x80070194 — orphan-сценарий,
	// репроба U-C4 28.11).
	for _, e := range planFiles {
		if rerr := deps.Reverter.RevertPlaceholder(e.path, cfapi.RevertFlagNone); rerr != nil {
			switch {
			case cleanupHydratedOrphan(e):
				rep.failApply("revert", e.rel, fmt.Errorf("%v; hydrated: local-only content; start a temporary serve with reachable meta (rescue) or remove manually", rerr))
			case opts.DeleteFiles && cleanupDehydratedOrphan(e):
				if rmErr := deps.FS.Remove(e.path); rmErr != nil {
					rep.failApply("remove-direct", e.rel, rmErr)
					continue
				}
				if !cleanupEntryGone(deps.FS, e.path) {
					rep.failApply("verify", e.rel, errors.New("entry still present after direct remove"))
					continue
				}
				gone[e.path] = true
				rep.RemovedDirect++
			default:
				rep.failApply("revert", e.rel, rerr)
			}
			continue
		}
		if !cleanupBecamePlain(deps.Detector, e.path) {
			rep.failApply("verify", e.rel, errors.New("placeholder after revert"))
			continue
		}
		reverted[e.path] = true
		rep.RevertedFiles++
	}

	// (6) --delete-files: os.Remove только подтверждённо-reverted файлов.
	if opts.DeleteFiles {
		for _, e := range planFiles {
			if !reverted[e.path] {
				continue
			}
			if rerr := deps.FS.Remove(e.path); rerr != nil {
				rep.failApply("remove", e.rel, rerr)
				continue
			}
			rep.RemovedFiles++
		}
	}

	// (7) Revert P-каталогов снизу вверх (descending-порядок пути: дети
	// раньше родителей): каталог revert'ится только если все потомки-
	// плейсхолдеры подтверждённо reverted или удалены напрямую (design.md
	// A4). Отказ revert'а самого каталога с --delete-files — тот же
	// dehydrated-fallback: прямой os.Remove ПУСТОГО каталога; непустой/
	// недоказанный → kept.
	sort.Slice(planDirs, func(i, j int) bool {
		return planDirs[i].path > planDirs[j].path
	})
	for _, d := range planDirs {
		if !cleanupSubtreeHandled(reverted, gone, d, planFiles, planDirs) {
			rep.failApply("revert", d.rel, errors.New("placeholders remain in the subtree"))
			continue
		}
		if rerr := deps.Reverter.RevertPlaceholder(d.path, cfapi.RevertFlagNone); rerr != nil {
			switch {
			case opts.DeleteFiles && cleanupDehydratedOrphan(d):
				if rmErr := deps.FS.Remove(d.path); rmErr != nil {
					if cleanupDirNotEmpty(rmErr) {
						rep.KeptDirs++
						rep.failApply("revert", d.rel, rerr)
					} else {
						rep.failApply("remove-direct", d.rel, rmErr)
					}
					continue
				}
				if !cleanupEntryGone(deps.FS, d.path) {
					rep.failApply("verify", d.rel, errors.New("entry still present after direct remove"))
					continue
				}
				gone[d.path] = true
				rep.RemovedDirect++
			default:
				rep.failApply("revert", d.rel, rerr)
			}
			continue
		}
		if !cleanupBecamePlain(deps.Detector, d.path) {
			rep.failApply("verify", d.rel, errors.New("placeholder after revert"))
			continue
		}
		reverted[d.path] = true
		rep.RevertedDirs++
	}

	// (8) --delete-files: os.Remove пустых reverted-каталогов снизу вверх;
	// непустые/занятые → kept (design.md A4).
	if opts.DeleteFiles {
		for _, d := range planDirs {
			if !reverted[d.path] {
				continue
			}
			if rerr := deps.FS.Remove(d.path); rerr != nil {
				if cleanupDirNotEmpty(rerr) {
					rep.KeptDirs++
					continue
				}
				rep.failApply("remove", d.rel, rerr)
				continue
			}
			rep.RemovedDirs++
		}
	}

	// (9) --unregister — ПОСЛЕДНИЙ шаг apply, после всех мутаций (design.md
	// A4; живая проба U-C1 28.11: CfUnregisterSyncRoot на зарегистрированном
	// корне удаляет дегидратированные плейсхолдер-скелеты с диска и
	// материализует гидратированные в plain — revert обязан выполняться на
	// ещё зарегистрированном корне). Незарегистрированный корень — WARN и
	// продолжение.
	if opts.Unregister {
		if rep.Registered {
			if uerr := deps.Registrar.Unregister(root); uerr != nil {
				return rep, fmt.Errorf("cleanup: unregister %s: %w", root, uerr)
			}
			rep.Unregistered = true
			logger.Infof("cleanup: sync root %s unregistered", root)
		} else {
			logger.Warnf("cleanup: --unregister requested, but sync root %s is not registered; continuing", root)
		}
	}

	if rep.Errors > 0 {
		return rep, fmt.Errorf("cleanup: %d operation(s) failed on %s (see the report)", rep.Errors, root)
	}
	return rep, nil
}

// classifyCleanupTree — WalkDir + классификация записей корня (design.md A4
// шаг 2): symlink/junction → L; STANDARD успешен → P; сентинел «не
// плейсхолдер» → N; иная ошибка → атрибуты DirEntry.Info() (RECALL_* → P
// by-attributes, иначе U). Сам корень не классифицируется (R5); каталоги
// probing'ом mutex (CleanupMutexes.Probe) выявляют вложенные живые корни —
// поддерево пропускается; каталог state-dir не трогается.
func classifyCleanupTree(deps *CleanupDeps, opts CleanupOptions, root string, rep *CleanupReport) []*cleanupEntry {
	var entries []*cleanupEntry
	stateDir := ""
	if opts.StateDir != "" {
		stateDir = filepath.Clean(opts.StateDir)
	}
	walkErr := deps.FS.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			rep.Errors++
			logger.Warnf("cleanup: %s: %v", path, err)
			return nil // остальные поддеревья продолжаются
		}
		if path == root {
			return nil // сам корень: не классифицируется и не удаляется (R5)
		}
		if stateDir != "" {
			if cleaned := filepath.Clean(path); strings.EqualFold(cleaned, stateDir) || isUnderPath(cleaned, stateDir) {
				logger.Debugf("cleanup: skipping state dir %s", path)
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		// symlink/junction — L: reparse-точки не трогаются (Type() помечает
		// symlink; junction распознаётся атрибутом REPARSE_POINT).
		if d.Type()&fs.ModeSymlink != 0 || cleanupIsReparse(d) {
			rep.Links++
			rep.addExample(cleanupLink, rel, "")
			entries = append(entries, &cleanupEntry{path: path, rel: rel, kind: cleanupLink})
			return nil
		}
		isDir := d.IsDir()
		if isDir {
			// Вложенный живой корень: mutex существует → поддерево пропускается.
			if perr := deps.Mutexes.Probe(path); perr != nil {
				rep.NestedRoots = append(rep.NestedRoots, path)
				logger.Warnf("cleanup: live sync root %s skipped: %v", path, perr)
				return fs.SkipDir
			}
		}
		e := &cleanupEntry{path: path, rel: rel, isDir: isDir}
		info, isPh, serr := deps.Detector.StandardInfoOf(path)
		switch {
		case serr == nil && isPh:
			e.kind = cleanupPlaceholder
			e.info = info
			if ifi, ierr := d.Info(); ierr == nil {
				e.size = ifi.Size()
			}
			rep.Placeholders++
			detail := ""
			if info != nil {
				_, hydration := cfapi.StandardStateDescriptions(e.size, info)
				switch hydration {
				case "dehydrated":
					rep.Dehydrated++
				case "hydrated":
					rep.Hydrated++
				}
				detail = fmt.Sprintf("[FileId=%d, %s, %s, %s]", info.FileId,
					cleanupInSyncString(info.InSyncState), cleanupPinString(info.PinState), hydration)
			}
			rep.addExample(cleanupPlaceholder, rel, detail)
		case serr == nil && !isPh, cfapi.IsNotAPlaceholder(serr):
			// Определённый ответ «не плейсхолдер»: STANDARD прошёл без
			// плейсхолдерности (nil-ошибка) либо сентинел
			// ERROR_NOT_A_CLOUD_FILE.
			e.kind = cleanupPlain
			rep.Plain++
			rep.addExample(cleanupPlain, rel, "")
		default:
			// Иная ошибка STANDARD → fallback по атрибутам DirEntry.Info()
			// (design.md A4): RECALL_ON_DATA_ACCESS у файла / RECALL_ON_OPEN
			// у каталога → P by-attributes; иначе U unknown (только отчёт).
			attrs, ok := cleanupEntryAttrs(d)
			phByAttr := ok && ((!isDir && attrs&windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS != 0) ||
				(isDir && attrs&windows.FILE_ATTRIBUTE_RECALL_ON_OPEN != 0))
			if phByAttr {
				e.kind = cleanupPlaceholder
				e.byAttr = true
				rep.Placeholders++
				rep.ByAttributes++
				rep.addExample(cleanupPlaceholder, rel, "[by attributes]")
			} else {
				e.kind = cleanupUnknown
				rep.Unknown++
				rep.addExample(cleanupUnknown, rel, "")
				if serr != nil {
					logger.Debugf("cleanup classify: %s: unknown (%v)", rel, serr)
				}
			}
		}
		if isDir {
			rep.Dirs++
		} else {
			rep.Files++
		}
		entries = append(entries, e)
		return nil
	})
	if walkErr != nil {
		rep.Errors++
		logger.Warnf("cleanup: walk %s: %v", root, walkErr)
	}
	return entries
}

// cleanupBecamePlain — пост-проверка revert'а (R4): STANDARD-запрос обязан
// дать определённый ответ «не плейсхолдер».
func cleanupBecamePlain(det CleanupDetector, path string) bool {
	_, isPh, err := det.StandardInfoOf(path)
	if err != nil || isPh {
		return false
	}
	return true
}

// cleanupEntryGone — пост-проверка отсутствия записи после прямого удаления
// (fallback dehydrated-сирот): путь не статится (ErrNotExist).
func cleanupEntryGone(fsys CleanupFS, path string) bool {
	_, err := fsys.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// cleanupDehydratedOrphan — запись плана доказанно без локального контента
// (содержимое остаётся в backend — прямой remove безопасен): STANDARD-ответ
// «dehydrated» либо пустой логический размер. Записи by-attributes (без
// STANDARD-доказательства) — false: fallback только по доказанному состоянию.
func cleanupDehydratedOrphan(e *cleanupEntry) bool {
	if e.info == nil {
		return false
	}
	_, hydration := cfapi.StandardStateDescriptions(e.size, e.info)
	return hydration == "dehydrated" || hydration == "empty"
}

// cleanupHydratedOrphan — запись плана с локальным контентом (STANDARD:
// hydrated): может нести правку, которой нет в backend — прямой remove
// запрещён, отказ revert'а дополняется rescue-указанием.
func cleanupHydratedOrphan(e *cleanupEntry) bool {
	if e.info == nil {
		return false
	}
	_, hydration := cfapi.StandardStateDescriptions(e.size, e.info)
	return hydration == "hydrated"
}

// cleanupSubtreeHandled — все потомки-плейсхолдеры каталога подтверждённо
// reverted или удалены с диска напрямую (условие revert'а/удаления каталога,
// design.md A4).
func cleanupSubtreeHandled(reverted, gone map[string]bool, dir *cleanupEntry, planFiles, planDirs []*cleanupEntry) bool {
	for _, e := range planFiles {
		if isUnderPath(e.path, dir.path) && !reverted[e.path] && !gone[e.path] {
			return false
		}
	}
	for _, d := range planDirs {
		if d.path != dir.path && isUnderPath(d.path, dir.path) && !reverted[d.path] && !gone[d.path] {
			return false
		}
	}
	return true
}

// isUnderPath — path лежит строго внутри dir (без учёта регистра: файловая
// система Windows регистронезависима).
func isUnderPath(path, dir string) bool {
	return strings.HasPrefix(strings.ToLower(path), strings.ToLower(dir)+`\`)
}

// cleanupEntryAttrs — атрибуты записи для fallback-классификации
// (DirEntry.Info().Sys() как *syscall.Win32FileAttributeData на Windows).
func cleanupEntryAttrs(d fs.DirEntry) (uint32, bool) {
	fi, err := d.Info()
	if err != nil {
		return 0, false
	}
	wd, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return 0, false
	}
	return wd.FileAttributes, true
}

// cleanupIsReparse — запись — reparse-точка (symlink/junction; L: не
// трогается).
func cleanupIsReparse(d fs.DirEntry) bool {
	attrs, ok := cleanupEntryAttrs(d)
	return ok && attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// cleanupDirNotEmpty — отказ os.Remove из-за непустого каталога: ожидаемый
// исход «kept», не failure.
func cleanupDirNotEmpty(err error) bool {
	var eno syscall.Errno
	if errors.As(err, &eno) {
		return eno == windows.ERROR_DIR_NOT_EMPTY
	}
	return false
}

// cleanupInSyncString — CF_IN_SYNC_STATE для отчёта.
func cleanupInSyncString(state uint32) string {
	switch state {
	case cfapi.InSyncStateInSync:
		return "in-sync"
	case cfapi.InSyncStateNotInSync:
		return "not in-sync"
	default:
		return fmt.Sprintf("in-sync state %d", state)
	}
}

// cleanupPinString — CF_PIN_STATE для отчёта.
func cleanupPinString(pin uint32) string {
	switch pin {
	case cfapi.PinStatePinned:
		return "pinned"
	case cfapi.PinStateUnpinned:
		return "unpinned"
	default:
		return fmt.Sprintf("pin state %d", pin)
	}
}

// failApply — отказ операции apply: счётчик, гистограмма и (до предела)
// список неудач.
func (rep *CleanupReport) failApply(op, rel string, err error) {
	rep.Errors++
	msg := fmt.Sprintf("%s %s: %v", op, rel, err)
	rep.ApplyErrors[msg]++
	logger.Warnf("cleanup apply: %s", msg)
}

// addExample — пример класса для отчёта (до cleanupExamplesPerClass на класс).
func (rep *CleanupReport) addExample(kind cleanupKind, rel, detail string) {
	line := rel
	if detail != "" {
		line += " " + detail
	}
	switch kind {
	case cleanupPlaceholder:
		if len(rep.ExamplesPlaceholder) < cleanupExamplesPerClass {
			rep.ExamplesPlaceholder = append(rep.ExamplesPlaceholder, line)
		}
	case cleanupPlain:
		if len(rep.ExamplesPlain) < cleanupExamplesPerClass {
			rep.ExamplesPlain = append(rep.ExamplesPlain, line)
		}
	case cleanupUnknown:
		if len(rep.ExamplesUnknown) < cleanupExamplesPerClass {
			rep.ExamplesUnknown = append(rep.ExamplesUnknown, line)
		}
	default:
		if len(rep.ExamplesLink) < cleanupExamplesPerClass {
			rep.ExamplesLink = append(rep.ExamplesLink, line)
		}
	}
}

// PrintCleanupReport — отчёт cleanup в w: заголовок (режим/mutex/ФС/
// регистрация), счётчики, план, примеры (≤20 на класс), итоги apply и
// гистограмма отказов.
func PrintCleanupReport(w io.Writer, rep *CleanupReport) {
	mode := "dry-run"
	if rep.Apply {
		mode = "apply"
		if rep.DeleteFiles {
			mode += " +delete-files"
		}
		if rep.Unregister {
			mode += " +unregister"
		}
	}
	fmt.Fprintf(w, "Cleanup:    %s\n", rep.Root)
	fmt.Fprintf(w, "Mode:       %s\n", mode)
	fmt.Fprintf(w, "Mutex:      %s", rep.MutexName)
	if rep.MutexHeld {
		fmt.Fprintf(w, " (held)")
	}
	fmt.Fprintf(w, "\n")
	if rep.FileSystem != "" {
		fmt.Fprintf(w, "FileSystem: %s\n", rep.FileSystem)
	}
	registered := "no"
	if rep.Registered {
		registered = "yes"
	}
	fmt.Fprintf(w, "Registered: %s\n", registered)
	fmt.Fprintf(w, "Entries:    files=%d dirs=%d links=%d\n", rep.Files, rep.Dirs, rep.Links)
	fmt.Fprintf(w, "Placeholders: %d (dehydrated=%d hydrated=%d by-attributes=%d)\n",
		rep.Placeholders, rep.Dehydrated, rep.Hydrated, rep.ByAttributes)
	fmt.Fprintf(w, "Plain:      %d\n", rep.Plain)
	fmt.Fprintf(w, "Unknown:    %d\n", rep.Unknown)
	fmt.Fprintf(w, "Errors:     %d\n", rep.Errors)
	if len(rep.NestedRoots) > 0 {
		fmt.Fprintf(w, "Nested live sync roots: %d (%s)\n", len(rep.NestedRoots), strings.Join(rep.NestedRoots, ", "))
	}
	fmt.Fprintf(w, "Plan:       revert files=%d, delete files=%d, revert dirs=%d, remove dirs=%d\n",
		rep.PlanRevertFiles, rep.PlanDeleteFiles, rep.PlanRevertDirs, rep.PlanRemoveDirs)
	printExamples(w, "placeholder", rep.ExamplesPlaceholder)
	printExamples(w, "plain", rep.ExamplesPlain)
	printExamples(w, "unknown", rep.ExamplesUnknown)
	printExamples(w, "link", rep.ExamplesLink)
	if rep.Apply {
		if rep.Unregister {
			fmt.Fprintf(w, "Unregistered: yes\n")
		}
		fmt.Fprintf(w, "Result:     reverted files=%d dirs=%d; removed files=%d dirs=%d; removed-direct=%d; kept dirs=%d\n",
			rep.RevertedFiles, rep.RevertedDirs, rep.RemovedFiles, rep.RemovedDirs, rep.RemovedDirect, rep.KeptDirs)
		printHistogram(w, rep.ApplyErrors)
	}
}

// printExamples — блок примеров класса (пропускается, когда класс пуст).
func printExamples(w io.Writer, class string, examples []string) {
	if len(examples) == 0 {
		return
	}
	fmt.Fprintf(w, "Examples %s (%d):\n", class, len(examples))
	for _, ex := range examples {
		fmt.Fprintf(w, "  %s\n", ex)
	}
}

// printHistogram — гистограмма отказов apply.
func printHistogram(w io.Writer, hist map[string]int) {
	if len(hist) == 0 {
		return
	}
	keys := make([]string, 0, len(hist))
	for k := range hist {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "Apply errors:\n")
	for _, k := range keys {
		fmt.Fprintf(w, "  %s x%d\n", k, hist[k])
	}
}
