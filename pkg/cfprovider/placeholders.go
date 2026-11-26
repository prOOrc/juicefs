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

// Наполнение плейсхолдерами (design.md D6): верхний уровень — при старте,
// дети каталога — при первом обращении (FETCH_PLACEHOLDERS). Идентификатор
// плейсхолдера — inode записи backend, 8 байт LE; размер и mtime — из
// метаданных backend. Локальный файл пользователя не перезаписывается:
// конфликт — запись в лог и пропуск.
//
// Обращения к cldapi.dll и диску — только через интерфейсы backendLister и
// placeholderStore: движок проверяется на подменных реализациях без
// зарегистрированного sync root (инвариант «Движок проверяется без CfAPI»).
package cfprovider

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/juicedata/juicefs/pkg/cfapi"
	"github.com/juicedata/juicefs/pkg/fs"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/utils"
)

// BackendEntry — запись каталога backend, из которой создаётся плейсхолдер.
type BackendEntry struct {
	Name      string
	Inode     uint64
	Size      int64
	MtimeSec  int64
	MtimeNsec uint32
	IsDir     bool
}

// backendLister — источник записей backend: минимальная поверхность
// fs.FileSystem, нужная наполнению. Реальная реализация — BackendFS;
// подменная — в тестах.
type backendLister interface {
	// List возвращает записи каталога dir (путь в стиле fs: "/", "/a/b"),
	// без "." и "..".
	List(ctx context.Context, dir string) ([]BackendEntry, error)
}

// placeholderStore — локальный диск и CfAPI-операции наполнения.
type placeholderStore interface {
	// Create — CfCreatePlaceholders: прямое создание плейсхолдеров в
	// базовом каталоге (наполнение верхнего уровня).
	Create(baseDirPath string, infos []cfapi.CFPlaceholderCreateInfo, createFlags uint32, entriesProcessed *uint32) error
	// AnswerFetchPlaceholders — ответ на FETCH_PLACEHOLDERS операцией
	// TRANSFER_PLACEHOLDERS (наполнение детей каталога).
	AnswerFetchPlaceholders(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, infos []cfapi.CFPlaceholderCreateInfo, total int64) error
	// StateOf — состояние локального пути: существует ли и является ли
	// плейсхолдером (виртуальным файлом) rather обычным файлом пользователя.
	StateOf(path string) (exists, isPlaceholder bool, err error)
}

// placeholderCreateFlags — CF_PLACEHOLDER_CREATE_FLAGS наполнения (проба U2).
//
// CF_PLACEHOLDER_CREATE_FLAG_DISABLE_ON_DEMAND_POPULATION (cfapi.h, 0x1)
// отключает выдачу фильтром FETCH_PLACEHOLDERS для созданного каталога —
// children должны быть наполнены провайдером самостоятельно и сразу. Прототип
// опирается на on-demand наполнение (design.md D6: «дети каталога — при
// первом обращении», политика PopulationPolicyPartial из D4), поэтому флаг
// НЕ ставится: фильтр сам пришлёт FETCH_PLACEHOLDERS при первом перечислении
// каталога. Если проба U2 покажет, что callback не приходит, запасной
// механизм — D6 («уведомление об открытии каталога и CfCreatePlaceholders»).
//
// CF_PLACEHOLDER_CREATE_FLAG_MARK_IN_SYNC (0x2): копия backend и так
// синхронизирована; без флага сверка при старте (design.md D9) считала бы
// каждый плейсхолдер «вне in-sync» и ставила его в очередь выгрузки.
const placeholderCreateFlags = cfapi.PlaceholderCreateFlagMarkInSync

// createFailedResult — ненулевой Result записей каталога при полном отказе
// CfCreatePlaceholders (статусы отдельных записей в этом случае
// недостоверны); значение только для пометки и лога.
const createFailedResult = cfapi.HRESULT(-1)

// localPlaceholderAttrMask — атрибуты, по которым локальный путь считается
// плейсхолдером/виртуальным: cldflt выставляет RECALL_ON_DATA_ACCESS не
// полностью гидратированным плейсхолдерам, RECALL_ON_OPEN и OFFLINE —
// родственные атрибуты виртуализации. Обычный файл пользователя ни одного
// не имеет — тогда путь трактуется как конфликт (пропуск без перезаписи).
// Если проба U8 покажет полностью гидратированные плейсхолдеры без этих
// атрибутов, проверка уточняется CfGetPlaceholderStateFromFileInfo (функция
// из списка D1).
const localPlaceholderAttrMask = windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS |
	windows.FILE_ATTRIBUTE_RECALL_ON_OPEN |
	windows.FILE_ATTRIBUTE_OFFLINE

// BackendFS — backendLister поверх fs.FileSystem: записи каталога —
// ReaddirPlus (inode и атрибуты в одном проходе meta). Используется
// провайдером (WithData) и командами clouddrive (ls).
type BackendFS struct {
	fsys *fs.FileSystem
}

// NewBackendFS — листинг backend через fs.FileSystem.
func NewBackendFS(fsys *fs.FileSystem) *BackendFS {
	return &BackendFS{fsys: fsys}
}

// List читает записи каталога backend. Контекст meta создаётся как в
// pkg/fs/http.go (pid + текущие uid/gid процесса) и привязывается к ctx
// вызывающего: отмена (остановка провайдера) прерывает чтение.
func (b *BackendFS) List(ctx context.Context, dir string) ([]BackendEntry, error) {
	mctx := meta.WrapWithCancel(ctx, uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
	defer mctx.Cancel()
	f, errno := b.fsys.Open(mctx, dir, 0)
	if errno != 0 {
		return nil, fmt.Errorf("open %s: %s", dir, errno)
	}
	defer f.Close(mctx) //nolint:errcheck
	entries, errno := f.ReaddirPlus(mctx, 0)
	if errno != 0 {
		return nil, fmt.Errorf("readdir %s: %s", dir, errno)
	}
	out := make([]BackendEntry, 0, len(entries))
	for _, e := range entries {
		name := string(e.Name)
		if name == "." || name == ".." {
			continue
		}
		out = append(out, BackendEntry{
			Name:      name,
			Inode:     uint64(e.Inode),
			Size:      int64(e.Attr.Length),
			MtimeSec:  e.Attr.Mtime,
			MtimeNsec: e.Attr.Mtimensec,
			IsDir:     e.Attr.Typ == meta.TypeDirectory,
		})
	}
	return out, nil
}

// osStore — placeholderStore на локальном диске и cldapi.dll.
type osStore struct{}

func (osStore) Create(baseDirPath string, infos []cfapi.CFPlaceholderCreateInfo, createFlags uint32, entriesProcessed *uint32) error {
	return cfapi.CfCreatePlaceholders(baseDirPath, infos, createFlags, entriesProcessed)
}

func (osStore) AnswerFetchPlaceholders(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, infos []cfapi.CFPlaceholderCreateInfo, total int64) error {
	return cfapi.AnswerFetchPlaceholders(conn, transferKey, requestKey, status, infos, total)
}

// StateOf читает атрибуты локального пути через os.Stat: на Windows атрибуты
// доступны в Sys() (*syscall.Win32FileAttributeData). Ошибка определения
// атрибутов консервативно трактуется как «не плейсхолдер» — защита локального
// файла важнее лишнего плейсхолдера.
func (osStore) StateOf(path string) (bool, bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, err
	}
	wd, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return true, false, nil
	}
	return true, wd.FileAttributes&localPlaceholderAttrMask != 0, nil
}

// populatedDir — результат сборки записей одного каталога: массив infos и
// параллельный список записей backend, вошедших в массив (для записи в
// индекс по результату создания), плюс существующие плейсхолдеры (без
// повторного создания, но с записью в индекс).
type populatedDir struct {
	baseDirPath string // локальный базовый каталог CfCreatePlaceholders
	infos       []cfapi.CFPlaceholderCreateInfo
	entries     []BackendEntry // параллелен infos; пустое имя — не создан
	identities  [][]byte       // копии FileIdentity — удержание памяти до Cf-вызова
	existing    []BackendEntry // уже на диске плейсхолдеры — только в индекс
	conflicts   int            // пропущено из-за одноимённого локального не-плейсхолдера
}

// buildPopulatedDir готовит массив CF_PLACEHOLDER_CREATE_INFO для каталога
// dir (путь в стиле fs) по записям backend:
//   - одноимённый локальный не-плейсхолдер — конфликт: запись в лог, пропуск
//     (спека «Одноимённый локальный файл», инвариант «локальный файл
//     пользователя не перезаписывается и не удаляется»);
//   - существующий плейсхолдер — без повторного создания (спека
//     «Перезапуск без дублей»); соответствие inode → путь всё равно
//     сохраняется в индекс (журнал мог быть утерян при живом диске).
func buildPopulatedDir(ctx context.Context, p *Provider, dir string, entries []BackendEntry) (*populatedDir, error) {
	out := &populatedDir{baseDirPath: localJoin(p.root, dir)}
	for _, e := range entries {
		rel := fsJoin(dir, e.Name)
		local := localJoin(p.root, rel)
		exists, isPlaceholder, err := p.store.StateOf(local)
		if err != nil {
			logger.Warnf("placeholder %s: stat local %s: %v; entry skipped", rel, local, err)
			continue
		}
		if exists && !isPlaceholder {
			logger.Warnf("conflict: %s exists and is not a placeholder; backend entry %s skipped (local file is left untouched)", local, rel)
			out.conflicts++
			continue
		}
		if exists && isPlaceholder {
			// Перезапуск: плейсхолдер уже на диске — повторное создание
			// не нужно (дубль), индекс обновляем.
			out.existing = append(out.existing, e)
			continue
		}
		name16, err := windows.UTF16PtrFromString(e.Name)
		if err != nil {
			logger.Warnf("placeholder %s: invalid name: %v; entry skipped", rel, err)
			continue
		}
		// FileIdentity — inode, 8 байт little-endian (design.md D6); копия
		// держится рядом с info до завершения Cf-вызова.
		identity := make([]byte, 8)
		binary.LittleEndian.PutUint64(identity, e.Inode)
		// Метаданные из backend: mtime (FILETIME → LARGE_INTEGER) и размер записи.
		ft := windows.NsecToFiletime(e.MtimeSec*1e9 + int64(e.MtimeNsec))
		lastWrite := int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
		attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
		if e.IsDir {
			attrs = windows.FILE_ATTRIBUTE_DIRECTORY
		}
		out.infos = append(out.infos, cfapi.CFPlaceholderCreateInfo{
			RelativeFileName: name16,
			FsMetadata: cfapi.CFFileMetadata{
				BasicInfo: cfapi.FileBasicInfo{
					CreationTime:   lastWrite,
					LastAccessTime: lastWrite,
					LastWriteTime:  lastWrite,
					ChangeTime:     lastWrite,
					FileAttributes: attrs,
				},
				FileSize: e.Size,
			},
			FileIdentity:       unsafe.Pointer(&identity[0]),
			FileIdentityLength: uint32(len(identity)),
			Flags:              placeholderCreateFlags,
		})
		out.entries = append(out.entries, e)
		// Удержание identity в живых до конца Cf-вызова (GC видит срез).
		out.identities = append(out.identities, identity)
	}
	return out, nil
}

// saveIndexed сохраняет соответствие inode → путь для созданных (или уже
// существовавших на диске) плейсхолдеров; записи с пустым именем —
// неудавшиеся при создании — не сохраняются.
func (d *populatedDir) saveIndexed(p *Provider, dir string) {
	for _, e := range append(append([]BackendEntry(nil), d.existing...), d.entries...) {
		if e.Name == "" {
			continue
		}
		if err := p.idx.Put(e.Inode, fsJoin(dir, e.Name)); err != nil {
			logger.Warnf("index journal: inode %d: %v", e.Inode, err)
		}
	}
}

// populateTopLevel наполняет верхний уровень sync root (design.md D6,
// шаг 4 Execution Flow). Ошибка чтения backend возвращается вызывающему
// (retry-цикл готовности), ошибки отдельных записей — в лог.
func populateTopLevel(ctx context.Context, p *Provider) error {
	entries, err := p.backend.List(ctx, "/")
	if err != nil {
		return fmt.Errorf("list backend root: %w", err)
	}
	built, err := buildPopulatedDir(ctx, p, "/", entries)
	if err != nil {
		return err
	}
	created := 0
	if len(built.infos) > 0 {
		var processed uint32
		// CfCreatePlaceholders без STOP_ON_ERROR создаёт все записи, статус
		// каждой — в поле Result. Полный отказ вызова оставляет Result
		// записей нулевым: статус недостоверен — все записи каталога
		// помечаются неудавшимися (createFailedResult).
		if err := p.store.Create(built.baseDirPath, built.infos, cfapi.CreateFlagNone, &processed); err != nil {
			logger.Warnf("CfCreatePlaceholders(%s): %v (processed %d/%d)", built.baseDirPath, err, processed, len(built.infos))
			for i := range built.infos {
				built.infos[i].Result = createFailedResult
			}
		}
		for i := range built.infos {
			if res := built.infos[i].Result; res != 0 {
				logger.Warnf("placeholder %s: create failed: HRESULT 0x%08x", fsJoin("/", built.entries[i].Name), uint32(res))
				built.entries[i] = BackendEntry{} // неудавшаяся — не в индекс
				continue
			}
			created++
		}
	}
	// В индекс попадают только фактически созданные записи (неудавшиеся
	// обнулены выше — при полном отказе Create новых в индексе нет) и
	// существовавшие на диске плейсхолдеры независимо от исхода Create.
	built.saveIndexed(p, "/")
	if built.conflicts > 0 {
		logger.Warnf("top-level population: %d entr(ies) skipped due to conflicting local files", built.conflicts)
	}
	logger.Infof("top-level population: %d placeholders created in %s", created, built.baseDirPath)
	return nil
}

// runPopulation — воркер очереди FETCH_PLACEHOLDERS: дети каталога
// наполняются при первом обращении (design.md D6, основной механизм;
// проба U2 уточняет живое поведение). Каждое задание завершается ровно
// одним ответом TRANSFER_PLACEHOLDERS — с записями или со статусом отказа.
func runPopulation(ctx context.Context, p *Provider) {
	d, ok := p.dispatch.(*cfapi.Dispatcher)
	if !ok {
		// Заглушка-логгер этапа 1 очередей не имеет — наполнение по
		// обращению недоступно (тесты и stage-1 режим).
		return
	}
	for {
		// Снимок канала остановки под stopMu (Stop закрывает его и зануляет
		// под тем же замком): nil — провайдер остановлен, выход; захваченный
		// закрытый канал остаётся читаемым навсегда — select не зависнет.
		// После завершения задания — новая итерация: если Stop случился во
		// время обработки (в т.ч. занулил p.stop), снапшот вернёт nil.
		stop := p.stopChan()
		if stop == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case job := <-d.FetchPlaceholders():
			if job == nil {
				continue
			}
			handleFetchPlaceholders(ctx, p, job)
		}
	}
}

// Конкурентность наполнения (проба U2): FETCH_PLACEHOLDERS на корень может
// прийти одновременно с populateTopLevel — дублирующее создание того же
// плейсхолдера отсекает фильтр (per-entry Result при Create) или повторный
// ответ TRANSFER (создание уже существующего — отказ записи); пути
// в индекс пишутся идемпотентно (одно и то же ino → path).

// handleFetchPlaceholders отвечает на одно задание наполнения.
func handleFetchPlaceholders(ctx context.Context, p *Provider, job *cfapi.FetchPlaceholdersJob) {
	defer func() {
		if d, ok := p.dispatch.(*cfapi.Dispatcher); ok {
			d.CompletePlaceholders(job)
		}
	}()
	if job.Cancel.Load() {
		logger.Debugf("FETCH_PLACEHOLDERS canceled: transferKey=%d", job.TransferKey)
		// Отмена: ответ со статусом отмены без записей.
		if err := p.store.AnswerFetchPlaceholders(job.ConnectionKey, job.TransferKey, job.RequestKey, cfapi.StatusCancelled, nil, cfapi.EOF); err != nil {
			logger.Warnf("answer canceled FETCH_PLACEHOLDERS: %v", err)
		}
		return
	}
	dir := p.fsPathOf(job.Path)
	entries, err := p.backend.List(ctx, dir)
	if err != nil {
		logger.Warnf("FETCH_PLACEHOLDERS %s: list backend: %v", dir, err)
		_ = p.store.AnswerFetchPlaceholders(job.ConnectionKey, job.TransferKey, job.RequestKey, cfapi.StatusCloudFileUnsuccessful, nil, cfapi.EOF)
		return
	}
	built, err := buildPopulatedDir(ctx, p, dir, entries)
	if err != nil {
		logger.Warnf("FETCH_PLACEHOLDERS %s: build placeholders: %v", dir, err)
		_ = p.store.AnswerFetchPlaceholders(job.ConnectionKey, job.TransferKey, job.RequestKey, cfapi.StatusCloudFileUnsuccessful, nil, cfapi.EOF)
		return
	}
	// Записи передаются фильтру в ответе — отдельный CfCreatePlaceholders не
	// нужен: TRANSFER_PLACEHOLDERS сам создаёт плейсхолдеры относительно
	// каталога задания. Конфликты уже отфильтрованы (см. buildPopulatedDir):
	// фильтр не получит записей поверх локальных файлов.
	if err := p.store.AnswerFetchPlaceholders(job.ConnectionKey, job.TransferKey, job.RequestKey, cfapi.StatusSuccess, built.infos, int64(len(entries))); err != nil {
		logger.Warnf("FETCH_PLACEHOLDERS %s: answer: %v", dir, err)
		return
	}
	built.saveIndexed(p, dir)
	logger.Debugf("FETCH_PLACEHOLDERS %s: %d placeholders transferred (%d conflicts skipped)", dir, len(built.infos), built.conflicts)
}

// fsJoin — путь записи в стиле fs: каталог + имя через "/".
func fsJoin(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return strings.TrimSuffix(dir, "/") + "/" + name
}

// localJoin — локальный путь по fs-пути относительно sync root.
func localJoin(root, fsPath string) string {
	if fsPath == "/" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(fsPath, "/")))
}

// resolveInoMeta — подъём по родителям через meta (U12): GetAttr даёт
// Parent, имена уровней восстанавливаются Readdir'ом по inode (это не обход
// дерева от корня — по одному чтению на уровень пути). Запасной вариант U12
// (полный обход) не нужен, пока Attr несёт Parent. Ограничение глубины
// защищает от циклов в цепочке родителей.
func resolveInoMeta(ctx context.Context, p *Provider, ino uint64) (string, bool) {
	if p.fsys == nil {
		return "", false
	}
	m := p.fsys.Meta()
	mctx := meta.WrapWithCancel(ctx, uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
	defer mctx.Cancel()

	const maxClimbDepth = 128
	chain := make([]meta.Ino, 0, 8)
	cur := meta.Ino(ino)
	for i := 0; i < maxClimbDepth; i++ {
		chain = append(chain, cur)
		if cur == 1 {
			break
		}
		var attr meta.Attr
		if errno := m.GetAttr(mctx, cur, &attr); errno != 0 {
			logger.Debugf("resolve inode %d: GetAttr: %s", cur, errno)
			return "", false
		}
		if attr.Parent == 0 {
			// Parent не отслеживается (жёсткая ссылка) — подъём невозможен;
			// запасной вариант U12 (обход от корня) на этапе 3 не требуется.
			logger.Debugf("resolve inode %d: parent is not tracked in attr", cur)
			return "", false
		}
		cur = attr.Parent
	}
	if len(chain) == 0 || chain[len(chain)-1] != 1 {
		logger.Debugf("resolve inode %d: parent chain does not reach the root", ino)
		return "", false
	}
	parts := make([]string, 0, len(chain)-1)
	for i := len(chain) - 2; i >= 0; i-- {
		name, ok := childNameByIno(mctx, m, chain[i+1], chain[i])
		if !ok {
			return "", false
		}
		parts = append(parts, name)
	}
	if len(parts) == 0 {
		return "/", true
	}
	return "/" + strings.Join(parts, "/"), true
}

// childNameByIno ищет имя записи inode в каталоге parent (Readdir по inode).
func childNameByIno(mctx meta.Context, m meta.Meta, parent, ino meta.Ino) (string, bool) {
	var entries []*meta.Entry
	if errno := m.Readdir(mctx, parent, 0, &entries); errno != 0 {
		logger.Debugf("resolve inode %d: readdir %d: %s", ino, parent, errno)
		return "", false
	}
	for _, e := range entries {
		if e.Inode == ino {
			return string(e.Name), true
		}
	}
	logger.Debugf("resolve inode %d: not found in directory %d", ino, parent)
	return "", false
}
