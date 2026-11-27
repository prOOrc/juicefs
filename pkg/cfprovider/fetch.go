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

// Пул гидратации (design.md D7, задачи 3.1–3.5): воркеры разбирают очередь
// FETCH_DATA диспетчера, разрешают inode в путь (индекс → ResolveIno, U12),
// читают данные через fetchBackend (минимальная поверхность pkg/fs —
// fs.Open/Pread порциями не больше fetchBlock, 4 MiB) и завершают каждое
// задание РОВНО ОДНИМ CfExecute(TRANSFER_DATA) — с данными или с кодом
// отказа (инвариант design.md). CANCEL_FETCH_DATA ставит флаг job.Cancel
// (реестр активных заданий диспетчера, адресация (TransferKey, RequestKey));
// воркер проверяет флаг между порциями и отвечает статусом отмены; буфер
// возвращается в пул defer'ом. Переполнение очереди (U13): отброшенное
// задание попадает в канал переполнения диспетчера, отдельная горутина
// отвечает на него StatusCloudFileRequestCanceled — из воркера, не из
// thunk'а (thunk без I/O).
//
// Замер M2 (design.md D11, задача 3.5): длительность и байты каждого
// задания логируются Debugf; число воркеров — конфиг (WithFetchWorkers /
// --hydration-workers), ёмкость очереди — QueueOptions.FetchData.
package cfprovider

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/cfapi"
	"github.com/juicedata/juicefs/pkg/fs"
	"github.com/juicedata/juicefs/pkg/vfs"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/utils"
)

// fetchBlock — порция чтения backend (design.md D7: «порциями не больше
// 4 MiB»; проверка отмены — между порциями; проба U4 — выравнивание и
// размер буфера TRANSFER_DATA).
const fetchBlock = 4 << 20

// fetchFile — открытый для чтения файл: минимальная поверхность *fs.File,
// нужная гидрации. Реальная реализация — *fs.File (fs.FileSystem.Open
// с flags=0, как в cmd/clouddrive.go cloudDriveHash); подменная — в тестах.
type fetchFile interface {
	// Pread читает len(b) байт с offset; err — io.EOF при чтении за концом
	// файла, syscall.Errno — ошибка слоя fs/meta.
	Pread(ctx meta.Context, b []byte, off int64) (int, error)
	// Close закрывает файл (снятие read-хендла; вызывается defer'ом).
	Close(ctx meta.Context) syscall.Errno
}

// fetchBackend — источник данных гидрации: минимальная поверхность
// fs.FileSystem, нужная чтению. Реальная реализация — fsFetchBackend;
// подменная — в тестах (инвариант «Движок проверяется без CfAPI»).
type fetchBackend interface {
	// Open открывает путь (стиль fs: "/", "/a/b") для чтения.
	Open(ctx meta.Context, path string) (fetchFile, syscall.Errno)
}

// fsFetchBackend — fetchBackend на fs.FileSystem этапа 2 (WithData).
type fsFetchBackend struct {
	fsys *fs.FileSystem
}

func (b fsFetchBackend) Open(ctx meta.Context, path string) (fetchFile, syscall.Errno) {
	// flags != 0 обязателен: fs.open с нулевыми флагами пропускает meta-Open,
	// и на зашифрованном томе Open-ответ не доставляет FEK — чтение AGDF-блоков
	// падает EIO (живой кейс 27.11 facility-маунта: FETCH читал файлы без FEK).
	// MODE_MASK_R без W — fs.open открывает O_RDONLY и выполняет meta-Open.
	return b.fsys.Open(ctx, path, vfs.MODE_MASK_R)
}

// startFetchPool запускает пул гидратации: p.fetchWorkers воркеров очереди
// FETCH_DATA и одну горутину ответов на переполнение (U13). Вызывается из
// Start при подключённом data path; в logging/fallback-режиме (dispatch —
// заглушка этапа 1, очередей нет) пул не запускается — служба всегда
// работает с реальным диспетчером.
func (p *Provider) startFetchPool(ctx context.Context) {
	d, ok := p.dispatch.(*cfapi.Dispatcher)
	if !ok {
		// Заглушка-логгер этапа 1 очередей не имеет — гидрации нет (тесты
		// и stage-1 режим; serve всегда передаёт *cfapi.Dispatcher).
		return
	}
	// Снимок канала остановки на жизнь пула (паттерн walk/runPopulation):
	// Stop закрывает канал и зануляет p.stop под stopMu — захваченный
	// закрытый канал читаем навсегда, select не зависает.
	stop := p.stopChan()
	if stop == nil {
		return
	}
	logger.Debugf("hydration pool started: workers=%d queue=%d block=%d", p.fetchWorkers, d.QueueCapacity().FetchData, fetchBlock)
	for i := 0; i < p.fetchWorkers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.fetchWorker(ctx, d, stop)
		}()
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.fetchOverflowDrain(ctx, d, stop)
	}()
}

// fetchWorker — воркер пула гидратации: задание из очереди FETCH_DATA →
// handleFetchData. Остановка — отмена ctx или закрытие stop (воркер
// дорабатывает текущее задание — каждый получает ровно один ответ).
func (p *Provider) fetchWorker(ctx context.Context, d *cfapi.Dispatcher, stop chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case job := <-d.FetchData():
			if job == nil {
				continue
			}
			handleFetchData(ctx, p, job)
		}
	}
}

// fetchOverflowDrain отвечает на задания, отброшенные при переполнении
// очереди FETCH_DATA (U13): ровно один CfExecute со статусом отмены из
// воркера (не из thunk'а — thunk без I/O). Задание уже снято с учёта
// диспетчером; Complete — страховка от гонки (no-op для снятых).
func (p *Provider) fetchOverflowDrain(ctx context.Context, d *cfapi.Dispatcher, stop chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case job := <-d.FetchDataOverflow():
			if job == nil {
				continue
			}
			logger.Warnf("FETCH_DATA queue overflow: fileID=%d off=%d len=%d; answering request canceled", job.FileID, job.RequiredOffset, job.RequiredLength)
			// Отказ тоже несёт запрошенный диапазон (см. handleFetchData:
			// Length=0 отвергается фильтром).
			if err := p.store.TransferData(job.ConnectionKey, job.TransferKey, job.RequestKey, cfapi.StatusCloudFileRequestCanceled, job.RequiredOffset, job.RequiredLength, nil); err != nil {
				logger.Warnf("FETCH_DATA overflow answer: %v", err)
			}
			d.Complete(job)
		}
	}
}

// handleFetchData обслуживает одно задание гидрации: разрешение inode →
// путь, чтение backend порциями, ровно один CfExecute(TRANSFER_DATA) —
// с данными или с кодом отказа (инвариант design.md).
func handleFetchData(ctx context.Context, p *Provider, job *cfapi.FetchDataJob) {
	defer func() {
		if d, ok := p.dispatch.(*cfapi.Dispatcher); ok {
			d.Complete(job)
		}
	}()
	// Инструментирование пробы U3 (этап 3): FileIdentity передаётся в
	// параметрах FETCH_DATA — задание логируется целиком (Debug).
	logger.Debugf("FETCH_DATA: transferKey=%d requestKey=%d fileID=%d off=%d len=%d optional=%d+%d flags=%#x path=%q",
		job.TransferKey, job.RequestKey, job.FileID, job.RequiredOffset, job.RequiredLength,
		job.OptionalOffset, job.OptionalLength, job.Flags, job.Path)
	started := time.Now()
	status, buf, length, release := p.readFetch(ctx, job)
	// Буфер живёт до ответа фильтру (release defer'ом): CfExecute читает
	// его синхронно внутри TransferData — возврат в пул до CfExecute дал бы
	// VirtualFree/переиспользование под живыми данными.
	defer release()
	if length > 0 && length <= int64(len(buf)) {
		buf = buf[:length] // буфер начинается на странице-выровненной базе; срез — ровно передаваемые данные
	}
	// Отказ гидрации: в ответе ОБЯЗАН стоять ЗАПРОШЕННЫЙ диапазон — Length=0
	// фильтр отвергает ERROR_CLOUD_FILE_INVALID_REQUEST (0x8007017c, живой
	// кейс 27.11 facility-маунта: отказ ответа не завершал гидратацию, файл
	// застревал в re-fetch-цикле, открытые на него CreateFile подвисали).
	// Паттерн сэмпла MS CloudMirror (FileCopierWithProgress.cpp:78-84):
	// Buffer=NULL, Offset=RequiredFileOffset, Length=RequiredLength,
	// CompletionStatus=код отказа — читающий процесс получает ошибку сразу.
	answerLength := length
	if status != cfapi.StatusSuccess {
		answerLength = job.RequiredLength
	}
	if err := p.store.TransferData(job.ConnectionKey, job.TransferKey, job.RequestKey, status, job.RequiredOffset, answerLength, buf); err != nil {
		logger.Warnf("FETCH_DATA fileID=%d: transfer status=%#x: %v", job.FileID, uint32(status), err)
	}
	// Замер M2 (design.md D11, задача 3.5): длительность и байты задания.
	logger.Debugf("FETCH_DATA done: fileID=%d status=%#x bytes=%d dur=%s",
		job.FileID, uint32(status), length, time.Since(started).Truncate(time.Microsecond))
}

// readFetch читает данные задания: путь по inode (индекс, промах —
// ResolveIno с подъёмом по родителям, U12), чтение fetchBackend порциями
// не больше fetchBlock с проверкой job.Cancel между порциями. Возвращает
// (статус, буфер, длину, release): успех — StatusSuccess с buf[:length];
// любой отказ — код статуса с nil-буфером. Буфер берётся из пула
// (страница-выровненный VirtualAlloc); release возвращает его пулу —
// вызывающий ОБЯЗАН вызвать release после ответа фильтру (страницы не
// должны освободиться/переиспользоваться до CfExecute).
func (p *Provider) readFetch(ctx context.Context, job *cfapi.FetchDataJob) (cfapi.NTStatus, []byte, int64, func()) {
	noRelease := func() {}
	// Отмена до начала работы (задание могло быть отменено в очереди).
	if job.Cancel.Load() {
		logger.Debugf("FETCH_DATA fileID=%d: canceled before start", job.FileID)
		return cfapi.StatusCloudFileRequestCanceled, nil, 0, noRelease
	}
	// Границы диапазона — до проверки длины: отрицательный offset при
	// RequiredLength == 0 в противном случае отвечал бы успехом.
	if job.RequiredOffset < 0 || job.RequiredLength < 0 {
		return cfapi.StatusCloudFileInvalidRequest, nil, 0, noRelease
	}
	if job.RequiredLength == 0 {
		return cfapi.StatusSuccess, nil, 0, noRelease
	}
	buf, release := p.bufs.Get(int(job.RequiredLength))
	if buf == nil {
		return cfapi.StatusInsufficientResources, nil, 0, noRelease
	}
	path, ok := p.idx.Path(job.FileID)
	if !ok {
		path, ok = p.idx.ResolveIno(ctx, job.FileID) // подъём по родителям (U12)
	}
	if !ok {
		logger.Warnf("FETCH_DATA fileID=%d: path not resolved (index miss, no parent chain)", job.FileID)
		return cfapi.StatusObjectNameNotFound, nil, 0, release
	}
	mctx := meta.WrapWithCancel(ctx, uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
	defer mctx.Cancel()
	f, errno := p.fbackend.Open(mctx, path)
	if errno != 0 {
		return cfapi.StatusFromErrno(errno), nil, 0, release
	}
	defer func() {
		if errno := f.Close(mctx); errno != 0 {
			logger.Warnf("FETCH_DATA %s: close: %v", path, errno)
		}
	}()
	done := int64(0)
	for done < job.RequiredLength {
		// Отмена (CANCEL_FETCH_DATA) — между порциями (design.md D7,
		// задача 3.3): ответ статусом отмены, буфер вернёт release().
		if job.Cancel.Load() {
			logger.Debugf("FETCH_DATA fileID=%d: canceled after %d of %d bytes", job.FileID, done, job.RequiredLength)
			return cfapi.StatusCloudFileRequestCanceled, nil, 0, release
		}
		end := done + fetchBlock
		if end > job.RequiredLength {
			end = job.RequiredLength
		}
		portion := buf[done:end]
		n, err := f.Pread(mctx, portion, job.RequiredOffset+done)
		if n < 0 {
			n = 0
		}
		if n < len(portion) {
			// Короткое чтение: хвост обнуляется — буфер из пула может
			// содержать данные предыдущего задания (чужие байты не должны
			// уйти фильтру; страницы VirtualAlloc нулевые только при первой
			// фиксации).
			clear(portion[n:])
		}
		if err != nil {
			// Ошибка чтения backend — один CfExecute с кодом отказа
			// (задача 3.4), процесс жив.
			return fetchStatusOf(err), nil, 0, release
		}
		if n == 0 {
			// Нет прогресса без ошибки — конец данных (defensive; Pread
			// обычно возвращает io.EOF).
			return cfapi.StatusEndOfFile, nil, 0, release
		}
		done += int64(n)
	}
	return cfapi.StatusSuccess, buf, done, release
}

// fetchStatusOf — ошибка Pread → NTSTATUS ответа TRANSFER_DATA (задача 3.4):
// context.Canceled — движок останавливается (ctx serve отменён, mctx с ним)
// — отменный статус, как при CANCEL_FETCH_DATA; io.EOF — конец данных
// (backend короче плейсхолдера — гонка с усечением); errno — таблица
// StatusFromErrno, остальное — STATUS_INTERNAL_ERROR.
func fetchStatusOf(err error) cfapi.NTStatus {
	if errors.Is(err, context.Canceled) {
		return cfapi.StatusCloudFileRequestCanceled
	}
	if errors.Is(err, io.EOF) {
		return cfapi.StatusEndOfFile
	}
	var eno syscall.Errno
	if errors.As(err, &eno) {
		return cfapi.StatusFromErrno(eno)
	}
	return cfapi.StatusInternalError
}
