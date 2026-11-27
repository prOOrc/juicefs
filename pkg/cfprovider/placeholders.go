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

// Наполнение плейсхолдерами (design.md D6, редакция пивота 27.11): основной
// путь — полный upfront-walk hub (такт 1 — верхний уровень при старте, такт
// 2 — фоновый BFS-walk в walk.go) через convert-примитив createEntry
// (D6.4: реальная запись + CfConvertToPlaceholder). FETCH_PLACEHOLDERS —
// fallback при population PARTIAL/FULL: воркер runPopulation отвечает
// TRANSFER_PLACEHOLDERS, при дефолтном ALWAYS_FULL платформа его не
// вызывает. Идентификатор плейсхолдера — inode записи backend, 8 байт LE;
// размер и mtime — из метаданных backend. Локальный файл пользователя не
// перезаписывается: конфликт — запись в лог и пропуск.
//
// Обращения к cldapi.dll и диску — только через интерфейсы backendLister и
// placeholderStore: движок проверяется на подменных реализациях без
// зарегистрированного sync root (инвариант «Движок проверяется без CfAPI»).
package cfprovider

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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
	// Convert — convert-примитив (design.md D6.4): путь уже существует как
	// реальная запись (создана createEntry); CfConvertToPlaceholder
	// переводит её в плейсхолдер с identity; convertFlags —
	// CF_CONVERT_FLAGS (MARK_IN_SYNC, DEHYDRATE).
	Convert(path string, isDir bool, fileIdentity []byte, convertFlags uint32) error
	// Update — CfUpdatePlaceholder по пути (fallback дегидратации
	// convert-примитива и нормализация усыновления, design.md D6.4);
	// updateFlags — CF_UPDATE_FLAGS.
	Update(path string, fsMetadata cfapi.CFFileMetadata, updateFlags uint32) error
	// StandardInfo — CF_PLACEHOLDER_STANDARD_INFO плейсхолдера (файлы):
	// OnDiskDataSize — дегидратированность; проверка при усыновлении
	// плейсхолдера без индексной записи (краш-окно fallback-дегидратации).
	StandardInfo(path string) (*cfapi.CFPlaceholderStandardInfo, error)
	// AnswerFetchPlaceholders — ответ на FETCH_PLACEHOLDERS операцией
	// TRANSFER_PLACEHOLDERS (fallback-наполнение детей каталога);
	// entriesProcessed — кумулятивный счётчик обработанных записей
	// (завершение population — processed >= total, проба 27.11).
	AnswerFetchPlaceholders(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, infos []cfapi.CFPlaceholderCreateInfo, total int64, entriesProcessed uint32) error
	// TransferData — ответ на FETCH_DATA операцией TRANSFER_DATA (гидрация,
	// design.md D7, этап 3): данные диапазона [offset, offset+length) в buf
	// (страница-выровненный VirtualAlloc из BufferPool) либо при
	// status != SUCCESS — код отказа (buf nil, length 0). Ровно один вызов
	// на задание — инвариант design.md.
	TransferData(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, offset, length int64, buf []byte) error
	// StateOf — состояние локального пути: существует ли и является ли
	// плейсхолдером (виртуальным файлом), а не обычным файлом пользователя.
	StateOf(path string) (exists, isPlaceholder bool, err error)
}

// localLister — локальное перечисление каталога sync root: имена записей
// (без "."/"..") для классификации (design.md D6.5, сравнение имён без
// учёта регистра — Windows-семантика). Одно перечисление на каталог вместо
// os.Stat на запись — M1-замер (design.md D11). Реальная реализация —
// osLocalLister; подменная — в тестах (DLL-независимость).
type localLister interface {
	// ListNames возвращает имена записей каталога localDir. Отсутствие
	// каталога — пустой список без ошибки (все записи будут to-create).
	ListNames(localDir string) ([]string, error)
}

// osLocalLister — localLister на реальном диске: os.ReadDir каталога.
type osLocalLister struct{}

func (osLocalLister) ListNames(localDir string) ([]string, error) {
	des, err := os.ReadDir(localDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // каталога ещё нет — все записи to-create
		}
		return nil, err
	}
	names := make([]string, 0, len(des))
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names, nil
}

// placeholderCreateFlags — CF_PLACEHOLDER_CREATE_FLAGS записей в ответах
// FETCH_PLACEHOLDERS (fallback-наполнение при population PARTIAL/FULL;
// основной путь — walk с convert-примитивом, design.md D6).
//
// CF_PLACEHOLDER_CREATE_FLAG_DISABLE_ON_DEMAND_POPULATION (cfapi.h, 0x1)
// не ставится: в fallback-режиме фильтр сам присылает FETCH_PLACEHOLDERS
// при перечислении необслужённого каталога, и воркер отвечает
// TRANSFER_PLACEHOLDERS по мере заданий (проба U2).
//
// CF_PLACEHOLDER_CREATE_FLAG_MARK_IN_SYNC (0x2): копия backend и так
// синхронизирована; без флага сверка при старте (design.md D9) считала бы
// каждый плейсхолдер «вне in-sync» и ставила его в очередь выгрузки.
const placeholderCreateFlags = cfapi.PlaceholderCreateFlagMarkInSync

// quarantineError — реальная запись, созданная createEntry, не удалена при
// откате: путь занят другим процессом (design.md D6.4 «карантин» — на
// следующем walk это конфликт). Возвращается в цепочке ошибки createEntry.
type quarantineError struct {
	path string
	err  error
}

func (e *quarantineError) Error() string {
	return fmt.Sprintf("rollback of %s failed: %v (quarantine)", e.path, e.err)
}

func (e *quarantineError) Unwrap() error { return e.err }

// rollbackEntry удаляет реальную запись, созданную до отказа createEntry
// (design.md D6.4: «ни реальных неконвертированных записей, ни
// гидратированных нулей не остаётся»). Отказ удаления из-за занятости
// (sharing violation — запись открыта другим процессом) — ошибка карантина.
func rollbackEntry(path string) error {
	if err := os.Remove(path); err != nil {
		var eno syscall.Errno
		if errors.As(err, &eno) && eno == windows.ERROR_SHARING_VIOLATION {
			return &quarantineError{path: path, err: err}
		}
		return err
	}
	return nil
}

// failedAfterCreate — отказ createEntry после создания реальной записи:
// откат (удаление) и ошибка с причиной; неудавшийся откат — карантин в
// цепочке ошибки (errors.As находит *quarantineError). Гонка с
// notifyDirCreate/повтором журнала (между созданием реальной записи и нашей
// конверсией запись уже сконвертирована в плейсхолдер): откат удалил бы
// валидный плейсхолдер — усыновление (resultExists, без отката, fix-1.7).
func failedAfterCreate(p *Provider, local, rel, step string, cause error) (placeholderResultKind, error) {
	if exists, isPh, serr := p.store.StateOf(local); serr == nil && exists && isPh {
		logger.Debugf("create %s: converted by a concurrent op while pending; adopted", rel)
		return resultExists, nil
	}
	if rerr := rollbackEntry(local); rerr != nil {
		return resultFailed, fmt.Errorf("%s %s: %v; rollback: %w", step, rel, cause, rerr)
	}
	return resultFailed, fmt.Errorf("%s %s: %w", step, rel, cause)
}

// createEntry — convert-примитив создания записи hub (design.md D6.4,
// задача 2.8; замена порций CfCreatePlaceholders, которые фильтр отвергает
// при population=ALWAYS_FULL — 0x8007017c): реальная запись +
// CfConvertToPlaceholder, поштучно.
//
// Файл: CREATE_NEW (O_CREATE|O_EXCL, пустой) → Truncate(N) (SetEndOfFile —
// логический EOF достоверных нулей БЕЗ записи данных) → Chtimes(mtime) →
// convert(MARK_IN_SYNC|DEHYDRATE); отказ convert с DEHYDRATE → fallback:
// convert(MARK_IN_SYNC) → UpdatePlaceholder(FsMetadata{size,mtime},
// DEHYDRATE|MARK_IN_SYNC). Каталог: Mkdir → Chtimes → convert(MARK_IN_SYNC).
//
// Результат: resultCreated — запись доведена до конечного состояния
// (дегидратированный плейсхолдер in sync); resultExists — имя занято
// (O_EXCL-отказ «already exists» — переклассификация вызывающим по
// индексу/состоянию, design.md D6.4, не ошибка); resultFailed — отказ на
// любом шаге (созданная реальная запись удалена; неудавшееся удаление —
// *quarantineError в цепочке err). FileIdentity — inode LE 8 байт, та же
// сериализация, что в makeCreateInfo.
func createEntry(p *Provider, dir string, e BackendEntry) (placeholderResultKind, error) {
	rel := fsJoin(dir, e.Name)
	local := localJoin(p.root, rel)
	mtime := time.Unix(e.MtimeSec, int64(e.MtimeNsec))
	identity := make([]byte, 8)
	binary.LittleEndian.PutUint64(identity, e.Inode)
	// Окно супрессии watcher-событий на СОБСТВЕННОЕ создание (живой кейс
	// 27.11, AgioDrive9/facility: watcher видел walk-созданный каталог
	// ДО записи в индекс → notifyDirCreate «не знал» его → лишний backend
	// Mkdir в чужой компании → EXDEV и вечный retry журнала). Убирается
	// гонка только по времени: событие в 10-секундном окне супрессии
	// (watcher-путь) игнорируется, callback-путь фильтра не затрагивается.
	p.markSelfEvent(strings.ToLower(rel))
	if e.IsDir {
		// Каталог (design.md D6.4): CreateDirectory → SetFileTime →
		// convert(MARK_IN_SYNC); fallback для каталогов не предусмотрен.
		if err := os.Mkdir(local, 0o755); err != nil {
			if errors.Is(err, os.ErrExist) {
				return resultExists, nil
			}
			return resultFailed, fmt.Errorf("mkdir %s: %w", rel, err)
		}
		if err := os.Chtimes(local, mtime, mtime); err != nil {
			return failedAfterCreate(p, local, rel, "chtimes", err)
		}
		if err := p.store.Convert(local, true, identity, cfapi.ConvertFlagMarkInSync); err != nil {
			return failedAfterCreate(p, local, rel, "convert dir", err)
		}
		return resultCreated, nil
	}
	f, err := os.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return resultExists, nil
		}
		return resultFailed, fmt.Errorf("create %s: %w", rel, err)
	}
	if err := f.Truncate(e.Size); err != nil {
		_ = f.Close()
		return failedAfterCreate(p, local, rel, "truncate", err)
	}
	if err := os.Chtimes(local, mtime, mtime); err != nil {
		_ = f.Close()
		return failedAfterCreate(p, local, rel, "chtimes", err)
	}
	if err := f.Close(); err != nil {
		return failedAfterCreate(p, local, rel, "close", err)
	}
	if err := p.store.Convert(local, false, identity, cfapi.ConvertFlagMarkInSync|cfapi.ConvertFlagDehydrate); err != nil {
		// Fallback (design.md D6.4): convert без дегидратации, затем
		// UpdatePlaceholder с DEHYDRATE|MARK_IN_SYNC; результат пробы —
		// одна строка.
		logger.Infof("create %s: convert(DEHYDRATE) rejected: %v; fallback convert+update", rel, err)
		if ferr := p.store.Convert(local, false, identity, cfapi.ConvertFlagMarkInSync); ferr != nil {
			return failedAfterCreate(p, local, rel, "convert", ferr)
		}
		if uerr := p.store.Update(local, makeFsMetadata(e), cfapi.UpdateFlagDehydrate|cfapi.UpdateFlagMarkInSync); uerr != nil {
			// Остался бы гидратированный плейсхолдер (EOF-нули без DEHYDRATE
			// не дают FETCH_DATA) — недопустимое конечное состояние (D6.4),
			// откат.
			return failedAfterCreate(p, local, rel, "dehydrate", uerr)
		}
	}
	return resultCreated, nil
}

// errorAlreadyExistsResult — HRESULT_FROM_WIN32(ERROR_ALREADY_EXISTS),
// бит-паттерн 0x800700B7 (HRESULT — int32; знаковое представление
// -2147024713 = 0x800700B7 - 2^32): per-entry Result
// CF_PLACEHOLDER_CREATE_INFO — HRESULT (cfapi.h), Win32-коды приходят
// обёрнутыми через HRESULT_FROM_WIN32; фильтр возвращает этот код для
// записи, чей плейсхолдер уже существует. Это НЕ отказ: запись — наш
// плейсхолдер, она попадает в индекс (спека «Перезапуск без дублей» —
// дубль фильтр не создаёт).
const errorAlreadyExistsResult = cfapi.HRESULT(-2147024713) // 0x800700B7

// Классификация per-entry Result после Cf-вызова (Result — наш буфер,
// фильтр записывает его при обработке Create/TRANSFER-ответа).
type placeholderResultKind int

const (
	resultCreated placeholderResultKind = iota // 0 — запись создана
	resultExists                               // ALREADY_EXISTS — плейсхолдер уже был
	resultFailed                               // прочее — отказ фильтра
)

func classifyPlaceholderResult(res cfapi.HRESULT) placeholderResultKind {
	switch res {
	case 0:
		return resultCreated
	case errorAlreadyExistsResult:
		return resultExists
	default:
		return resultFailed
	}
}

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
		// %w — классификация ошибок листинга (design.md D6.6) через
		// errors.As на syscall.Errno.
		return nil, fmt.Errorf("open %s: %w", dir, errno)
	}
	defer f.Close(mctx) //nolint:errcheck
	entries, errno := f.ReaddirPlus(mctx, 0)
	if errno != 0 {
		return nil, fmt.Errorf("readdir %s: %w", dir, errno)
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

func (osStore) Convert(path string, isDir bool, fileIdentity []byte, convertFlags uint32) error {
	return cfapi.ConvertToPlaceholderByPath(path, isDir, fileIdentity, convertFlags)
}

// SetInSync — пометка плейсхолдера синхронизированным по пути
// (CfSetInSyncState(InSyncStateInSync); write-путь выгрузки, design.md D8:
// при inSync=TrackNone фильтр сам состояние не переключает — обязанность
// провайдера после подтверждённого Flush). cfapi.PlaceholderStateByPath
// (FileBasicInfo-эвристика) классификацией больше не используется — после
// convert-DEHYDRATE фильтр чистит basic-атрибуты и она возвращает 0 для
// реального плейсхолдера (живой ретест 27.11); осталась для `status <путь>`.
func (osStore) SetInSync(path string) error {
	return cfapi.SetInSyncStateByPath(path, cfapi.InSyncStateInSync)
}

// AnswerDelete — ACK_DELETE на NOTIFY_DELETE (U19: blocked-style, rm
// пользователя не завершается без ответа; каталоги — тот же колбэк с флагом
// IS_DIRECTORY).
func (osStore) AnswerDelete(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, flags uint32, status cfapi.NTStatus) error {
	return cfapi.AnswerDelete(conn, transferKey, requestKey, flags, status)
}

// AnswerRename — ACK_RENAME на NOTIFY_RENAME (U19; blocked-style).
func (osStore) AnswerRename(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, flags uint32, status cfapi.NTStatus) error {
	return cfapi.AnswerRename(conn, transferKey, requestKey, flags, status)
}

func (osStore) Update(path string, fsMetadata cfapi.CFFileMetadata, updateFlags uint32) error {
	return cfapi.UpdatePlaceholderByPath(path, &fsMetadata, updateFlags)
}

func (osStore) StandardInfo(path string) (*cfapi.CFPlaceholderStandardInfo, error) {
	return cfapi.PlaceholderStandardInfoByPath(path, false)
}

func (osStore) AnswerFetchPlaceholders(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, infos []cfapi.CFPlaceholderCreateInfo, total int64, entriesProcessed uint32) error {
	return cfapi.AnswerFetchPlaceholders(conn, transferKey, requestKey, status, infos, total, entriesProcessed)
}

func (osStore) TransferData(conn cfapi.ConnectionKey, transferKey cfapi.TransferKey, requestKey cfapi.RequestKey, status cfapi.NTStatus, offset, length int64, buf []byte) error {
	return cfapi.AnswerFetchData(conn, transferKey, requestKey, status, buf, offset, length)
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
// параллельный список записей backend (для записи в индекс по результату),
// плюс existing — существующие плейсхолдеры, отфильтрованные от
// пересоздания (только create-путь; в ответе FETCH_PLACEHOLDERS они входят
// в infos — «batch complete», см. комментарий handleFetchPlaceholders).
type populatedDir struct {
	baseDirPath string // локальный базовый каталог CfCreatePlaceholders
	infos       []cfapi.CFPlaceholderCreateInfo
	entries     []BackendEntry // параллелен infos; пустое имя — не создан
	identities  [][]byte       // копии FileIdentity — удержание памяти до Cf-вызова
	existing    []BackendEntry // create-путь: уже на диске плейсхолдеры — только в индекс
	conflicts   int            // пропущено из-за одноимённого локального не-плейсхолдера
}

// buildPopulatedDir готовит массив CF_PLACEHOLDER_CREATE_INFO для каталога
// dir (путь в стиле fs) по записям backend:
//   - одноимённый локальный не-плейсхолдер — конфликт: запись в лог, пропуск
//     (спека «Одноимённый локальный файл», инвариант «локальный файл
//     пользователя не перезаписывается и не удаляется»); из массива
//     исключается на обоих путях;
//   - существующий плейсхолдер: на create-пути (includeExisting=false,
//     наполнение верхнего уровня) — без повторного создания, только в индекс
//     (спека «Перезапуск без дублей»; журнал мог быть утерян при живом
//     диске); в ответе FETCH_PLACEHOLDERS (includeExisting=true) — входит в
//     массив: «batch complete»-ответ несёт все записи каталога, фильтр
//     ответит per-entry ERROR_ALREADY_EXISTS (классификатор различает).
//
// makeFsMetadata — CF_FS_METADATA записи backend: mtime (FILETIME →
// LARGE_INTEGER), размер и атрибуты (DIRECTORY/NORMAL). Используется и в
// CF_PLACEHOLDER_CREATE_INFO (FETCH-fallback), и в fallback-дегидратации
// convert-примитива (design.md D6.4).
func makeFsMetadata(e BackendEntry) cfapi.CFFileMetadata {
	ft := windows.NsecToFiletime(e.MtimeSec*1e9 + int64(e.MtimeNsec))
	lastWrite := int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if e.IsDir {
		attrs = windows.FILE_ATTRIBUTE_DIRECTORY
	}
	return cfapi.CFFileMetadata{
		BasicInfo: cfapi.FileBasicInfo{
			CreationTime:   lastWrite,
			LastAccessTime: lastWrite,
			LastWriteTime:  lastWrite,
			ChangeTime:     lastWrite,
			FileAttributes: attrs,
		},
		FileSize: e.Size,
	}
}

// makeCreateInfo — CF_PLACEHOLDER_CREATE_INFO записи backend: имя UTF-16,
// метаданные (mtime, размер, атрибуты), FileIdentity = inode 8B LE
// (design.md D6, инвариант). Возвращает копию identity — удержание памяти
// до завершения Cf-вызова (GC видит срез).
func makeCreateInfo(e BackendEntry) (cfapi.CFPlaceholderCreateInfo, []byte, error) {
	name16, err := windows.UTF16PtrFromString(e.Name)
	if err != nil {
		return cfapi.CFPlaceholderCreateInfo{}, nil, err
	}
	identity := make([]byte, 8)
	binary.LittleEndian.PutUint64(identity, e.Inode)
	info := cfapi.CFPlaceholderCreateInfo{
		RelativeFileName:   name16,
		FsMetadata:         makeFsMetadata(e),
		FileIdentity:       unsafe.Pointer(&identity[0]),
		FileIdentityLength: uint32(len(identity)),
		Flags:              placeholderCreateFlags,
	}
	return info, identity, nil
}

func buildPopulatedDir(ctx context.Context, p *Provider, dir string, entries []BackendEntry, includeExisting bool) (*populatedDir, error) {
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
		if exists && isPlaceholder && !includeExisting {
			// Create-путь: перезапуск, плейсхолдер уже на диске — повторное
			// создание не нужно (дубль), индекс обновляем.
			out.existing = append(out.existing, e)
			continue
		}
		info, identity, err := makeCreateInfo(e)
		if err != nil {
			logger.Warnf("placeholder %s: invalid name: %v; entry skipped", rel, err)
			continue
		}
		out.infos = append(out.infos, info)
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

// dirPopulation — итог примитива «обработать каталог D» (design.md
// D6.3–6.7): классификация записей, поштучное создание (convert-примитив,
// D6.4), индекс/журнал, подкаталоги к обходу. Листинг D — у вызывающего:
// семантика retry различается (готовность — 2с→30с целиком, walk —
// 1с→30с на каталог).
type dirPopulation struct {
	dir              string
	subdirs          []string // fs-пути подкаталогов к обходу (created + existing; конфликтные — нет)
	createdFiles     int      // создано плейсхолдеров-файлов
	createdDirs      int      // создано плейсхолдеров-каталогов
	skipped          int      // existing: по индексу + восстановленные + переклассифицированные EEXIST
	conflicts        int      // одноимённые локальные не-плейсхолдеры, не в индексе
	conflictExamples []string // до 10 имён для Warn
	failedEntries    int      // записи с отказом createEntry (после отката; карантин тоже здесь)
}

// populateDirectory — примитив «обработать каталог D» (design.md D6, такт 1
// и шаги 6.2–6.7 полного walk): локальное перечисление D без учёта регистра
// → классификация to-create/existing/conflict → создание to-create записей
// convert-примитивом createEntry ПОШТУЧНО, последовательно одним воркером
// (design.md D6.4) → индекс/журнал после доведения записи до конечного
// состояния → подкаталоги (created и existing) к обходу. Conflict-каталог
// в поддерево не идёт.
//
// Классификация (design.md D6.5 + защита локальных файлов):
//   - имени нет локально → to-create (createEntry);
//   - имя есть, inode известен индексу → existing: skip без Warn, журнал —
//     только если путь в индексе изменился;
//   - имя есть, в индексе нет, локально наш плейсхолдер → восстановление
//     индекса (утерянный журнал при живом диске), skip;
//   - имя есть, в индексе нет, локально не плейсхолдер → conflict: skip,
//     один Warn на каталог у вызывающего.
//
// Сбои createEntry (design.md D6.4): созданная реальная запись удаляется;
// групповой Warn на каталог до 10 имён вместо одиночных; неудавшееся
// удаление — «карантин» (`walk quarantine:`), на следующем walk конфликт.
func populateDirectory(ctx context.Context, p *Provider, dir string, entries []BackendEntry) *dirPopulation {
	res := &dirPopulation{dir: dir}
	baseDir := localJoin(p.root, dir)
	// Локальное перечисление D (design.md D6.5). Ошибка чтения — в лог,
	// классификация идёт по пустому списку: createEntry с O_EXCL/Mkdir
	// поверх обычного файла пользователя не пройдёт — EEXIST-переклассификация
	// даст конфликт, запись не затирается.
	present := make(map[string]struct{})
	presentNames := make(map[string]string) // нижний регистр → оригинал (сверка)
	names, err := p.local.ListNames(baseDir)
	if err != nil {
		logger.Warnf("walk %s: list local %s: %v; conflict detection is limited", dir, baseDir, err)
	}
	for _, n := range names {
		low := strings.ToLower(n)
		present[low] = struct{}{}
		presentNames[low] = n
	}
	var failedExamples []string // до 10 имён для группового Warn каталога
	for _, e := range entries {
		rel := fsJoin(dir, e.Name)
		localPath := localJoin(p.root, rel)
		if _, isPresent := present[strings.ToLower(e.Name)]; isPresent {
			classifyOccupied(p, res, e, rel, localPath, true, &failedExamples)
			continue
		}
		if _, known := p.idx.Path(e.Inode); known {
			// Inode известен индексу, локального имени нет: удаление
			// известного плейсхолдера, не дошедшее до backend (сбой после
			// локального удаления). Повторное создание потеряло бы удаление
			// пользователя; удаление в backend ставит сверка (reconcileDir,
			// сценарий «Восстановление после сбоя»).
			res.skipped++
			continue
		}
		// to-create: convert-примитив (design.md D6.4).
		kind, cerr := createEntry(p, dir, e)
		applyCreateResult(p, res, e, rel, localPath, kind, cerr, &failedExamples)
	}
	// Write-часть сверки (design.md D9, задача 6.4): каталог обработан —
	// плейсхолдеры вне in-sync и локальные файлы без записи backend →
	// очередь выгрузки; записи индекса без локального файла → очередь
	// удаления backend.
	reconcileDir(ctx, p, dir, entries, presentNames)
	if res.failedEntries > 0 {
		// Групповой Warn на каталог (design.md D6.4), до 10 имён — замена
		// одиночных Warn'ов батч-пути.
		logger.Warnf("walk entry failed: %s: skipped %d, e.g. %v", dir, res.failedEntries, failedExamples)
	}
	return res
}

// applyCreateResult — проводка результата createEntry: созданная запись —
// индекс/журнал и счётчики (после доведения до конечного состояния,
// design.md D6.4); EEXIST — переклассификация занятого имени (без повтора
// создания); сбой — failed++, групповой Warn, карантин — отдельная строка.
func applyCreateResult(p *Provider, res *dirPopulation, e BackendEntry, rel, localPath string, kind placeholderResultKind, cerr error, failedExamples *[]string) {
	switch kind {
	case resultCreated:
		if err := p.idx.Put(e.Inode, rel); err != nil {
			logger.Warnf("walk %s: index %s: %v", res.dir, rel, err)
		}
		if e.IsDir {
			res.createdDirs++
			res.subdirs = append(res.subdirs, rel)
		} else {
			res.createdFiles++
		}
	case resultExists:
		// CREATE_NEW-отказ «уже существует» (design.md D6.4):
		// переклассификация по индексу/состоянию — existing/conflict,
		// не ошибка. Повторное создание запрещено (allowCreate=false).
		classifyOccupied(p, res, e, rel, localPath, false, failedExamples)
	default:
		res.failedEntries++
		var q *quarantineError
		if errors.As(cerr, &q) {
			logger.Warnf("walk quarantine: %s", localPath)
			return
		}
		if len(*failedExamples) < 10 {
			*failedExamples = append(*failedExamples, e.Name)
		}
	}
}

// classifyOccupied — классификация записи, чьё имя занято локально
// (design.md D6.5); вызывается также после CREATE_NEW-отказа createEntry
// (D6.4 «переклассификация по индексу»). existing по индексу — skip;
// наш плейсхолдер без записи индекса — восстановление; не-плейсхолдер —
// конфликт; имя исчезло между перечислением и проверкой — to-create
// (только при allowCreate: при переклассификации EEXIST повторного
// создания нет — гонка трактуется как конфликт, без циклов).
func classifyOccupied(p *Provider, res *dirPopulation, e BackendEntry, rel, localPath string, allowCreate bool, failedExamples *[]string) {
	if prev, known := p.idx.Path(e.Inode); known {
		// existing по индексу: skip без Warn (design.md D6.5); журнал —
		// только изменившиеся пути (D6.7).
		if prev != rel {
			if err := p.idx.Put(e.Inode, rel); err != nil {
				logger.Warnf("walk %s: index %s: %v", res.dir, rel, err)
			}
		}
		res.skipped++
		if e.IsDir {
			res.subdirs = append(res.subdirs, rel)
		}
		return
	}
	exists, isPlaceholder, err := p.store.StateOf(localPath)
	if err == nil && !exists {
		if !allowCreate {
			// Имя занято (EEXIST от createEntry), но состояние уже пусто:
			// двойная гонка — конфликт (без повторов создания).
			res.conflicts++
			if len(res.conflictExamples) < 10 {
				res.conflictExamples = append(res.conflictExamples, e.Name)
			}
			return
		}
		// Имя было в перечислении и исчезло к проверке (гонка с
		// пользователем): локально пусто — создание безопасно.
		kind, cerr := createEntry(p, res.dir, e)
		applyCreateResult(p, res, e, rel, localPath, kind, cerr, failedExamples)
		return
	}
	if err != nil || !isPlaceholder {
		// conflict: одноимённый локальный не-плейсхолдер, индексу
		// неизвестный — skip (инвариант «локальный файл пользователя не
		// перезаписывается и не удаляется»); один Warn на каталог.
		res.conflicts++
		if len(res.conflictExamples) < 10 {
			res.conflictExamples = append(res.conflictExamples, e.Name)
		}
		return
	}
	// Наш плейсхолдер, записи в индексе нет (утерянный журнал при живом
	// диске) — восстановление: изменение → строка журнала (D6.7). Краш-окно
	// fallback-дегидратации (design.md D6.4): convert(MARK_IN_SYNC) прошёл,
	// а Update(DEHYDRATE) нет — остался плейсхолдер с EOF-нулями без
	// индексной записи; перед усыновлением файла дегидратированность
	// проверяется и нормализуется — иначе чтение вернуло бы «молчаливые
	// гидратированные нули» (FETCH_DATA не пришёл бы). У каталогов ветки
	// дегидратации нет (D6.4: только convert(MARK_IN_SYNC)) — принимаются
	// без проверки.
	if !e.IsDir {
		if nerr := ensureDehydrated(p, e, localPath); nerr != nil {
			if rerr := rollbackEntry(localPath); rerr != nil {
				nerr = fmt.Errorf("%v; rollback: %w", nerr, rerr)
				var q *quarantineError
				if errors.As(rerr, &q) {
					logger.Warnf("walk quarantine: %s", localPath)
				}
			}
			res.failedEntries++
			if len(*failedExamples) < 10 {
				*failedExamples = append(*failedExamples, e.Name)
			}
			return
		}
	}
	if err := p.idx.Put(e.Inode, rel); err != nil {
		logger.Warnf("walk %s: index %s: %v", res.dir, rel, err)
	}
	res.skipped++
	if e.IsDir {
		res.subdirs = append(res.subdirs, rel)
	}
}

// ensureDehydrated — нормализация плейсхолдера перед усыновлением (design.md
// D6.4, краш-окно fallback-ветки createEntry): дегидратированный плейсхолдер
// (OnDiskDataSize == 0) проходит без записи; с данными на диске
// (OnDiskDataSize > 0) дегидратируется повторно Update(DEHYDRATE|
// MARK_IN_SYNC); недоступное состояние (ошибка CfGetPlaceholderInfo) —
// консервативная повторная дегидратация. Отказ нормализации — ошибка:
// вызывающий откатывает запись (гидратированных нулей не остаётся).
func ensureDehydrated(p *Provider, e BackendEntry, localPath string) error {
	info, err := p.store.StandardInfo(localPath)
	if err == nil && info.OnDiskDataSize == 0 {
		return nil // дегидратирован — нормализация не нужна
	}
	if err != nil {
		logger.Debugf("adopt %s: placeholder info unavailable: %v; re-dehydrating conservatively", localPath, err)
	}
	return p.store.Update(localPath, makeFsMetadata(e), cfapi.UpdateFlagDehydrate|cfapi.UpdateFlagMarkInSync)
}

// populateTopLevel наполняет верхний уровень sync root примитивом
// «обработать каталог» (design.md D6, такт 1) и сохраняет подкаталоги как
// стартовую очередь фонового walk (такт 2). Ошибка чтения backend
// возвращается вызывающему (retry-цикл готовности 2с→30с до маркера),
// ошибки отдельных записей — в лог.
func populateTopLevel(ctx context.Context, p *Provider) error {
	entries, err := p.backend.List(ctx, "/")
	if err != nil {
		return fmt.Errorf("list backend root: %w", err)
	}
	res := populateDirectory(ctx, p, "/", entries)
	p.setRootSubdirs(res.subdirs)
	if res.conflicts > 0 {
		logger.Warnf("top-level population: %d entr(ies) skipped due to conflicting local files, e.g. %v", res.conflicts, res.conflictExamples)
	}
	created := res.createdFiles + res.createdDirs
	if res.skipped > 0 {
		logger.Infof("top-level population: %d placeholder(s) already present in %s", res.skipped, localJoin(p.root, "/"))
	}
	logger.Infof("top-level population: %d placeholders created in %s", created, localJoin(p.root, "/"))
	return nil
}

// runPopulation — fallback-воркер очереди FETCH_PLACEHOLDERS (design.md D6,
// редакция 27.11): основной путь наполнения — полный upfront-walk через
// convert-примитив; FETCH-воркер обслуживает только population
// PARTIAL/FULL (при дефолтном ALWAYS_FULL платформа FETCH не вызывает —
// U14). Каждое задание завершается ровно одним ответом
// TRANSFER_PLACEHOLDERS — с записями или со статусом отказа.
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
		if err := p.store.AnswerFetchPlaceholders(job.ConnectionKey, job.TransferKey, job.RequestKey, cfapi.StatusCancelled, nil, cfapi.EOF, 0); err != nil {
			logger.Warnf("answer canceled FETCH_PLACEHOLDERS: %v", err)
		}
		return
	}
	// Деривация каталога задания (инструментирование пробы U2: семантика
	// NormalizedPath на живом фильтре уточняется — в лог пишется, какая
	// ветка сработала). Ветка 1: job.Path распознан — полный путь под sync
	// root (ConnectFlagRequireFullFilePath → CF_CALLBACK_INFO.NormalizedPath)
	// или относительный. Ветка 2: путь пуст/вне корня — каталог через
	// FileIdentity: job.FileID — наш inode с момента создания плейсхолдера
	// (design.md D6), ResolveIno поднимается по родителям через meta.
	// Ветка 3: не распозналось ничего — корень: ожидаемо для перечисления
	// самого sync root (его FileIdentity — не наш inode); другой случай —
	// дефект деривации, лог warns (ответ корневыми записями в чужом каталоге
	// создал вложенный дубль на живой пробе 2026-11-27 — потому ветки 1-2
	// обязательны перед этим fallback'ом).
	dir := p.fsPathOf(job.Path)
	via := "NormalizedPath"
	switch {
	case dir != "":
	case job.FileID != 0 && p.idx != nil:
		if resolved, ok := p.idx.ResolveIno(ctx, job.FileID); ok {
			dir = resolved
			via = "FileIdentity"
		}
	}
	if dir == "" {
		dir = "/"
		via = "fallback-root"
		logger.Warnf("FETCH_PLACEHOLDERS: directory not derivable from NormalizedPath %q and FileIdentity %d; assuming root", job.Path, job.FileID)
	}
	logger.Debugf("FETCH_PLACEHOLDERS dir=%s via=%s normalized=%q fileID=%d", dir, via, job.Path, job.FileID)
	entries, err := p.backend.List(ctx, dir)
	if err != nil {
		logger.Warnf("FETCH_PLACEHOLDERS %s: list backend: %v", dir, err)
		// EOF в ошибочных ответах не ставится: для фильтра EOF =
		// «полное число записей неизвестно — population не завершится
		// никогда» (проба 27.11: два FETCH на корень с дельтой ровно 60 c —
		// таймаут callback'а при total=EOF). Неуспешный статус сам сообщает
		// фильтру об отказе; total 0 ничего не обещает.
		_ = p.store.AnswerFetchPlaceholders(job.ConnectionKey, job.TransferKey, job.RequestKey, cfapi.StatusCloudFileUnsuccessful, nil, 0, 0)
		return
	}
	// Итоговая семантика FETCH_PLACEHOLDERS — канонический «batch complete»
	// ответ; полная история живых проб U2 (27.11):
	//   (1) ParamSize = размер ветки (32) + count=0/NULL → E_INVALIDARG,
	//       перечисление подвисло; лечится union-размером (operations.go);
	//   (2) total=EOF(-1) → фильтр «никогда не завершит» population: 60 c
	//       таймаут callback'а, повторный FETCH (дельта ровно 60 c);
	//   (3) реальный total + fresh-only (count=0, processed=0): create
	//       перестал отказывать, но ПЕНДИТСЯ вечно — population завершается
	//       по EntriesProcessed >= PlaceholderTotalCount («0 из 1» = не
	//       завершено);
	//   (4) ИТОГ: массив несёт ВСЕ записи каталога (fresh + existing;
	//       конфликты — локальные не-плейсхолдеры — по-прежнему исключены),
	//       EntriesProcessed = полное число записей каталога = total →
	//       processed >= total → population завершается сразу. Per-entry
	//       Result: existing → ожидаемо ALREADY_EXISTS (классификатор — в
	//       индекс), fresh → 0.
	built, err := buildPopulatedDir(ctx, p, dir, entries, true)
	if err != nil {
		logger.Warnf("FETCH_PLACEHOLDERS %s: build placeholders: %v", dir, err)
		_ = p.store.AnswerFetchPlaceholders(job.ConnectionKey, job.TransferKey, job.RequestKey, cfapi.StatusCloudFileUnsuccessful, nil, int64(len(entries)), 0)
		return
	}
	// Записи передаются фильтру в ответе — отдельный CfCreatePlaceholders не
	// нужен: TRANSFER_PLACEHOLDERS сам создаёт плейсхолдеры относительно
	// каталога задания. Истинно пустой каталог отвечает count=0+SUCCESS с
	// processed=total=0 — завершение сразу.
	//
	// PlaceholderTotalCount — РЕАЛЬНОЕ полное число записей backend каталога
	// (fresh + existing + conflicts — всё, что вернул List); EntriesProcessed
	// — то же число (кумулятивный счётчик, single-shot ответ = всё отдано;
	// конфликты не передаются, но счётчик должен дойти до total — иначе
	// population не завершится). EOF остаётся только в cancel-ветке.
	if err := p.store.AnswerFetchPlaceholders(job.ConnectionKey, job.TransferKey, job.RequestKey, cfapi.StatusSuccess, built.infos, int64(len(entries)), uint32(len(entries))); err != nil {
		logger.Warnf("FETCH_PLACEHOLDERS %s: answer: %v", dir, err)
		return
	}
	// Per-entry Result — наш буфер: фильтр пишет его при обработке ответа
	// (массив валиден до возврата CfExecute — он и есть). 0 = создана;
	// ALREADY_EXISTS = уже наш плейсхолдер (existing в batch-ответе —
	// ожидаемо, и race с параллельным наполнением) — остаётся в индексе;
	// прочее — отказ фильтра: из индекса исключается.
	for i := range built.infos {
		switch classifyPlaceholderResult(built.infos[i].Result) {
		case resultCreated:
		case resultExists:
			logger.Debugf("placeholder %s: already exists (filter reported)", fsJoin(dir, built.entries[i].Name))
		default:
			logger.Warnf("placeholder %s: transfer failed: HRESULT 0x%08x", fsJoin(dir, built.entries[i].Name), uint32(built.infos[i].Result))
			built.entries[i] = BackendEntry{} // отказ фильтра — не в индекс
		}
	}
	built.saveIndexed(p, dir)
	logger.Debugf("FETCH_PLACEHOLDERS dir=%s: %d entries listed (total=%d processed=%d), %d placeholders transferred (%d conflicts skipped)", dir, len(entries), int64(len(entries)), uint32(len(entries)), len(built.infos), built.conflicts)
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
