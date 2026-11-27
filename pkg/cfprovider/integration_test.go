//go:build integration
// +build integration

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

// Интеграционные сценарии спеки domain-cfapi-provider на ЖИВОМ sync root
// (задача 7.4): компилируются только с тегом `integration`; без переменной
// окружения CLAUDDrive_TEST_ROOT пропускаются. Запуск — на машине с
// зарегистрированным sync root и запущенным serve:
//
//		go test -tags integration -run TestIntegration -v ./pkg/cfprovider
//
//	  - CLAUDDrive_TEST_ROOT — путь sync root (например %USERPROFILE%\AgioDrive);
//	  - serve должен быть запущен оркестратором (juicefs clouddrive serve ...);
//	  - лог serve (stderr/файл) используется для проверки FETCH_DATA/ACK.
//
// Тела тестов — живая часть этапа 7: их выполняет оркестратор на реальном
// фильтре; здесь — скелет с точными шагами сценариев (не реализация: живые
// проверки штатно выполняются вручную/скриптами по spec, автоматизация —
// предмет отдельного решения).
package cfprovider

import (
	"os"
	"testing"
)

// integrationRoot — sync root интеграционного прогона; без него — skip.
func integrationRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("CLAUDDrive_TEST_ROOT")
	if root == "" {
		t.Skip("CLAUDDrive_TEST_ROOT не задан; живая часть — оркестратор (serve + зарегистрированный sync root)")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("sync root %s недоступен: %v", root, err)
	}
	return root
}

// TestIntegrationCreate — сценарий «Верхний уровень» + «Новый файл» (этап 2/5):
//
//  1. Убедиться, что serve запущен и walk завершён: в логе есть
//     `walk complete:` (полнота namespace) и маркер `OK, <том> is ready`.
//  2. PowerShell: New-Item "$env:CLAUDDrive_TEST_ROOT\intg_new.txt" -ItemType File
//     (create в заселённом каталоге завершается платформой — без FETCH,
//     design.md D6.2).
//  3. Записать байты: Set-Content ... -Value "..."; дождаться выгрузки:
//     watcher → upload PUT ×1 → replace-after-upload → в логе serve
//     «conversion pending»/SetInSync; backend-хэш совпадает
//     (juicefs clouddrive hash <root>/intg_new.txt против Get-FileHash).
//  4. status: juicefs clouddrive status <root>\intg_new.txt — плейсхолдер,
//     in-sync (для FULL-in-sync см. follow-up 5.8).
func TestIntegrationCreate(t *testing.T) {
	integrationRoot(t)
	// TODO(orchestrator): шаги 1–4 сценария выше (живая часть).
	t.Log("скелет: живые шаги выполняются оркестратором")
}

// TestIntegrationRead — сценарий «Содержимое совпадает с backend» + дегидратация (7.10):
//
//  1. attrib <root>\frame_001.bin — атрибуты P (pinned/offline-плейсхолдер),
//     размер на диске ≈ 0 (дегидратирован): fsutil file layout / PowerShell
//     (Get-Item).AllocSize ≈ 0.
//  2. Get-Content <root>\frame_001.bin -TotalCount 1 → в логе serve ровно
//     FETCH_DATA с fileID = inode этой записи (проба U3), статус 0x0.
//  3. Сравнить хэш: clouddrive hash <путь> == Get-FileHash прочитанного.
//  4. Повторное чтение — FETCH_DATA снова (write-back выключен).
func TestIntegrationRead(t *testing.T) {
	integrationRoot(t)
	// TODO(orchestrator): шаги 1–4 (живая часть).
	t.Log("скелет: живые шаги выполняются оркестратором")
}

// TestIntegrationRename — сценарий «Переименование» (этап 6, U19):
//
//  1. Rename-Item <root>\frame_003.bin frame_003_moved → в логе serve
//     ACK_RENAME 0x0, backend Rename ✓ (живая верификация 27.11).
//  2. Индекс: старый путь снят, новый добавлен (status обоих путей:
//     старый → not found, новый → placeholder).
//  3. Rename каталога с потомками (RenamePrefix): Rename-Item каталога →
//     backend Rename каталога + пере-резолв inode потомков.
func TestIntegrationRename(t *testing.T) {
	integrationRoot(t)
	// TODO(orchestrator): шаги 1–3 (живая часть).
	t.Log("скелет: живые шаги выполняются оркестратором")
}

// TestIntegrationDelete — сценарий «Удаление плейсхолдера» + U19:
//
//  1. Remove-Item <root>\frame_002.bin → callback → (pending-delete
//     блокировка 3с по U19) → plain → backend Unlink → ACK_DELETE 0x0.
//  2. Backend gone: clouddrive ls родителя — записи нет.
//  3. Удаление локального файла ДО выгрузки (задача 6.5): создать файл,
//     удалить до Flush → очередь выгрузки отменена, журнал чист.
func TestIntegrationDelete(t *testing.T) {
	integrationRoot(t)
	// TODO(orchestrator): шаги 1–3 (живая часть).
	t.Log("скелет: живые шаги выполняются оркестратором")
}

// TestIntegrationDirs — сценарии «Дочерний каталог», «Новый каталог»,
// «Создание офлайн», «Удаление офлайн», «Одноимённый локальный файл»:
//
//  1. Дочерний каталог: walk создал детей ≥3 уровней (walk complete:,
//     все записи hub созданы — clouddrive ls сверка).
//  2. Новый каталог: New-Item -ItemType Directory → watcher → mkdir в
//     backend + плейсхолдер (ACK/конверсия каталога).
//  3. Создание офлайн (serve остановлен): создать файл/каталог в корне,
//     запустить serve → сверка при старте выгружает (коалисер/reconcileDir).
//  4. Одноимённый локальный файл: положить обычный файл с именем записи
//     hub ДО запуска serve → walk: один Warn на каталог, запись пропущена,
//     локальный файл не тронут.
func TestIntegrationDirs(t *testing.T) {
	integrationRoot(t)
	// TODO(orchestrator): шаги 1–4 (живая часть).
	t.Log("скелет: живые шаги выполняются оркестратором")
}

// TestIntegrationPin — сценарии «Pin файла и чтение без провайдера»,
// «Pin каталога», «Unpin» (этап 4, U5):
//
//  1. juicefs clouddrive pin <root>\docs\report.pdf → FETCH_DATA ×2,
//     attrib P без O; kill serve; чтение файла → содержимое читается,
//     SHA совпадает (гидратированные данные локальны).
//  2. juicefs clouddrive pin --recursive docs → только состояния
//     (RECURSE=0x1), гидратация ленивая (FETCH=0 в течение 15с).
//  3. juicefs clouddrive unpin docs\report.pdf → U-атрибут; дешидратация
//     отложенная (проверить позже attrib/AllocSize).
func TestIntegrationPin(t *testing.T) {
	integrationRoot(t)
	// TODO(orchestrator): шаги 1–3 (живая часть).
	t.Log("скелет: живые шаги выполняются оркестратором")
}

// TestIntegrationCycles10 — задача 7.5 (десять циклов «запуск — чтение —
// запись — удаление — остановка»): автоматизация — scripts/dod-cycles.ps1
// (goroutines через pprof 127.0.0.1:6060, инвариант пула — отсутствие
// застрявших FETCH_DATA в goroutine-дампе и строки «FETCH_DATA done»
// по числу заданий). Здесь — только оркестрация скрипта, не логика.
func TestIntegrationCycles10(t *testing.T) {
	integrationRoot(t)
	// TODO(orchestrator): запустить scripts/dod-cycles.ps1 -Cycles 10 и
	// приложить таблицу циклов к change (живая часть).
	t.Log("скелет: см. scripts/dod-cycles.ps1")
}
