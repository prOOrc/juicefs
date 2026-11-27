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
// fs (разделитель "/", от корня): "/a/b"; корень — "/".
type indexRecord struct {
	Ino  uint64 `json:"ino"`
	Path string `json:"path"`
}

// indexJournalName — файл журнала в каталоге состояния. Простой append:
// каждая строка самостоятельная запись, последняя может быть обрезана при
// падении — при загрузке неполные строки пропускаются (ротация и повтор
// невыполненных операций — задача 6.3, отдельный Journal).
const indexJournalName = "placeholders.journal"

// Index — inode → путь: карта в памяти + append-журнал. События «известного
// плейсхолдера» (переименование, удаление) журнал не содержит до этапа 6.
type Index struct {
	mu    sync.RWMutex
	byIno map[uint64]string
	jPath string
	jFile *os.File

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
	ix := &Index{byIno: make(map[uint64]string), jPath: jPath}
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

// load читает журнал: валидные JSONL-строки в карту, обрезанные/битые
// пропускаются с предупреждением (журнал — append-file, падение могло
// оборвать последнюю строку).
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
		if rec.Path == "" {
			logger.Warnf("index journal %s line %d has no path, skipping", ix.jPath, line)
			continue
		}
		// Повтор записи по тому же inode — последняя версия побеждает
		// (переименование переписывает путь, задача 6.3 продолжит контракт).
		ix.byIno[rec.Ino] = rec.Path
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

// Put сохраняет соответствие inode → путь: память + append в журнал.
func (ix *Index) Put(ino uint64, path string) error {
	ix.mu.Lock()
	ix.byIno[ino] = path
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

// Path возвращает путь по inode из памяти.
func (ix *Index) Path(ino uint64) (string, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	p, ok := ix.byIno[ino]
	return p, ok
}

// LookupPath — inode записи по пути (обратный поиск по карте; запись
// `status <путь>`, задача 5.7). Пути сравниваются в нижнем регистре —
// Windows-семантика, как в классификации walk (design.md D6.5).
func (ix *Index) LookupPath(path string) (uint64, bool) {
	low := strings.ToLower(path)
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	for ino, p := range ix.byIno {
		if strings.ToLower(p) == low {
			return ino, true
		}
	}
	return 0, false
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
