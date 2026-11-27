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

package cfapi

import (
	"errors"
	"sync"
	"sync/atomic"
)

// CallbackDispatcher — интерфейс-разделитель между cfapi (thunk'и) и движком
// провайдера (pkg/cfprovider). Движок либо потребляет очереди Dispatcher,
// либо реализует интерфейс сам (подменная реализация в тестах — без
// cldapi.dll, спека «Движок проверяется без Cloud Files API»).
//
// Десять методов — по таблице callback'ов design.md D5. Методы вызываются
// на стеке callback'а: implementations must not block on I/O — только
// постановка в очередь; переполнение — ошибка без блокировки (U13).
type CallbackDispatcher interface {
	OnFetchData(job *FetchDataJob) error
	OnCancelFetchData(job *CancelFetchDataJob) error
	OnFetchPlaceholders(job *FetchPlaceholdersJob) error
	OnCancelFetchPlaceholders(job *CancelFetchPlaceholdersJob) error
	OnNotifyFileCloseCompletion(job *NotifyFileCloseCompletionJob) error
	OnNotifyFileRenamed(job *NotifyFileRenamedJob) error
	OnNotifyFileDeleted(job *NotifyFileDeletedJob) error
	OnNotifyDirectoryCreated(job *NotifyDirectoryCreatedJob) error
	OnNotifyDirectoryRenamed(job *NotifyDirectoryRenamedJob) error
	OnNotifyDirectoryDeleted(job *NotifyDirectoryDeletedJob) error
}

// ErrQueueFull — очередь заданий переполнена: задание не поставлено,
// callback вернулся не блокируя (спека «Переполненная очередь»).
var ErrQueueFull = errors.New("cfapi: очередь заданий переполнена")

// QueueOptions — ёмкости очередей диспетчера.
type QueueOptions struct {
	FetchData         int // FETCH_DATA (гидратация)
	FetchPlaceholders int // FETCH_PLACEHOLDERS (наполнение)
	CloseCompletion   int // NOTIFY_FILE_CLOSE_COMPLETION (выгрузка)
	FileRenamed       int
	FileDeleted       int
	DirectoryCreated  int // без OS-callback'а; источник — запасной механизм U9
	DirectoryRenamed  int
	DirectoryDeleted  int
	// FetchDataOverflow — канал заданий FETCH_DATA, отброшенных при
	// переполнении основной очереди (U13): движок отвечает на каждое
	// StatusCloudFileRequestCanceled из воркера (не из thunk'а).
	FetchDataOverflow int
}

// DefaultQueueOptions — ёмкости по умолчанию. Гидратация и уведомления —
// с запасом: постановка из callback'а обязана быть ненаказуемо быстрой
// (спека «Очередь без воркеров»: 1000 событий < 1 c).
func DefaultQueueOptions() QueueOptions {
	return QueueOptions{
		FetchData:         4096,
		FetchPlaceholders: 1024,
		CloseCompletion:   4096,
		FileRenamed:       1024,
		FileDeleted:       1024,
		DirectoryCreated:  256,
		DirectoryRenamed:  512,
		DirectoryDeleted:  512,
		FetchDataOverflow: 256,
	}
}

// fetchJobKey — ключ реестра активных заданий гидратации: пара
// (TransferKey, RequestKey). Контракт этапа 3 (design.md D7): при двух
// конкурентных FETCH_DATA по одному файлу (общий TransferKey, разные
// RequestKey) отмена адресуется по RequestKey — реестр только по
// TransferKey доходит отменой лишь до последнего задания.
type fetchJobKey struct {
	transfer TransferKey
	request  RequestKey
}

// Dispatcher — реализация CallbackDispatcher на буферизованных очередях:
// thunk'и ставят задания, воркеры движка разбирают по каналам-геттерам.
// Отмена: реестр активных заданий по паре (TransferKey, RequestKey) —
// CANCEL_* ставит флаг Cancel на задании, воркер проверяет его между
// порциями (design.md D7). I/O (hub, диск, CfExecute) в Dispatcher
// отсутствует — только очереди и счётчики.
type Dispatcher struct {
	fetchData         chan *FetchDataJob
	fetchDataOverflow chan *FetchDataJob
	fetchPlaceholders chan *FetchPlaceholdersJob
	closeCompletion   chan *NotifyFileCloseCompletionJob
	fileRenamed       chan *NotifyFileRenamedJob
	fileDeleted       chan *NotifyFileDeletedJob
	directoryCreated  chan *NotifyDirectoryCreatedJob
	directoryRenamed  chan *NotifyDirectoryRenamedJob
	directoryDeleted  chan *NotifyDirectoryDeletedJob

	mu               sync.Mutex
	activeFetch      map[fetchJobKey]*FetchDataJob         // ключ — (TransferKey, RequestKey)
	activePopulation map[TransferKey]*FetchPlaceholdersJob // ключ — TransferKey

	enqueued  atomic.Int64 // принятых заданий
	dropped   atomic.Int64 // отброшено при переполнении очередей
	cancelled atomic.Int64 // обработанных CANCEL_*
}

// NewDispatcher создаёт диспетчер с очередями заданных ёмкостей
// (обрезать до 1 при неадекватных значениях).
func NewDispatcher(opts QueueOptions) *Dispatcher {
	minCap := func(n int) int {
		if n < 1 {
			return 1
		}
		return n
	}
	return &Dispatcher{
		fetchData:         make(chan *FetchDataJob, minCap(opts.FetchData)),
		fetchDataOverflow: make(chan *FetchDataJob, minCap(opts.FetchDataOverflow)),
		fetchPlaceholders: make(chan *FetchPlaceholdersJob, minCap(opts.FetchPlaceholders)),
		closeCompletion:   make(chan *NotifyFileCloseCompletionJob, minCap(opts.CloseCompletion)),
		fileRenamed:       make(chan *NotifyFileRenamedJob, minCap(opts.FileRenamed)),
		fileDeleted:       make(chan *NotifyFileDeletedJob, minCap(opts.FileDeleted)),
		directoryCreated:  make(chan *NotifyDirectoryCreatedJob, minCap(opts.DirectoryCreated)),
		directoryRenamed:  make(chan *NotifyDirectoryRenamedJob, minCap(opts.DirectoryRenamed)),
		directoryDeleted:  make(chan *NotifyDirectoryDeletedJob, minCap(opts.DirectoryDeleted)),
		activeFetch:       make(map[fetchJobKey]*FetchDataJob),
		activePopulation:  make(map[TransferKey]*FetchPlaceholdersJob),
	}
}

// Геттеры очередей — воркеры движка читают из них.

// FetchData — очередь заданий гидратации.
func (d *Dispatcher) FetchData() <-chan *FetchDataJob { return d.fetchData }

// FetchDataOverflow — очередь отброшенных заданий гидратации (переполнение
// FetchData, U13): воркер движка отвечает на каждое задание
// STATUS_CLOUD_FILE_REQUEST_CANCELED через CfExecute — сам Dispatcher,
// как и thunk, I/O не выполняет.
func (d *Dispatcher) FetchDataOverflow() <-chan *FetchDataJob { return d.fetchDataOverflow }

// FetchPlaceholders — очередь наполнения каталогов.
func (d *Dispatcher) FetchPlaceholders() <-chan *FetchPlaceholdersJob { return d.fetchPlaceholders }

// CloseCompletion — очередь уведомлений о закрытии файла (выгрузка).
func (d *Dispatcher) CloseCompletion() <-chan *NotifyFileCloseCompletionJob { return d.closeCompletion }

// FileRenamed — очередь переименований файлов.
func (d *Dispatcher) FileRenamed() <-chan *NotifyFileRenamedJob { return d.fileRenamed }

// FileDeleted — очередь удалений файлов.
func (d *Dispatcher) FileDeleted() <-chan *NotifyFileDeletedJob { return d.fileDeleted }

// DirectoryCreated — очередь создания каталогов (наполняется запасным
// механизмом U9, OS-callback отсутствует).
func (d *Dispatcher) DirectoryCreated() <-chan *NotifyDirectoryCreatedJob { return d.directoryCreated }

// DirectoryRenamed — очередь переименований каталогов.
func (d *Dispatcher) DirectoryRenamed() <-chan *NotifyDirectoryRenamedJob { return d.directoryRenamed }

// DirectoryDeleted — очередь удалений каталогов.
func (d *Dispatcher) DirectoryDeleted() <-chan *NotifyDirectoryDeletedJob { return d.directoryDeleted }

// OnFetchData ставит задание гидратации в очередь; при переполнении
// возвращает ErrQueueFull, не блокируя (дизайн D7, проба U13), а задание
// переносится в очередь переполнения: воркер движка ответит на него
// STATUS_CLOUD_FILE_REQUEST_CANCELED через CfExecute (из воркера, не из
// thunk'а). Если и она переполнена, задание теряется окончательно — фильтр
// завершит запрос по собственному таймауту.
func (d *Dispatcher) OnFetchData(job *FetchDataJob) error {
	key := fetchJobKey{transfer: job.TransferKey, request: job.RequestKey}
	d.mu.Lock()
	d.activeFetch[key] = job // регистрация до постановки: отмена возможна сразу
	d.mu.Unlock()
	select {
	case d.fetchData <- job:
		d.enqueued.Add(1)
		return nil
	default:
		d.mu.Lock()
		if cur, ok := d.activeFetch[key]; ok && cur == job {
			delete(d.activeFetch, key)
		}
		d.mu.Unlock()
		select {
		case d.fetchDataOverflow <- job:
		default:
		}
		d.dropped.Add(1)
		return ErrQueueFull
	}
}

// OnCancelFetchData ставит флаг отмены активному заданию с парой ключей
// (TransferKey, RequestKey) — контракт этапа 3 (design.md D7: конкурентные
// FETCH_DATA одного файла различаются RequestKey).
//
// Семантика RequestKey в CANCEL-колбэке живьём не подтверждена (проба U13
// этапа 3), поэтому при точном промахе применяется запас: ровно одно
// активное задание с этим TransferKey отменяется (однозначный случай —
// один читатель файла); при нескольких активных заданиях никто не отменяется
// — угадывание отменяло бы чужой пользовательский запрос. Задание с
// незнакомым TransferKey (завершено или не было) — не ошибка: фильтр может
// прислать отмену после завершения.
func (d *Dispatcher) OnCancelFetchData(job *CancelFetchDataJob) error {
	d.mu.Lock()
	key := fetchJobKey{transfer: job.TransferKey, request: job.RequestKey}
	if active, ok := d.activeFetch[key]; ok {
		active.Cancel.Store(true)
	} else {
		var single *FetchDataJob
		matches := 0
		for k, j := range d.activeFetch {
			if k.transfer == job.TransferKey {
				matches++
				single = j
			}
		}
		if matches == 1 {
			single.Cancel.Store(true)
		}
	}
	d.mu.Unlock()
	d.cancelled.Add(1)
	return nil
}

// OnFetchPlaceholders ставит задание наполнения в очередь.
func (d *Dispatcher) OnFetchPlaceholders(job *FetchPlaceholdersJob) error {
	d.mu.Lock()
	d.activePopulation[job.TransferKey] = job
	d.mu.Unlock()
	select {
	case d.fetchPlaceholders <- job:
		d.enqueued.Add(1)
		return nil
	default:
		d.mu.Lock()
		delete(d.activePopulation, job.TransferKey)
		d.mu.Unlock()
		d.dropped.Add(1)
		return ErrQueueFull
	}
}

// OnCancelFetchPlaceholders ставит флаг отмены заданию наполнения.
func (d *Dispatcher) OnCancelFetchPlaceholders(job *CancelFetchPlaceholdersJob) error {
	d.mu.Lock()
	if active, ok := d.activePopulation[job.TransferKey]; ok {
		active.Cancel.Store(true)
	}
	d.mu.Unlock()
	d.cancelled.Add(1)
	return nil
}

// OnNotifyFileCloseCompletion ставит уведомление о закрытии файла в очередь
// выгрузки (design.md D8: классификация файла — в движке).
func (d *Dispatcher) OnNotifyFileCloseCompletion(job *NotifyFileCloseCompletionJob) error {
	select {
	case d.closeCompletion <- job:
		d.enqueued.Add(1)
		return nil
	default:
		d.dropped.Add(1)
		return ErrQueueFull
	}
}

// OnNotifyFileRenamed ставит переименование файла в очередь.
func (d *Dispatcher) OnNotifyFileRenamed(job *NotifyFileRenamedJob) error {
	select {
	case d.fileRenamed <- job:
		d.enqueued.Add(1)
		return nil
	default:
		d.dropped.Add(1)
		return ErrQueueFull
	}
}

// OnNotifyFileDeleted ставит удаление файла в очередь.
func (d *Dispatcher) OnNotifyFileDeleted(job *NotifyFileDeletedJob) error {
	select {
	case d.fileDeleted <- job:
		d.enqueued.Add(1)
		return nil
	default:
		d.dropped.Add(1)
		return ErrQueueFull
	}
}

// OnNotifyDirectoryCreated ставит создание каталога в очередь (источник —
// запасной механизм U9; OS-callback для этого типа отсутствует).
func (d *Dispatcher) OnNotifyDirectoryCreated(job *NotifyDirectoryCreatedJob) error {
	select {
	case d.directoryCreated <- job:
		d.enqueued.Add(1)
		return nil
	default:
		d.dropped.Add(1)
		return ErrQueueFull
	}
}

// OnNotifyDirectoryRenamed ставит переименование каталога в очередь.
func (d *Dispatcher) OnNotifyDirectoryRenamed(job *NotifyDirectoryRenamedJob) error {
	select {
	case d.directoryRenamed <- job:
		d.enqueued.Add(1)
		return nil
	default:
		d.dropped.Add(1)
		return ErrQueueFull
	}
}

// OnNotifyDirectoryDeleted ставит удаление каталога в очередь.
func (d *Dispatcher) OnNotifyDirectoryDeleted(job *NotifyDirectoryDeletedJob) error {
	select {
	case d.directoryDeleted <- job:
		d.enqueued.Add(1)
		return nil
	default:
		d.dropped.Add(1)
		return ErrQueueFull
	}
}

// Complete снимает задание гидратации с учёта — движок вызывает после
// завершения обработки (ровно один CfExecute на задание, design.md).
func (d *Dispatcher) Complete(job *FetchDataJob) {
	d.mu.Lock()
	key := fetchJobKey{transfer: job.TransferKey, request: job.RequestKey}
	if cur, ok := d.activeFetch[key]; ok && cur == job {
		delete(d.activeFetch, key)
	}
	d.mu.Unlock()
}

// CompletePlaceholders снимает задание наполнения с учёта.
func (d *Dispatcher) CompletePlaceholders(job *FetchPlaceholdersJob) {
	d.mu.Lock()
	if cur, ok := d.activePopulation[job.TransferKey]; ok && cur == job {
		delete(d.activePopulation, job.TransferKey)
	}
	d.mu.Unlock()
}

// Enqueued — всего принятых заданий.
func (d *Dispatcher) Enqueued() int64 { return d.enqueued.Load() }

// Dropped — всего отброшено при переполнении очередей.
func (d *Dispatcher) Dropped() int64 { return d.dropped.Load() }

// Cancelled — всего обработанных отмен.
func (d *Dispatcher) Cancelled() int64 { return d.cancelled.Load() }

// ActiveFetch — число заданий гидратации в реестре активных (очередь + воркеры).
func (d *Dispatcher) ActiveFetch() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.activeFetch)
}

// QueueCapacity — ёмкости очередей диспетчера (диагностика; ёмкость канала
// не изменяется после NewDispatcher).
func (d *Dispatcher) QueueCapacity() QueueOptions {
	return QueueOptions{
		FetchData:         cap(d.fetchData),
		FetchPlaceholders: cap(d.fetchPlaceholders),
		CloseCompletion:   cap(d.closeCompletion),
		FileRenamed:       cap(d.fileRenamed),
		FileDeleted:       cap(d.fileDeleted),
		DirectoryCreated:  cap(d.directoryCreated),
		DirectoryRenamed:  cap(d.directoryRenamed),
		DirectoryDeleted:  cap(d.directoryDeleted),
		FetchDataOverflow: cap(d.fetchDataOverflow),
	}
}

// ActivePlaceholders — число заданий наполнения в реестре активных.
func (d *Dispatcher) ActivePlaceholders() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.activePopulation)
}
