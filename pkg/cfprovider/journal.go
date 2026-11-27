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

// Journal невыполненных операций backend (design.md D9, задачи 6.3/6.5):
// append-файл JSONL в каталоге состояния (тот же каталог, что у индекса —
// флаг --state-dir serve; файл ops.journal). Операция записи, не выполненная
// из-за недоступного hub (выгрузка, удаление, переименование, создание
// каталога), сохраняется и повторяется фоновой горутиной (replayer) с
// нарастающей паузой 1с→30с до успеха, в том числе после перезапуска serve.
// Успешно исполненная операция снимается с учёта методом Ack: файл
// переуплотняется (rewrite атомарной заменой через временный файл —
// обрыв между усечением и записью не теряет не исполненные операции).
//
// Формат строки (JSONL): {type, path, new_path, ino, is_dir, ts} — type
// upload|delete|rename|mkdir; path/NewPath — пути в стиле fs ("/a/b");
// ino — inode backend для удаления по inode (задача 6.5); ts — unix-nano
// добавления. Ключ операции = type:lower(path) — повторное задание того же
// типа по тому же пути заменяет прежнюю запись (дедупликация), порядок
// исполнения (order) сохраняется — вложенные mkdir исполняются родители
// раньше детей.
//
// Классификация повторов: транзиентные ошибки (сеть, EIO) повторяются
// бесконечно с нарастающей паузой (инвариант «повтор до успеха»);
// перманентные (EACCES/EPERM/EINVAL/ENOTDIR/EISDIR — операция не может
// выполниться никогда: например, удаление поверх локально пересозданного
// файла) снимаются с учёта одним Warn — бесконечный повтор невозможного
// заблокировал бы очередь журнала.
//
// cldapi.dll не вызывается: исполнение операций идёт через ingestBackend
// (pkg/fs) и placeholderStore — инвариант «Движок проверяется без CfAPI».
package cfprovider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/utils"
)

// Типы операций журнала (design.md D9: выгрузка, удаление, переименование;
// mkdir — создание каталога, задача 6.1).
const (
	OpUpload = "upload"
	OpDelete = "delete"
	OpRename = "rename"
	OpMkdir  = "mkdir"
)

// opsJournalName — файл журнала операций в каталоге состояния.
const opsJournalName = "ops.journal"

// JournalOp — одна операция над backend, ожидающая исполнения. Path —
// путь записи в стиле fs; NewPath — цель переименования; Ino — inode
// backend (удаление по inode, задача 6.5); IsDir — тип записи для
// удаления (Unlink/Rmdir); Ts — unix-nano добавления.
type JournalOp struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	NewPath string `json:"new_path,omitempty"`
	Ino     uint64 `json:"ino,omitempty"`
	IsDir   bool   `json:"is_dir,omitempty"`
	Ts      int64  `json:"ts"`
}

// Key — ключ дедупликации: тип + путь в нижнем регистре (Windows-семантика).
func (op JournalOp) Key() string {
	return op.Type + ":" + strings.ToLower(op.Path)
}

// opPermanentError — перманентный отказ операции: повтор не изменит исход,
// операция снимается с учёта одним Warn (см. классификацию повторов выше).
type opPermanentError struct{ err error }

func (e *opPermanentError) Error() string { return e.err.Error() }

func (e *opPermanentError) Unwrap() error { return e.err }

func permanentOpErrorf(format string, args ...interface{}) error {
	return &opPermanentError{err: fmt.Errorf(format, args...)}
}

func isPermanentOpError(err error) bool {
	var p *opPermanentError
	return errors.As(err, &p)
}

// errNeedsDecompose — удаление непустого каталога (ENOTEMPTY): прямые
// потомки (по индексу) поставлены в очередь ПЕРЕД родителем, родитель
// перенесён в хвост (см. decomposeDeleteDir). Replayer не бэкофит и не
// снимает операцию — она исполнится после детей.
var errNeedsDecompose = errors.New("cfprovider: directory not empty; children enqueued before parent")

// Journal — журнал невыполненных операций: карты в памяти (дедупликация и
// порядок) + append-файл JSONL. Все методы потокобезопасны (Add — из любых
// источников: notify-обработчики, выгрузка, reconcile; Ack — из replayer'а).
type Journal struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	ops    map[string]*JournalOp
	order  []string // ключи в порядке добавления (FIFO исполнения)
	wake   chan struct{}
	closed bool  // Close(): appendLocked не переоткрывает файл после явного закрытия
	lastTs int64 // монотонность ts (гвард Ack; wall-clock на Windows дискретен)
}

// NewJournal открывает (создаёт) каталог состояния и журнал операций,
// загружает накопленные записи (перезапуск serve: отложенные операции
// исполняются после восстановления hub — спека «Отложенные операции при
// недоступном hub»).
func NewJournal(stateDir string) (*Journal, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("cfprovider: state directory is empty")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("cfprovider: create state dir %s: %w", stateDir, err)
	}
	jPath := filepath.Join(stateDir, opsJournalName)
	j := &Journal{path: jPath, ops: make(map[string]*JournalOp), wake: make(chan struct{}, 1)}
	if err := j.load(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(jPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cfprovider: open journal %s: %w", jPath, err)
	}
	j.file = f
	return j, nil
}

// load читает журнал: валидные JSONL-строки в карту с сохранением порядка,
// обрезанные/битые пропускаются с предупреждением (падение могло оборвать
// последнюю строку).
func (j *Journal) load() error {
	f, err := os.Open(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cfprovider: open journal %s: %w", j.path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var op JournalOp
		if err := json.Unmarshal(scanner.Bytes(), &op); err != nil {
			logger.Warnf("ops journal %s line %d is not valid JSON, skipping: %v", j.path, line, err)
			continue
		}
		if op.Type == "" || op.Path == "" {
			logger.Warnf("ops journal %s line %d has no type/path, skipping", j.path, line)
			continue
		}
		if op.Ts > j.lastTs {
			j.lastTs = op.Ts
		}
		key := op.Key()
		if _, ok := j.ops[key]; !ok {
			j.order = append(j.order, key)
		}
		j.ops[key] = &op
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("cfprovider: scan journal %s: %w", j.path, err)
	}
	if len(j.ops) > 0 {
		logger.Infof("ops journal %s loaded: %d pending operation(s)", j.path, len(j.ops))
	}
	return nil
}

// bumpTsLocked — монотонный ts записи (mu держится вызывающим): wall-clock
// на Windows дискретен — две подряд записи могли получить одинаковый ts и
// обмануть гвард Ack.
func (j *Journal) bumpTsLocked() int64 {
	ts := time.Now().UnixNano()
	if ts <= j.lastTs {
		ts = j.lastTs + 1
	}
	j.lastTs = ts
	return ts
}

// Add сохраняет операцию: память (дедупликация по ключу) + append-строка.
// Ошибка append'а возвращается вызывающему (операция считается
// несохранённой — её повторит следующий close-уведомление/рестарт).
func (j *Journal) Add(op JournalOp) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	op.Ts = j.bumpTsLocked()
	key := op.Key()
	if _, ok := j.ops[key]; !ok {
		j.order = append(j.order, key)
	}
	j.ops[key] = &op
	if err := j.appendLocked(op); err != nil {
		return err
	}
	select {
	case j.wake <- struct{}{}:
	default:
	}
	return nil
}

// ReplaceUploadWithDelete заменяет ожидающую выгрузку пути удалением
// (задача 6.5: локальный файл удалён при незавершённой выгрузке без
// активного задания — подтверждённая версия (если есть) подлежит удалению;
// если версии нет, исполнение вернёт ENOENT и снимется как исполненное).
// Путь новой операции — путь заменённой (регистр сохранён); ino — inode из
// индекса, если запись известна (чистка индекса при исполнении; 0 —
// неизвестна, индекс не трогается).
func (j *Journal) ReplaceUploadWithDelete(fsPath string, ino uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	oldKey := OpUpload + ":" + strings.ToLower(fsPath)
	old, ok := j.ops[oldKey]
	if !ok {
		return nil // выгрузки не ожидалось — нечего заменять
	}
	delete(j.ops, oldKey)
	for i, k := range j.order {
		if k == oldKey {
			j.order = append(j.order[:i], j.order[i+1:]...)
			break
		}
	}
	op := JournalOp{Type: OpDelete, Path: old.Path, Ino: ino, Ts: j.bumpTsLocked()}
	key := op.Key()
	if _, ok := j.ops[key]; !ok {
		j.order = append(j.order, key)
	}
	j.ops[key] = &op
	if err := j.appendLocked(op); err != nil {
		return err
	}
	select {
	case j.wake <- struct{}{}:
	default:
	}
	return nil
}

// Ack снимает исполненную операцию с учёта. Гвард по ts: за время исполнения
// операция могла быть заменена новой записью того же ключа (повторный отказ
// runIngest обновляет ts) — снимается только та версия, что исполнялась
// (снапшот replayer'а); заменённая остаётся в очереди. Переуплотнение —
// rewrite атомарной заменой (tmp+rename): обрыв на любом шаге сохраняет
// либо прежнее, либо новое содержимое целиком. Windows: хендл append'а
// закрывается ДО переименования (открытый хендл блокирует rename).
func (j *Journal) Ack(op *JournalOp) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	key := op.Key()
	cur, ok := j.ops[key]
	if !ok {
		return nil
	}
	if cur.Ts != op.Ts {
		logger.Debugf("ops journal %s: %s was replaced mid-flight (ts %d -> %d); not acknowledged", j.path, key, op.Ts, cur.Ts)
		return nil
	}
	delete(j.ops, key)
	for i, k := range j.order {
		if k == key {
			j.order = append(j.order[:i], j.order[i+1:]...)
			break
		}
	}
	return j.rewriteLocked()
}

// PrependAll ставит операции в ГОЛОВУ очереди — деконпозиция удаления
// каталога: прямые потомки исполняются раньше родителя. Дубликат по ключу
// переносится в голову без повторной строки журнала (свежая запись — новая
// строка + wake).
func (j *Journal) PrependAll(ops []JournalOp) {
	j.mu.Lock()
	defer j.mu.Unlock()
	keys := make([]string, 0, len(ops))
	for _, op := range ops {
		op.Ts = j.bumpTsLocked()
		key := op.Key()
		for i, k := range j.order {
			if k == key {
				j.order = append(j.order[:i], j.order[i+1:]...)
				break
			}
		}
		if _, ok := j.ops[key]; !ok {
			if err := j.appendLocked(op); err != nil {
				logger.Warnf("ops journal %s: prepend %s: %v", j.path, key, err)
			}
		}
		j.ops[key] = &op
		keys = append(keys, key)
	}
	j.order = append(append([]string(nil), keys...), j.order...)
	select {
	case j.wake <- struct{}{}:
	default:
	}
}

// RotateToTail переносит операцию в хвост очереди (анти-livelock: неудавшийся
// оп не блокирует остальные — replayer продолжает со следующего). Порядок в
// файле не меняется: после перезапуска порядок восстанавливается из журнала
// и деконпозиция самовосстанавливается (родитель снова ENOTEMPTY → дети
// снова перед ним).
func (j *Journal) RotateToTail(key string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i, k := range j.order {
		if k == key {
			j.order = append(j.order[:i], j.order[i+1:]...)
			j.order = append(j.order, key)
			return
		}
	}
}

// appendLocked дописывает строку операции (mu держится вызывающим). Хендл
// отсутствует (отказ compaction) — попытка переоткрытия при следующей записи
// (fix-1.3); после явного Close записи не переоткрывают файл.
func (j *Journal) appendLocked(op JournalOp) error {
	if j.closed {
		return nil // закрытый журнал (тесты логики) — только память
	}
	if j.file == nil {
		f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("cfprovider: reopen journal %s: %w", j.path, err)
		}
		j.file = f
		logger.Infof("ops journal %s reopened after compaction failure", j.path)
	}
	b, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("cfprovider: marshal journal op: %w", err)
	}
	if _, err := j.file.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("cfprovider: append journal %s: %w", j.path, err)
	}
	return nil
}

// rewriteLocked переуплотняет журнал (mu держится вызывающим). Хендл append'а
// закрывается до rename и открывается заново после: Windows не даёт
// переименовать файл с открытым хендлом. Отказ на любом шаге — попытка
// переоткрытия (append'ы продолжатся с места; порядок восстановится при
// следующей перезагрузке журнала).
func (j *Journal) rewriteLocked() error {
	if j.file == nil {
		return nil
	}
	var buf strings.Builder
	for _, key := range j.order {
		op, ok := j.ops[key]
		if !ok {
			continue
		}
		b, err := json.Marshal(*op)
		if err != nil {
			return fmt.Errorf("cfprovider: marshal journal op: %w", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := j.file.Close(); err != nil {
		logger.Warnf("close journal %s for compaction: %v", j.path, err)
	}
	j.file = nil
	reopen := func() {
		f, err := os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			logger.Warnf("reopen journal %s after failed compaction: %v (appendLocked will retry)", j.path, err)
			return
		}
		j.file = f
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		reopen()
		return fmt.Errorf("cfprovider: rewrite journal %s: %w", j.path, err)
	}
	if err := os.Rename(tmp, j.path); err != nil {
		_ = os.Remove(tmp)
		reopen()
		return fmt.Errorf("cfprovider: rename journal %s: %w", j.path, err)
	}
	f, err := os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("cfprovider: reopen journal %s: %w", j.path, err)
	}
	j.file = f
	return nil
}

// Ops — снимок ожидающих операций в порядке добавления (FIFO исполнения:
// вложенные mkdir — родители раньше детей).
func (j *Journal) Ops() []*JournalOp {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*JournalOp, 0, len(j.order))
	for _, key := range j.order {
		if op, ok := j.ops[key]; ok {
			out = append(out, op)
		}
	}
	return out
}

// Len — число ожидающих операций (диагностика/синхронизация тестов).
func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.ops)
}

// Wake — канал сигналов о новых операциях (replayer ждёт пустой журнал).
func (j *Journal) Wake() <-chan struct{} {
	return j.wake
}

// HasPending — есть ли ожидающая операция типа opType по пути fsPath
// (гарды двойной постановки: notify-обработчик не дублирует операцию,
// которую уже ведёт replayer).
func (j *Journal) HasPending(opType, fsPath string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.ops[opType+":"+strings.ToLower(fsPath)]
	return ok
}

// HasDeleteFor — есть ли ожидающее удаление по inode (гард walk: запись hub,
// ожидающую удаления, повторное создание теряло бы — этап 6.4).
func (j *Journal) HasDeleteFor(ino uint64) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if ino == 0 {
		return false
	}
	for _, op := range j.ops {
		if op.Type == OpDelete && op.Ino == ino {
			return true
		}
	}
	return false
}

// Close закрывает журнал.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.closed = true
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

// journalRetryBase/journalRetryMax — нарастающая пауза повторов replayer'а
// (задача 6.3: 1с → 30с).
const (
	journalRetryBase = time.Second
	journalRetryMax  = 30 * time.Second
)

// sleepJournal ждёт d либо первой из отмен: ctx, закрытия stop, срабатывания
// таймера часов провайдера (injectable — тесты двигают fakeClock).
func (p *Provider) sleepJournal(ctx context.Context, stop chan struct{}, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		case <-stop:
			return false
		default:
			return true
		}
	}
	timer := p.clock.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-stop:
		return false
	case <-timer.Fired():
		return true
	}
}

// startJournalReplay запускает горутину повторов журнала (задача 6.3) —
// вызывается из startIngestPool после поднятия пулов: replayer исполняет
// операции тем же ingestBackend, что и выгрузка. journal == nil (режим без
// каталога состояния) — повтора нет.
func (p *Provider) startJournalReplay(ctx context.Context, stop chan struct{}) {
	if p.journal == nil {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runJournalReplay(ctx, stop)
	}()
}

// journalMaxRotations — максимум переносов одной операции в хвост за цикл
// replayer'а (цикл = серия проходов до чистого прохода/бэкоф-паузы). После
// лимита — Warn, операция пропускается до конца цикла (остаётся в очереди —
// следующий цикл повторит): один сбойный оп не останавливает журнал.
const journalMaxRotations = 8

// runJournalReplay — цикл повторов: снимок очереди → исполнение по порядку;
// успех (или перманентный отказ) — Ack (гвард по ts: заменённая за время
// исполнения запись не снимается); транзиентный отказ — операция переносится
// В ХВОСТ очереди (FIFO-ротация: остальные операции исполняются, живелока
// «голова блокирует всех» нет), лимит ротаций на операцию за цикл — Warn и
// пропуск до конца цикла; после прохода с отказами — нарастающая пауза
// base→max. Деконпозиция (errNeedsDecompose) — без бэкофа: дети уже перед
// родителем. Пустой журнал — ожидание сигнала wake или остановки.
func (p *Provider) runJournalReplay(ctx context.Context, stop chan struct{}) {
	delay := p.journalRetryBase
	skip := make(map[string]bool)
	rotations := make(map[string]int)
	for {
		ops := p.journal.Ops()
		if len(ops) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-p.journal.Wake():
			}
			continue
		}
		passFailed := false
		for _, op := range ops {
			if ctx.Err() != nil || stopClosed(stop) {
				return
			}
			if skip[op.Key()] {
				continue // лимит ротаций исчерпан — до конца цикла
			}
			err := p.executeJournalOp(ctx, op)
			switch {
			case err == nil:
				logger.Debugf("journal %s %s: executed", op.Type, op.Path)
				if aerr := p.journal.Ack(op); aerr != nil {
					logger.Warnf("journal ack %s %s: %v", op.Type, op.Path, aerr)
				}
			case isPermanentOpError(err):
				logger.Warnf("journal %s %s: %v (permanent; operation dropped)", op.Type, op.Path, err)
				if aerr := p.journal.Ack(op); aerr != nil {
					logger.Warnf("journal ack %s %s: %v", op.Type, op.Path, aerr)
				}
			case errors.Is(err, errNeedsDecompose):
				// Дети поставлены перед родителем, родитель в хвосте —
				// очередь продолжается без бэкофа и без снятия.
			default:
				passFailed = true
				p.journal.RotateToTail(op.Key())
				rotations[op.Key()]++
				if rotations[op.Key()] >= journalMaxRotations {
					logger.Warnf("journal %s %s: %v; giving up for this cycle (%d rotations), continuing with the rest", op.Type, op.Path, err, journalMaxRotations)
					skip[op.Key()] = true
					continue
				}
				logger.Warnf("journal %s %s: %v; moved to tail; retrying", op.Type, op.Path, err)
			}
		}
		if passFailed {
			if !p.sleepJournal(ctx, stop, delay) {
				return
			}
			delay *= 2
			if delay > p.journalRetryMax {
				delay = p.journalRetryMax
			}
			continue
		}
		// Чистый проход — новый цикл: лимиты ротаций сбрасываются.
		delay = p.journalRetryBase
		skip = make(map[string]bool)
		rotations = make(map[string]int)
	}
}

// executeJournalOp исполняет одну операцию журнала. nil — исполнена
// (вызывающий Ack'ает); opPermanentError — снимается с учёта; прочее —
// транзиентное (повтор с паузой).
func (p *Provider) executeJournalOp(ctx context.Context, op *JournalOp) error {
	switch op.Type {
	case OpUpload:
		return p.executeUploadOp(ctx, op)
	case OpDelete:
		return p.executeDeleteOp(ctx, op)
	case OpRename:
		return p.executeRenameOp(ctx, op)
	case OpMkdir:
		return p.executeMkdirOp(ctx, op)
	default:
		return permanentOpErrorf("unknown op type %q", op.Type)
	}
}

// executeUploadOp — повтор выгрузки: тот же runIngest (классификация и
// финализация те же); неуспех внутри runIngest обновляет запись журнала
// (дедупликация по ключу). Локальный файл исчез — выгрузить нечего:
// перманентное снятие (защиту подтверждённой версии делает
// ReplaceUploadWithDelete на событии REMOVED, задача 6.5).
func (p *Provider) executeUploadOp(ctx context.Context, op *JournalOp) error {
	local := localJoin(p.root, op.Path)
	if _, err := os.Stat(local); err != nil {
		if os.IsNotExist(err) {
			return permanentOpErrorf("local file %s is gone; upload dropped", op.Path)
		}
		return err
	}
	return p.runIngest(ctx, ingestTask{fsPath: op.Path, local: local, key: strings.ToLower(op.Path)})
}

// permanentJournalErrno — errno перманентных отказов backend-операций:
// операция не может выполниться ни при каких повторах (права доступа, тип
// записи, занятость имени). ENOENT обрабатывается исполнителями отдельно
// (отсутствие записи = цель достигнута); ENOTEMPTY/EIO/сетевые — транзиентные.
func permanentJournalErrno(eno syscall.Errno) bool {
	switch eno {
	case syscall.EACCES, syscall.EPERM, syscall.EINVAL, syscall.ENOTDIR, syscall.EISDIR:
		return true
	}
	if eno == syscall.Errno(5) { // ERROR_ACCESS_DENIED (OS-код)
		return true
	}
	return false
}

// executeDeleteOp — повтор удаления (спека «Удаление офлайн»): тип записи
// индексом не всегда известен (reconcile-очередь строится по IndexPathEntry
// без IsDir) — удаление начинается Unlink'ом, каталог определяется по отказу
// EISDIR (fallback Rmdir; fix-1.6). ENOTEMPTY (непустой каталог) — не
// транзиент и не перманент: деконпозиция — прямые потомки (по индексу)
// ставятся в очередь ПЕРЕД родителем, родитель переносится в хвост (fix-1.2:
// иначе живелок — replayer один, вся очередь ждала бы детей). Локальная
// запись появилась снова — снятие (локальное состояние истина); backend-
// записи нет (ENOENT) — исполнено. Удаление sync root недопустимо по
// построению (гард notifyDelete); здесь повторный гард — защита от
// испорченного журнала.
func (p *Provider) executeDeleteOp(ctx context.Context, op *JournalOp) error {
	if op.Path == "/" || op.Path == "" {
		return permanentOpErrorf("sync root deletion is never propagated")
	}
	if _, err := os.Stat(localJoin(p.root, op.Path)); err == nil {
		return permanentOpErrorf("local entry %s exists again; delete dropped", op.Path)
	}
	mctx := meta.WrapWithCancel(ctx, uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
	defer mctx.Cancel()
	eno := p.ibackend.Delete(mctx, op.Path)
	if eno == syscall.EISDIR {
		// Каталог — тип по отказу Unlink (fallback Rmdir).
		eno = p.ibackend.Rmdir(mctx, op.Path)
	}
	switch {
	case eno == 0, eno == syscall.ENOENT:
		// Исполнено (ENOENT — записи уже нет: удалена другим клиентом/ранее).
	case eno == syscall.ENOTEMPTY:
		return p.decomposeDeleteDir(op)
	case permanentJournalErrno(eno):
		return permanentOpErrorf("backend delete %s: %v", op.Path, eno)
	default:
		return eno
	}
	if op.Ino != 0 {
		if err := p.idx.Delete(op.Ino); err != nil {
			logger.Warnf("journal delete %s: index: %v", op.Path, err)
		}
	}
	if op.IsDir {
		if _, err := p.idx.DeletePrefix(op.Path); err != nil {
			logger.Warnf("journal delete %s: index prefix: %v", op.Path, err)
		}
	}
	return nil
}

// decomposeDeleteDir — ENOTEMPTY: прямые потомки каталога (по индексу)
// ставятся в ГОЛОВУ очереди (исполнятся раньше родителя), родитель
// переносится в хвост (анти-livelock). Известных потомков нет, а каталог
// непуст (чужие записи от другого клиента, U17) — декомпозировать нечего:
// перманентное снятие (fix-ре-чек: иначе errNeedsDecompose без счётчиков
// ротаций давал горячий цикл без бэкофа — replayer долбил backend каждый
// цикл, сбрасывая паузу прочих). Запись сохраняется — консистентно с
// fix-1.5; пересечение с U15/U16 помечено. Порядок детей —
// детерминированный (по пути): PathsInDir читает карту.
func (p *Provider) decomposeDeleteDir(op *JournalOp) error {
	if p.journal == nil {
		return permanentOpErrorf("directory %s not empty and journal is unavailable", op.Path)
	}
	var children []JournalOp
	if p.idx != nil {
		for _, ie := range p.idx.PathsInDir(op.Path) {
			if p.journal.HasPending(OpDelete, ie.Path) {
				continue // уже в очереди — PrependAll перенесёт в голову
			}
			children = append(children, JournalOp{Type: OpDelete, Path: ie.Path, Ino: ie.Ino})
		}
	}
	if len(children) == 0 {
		return permanentOpErrorf("directory %s not empty; no known children (foreign entries); delete dropped", op.Path)
	}
	sort.Slice(children, func(i, k int) bool { return children[i].Path < children[k].Path })
	p.journal.PrependAll(children)
	p.journal.RotateToTail(op.Key())
	return errNeedsDecompose
}

// executeRenameOp — повтор переименования: успех — индекс переносится на
// новый путь с пере-резолвом inode через backend Stat нового пути (rename
// в meta сохраняет inode — рассинхрон всё равно сверяется, design.md D5
// точка «Переименование файла/каталога»); каталог — пути потомков
// (RenamePrefix). ENOENT старой записи — снять (индекс чистится); занятая
// цель (ENOTEMPTY/EEXIST) — перманентный отказ (цель создана другим
// клиентом — повтор не изменит исход).
func (p *Provider) executeRenameOp(ctx context.Context, op *JournalOp) error {
	if op.Path == "/" || op.NewPath == "" || op.Path == "" {
		return permanentOpErrorf("invalid rename op %s -> %s", op.Path, op.NewPath)
	}
	mctx := meta.WrapWithCancel(ctx, uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
	defer mctx.Cancel()
	eno := p.ibackend.Rename(mctx, op.Path, op.NewPath)
	if eno != 0 {
		if eno == syscall.ENOENT {
			// Ложный ENOENT (гонка: переименование успел исполнить другой
			// источник или запись появилась на новом пути) — цель существует,
			// индекс переносится, операция исполнена (fix-1.8: не сносить
			// валидную запись).
			if _, _, serr := p.ibackend.Stat(mctx, op.NewPath); serr == 0 {
				logger.Debugf("journal rename %s -> %s: ENOENT raced, target exists; keeping entry", op.Path, op.NewPath)
				if err := p.indexAfterBackendRename(mctx, op.Path, op.NewPath, op.Ino, op.IsDir); err != nil {
					return err
				}
				return nil
			}
			if op.Ino != 0 {
				if err := p.idx.Delete(op.Ino); err != nil {
					logger.Warnf("journal rename %s: index: %v", op.Path, err)
				}
			}
			if op.IsDir {
				if _, err := p.idx.DeletePrefix(op.Path); err != nil {
					logger.Warnf("journal rename %s: index prefix: %v", op.Path, err)
				}
			}
			return permanentOpErrorf("backend entry %s is gone; rename dropped", op.Path)
		}
		if permanentJournalErrno(eno) || eno == syscall.ENOTEMPTY || eno == syscall.EEXIST {
			return permanentOpErrorf("backend rename %s -> %s: %v", op.Path, op.NewPath, eno)
		}
		return eno
	}
	if err := p.indexAfterBackendRename(mctx, op.Path, op.NewPath, op.Ino, op.IsDir); err != nil {
		return err
	}
	return nil
}

// indexAfterBackendRename — проводка индекса после успешного backend
// rename (notify-путь и повтор журнала): inode нового пути пере-резолвится
// через backend Stat (пере-резолв, design.md D5); каталог — пути потомков.
func (p *Provider) indexAfterBackendRename(mctx meta.Context, oldFsPath, newFsPath string, ino uint64, isDir bool) error {
	actual := ino
	if got, _, serr := p.ibackend.Stat(mctx, newFsPath); serr == 0 {
		actual = got
	} else {
		logger.Debugf("journal rename %s: stat %s: %v (keeping known inode)", oldFsPath, newFsPath, serr)
	}
	if actual != ino {
		if err := p.idx.Delete(ino); err != nil {
			logger.Warnf("journal rename %s: index: %v", oldFsPath, err)
		}
	}
	if err := p.idx.Put(actual, newFsPath); err != nil {
		logger.Warnf("journal rename %s: index: %v", newFsPath, err)
	}
	if isDir {
		if _, err := p.idx.RenamePrefix(oldFsPath, newFsPath); err != nil {
			logger.Warnf("journal rename %s: index prefix: %v", oldFsPath, err)
		}
	}
	return nil
}

// executeMkdirOp — повтор создания каталога (спека «Новый каталог» при
// недоступном hub): локальный каталог исчез или стал файлом — снятие
// (созданный в backend каталог остаётся осиротевшим до сверки — прототип);
// mkdir поверх существующей записи (EEXIST) — не ошибка ( adopting);
// конверсия локального каталога в плейсхолдер — с повтором (отказ сразу
// после создания отвергается фильтром с 0x8007017c — опыт этапа 5).
func (p *Provider) executeMkdirOp(ctx context.Context, op *JournalOp) error {
	if op.Path == "/" || op.Path == "" {
		return permanentOpErrorf("invalid mkdir op %s", op.Path)
	}
	local := localJoin(p.root, op.Path)
	if fi, err := os.Stat(local); err != nil {
		if os.IsNotExist(err) {
			return permanentOpErrorf("local dir %s is gone; mkdir dropped", op.Path)
		}
		return err
	} else if !fi.IsDir() {
		return permanentOpErrorf("local %s is not a directory anymore; mkdir dropped", op.Path)
	}
	return p.propagateDirCreate(ctx, op.Path)
}
