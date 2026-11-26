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
	}
}

// Dispatcher — реализация CallbackDispatcher на буферизованных очередях:
// thunk'и ставят задания, воркеры движка разбирают по каналам-геттерам.
// Отмена: реестр активных заданий по TransferKey — CANCEL_* ставит флаг
// Cancel на задании, воркер проверяет его между порциями (design.md D7).
// I/O (hub, диск, CfExecute) в Dispatcher отсутствует — только очереди
// и счётчики.
type Dispatcher struct {
	fetchData         chan *FetchDataJob
	fetchPlaceholders chan *FetchPlaceholdersJob
	closeCompletion   chan *NotifyFileCloseCompletionJob
	fileRenamed       chan *NotifyFileRenamedJob
	fileDeleted       chan *NotifyFileDeletedJob
	directoryCreated  chan *NotifyDirectoryCreatedJob
	directoryRenamed  chan *NotifyDirectoryRenamedJob
	directoryDeleted  chan *NotifyDirectoryDeletedJob

	mu               sync.Mutex
	activeFetch      map[TransferKey]*FetchDataJob         // ключ — TransferKey
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
		fetchPlaceholders: make(chan *FetchPlaceholdersJob, minCap(opts.FetchPlaceholders)),
		closeCompletion:   make(chan *NotifyFileCloseCompletionJob, minCap(opts.CloseCompletion)),
		fileRenamed:       make(chan *NotifyFileRenamedJob, minCap(opts.FileRenamed)),
		fileDeleted:       make(chan *NotifyFileDeletedJob, minCap(opts.FileDeleted)),
		directoryCreated:  make(chan *NotifyDirectoryCreatedJob, minCap(opts.DirectoryCreated)),
		directoryRenamed:  make(chan *NotifyDirectoryRenamedJob, minCap(opts.DirectoryRenamed)),
		directoryDeleted:  make(chan *NotifyDirectoryDeletedJob, minCap(opts.DirectoryDeleted)),
		activeFetch:       make(map[TransferKey]*FetchDataJob),
		activePopulation:  make(map[TransferKey]*FetchPlaceholdersJob),
	}
}

// Геттеры очередей — воркеры движка читают из них.

// FetchData — очередь заданий гидратации.
func (d *Dispatcher) FetchData() <-chan *FetchDataJob { return d.fetchData }

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
// возвращает ErrQueueFull, не блокируя (дизайн D7, проба U13).
func (d *Dispatcher) OnFetchData(job *FetchDataJob) error {
	d.mu.Lock()
	d.activeFetch[job.TransferKey] = job // регистрация до постановки: отмена возможна сразу
	d.mu.Unlock()
	select {
	case d.fetchData <- job:
		d.enqueued.Add(1)
		return nil
	default:
		d.mu.Lock()
		delete(d.activeFetch, job.TransferKey)
		d.mu.Unlock()
		d.dropped.Add(1)
		return ErrQueueFull
	}
}

// OnCancelFetchData ставит флаг отмены активному заданию с этим TransferKey.
// Задание с незнакомым ключом (завершено или не было) — не ошибка: фильтр
// может прислать отмену после завершения.
func (d *Dispatcher) OnCancelFetchData(job *CancelFetchDataJob) error {
	d.mu.Lock()
	if active, ok := d.activeFetch[job.TransferKey]; ok {
		active.Cancel.Store(true)
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
	if cur, ok := d.activeFetch[job.TransferKey]; ok && cur == job {
		delete(d.activeFetch, job.TransferKey)
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

// ActivePlaceholders — число заданий наполнения в реестре активных.
func (d *Dispatcher) ActivePlaceholders() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.activePopulation)
}
