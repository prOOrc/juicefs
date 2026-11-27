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

// Index — отображение inode → путь (design.md D6): в памяти, с журналом
// JSONL в каталоге состояния (дефолт %LOCALAPPDATA%\AgioDrive\<volume>,
// proposal D-8; флаг --state-dir). Наполняется при создании плейсхолдеров;
// при промахе ResolveIno путь поднимается по родителям через meta (U12).
package cfprovider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// indexRecord — одна строка журнала плейсхолдеров (JSONL). Путь — в стиле
// fs (разделитель "/", от корня): "/a/b"; корень — "/". Пустой путь —
// надгробие удаления записи (этап 6: события удаления/переименования
// известных плейсхолдеров тоже отражаются в журнале, чтобы перезапуск не
// воскресил удалённое).
type indexRecord struct {
	Ino  uint64 `json:"ino"`
	Path string `json:"path"`
}

// indexJournalName — файл журнала в каталоге состояния. Простой append:
// каждая строка самостоятельная запись, последняя может быть обрезана при
// падении — при загрузке неполные строки пропускаются (невыполненные
// операции backend — отдельный Journal, задача 6.3, ops.journal).
const indexJournalName = "placeholders.journal"

// Index — inode → путь: карты в памяти (прямая и обратная) + append-журнал.
// События известных плейсхолдеров (этап 6): Delete — удаление (надгробие в
// журнале), RenamePrefix — переименование каталога (пути потомков).
type Index struct {
	mu     sync.RWMutex
	byIno  map[uint64]string
	byPath map[string]uint64 // lower(path) → ino (LookupPath/PathsInDir без обхода)
	jPath  string
	jFile  *os.File

	// resolver — подъём по родителям через meta при промахе карты (U12);
	// ставит провайдер при подключённом data path (WithData). nil — только
	// память.
	resolver func(ctx context.Context, ino uint64) (string, bool)
}

// DefaultStateDir — каталог состояния по умолчанию для тома volume:
// %LOCALAPPDATA%\AgioDrive\<volume> (proposal D-8).
func DefaultStateDir(volume string) string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		// LOCALAPPDATA отсутствует (сервисная сессия): типовое расположение
		// профиля пользователя.
		base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
	}
	return filepath.Join(base, "AgioDrive", volume)
}

// NewIndex открывает (создаёт) каталог состояния и журнал, загружает
// накопленные записи в память.
func NewIndex(stateDir string) (*Index, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("cfprovider: state directory is empty")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("cfprovider: create state dir %s: %w", stateDir, err)
	}
	jPath := filepath.Join(stateDir, indexJournalName)
	ix := &Index{byIno: make(map[uint64]string), byPath: make(map[string]uint64), jPath: jPath}
	if err := ix.load(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(jPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cfprovider: open journal %s: %w", jPath, err)
	}
	ix.jFile = f
	return ix, nil
}

// load читает журнал: валидные JSONL-строки в карты, обрезанные/битые
// пропускаются с предупреждением (журнал — append-file, падение могло
// оборвать последнюю строку). Пустой путь — надгробие: запись inode
// удаляется (этап 6: переименование/удаление известных плейсхолдеров).
func (ix *Index) load() error {
	f, err := os.Open(ix.jPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cfprovider: open journal %s: %w", ix.jPath, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // путь может быть длинным
	line := 0
	loaded := 0
	for scanner.Scan() {
		line++
		var rec indexRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			logger.Warnf("index journal %s line %d is not valid JSON, skipping: %v", ix.jPath, line, err)
			continue
		}
		// Повтор записи по тому же inode — последняя версия побеждает;
		// надгробие (пустой путь) снимает запись (удаление плейсхолдера).
		if prev, ok := ix.byIno[rec.Ino]; ok {
			delete(ix.byPath, strings.ToLower(prev))
		}
		if rec.Path == "" {
			delete(ix.byIno, rec.Ino)
			loaded++
			continue
		}
		ix.byIno[rec.Ino] = rec.Path
		ix.byPath[strings.ToLower(rec.Path)] = rec.Ino
		loaded++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("cfprovider: scan journal %s: %w", ix.jPath, err)
	}
	logger.Debugf("index journal %s loaded: %d records, %d inodes", ix.jPath, loaded, len(ix.byIno))
	return nil
}

// SetResolver ставит функцию подъёма по родителям (meta-обход, U12).
func (ix *Index) SetResolver(r func(ctx context.Context, ino uint64) (string, bool)) {
	ix.mu.Lock()
	ix.resolver = r
	ix.mu.Unlock()
}

// Put сохраняет соответствие inode → путь: память (обе карты) + append в
// журнал. Повторный Put по тому же inode (переименование) обновляет обе
// карты.
func (ix *Index) Put(ino uint64, path string) error {
	ix.mu.Lock()
	if prev, ok := ix.byIno[ino]; ok {
		delete(ix.byPath, strings.ToLower(prev))
	}
	ix.byIno[ino] = path
	ix.byPath[strings.ToLower(path)] = ino
	ix.mu.Unlock()
	if ix.jFile == nil {
		return nil
	}
	b, err := json.Marshal(indexRecord{Ino: ino, Path: path})
	if err != nil {
		return fmt.Errorf("cfprovider: marshal index record: %w", err)
	}
	if _, err := ix.jFile.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("cfprovider: append journal %s: %w", ix.jPath, err)
	}
	return nil
}

// Delete снимает запись inode (удаление известного плейсхолдера, этап 6):
// память + надгробие в журнале (пустой путь), чтобы перезапуск не
// воскресил запись.
func (ix *Index) Delete(ino uint64) error {
	ix.mu.Lock()
	prev, ok := ix.byIno[ino]
	if ok {
		delete(ix.byIno, ino)
		delete(ix.byPath, strings.ToLower(prev))
	}
	ix.mu.Unlock()
	if !ok || ix.jFile == nil {
		return nil
	}
	b, err := json.Marshal(indexRecord{Ino: ino})
	if err != nil {
		return fmt.Errorf("cfprovider: marshal index tombstone: %w", err)
	}
	if _, err := ix.jFile.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("cfprovider: append journal %s: %w", ix.jPath, err)
	}
	return nil
}

// RenamePrefix переписывает пути каталога dir и всех его потомков на newDir
// (переименование каталога-плейсхолдера, этап 6): каждой изменённой записи —
// строка журнала. Возвращает число переписанных записей.
func (ix *Index) RenamePrefix(oldDir, newDir string) (int, error) {
	oldTrim := strings.TrimSuffix(oldDir, "/")
	oldLow := strings.ToLower(oldTrim)
	prefixLow := oldLow + "/"
	ix.mu.Lock()
	type moved struct {
		ino  uint64
		path string
	}
	var moves []moved
	for ino, p := range ix.byIno {
		low := strings.ToLower(p)
		if low != oldLow && !strings.HasPrefix(low, prefixLow) {
			continue
		}
		np := newDir + p[len(oldTrim):]
		moves = append(moves, moved{ino: ino, path: np})
	}
	for _, m := range moves {
		delete(ix.byPath, strings.ToLower(ix.byIno[m.ino]))
		ix.byIno[m.ino] = m.path
		ix.byPath[strings.ToLower(m.path)] = m.ino
	}
	jFile := ix.jFile
	ix.mu.Unlock()
	renamed := 0
	for _, m := range moves {
		if jFile == nil {
			renamed++
			continue
		}
		b, err := json.Marshal(indexRecord{Ino: m.ino, Path: m.path})
		if err != nil {
			return renamed, fmt.Errorf("cfprovider: marshal index record: %w", err)
		}
		if _, err := jFile.Write(append(b, '\n')); err != nil {
			return renamed, fmt.Errorf("cfprovider: append journal %s: %w", ix.jPath, err)
		}
		renamed++
	}
	return renamed, nil
}

// DeletePrefix снимает записи каталога dir и всех его потомков (удаление
// каталога-плейсхолдера, этап 6): каждой снятой записи — надгробие в
// журнале. Возвращает число снятых записей.
func (ix *Index) DeletePrefix(dir string) (int, error) {
	trim := strings.TrimSuffix(dir, "/")
	low := strings.ToLower(trim)
	prefixLow := low + "/"
	ix.mu.Lock()
	var inos []uint64
	for ino, p := range ix.byIno {
		lp := strings.ToLower(p)
		if lp != low && !strings.HasPrefix(lp, prefixLow) {
			continue
		}
		inos = append(inos, ino)
	}
	for _, ino := range inos {
		delete(ix.byPath, strings.ToLower(ix.byIno[ino]))
		delete(ix.byIno, ino)
	}
	jFile := ix.jFile
	ix.mu.Unlock()
	removed := 0
	for _, ino := range inos {
		if jFile == nil {
			removed++
			continue
		}
		b, err := json.Marshal(indexRecord{Ino: ino})
		if err != nil {
			return removed, fmt.Errorf("cfprovider: marshal index tombstone: %w", err)
		}
		if _, err := jFile.Write(append(b, '\n')); err != nil {
			return removed, fmt.Errorf("cfprovider: append journal %s: %w", ix.jPath, err)
		}
		removed++
	}
	return removed, nil
}

// IndexPathEntry — прямая запись каталога для сверки (этап 6.4).
type IndexPathEntry struct {
	Ino  uint64
	Path string
}

// PathsInDir возвращает прямых детей каталога dir (путь в стиле fs) из
// карты byPath — записи сверки при старте (design.md D9: «записи индекса,
// отсутствующие на диске в каталогах с завершённым наполнением»).
func (ix *Index) PathsInDir(dir string) []IndexPathEntry {
	prefix := strings.ToLower(strings.TrimSuffix(dir, "/")) + "/"
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var out []IndexPathEntry
	for low, ino := range ix.byPath {
		if !strings.HasPrefix(low, prefix) || strings.Contains(low[len(prefix):], "/") {
			continue // не прямой ребёнок (свои каталоги проверяются отдельно)
		}
		out = append(out, IndexPathEntry{Ino: ino, Path: ix.byIno[ino]})
	}
	return out
}

// Path возвращает путь по inode из памяти.
func (ix *Index) Path(ino uint64) (string, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	p, ok := ix.byIno[ino]
	return p, ok
}

// LookupPath — inode записи по пути (обратный поиск по карте; запись
// `status <путь>`, задача 5.7; события известных плейсхолдеров, этап 6).
// Пути сравниваются в нижнем регистре — Windows-семантика, как в
// классификации walk (design.md D6.5).
func (ix *Index) LookupPath(path string) (uint64, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	ino, ok := ix.byPath[strings.ToLower(path)]
	return ino, ok
}

// IndexPathJournal — путь журнала индекса в каталоге состояния (команды CLI
// проверяют существование журнала перед открытием индекса; `status <путь>`,
// задача 5.7).
func IndexPathJournal(stateDir string) string {
	return filepath.Join(stateDir, indexJournalName)
}

// ResolveIno — путь по inode: карта, затем подъём по родителям через meta
// (U12); найденный resolver'ом путь кешируется как обычная запись.
func (ix *Index) ResolveIno(ctx context.Context, ino uint64) (string, bool) {
	if p, ok := ix.Path(ino); ok {
		return p, true
	}
	ix.mu.RLock()
	resolver := ix.resolver
	ix.mu.RUnlock()
	if resolver == nil {
		return "", false
	}
	path, ok := resolver(ctx, ino)
	if !ok {
		return "", false
	}
	if err := ix.Put(ino, path); err != nil {
		logger.Warnf("cache resolved path of inode %d: %v", ino, err)
	}
	return path, true
}

// Close закрывает журнал.
func (ix *Index) Close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.jFile == nil {
		return nil
	}
	err := ix.jFile.Close()
	ix.jFile = nil
	return err
}
