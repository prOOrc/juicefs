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

// Сверка при старте, по-каталожная часть (design.md D9 «Сверка при старте»,
// задача 6.4). Namespace-часть сверки выполняется самим walk (D6:
// по-каталожное сравнение локальных имён с hub); этот файл — write-часть,
// вызываемая из populateDirectory: каталог, который walk только что
// обработал, считается «каталогом с завершённым наполнением»:
//
//	(а) известные плейсхолдеры вне in-sync (выгрузка не завершилась — сбой
//	    после close) и локальные файлы без записи backend (потерянные
//	    события watcher'а) → очередь выгрузки (коалисер пула; до его
//	    поднятия — журнал операций, спека «Сверка при старте»: «локальные
//	    файлы без плейсхолдера и плейсхолдеры вне состояния in-sync»);
//	(б) записи индекса без локального файла → очередь удаления backend
//	    через журнал («записи индекса, отсутствующие на диске в каталогах
//	    с завершённым наполнением»; только созданные провайдером — по
//	    индексу; сценарий «Восстановление после сбоя»: удалённый
//	    плейсхолдер удаляется в backend).
//
// Локальные plain-файлы, чьё имя занято записью hub (конфликты), не
// выгружаются и не удаляются — инвариант «локальный файл пользователя не
// перезаписывается и не удаляется». Сверка отключается --read-only
// (спека «Режим без записи»).
//
// cldapi.dll не вызывается: состояние — через ingestStore (подменный в
// тестах), backend-очередь — через журнал операций.
package cfprovider

import (
	"context"
	"os"
	"strings"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// standardOnce — одиночный STANDARD-запрос без повторов занятости: сверка
// идёт в фоне walk-воркером, блокировать каталог повторами нельзя; занятая
// (открытая) запись пропускается — её классифицирует следующий close либо
// следующая сверка.
func (p *Provider) standardOnce(local string) (isPlaceholder, inSync bool, err error) {
	info, err := p.istore.StandardInfo(local)
	if err != nil {
		return false, false, err
	}
	return true, info.InSyncState == cfapi.InSyncStateInSync, nil
}

// reconcileDir — write-часть сверки одного каталога (см. шапку файла).
// entries — записи hub каталога (листинг walk), presentNames — локальные
// имена (нижний регистр → оригинал). Записи журнала — через journalAdd
// (дедупликация по ключу; исполнение — replayer с нарастающей паузой).
func reconcileDir(ctx context.Context, p *Provider, dir string, entries []BackendEntry, presentNames map[string]string) {
	if p.idx == nil || p.readOnly {
		return
	}
	hub := make(map[string]BackendEntry, len(entries))
	for _, e := range entries {
		hub[strings.ToLower(e.Name)] = e
	}
	// (а) очередь выгрузки.
	for low, orig := range presentNames {
		e, inHub := hub[low]
		rel := fsJoin(dir, orig)
		local := localJoin(p.root, rel)
		if inHub {
			// Имя занято записью hub: выгрузка — только для НАШЕГО
			// плейсхолдера вне in-sync (конфликтные локальные файлы walk
			// пропустил — их не трогаем; каталоги не выгружаются).
			if e.IsDir {
				continue
			}
			if _, known := p.idx.Path(e.Inode); !known {
				continue
			}
			isPh, inSync, err := p.standardOnce(local)
			if err != nil {
				logger.Debugf("reconcile %s: state unavailable: %v", rel, err)
				continue
			}
			if isPh && inSync {
				continue
			}
			logger.Debugf("reconcile %s: placeholder out of sync; upload queued", rel)
			p.queueUpload(ingestTask{fsPath: rel, local: local, key: low})
			continue
		}
		// Имени нет в hub: локальный plain-файл без записи backend
		// (потерянные события watcher'а) — выгрузка; плейсхолдер без записи
		// backend — stale (U16, решение за владельцем) — не трогаем.
		isPh, _, err := p.standardOnce(local)
		if err == nil && isPh {
			continue
		}
		if fi, serr := os.Stat(local); serr == nil && !fi.IsDir() {
			logger.Debugf("reconcile %s: local file without backend entry; upload queued", rel)
			p.queueUpload(ingestTask{fsPath: rel, local: local, key: low})
		}
	}
	// (б) очередь удаления: записи индекса каталога без локального файла.
	for _, ie := range p.idx.PathsInDir(dir) {
		local := localJoin(p.root, ie.Path)
		if _, err := os.Stat(local); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			logger.Warnf("reconcile %s: stat: %v", ie.Path, err)
			continue
		}
		if p.journal != nil && p.journal.HasPending(OpDelete, ie.Path) {
			continue // повтор уже ведёт replayer
		}
		logger.Infof("reconcile %s: index entry missing on disk; delete queued", ie.Path)
		p.journalAdd(JournalOp{Type: OpDelete, Path: ie.Path, Ino: ie.Ino})
	}
}
