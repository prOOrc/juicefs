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

// Restart-сверка свежести (fix-cfapi-followups A2, спека «Сверка при
// старте»): каждая своя запись-файл (известная индексу по совпавшему пути
// или усыновлённая) сверяется с записью hub по размеру (строго) и времени
// модификации (с допуском freshMtimeSlack). Каталоги исключены — NTFS
// двигает mtime каталога при любой смене записей, включая наши же
// createEntry (свалка ложных «локаль новее»). Stat-ошибка — ничего
// (удаление обработает reconcileDir). equal — ноль мутаций, строк и
// STANDARD-запросов (идемпотентность: рестарт без изменений выводит ноль
// строк свежести).
package cfprovider

import (
	"strings"
	"time"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// freshMtimeSlack — допуск сравнения mtime (design.md A2: конвенция грубых
// меток — rsync --modify-window=2, Unison -fat; маскирует возможное усечение
// до секунд на переходах; ужесточение по пробе U-F2 — константа в одном
// месте).
const freshMtimeSlack = 2 * time.Second

// freshVerdict — вердикт сверки свежести локальной записи и записи hub.
type freshVerdict int

const (
	freshEqual        freshVerdict = iota // размер и mtime (в допуске) совпадают
	freshLocalNewer                       // локальная запись новее — правка при выключенном serve
	freshBackendNewer                     // запись hub новее — правка с другой машины
	freshAmbiguous                        // размер разошёлся при |Δmtime| ≤ допуск — правили обе стороны
)

// String — причина для строк `walk classify:`/`walk fresh:` (Debug/Warn).
func (v freshVerdict) String() string {
	switch v {
	case freshEqual:
		return "equal"
	case freshLocalNewer:
		return "local-newer"
	case freshBackendNewer:
		return "backend-newer"
	default:
		return "ambiguous"
	}
}

// compareVersion — чистая сверка версии (design.md A2): размер строго,
// mtime с допуском freshMtimeSlack (границы допуска включительно); при
// разошедшемся размере и времени в пределах допуска — ambiguous.
func compareVersion(size int64, mtime time.Time, e BackendEntry) freshVerdict {
	backend := time.Unix(e.MtimeSec, int64(e.MtimeNsec))
	if size == e.Size {
		switch {
		case !mtime.Before(backend.Add(-freshMtimeSlack)) && !mtime.After(backend.Add(freshMtimeSlack)):
			return freshEqual
		case mtime.After(backend):
			return freshLocalNewer
		default:
			return freshBackendNewer
		}
	}
	switch {
	case mtime.After(backend.Add(freshMtimeSlack)):
		return freshLocalNewer
	case mtime.Before(backend.Add(-freshMtimeSlack)):
		return freshBackendNewer
	default:
		return freshAmbiguous
	}
}

// freshCounters — счётчики строки `walk fresh:` (design.md A2, Info при
// ненулевой сумме). Сверка в индекс не пишет.
type freshCounters struct {
	revalidated  int // ревалидаций backend-новее: markSelfEvent + Update(VerifyInSync|Dehydrate)
	queuedUpload int // локальнее — в очередь выгрузки (reconcile-механизм)
	readonly     int // локальнее при --read-only — только Warn, без очереди
	pinned       int // backend-новее, но pinned — skip + Warn
	ambiguous    int // размер разошёлся при |Δmtime| ≤ допуск — skip + Warn
	outOfSync    int // skip: локальная запись вне in-sync (ветка за очередью сверки/конфликт)
}

func (c *freshCounters) add(d freshCounters) {
	c.revalidated += d.revalidated
	c.queuedUpload += d.queuedUpload
	c.readonly += d.readonly
	c.pinned += d.pinned
	c.ambiguous += d.ambiguous
	c.outOfSync += d.outOfSync
}

func (c freshCounters) total() int {
	return c.revalidated + c.queuedUpload + c.readonly + c.pinned + c.ambiguous + c.outOfSync
}

// freshCheck — исход сверки одной записи: вердикт, флаги и дельта счётчиков.
type freshCheck struct {
	verdict freshVerdict
	isDir   bool
	statErr error
	counts  freshCounters
}

// checkFreshness — сверка свежести записи-файла (fix-cfapi-followups A2):
// StatOf → compareVersion; каталоги и stat-ошибки — без вердикта (ноль
// действий); equal — ноль STANDARD-запросов (идемпотентность тёплого
// рестарта). При разошедшейся версии — STANDARD-запрос гейтов (pin,
// in-sync) и ДЕЙСТВИЯ вердикта (2.3): localNewer — очередь выгрузки
// (read-only → Warn; out-of-sync → ветка за reconcileDir); backendNewer —
// markSelfEvent + Update(VerifyInSync|Dehydrate) (pinned/out-of-sync →
// skip + Warn); ambiguous — skip + Warn. Ошибка STANDARD-запроса —
// консервативный skip без действий и без счётчика (повтор на следующем
// walk: ревалидация на недоказанном состоянии могла бы задеть pinned).
func (p *Provider) checkFreshness(e BackendEntry, rel, localPath string) freshCheck {
	var fc freshCheck
	size, mtime, isDir, err := p.store.StatOf(localPath)
	if err != nil {
		fc.statErr = err
		return fc
	}
	if isDir {
		fc.isDir = true
		return fc
	}
	fc.verdict = compareVersion(size, mtime, e)
	if fc.verdict == freshEqual {
		return fc
	}
	// Состояние STANDARD-записи (гейты pin/in-sync). Недоказанное состояние —
	// консервативный skip без действий и без счётчика (см. шапку функции).
	info, isPh, serr := p.store.StandardInfoOf(localPath)
	if serr != nil || !isPh || info == nil {
		logger.Debugf("walk fresh: %s: state unavailable (%v); skipped", rel, serr)
		return fc
	}
	switch fc.verdict {
	case freshLocalNewer:
		// Локаль новее: out-of-sync по фильтру — ветка остаётся за очередью
		// выгрузки сверки (reconcileDir ставит её в очередь — дедупликация),
		// без Warn (design.md A2); read-only — только Warn; иначе — очередь
		// выгрузки тем же механизмом, что reconcileDir.
		if info.InSyncState != cfapi.InSyncStateInSync {
			fc.counts.outOfSync++
			logger.Debugf("walk fresh: %s: local newer, out-of-sync; left to reconcile queue", rel)
			return fc
		}
		if p.readOnly {
			fc.counts.readonly++
			logger.Warnf("walk fresh: %s: local newer; read-only mode, upload not queued", rel)
			return fc
		}
		logger.Debugf("walk fresh: %s: local newer; upload queued", rel)
		fc.counts.queuedUpload++
		p.queueUpload(ingestTask{fsPath: rel, local: localPath, key: strings.ToLower(rel)})
	case freshBackendNewer:
		// Backend новее: гейты — skip + Warn (design.md A2).
		if info.PinState == cfapi.PinStatePinned {
			fc.counts.pinned++
			logger.Warnf("walk fresh: %s: pinned stale (backend newer); skipped", rel)
			return fc
		}
		if info.InSyncState != cfapi.InSyncStateInSync {
			fc.counts.outOfSync++
			logger.Warnf("walk fresh: %s: backend newer, out-of-sync (both sides edited); skipped", rel)
			return fc
		}
		// Ревалидация: супрессия собственного watcher-события ОБЯЗАТЕЛЬНА до
		// Update (при сбое Update на середине или снятии in-sync фильтром
		// событие родило бы выгрузку устаревшего контента поверх новой
		// версии backend; цена — слепое окно 10с, самоизлечивается следующим
		// walk). Update обновляет size/mtime до записи hub и дегидратирует
		// (DEHYDRATE обязателен: гидратированный stale-контент без него
		// остался бы старым; следующее чтение FETCH'нет новую версию).
		p.markSelfEvent(strings.ToLower(rel))
		if uerr := p.store.Update(localPath, makeFsMetadata(e), cfapi.UpdateFlagVerifyInSync|cfapi.UpdateFlagDehydrate); uerr != nil {
			logger.Warnf("walk fresh: %s: revalidation failed: %v", rel, uerr)
			return fc
		}
		fc.counts.revalidated++
	case freshAmbiguous:
		fc.counts.ambiguous++
		logger.Warnf("walk fresh: %s: both sides diverged within %s slack; skipped", rel, freshMtimeSlack)
	}
	return fc
}
