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

// Фоновый walk hub (design.md D6, такт 2; задачи 2.7–2.11): после маркера
// готовности — обход всего backend в ширину (FIFO, пул из 4 воркеров),
// создание плейсхолдеров всех уровней примитивом populateDirectory
// (placeholders.go). Старт — StartWalk из serve после маркера; остановка —
// отмена ctx или Stop: воркеры дорабатывают текущий каталог, журнал
// сброшен, в логе `walk cancelled:` (ровно одна строка), затем disconnect.
//
// Логика walk не зависит от pkg/cfapi напрямую — backend и создание идут
// через интерфейсы backendLister/placeholderStore, локальное перечисление
// через localLister: юнит-тесты работают на подменных реализациях без
// cldapi.dll (инвариант «Движок проверяется без CfAPI»).
package cfprovider

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"time"
)

// walkWorkers — размер пула воркеров BFS (design.md D6.3).
const walkWorkers = 4

// permanentListError — явный маркер перманентной ошибки листинга: поддерево
// пропускается без повторов (design.md D6.6).
type permanentListError struct{ err error }

func (e *permanentListError) Error() string { return e.err.Error() }

func (e *permanentListError) Unwrap() error { return e.err }

// permanentListErrnos — errno-аналоги «4xx» для листинга (design.md D6.6:
// классификации HTTP-кодов в hub-клиенте нет — перманентные только 4xx).
// На Windows syscall.ENOENT/ENOTDIR равны ERROR_FILE_NOT_FOUND/
// ERROR_PATH_NOT_FOUND (zerrors_windows.go); EACCES/EPERM/EINVAL —
// прикладные значения syscall, которые возвращают слои meta/fs; Errno(5) —
// OS-код ERROR_ACCESS_DENIED. Всё прочее (сеть, таймауты, EIO) —
// транзиентное.
var permanentListErrnos = map[syscall.Errno]bool{
	syscall.ENOENT:   true, // каталога нет (аналог 404)
	syscall.ENOTDIR:  true, // путь — не каталог (аналог 400)
	syscall.EACCES:   true, // отказ доступа (аналог 403)
	syscall.EPERM:    true,
	syscall.EINVAL:   true,
	syscall.Errno(5): true, // ERROR_ACCESS_DENIED (OS-код)
}

type listErrorKind int

const (
	listTransient listErrorKind = iota
	listPermanent
)

// classifyListError — транзиентная/перманентная ошибка листинга (design.md
// D6.6): транзиентные — retry backoff без ограничения; перманентные — Warn,
// failed++, поддерево пропускается.
func classifyListError(err error) listErrorKind {
	var pl *permanentListError
	if errors.As(err, &pl) {
		return listPermanent
	}
	var eno syscall.Errno
	if errors.As(err, &eno) && permanentListErrnos[eno] {
		return listPermanent
	}
	return listTransient
}

// walkStats — счётчики прогресса walk (формат design.md D6.8:
// `walk: dirs=%d files=%d skipped=%d conflicts=%d failed=%d pending=%d elapsed=%s`).
type walkStats struct {
	mu        sync.Mutex
	dirs      int // каталогов обработано
	files     int // создано плейсхолдеров-файлов
	skipped   int // existing: по индексу + восстановленные + ALREADY_EXISTS
	conflicts int // конфликтов (одноимённые локальные не-плейсхолдеры)
	failed    int // каталогов с перманентной ошибкой листинга (поддеревья)
	startedAt time.Time
}

func (s *walkStats) record(res *dirPopulation) {
	s.mu.Lock()
	s.dirs++
	s.files += res.createdFiles
	s.skipped += res.skipped
	s.conflicts += res.conflicts
	s.failed += res.failedEntries // записи с отказом create-примитива (design.md D6.4)
	s.mu.Unlock()
}

func (s *walkStats) fail() {
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
}

// logf — строка лога walk: prefix — `walk:`, `walk complete:` или
// `walk cancelled:`; pending — очередь + каталоги в работе; elapsed — от
// старта walk.
func (s *walkStats) logf(prefix string, pending int) {
	s.mu.Lock()
	dirs, files, skipped, conflicts, failed := s.dirs, s.files, s.skipped, s.conflicts, s.failed
	startedAt := s.startedAt
	s.mu.Unlock()
	logger.Infof("%s dirs=%d files=%d skipped=%d conflicts=%d failed=%d pending=%d elapsed=%s",
		prefix, dirs, files, skipped, conflicts, failed, pending, time.Since(startedAt).Truncate(time.Millisecond))
}

// walkQueue — FIFO очередь BFS (design.md D6.3) с учётом каталогов в
// работе: `walk complete` — только когда очередь пуста и ничего не
// обрабатывается и не повторяется (design.md D6, шаг 8). Каждый каталог
// ставится в очередь ровно одним родителем; конфликт-каталоги не ставятся.
type walkQueue struct {
	mu       sync.Mutex
	notEmpty sync.Cond
	idleCh   chan struct{} // ёмкость 1: сигнал «очередь пуста и ничего в работе»
	items    []string
	inFlight int
	closing  bool // остановка: pop возвращает false, постановки игнорируются
}

func newWalkQueue() *walkQueue {
	q := &walkQueue{idleCh: make(chan struct{}, 1)}
	q.notEmpty.L = &q.mu
	return q
}

func (q *walkQueue) pushAll(dirs []string) {
	if len(dirs) == 0 {
		return
	}
	q.mu.Lock()
	if !q.closing {
		q.items = append(q.items, dirs...)
		// Ждущие в pop воркеры видят новые элементы (cond-протокол очереди).
		q.notEmpty.Broadcast()
	}
	q.mu.Unlock()
}

// pop блокируется до элемента; false — очередь закрыта (остановка walk).
func (q *walkQueue) pop() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.closing {
			return "", false
		}
		if len(q.items) > 0 {
			dir := q.items[0]
			q.items = q.items[1:]
			q.inFlight++
			return dir, true
		}
		q.notEmpty.Wait()
	}
}

// finish отмечает каталог обработанным (его дети уже поставлены); при
// переходе в простое сигналит координатору.
func (q *walkQueue) finish() {
	q.mu.Lock()
	q.inFlight--
	idle := !q.closing && q.inFlight == 0 && len(q.items) == 0
	q.mu.Unlock()
	if idle {
		select {
		case q.idleCh <- struct{}{}:
		default:
		}
	}
}

// close — остановка: заблокированные pop выходят, новые постановки
// игнорируются; каталоги в работе дорабатываются (design.md D6, шаг 9).
func (q *walkQueue) close() {
	q.mu.Lock()
	q.closing = true
	q.mu.Unlock()
	q.notEmpty.Broadcast()
}

// pending — осталось обработать: очередь + каталоги в работе/повторе.
func (q *walkQueue) pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) + q.inFlight
}

func (q *walkQueue) idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return !q.closing && q.inFlight == 0 && len(q.items) == 0
}

// StartWalk запускает фоновый walk hub (design.md D6, такт 2) — вызывается
// из serve после маркера готовности; стартовая очередь — подкаталоги
// верхнего уровня, найденные populateTopLevel. `walk complete:`/
// `walk cancelled:` — ровно одна строка на запуск. Повторный вызов и вызов
// на остановленном провайдере второй walk не создают: false.
func (p *Provider) StartWalk(ctx context.Context) bool {
	p.mu.Lock()
	if !p.running || p.walkStarted {
		p.mu.Unlock()
		return false
	}
	p.walkStarted = true
	p.mu.Unlock()
	if !p.hasDataPath() {
		// Режим этапа 1 (без data path): наполнения нет, walk не для чего.
		p.walkMu.Lock()
		p.walkStarted = false
		p.walkMu.Unlock()
		return false
	}
	initial := p.takeRootSubdirs()
	logger.Debugf("walk queued %d top-level directories", len(initial))
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runWalk(ctx, initial)
	}()
	return true
}

// runWalk — координатор walk: пул воркеров BFS, прогресс не чаще раза в
// 5с, `walk complete:` ровно один раз при опустошении очереди; при отмене —
// воркеры дорабатывают текущие каталоги, журнал сброшен, `walk cancelled:`
// (design.md D6, шаги 5–9). q.closing выставляет только этот координатор —
// сигнал idleCh внутри цикла всегда означает честное опустошение.
func (p *Provider) runWalk(ctx context.Context, initial []string) {
	stats := &walkStats{startedAt: time.Now()}
	q := newWalkQueue()
	// Снимок канала остановки на весь walk (паттерн воркеров наполнения,
	// provider.go): Stop закрывает канал — координатор завершается;
	// захваченный закрытый канал читаем навсегда, select не зависает.
	// nil-снимок (walk вне Start — юнит-тесты логики) в select не участвует.
	stop := p.stopChan()
	q.pushAll(initial)
	// Пустая стартовая очередь (верхний уровень без каталогов) — сразу
	// «complete».
	if q.idle() {
		select {
		case q.idleCh <- struct{}{}:
		default:
		}
	}
	var workers sync.WaitGroup
	for i := 0; i < walkWorkers; i++ {
		workers.Add(1)
		p.wg.Add(1)
		go func() {
			defer workers.Done()
			defer p.wg.Done()
			p.walkWorker(ctx, stop, q, stats)
		}()
	}
	progress := time.NewTicker(p.walkProgressInterval)
	defer progress.Stop()
	complete := false
	cancelled := false
	for !complete && !cancelled {
		select {
		case <-ctx.Done():
			cancelled = true
		case <-stop:
			cancelled = true
		case <-q.idleCh:
			// Очередь пуста и ничего в работе (design.md D6, шаг 8).
			complete = true
		case <-progress.C:
			if !q.idle() {
				stats.logf("walk:", q.pending())
			}
		}
	}
	// Остановка: воркеры дорабатывают текущие каталоги (pop в закрытой
	// очереди возвращает false), журнал к этому моменту сброшен порциями.
	q.close()
	workers.Wait()
	// Ровно одна финальная строка (design.md D6.8): остановка — cancelled,
	// честное опустошение — complete.
	if cancelled || ctx.Err() != nil {
		stats.logf("walk cancelled:", q.pending())
		return
	}
	stats.logf("walk complete:", q.pending())
}

// walkWorker — воркер пула BFS: каталог из очереди → рабочий цикл → дети в
// очередь → отметка обработки (design.md D6, шаг 6). stop — снимок канала
// остановки координатора (см. runWalk).
func (p *Provider) walkWorker(ctx context.Context, stop chan struct{}, q *walkQueue, stats *walkStats) {
	for {
		dir, ok := q.pop()
		if !ok {
			return
		}
		children := p.walkOne(ctx, stop, stats, dir)
		q.pushAll(children)
		q.finish()
	}
}

// stopClosed — остановлен ли walk по снятому каналу: закрытый канал — да;
// nil (юнит-тесты логики, walk вне Start) — нет.
func stopClosed(stop chan struct{}) bool {
	if stop == nil {
		return false
	}
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// walkOne — рабочий цикл каталога D (design.md D6, шаги 6.1–6.6): листинг с
// retry (транзиентные — backoff, перманентные — Warn+failed, поддерево
// пропускается), примитив наполнения, один Warn на конфликт-каталог,
// счётчики. Возвращает подкаталоги к обходу.
func (p *Provider) walkOne(ctx context.Context, stop chan struct{}, stats *walkStats, dir string) []string {
	entries, err := p.listDirWalk(ctx, stop, dir)
	if err != nil {
		// Остановка — не «сбой листинга» (финальную строку логирует
		// координатор); перманентная — Warn, failed++, поддерево
		// пропускается (design.md D6.6).
		if ctx.Err() == nil && !stopClosed(stop) {
			stats.fail()
			logger.Warnf("walk failed: %s: %v (subtree skipped)", dir, err)
		}
		return nil
	}
	res := populateDirectory(ctx, p, dir, entries)
	if res.conflicts > 0 {
		// Один Warn на каталог (design.md D6.8), до 10 примеров имён.
		logger.Warnf("walk conflict: %s: skipped %d hub entries, e.g. %v", dir, res.conflicts, res.conflictExamples)
	}
	stats.record(res)
	return res.subdirs
}

// listDirWalk — листинг каталога для walk (design.md D6.6): транзиентная
// ошибка — backoff p.walkRetryDelay → p.walkRetryMax без ограничения;
// перманентная — сразу наружу (поддерево пропускается). Отмена ctx или
// остановка провайдера (закрытый stop) прерывают ожидание.
func (p *Provider) listDirWalk(ctx context.Context, stop chan struct{}, dir string) ([]BackendEntry, error) {
	delay := p.walkRetryDelay
	for attempt := 1; ; attempt++ {
		entries, err := p.backend.List(ctx, dir)
		if err == nil {
			return entries, nil
		}
		if classifyListError(err) == listPermanent {
			return nil, err
		}
		if ctx.Err() != nil || stopClosed(stop) {
			return nil, err
		}
		logger.Debugf("walk %s: transient list error (attempt %d): %v; retrying in %s", dir, attempt, err, delay)
		if !sleepCtx(ctx, stop, delay) {
			return nil, err
		}
		delay *= 2
		if delay > p.walkRetryMax {
			delay = p.walkRetryMax
		}
	}
}
