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

package cmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unsafe"

	"github.com/urfave/cli/v2"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/juicedata/juicefs/pkg/cfapi"
	"github.com/juicedata/juicefs/pkg/cfprovider"
	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/fs"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/juicedata/juicefs/pkg/version"
	"github.com/juicedata/juicefs/pkg/vfs"
	"github.com/juicedata/juicefs/pkg/win"
)

// Константы регистрации AgioDrive (design.md D4). id провайдера в имени
// ключа Explorer — строка, аккаунт — Agio. GUID провайдера — постоянный
// идентификатор CfAPI-регистрации. Ветка реестра — HKCU (проба U1; HKLM —
// только если проба покажет необходимость). status <путь> — задачей 5.7.
const (
	cloudDriveDisplayName  = "AgioDrive"
	cloudDriveProviderID   = "AgioDrive"
	cloudDriveAccountID    = "Agio"
	cloudDriveProviderGUID = "{3F7A2C5D-8E41-4B6A-9C2D-5A7E1F0B8C4D}"

	syncRootManagerKeyPath = `Software\Microsoft\Windows\CurrentVersion\Explorer\SyncRootManager`
)

func cmdCloudDrive() *cli.Command {
	// Флаги объявлены на родителе (как в quota/tier): reorderOptions
	// валидирует опции против флагов верхнеуровневой команды, значения
	// действия подкоманд читают через parent-chain urfave/cli.
	return &cli.Command{
		Name:            "clouddrive",
		Category:        "SERVICE",
		Usage:           "Manage the AgioDrive Windows Cloud Files sync root",
		HideHelpCommand: true,
		Flags: append([]cli.Flag{
			&cli.StringFlag{
				Name:  "root",
				Value: defaultCloudDriveRoot(),
				Usage: "sync root directory (must be on an NTFS volume)",
			},
			&cli.StringFlag{
				Name:  "volume-name",
				Usage: "volume name for readiness logging (default: name from the volume format)",
			},
			&cli.StringFlag{
				Name:  "state-dir",
				Usage: "provider state directory (default: %LOCALAPPDATA%\\AgioDrive\\<volume>)",
			},
			&cli.IntFlag{
				// Политика гидрации (register, CF_HYDRATION_POLICY primary).
				// Дефолт — Progressive + StreamingAllowed (design.md D4,
				// гидрация on-demand). Modifier StreamingAllowed описан в
				// header для progressive-гидрации — при прочих primary
				// ставится ModifierNone. ALWAYS_FULL (3) — столбец
				// эмпирической матрицы проб 27.11: провайдерский
				// CfCreatePlaceholders отвергается фильтром с
				// ERROR_CLOUD_FILE_INVALID_REQUEST (0x8007017C) — register
				// выводит Warn.
				Name:  "hydration-policy",
				Value: int(cfapi.HydrationPolicyProgressive),
				Usage: "CF_HYDRATION_POLICY primary for register (advanced; 0 = partial, 1 = progressive + streaming allowed (default), 2 = full, 3 = always full — placeholder creation rejected by the filter)",
			},
			&cli.IntFlag{
				// Дефолт — пивот 27.11 (design.md D3/D6): ALWAYS_FULL (3) —
				// namespace строится полным walk провайдера, платформа
				// завершает пользовательский create сама, FETCH не
				// вызывается (эксперименты 1–6, U2/U14: FETCH-режимы не
				// дают фильтру завершённого namespace). PARTIAL (0) и FULL
				// (2) — FETCH-режимы для диагностики: register выводит Warn
				// о неполном namespace. Перерегистрация (register повторно)
				// обновляет политику.
				Name:  "population-policy",
				Value: int(cfapi.PopulationPolicyAlwaysFull),
				Usage: "CF_POPULATION_POLICY for register (advanced; 3 = always full — default: provider walks the whole volume, user-creates completed by the platform; 0 = provider-defined namespace; 2 = on-demand population — FETCH modes, incomplete namespace)",
			},
			&cli.IntFlag{
				// Advanced-флаг эмпирической матрицы проб (register): маска
				// CF_PLACEHOLDER_MANAGEMENT_POLICY (0..7). 0x0 = DEFAULT —
				// «провайдер делает всё», user-creates запрещены (проба
				// 27.11: ERROR_CANT_RESOLVE_FILENAME 1921/0x80070781);
				// биты: 0x1 CREATE, 0x2 CONVERT, 0x4 UPDATE unrestricted —
				// write-путь В3 требует 0x7 (по умолчанию).
				// Перерегистрация (register повторно) обновляет политику.
				Name:  "management-policy",
				Value: int(cfapi.PlaceholderManagementPolicyCreateUnrestricted | cfapi.PlaceholderManagementPolicyConvertToUnrestricted | cfapi.PlaceholderManagementPolicyUpdateUnrestricted),
				Usage: "CF_PLACEHOLDER_MANAGEMENT_POLICY mask for register (advanced; bit 0x1 = user-creates allowed, 0x2 = convert-to-placeholder allowed, 0x4 = placeholder edits allowed; 0 = provider does all, user-creates rejected)",
			},
			&cli.IntFlag{
				// Пул гидратации (serve, design.md D7/D11 — задача 3.5, замер
				// M2 при N=1/8/64 параллельных FETCH_DATA): число воркеров
				// очереди FETCH_DATA; буферы TRANSFER_DATA — VirtualAlloc по
				// 4 MiB порций, один буфер на задание.
				Name:  "hydration-workers",
				Value: cfprovider.DefaultFetchWorkers,
				Usage: "number of hydration workers for serve (FETCH_DATA pool size; parallel reads beyond the pool wait in the queue)",
			},
			&cli.BoolFlag{
				// Режим без записи (serve, спека «Режим без записи», задача
				// 5.6): close-уведомления игнорируются — локальные изменения
				// не передаются в backend; файлы остаются на диске без
				// изменений. Гидрация, walk и pin не меняются.
				Name:  "read-only",
				Usage: "for serve: do not upload local changes to the volume (notifications of file closes are ignored)",
			},
			&cli.BoolFlag{
				// pin/unpin (задача 4.1): CF_SET_PIN_FLAG_RECURSE — состояние
				// pin на всё поддерево каталога. На родителе (как все флаги
				// этой команды): reorderOptions в cmd/main.go валидирует
				// опции против флагов верхнеуровневой команды, значение
				// читается действием подкоманды через parent-chain.
				Name:  "recursive",
				Usage: "for pin/unpin: apply the pin state to the whole directory subtree (CF_SET_PIN_FLAG_RECURSE; recursive pin hydrates the subtree as FETCH_DATA batches to the running serve)",
			},
		}, nodeSTSFlags()...),
		Description: `
Manage a Windows Cloud Files API (CfAPI) sync root backed by a JuiceFS volume.

Examples:
# Register %USERPROFILE%\AgioDrive as the sync root (must be on an NTFS volume)
$ juicefs clouddrive register

# Show registration state of the sync root
$ juicefs clouddrive status

# Serve the sync root (single instance, Ctrl+C to stop)
$ juicefs clouddrive serve grpc://meta-proxy:9900?tls=1 --sts-enabled --company-id <uuid>

# List a backend directory (same entries Explorer shows under the sync root)
$ juicefs clouddrive ls grpc://meta-proxy:9900?tls=1 /docs

# SHA-256 of a backend file
$ juicefs clouddrive hash grpc://meta-proxy:9900?tls=1 /docs/report.pdf

# Pin a placeholder (downloads its content; requires a running serve)
$ juicefs clouddrive pin docs/report.pdf

# Pin a directory subtree recursively (FETCH_DATA batches to the running serve)
$ juicefs clouddrive pin --recursive docs

# Unpin (the filter dehydrates the placeholder and frees disk space)
$ juicefs clouddrive unpin docs/report.pdf

# Unregister the sync root
$ juicefs clouddrive unregister`,
		Subcommands: []*cli.Command{
			{
				Name:   "register",
				Usage:  "Register the sync root with the Cloud Files API",
				Action: cloudDriveRegister,
			},
			{
				Name:   "unregister",
				Usage:  "Unregister the sync root and remove the Explorer registry key",
				Action: cloudDriveUnregister,
			},
			{
				Name:      "status",
				ArgsUsage: "[PATH]",
				Usage:     "Show sync root registration state; with PATH — placeholder state of the entry (placeholder/in-sync)",
				Action:    cloudDriveStatus,
			},
			{
				Name:      "serve",
				ArgsUsage: "META-URL",
				Usage:     "Serve the sync root as the Cloud Files API provider (Ctrl+C to stop)",
				Action:    cloudDriveServe,
			},
			{
				Name:      "ls",
				ArgsUsage: "META-URL PATH",
				Usage:     "List a backend directory (path relative to the volume root)",
				Action:    cloudDriveLs,
			},
			{
				Name:      "hash",
				ArgsUsage: "META-URL PATH",
				Usage:     "Print SHA-256 of a backend file (hex)",
				Action:    cloudDriveHash,
			},
			{
				Name:      "pin",
				ArgsUsage: "PATH",
				Usage:     "Pin a file or directory in the sync root: the filter hydrates its content via FETCH_DATA to a running serve (requires running serve for hydration)",
				Action:    cloudDrivePin,
			},
			{
				Name:      "unpin",
				ArgsUsage: "PATH",
				Usage:     "Unpin a file or directory in the sync root: the filter dehydrates the placeholder and frees its disk space (works without serve)",
				Action:    cloudDriveUnpin,
			},
		},
	}
}

// defaultCloudDriveRoot — корень по умолчанию: %USERPROFILE%\AgioDrive
// (proposal.md D-2).
func defaultCloudDriveRoot() string {
	return filepath.Join(os.Getenv("USERPROFILE"), "AgioDrive")
}

// cloudDriveRegister — спека «Регистрация sync root»: предпроверка NTFS,
// CfRegisterSyncRoot, ключ реестра Explorer. Повторный запуск обновляет
// регистрацию без дублей.
func cloudDriveRegister(c *cli.Context) error {
	setup(c, 0)
	root := c.String("root")

	// Предпроверка тома до регистрации: cldflt.sys работает только на NTFS.
	fsName, err := fileSystemOf(root)
	if err != nil {
		return fmt.Errorf("cannot determine the file system of %s: %w", root, err)
	}
	if fsName != "NTFS" {
		return fmt.Errorf("sync root %s is on %s; the Cloud Files API requires NTFS", root, fsName)
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return fmt.Errorf("cannot create sync root directory %s: %w", root, err)
	}

	providerID, err := windows.GUIDFromString(cloudDriveProviderGUID)
	if err != nil {
		return fmt.Errorf("provider GUID %s: %w", cloudDriveProviderGUID, err)
	}
	// Идентичность sync root/файла при регистрации не задаётся: плейсхолдеры
	// получают свою (inode, design.md D6) при создании, начиная с этапа 2.
	registration, err := cfapi.NewSyncRegistration(cloudDriveDisplayName, version.Version(), nil, nil, providerID)
	if err != nil {
		return fmt.Errorf("sync registration: %w", err)
	}
	// Политика управления плейсхолдерами (cfapi.h:206-211,
	// CF_PLACEHOLDER_MANAGEMENT_POLICY). По умолчанию все три
	// unrestricted-бита (маска 0x7) — write-путь В3: CREATE (новые файлы
	// пользователя), CONVERT (конверсия full-файла в плейсхолдер после
	// выгрузки), UPDATE (правки содержимого плейсхолдеров). Живая проба
	// 27.11: DEFAULT (0x0, «провайдер делает всё») → любой create в sync
	// root отклоняется фильтром с ERROR_CANT_RESOLVE_FILENAME (Win32 1921 /
	// HRESULT 0x80070781).
	management := c.Int("management-policy")
	if management < 0 || management > 7 {
		return fmt.Errorf("invalid --management-policy %d (supported: mask 0..7 — CREATE|CONVERT|UPDATE unrestricted)", management)
	}
	// Политика наполнения (population). Дефолт — ALWAYS_FULL (3), пивот
	// 27.11 (design.md D3/D6): namespace строится полным фоновым walk
	// провайдера; платформа завершает пользовательский create сама, FETCH
	// не вызывается (эксперименты 1–6, U2/U14). PARTIAL (0) и FULL (2) —
	// FETCH-режимы для диагностики: независимо от содержимого FETCH-ответов
	// фильтр не получает завершённого namespace для user-create; register
	// предупреждает о неполном namespace.
	population := c.Int("population-policy")
	if population < 0 || population > 3 {
		// Числовой диапазон ДО приведения к uint16: большие значения иначе
		// усекаются молча (например, int64(65539) → uint16(3) = ALWAYS_FULL).
		return fmt.Errorf("invalid --population-policy %d (supported: 0, 2, 3)", population)
	}
	switch uint16(population) {
	case cfapi.PopulationPolicyPartial, cfapi.PopulationPolicyFull, cfapi.PopulationPolicyAlwaysFull:
	default:
		return fmt.Errorf("invalid --population-policy %d (supported: 0, 2, 3)", population)
	}
	if population != int(cfapi.PopulationPolicyAlwaysFull) {
		logger.Warnf("FETCH-режим: namespace неполный, пользовательский create не гарантируется (--population-policy %d)", population)
	}
	// Политика гидрации (cfapi.h CF_HYDRATION_POLICY). Modifier
	// StreamingAllowed описан в header для progressive-гидрации — при прочих
	// primary ставится ModifierNone (связки «не-progressive + streaming» в
	// header нет). ALWAYS_FULL (3): плейсхолдер обязан создаваться полностью
	// гидратированным — провайдерский CfCreatePlaceholders отвергается
	// фильтром с ERROR_CLOUD_FILE_INVALID_REQUEST (0x8007017C), проба
	// матрицы 27.11.
	hydration := c.Int("hydration-policy")
	if hydration < 0 || hydration > 3 {
		// Числовой диапазон ДО приведения к uint16 — усечение молча
		// пропустило бы значение (например, int64(65536) → uint16(0)).
		return fmt.Errorf("invalid --hydration-policy %d (supported: 0, 1, 2, 3)", hydration)
	}
	switch uint16(hydration) {
	case cfapi.HydrationPolicyPartial, cfapi.HydrationPolicyProgressive, cfapi.HydrationPolicyFull, cfapi.HydrationPolicyAlwaysFull:
	default:
		return fmt.Errorf("invalid --hydration-policy %d (supported: 0, 1, 2, 3)", hydration)
	}
	hydrationModifier := cfapi.HydrationPolicyModifierNone
	if uint16(hydration) == cfapi.HydrationPolicyProgressive {
		hydrationModifier = cfapi.HydrationPolicyModifierStreamingAllowed
	}
	if uint16(hydration) == cfapi.HydrationPolicyAlwaysFull {
		logger.Warnf("ALWAYS_FULL: CfCreatePlaceholders будет отвергнут фильтром (MS-дока)")
	}
	// Остальные политики прототипа (design.md D4): атрибутный in-sync-
	// трекинг выключен; гидрация и наполнение — из флагов выше.
	policies := cfapi.NewSyncPolicies(
		cfapi.CFHydrationPolicy{Primary: uint16(hydration), Modifier: hydrationModifier},
		cfapi.CFPopulationPolicy{Primary: uint16(population), Modifier: cfapi.PopulationPolicyModifierNone},
		cfapi.InSyncPolicyTrackNone,
		cfapi.HardlinkPolicyNone,
		uint32(management),
	)
	if err := cfapi.CfRegisterSyncRoot(root, registration, policies, cfapi.RegisterFlagNone); err != nil {
		// Повторная регистрация (спека «Повторная регистрация»): корень уже
		// зарегистрирован — обновляем описание без дублей записей.
		logger.Debugf("direct register of %s failed (%v), retrying with CF_REGISTER_FLAG_UPDATE", root, err)
		if err := cfapi.CfRegisterSyncRoot(root, registration, policies, cfapi.RegisterFlagUpdate); err != nil {
			return fmt.Errorf("CfRegisterSyncRoot(%s): %w", root, err)
		}
	}

	sid, err := currentUserSIDStr()
	if err != nil {
		return fmt.Errorf("current user SID: %w", err)
	}
	if err := writeSyncRootRegistry(root, sid); err != nil {
		return fmt.Errorf("cannot write Explorer registry key: %w", err)
	}
	logger.Infof("sync root %s registered", root)
	return nil
}

// cloudDriveUnregister — спека «Отмена регистрации»: CfUnregisterSyncRoot
// и удаление ключа реестра.
func cloudDriveUnregister(c *cli.Context) error {
	setup(c, 0)
	root := c.String("root")

	if err := cfapi.CfUnregisterSyncRoot(root); err != nil {
		// Корень мог быть не зарегистрирован — реестр всё равно чистим.
		logger.Warnf("CfUnregisterSyncRoot(%s): %v; cleaning up registry key anyway", root, err)
	}
	sid, err := currentUserSIDStr()
	if err != nil {
		return fmt.Errorf("current user SID: %w", err)
	}
	if err := deleteSyncRootRegistry(sid); err != nil {
		return fmt.Errorf("cannot delete Explorer registry key: %w", err)
	}
	logger.Infof("sync root %s unregistered", root)
	return nil
}

// cloudDriveStatus — спека «Первая регистрация»: зарегистрирован/не
// зарегистрирован (ключ реестра), путь корня, существование каталога.
func cloudDriveStatus(c *cli.Context) error {
	setup(c, 0)
	root := c.String("root")

	// `status <путь>` (задача 5.7, сценарии «Новый файл»/«Изменённый
	// плейсхолдер»): состояние записи sync root — плейсхолдер и in-sync.
	if arg := strings.TrimSpace(c.Args().Get(0)); arg != "" {
		return cloudDriveStatusPath(c, root, arg)
	}

	registered := "no"
	keyPath := ""
	if sid, err := currentUserSIDStr(); err == nil {
		keyPath = syncRootRegistryPath(sid)
		if k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE); err == nil {
			k.Close()
			registered = "yes"
		}
	}

	// Фактические политики зарегистрированного sync root — readback
	// CfGetSyncRootInfoByPath(CF_SYNC_ROOT_INFO_STANDARD): фильтр хранит то,
	// что записал register (матрица проб: сверка hydration/population после
	// перерегистрации). mgmt фильтр не отдаёт: класса POLICIES в CfAPI нет
	// (эталонный header, CF_SYNC_ROOT_INFO_CLASS: BASIC/STANDARD/PROVIDER) —
	// фактическое значение видно только поведением фильтра. Печатается в
	// общем порядке вывода ниже (после Registry).
	policiesLine := ""
	if registered == "yes" {
		var info cfapi.CFSyncRootStandardInfo
		buf := make([]byte, unsafe.Sizeof(info)+4<<10) // запас под SyncRootIdentity
		var returned uint32
		if err := cfapi.CfGetSyncRootInfoByPath(root, cfapi.SyncRootInfoStandard, buf, &returned); err == nil {
			si := (*cfapi.CFSyncRootStandardInfo)(unsafe.Pointer(&buf[0]))
			policiesLine = fmt.Sprintf("hydration=%d+%d population=%d inSync=0x%08x hardlink=0x%08x mgmt=n/a",
				si.HydrationPolicy.Primary, si.HydrationPolicy.Modifier, si.PopulationPolicy.Primary, si.InSyncPolicy, si.HardLinkPolicy)
		} else {
			policiesLine = fmt.Sprintf("unavailable (%v)", err)
		}
	}
	directory := "missing"
	if info, err := os.Stat(root); err == nil {
		if info.IsDir() {
			directory = "present"
		} else {
			directory = "not a directory"
		}
	}

	fmt.Printf("Sync root:  %s\n", root)
	fmt.Printf("Registered: %s\n", registered)
	if keyPath != "" {
		fmt.Printf("Registry:   HKCU\\%s\n", keyPath)
	}
	if policiesLine != "" {
		fmt.Printf("Policies:   %s\n", policiesLine)
	}
	fmt.Printf("Directory:  %s\n", directory)
	return nil
}

// cloudDriveStatusPath — `status <путь>` (задача 5.7, спека «Выгрузка новых
// и изменённых файлов»): CF_PLACEHOLDER_STATE локальной записи sync root
// (cfapi.PlaceholderStateByPath) — плейсхолдер и in-sync; путь в стиле
// индекса ("/a/b") и inode из журнала каталога состояния.
func cloudDriveStatusPath(c *cli.Context, root, arg string) error {
	path, err := cloudDriveResolveLocalPath(root, arg)
	if err != nil {
		return err
	}
	state, err := cfapi.PlaceholderStateByPath(path)
	if err != nil {
		return fmt.Errorf("placeholder state of %s: %w (the path must exist inside the sync root)", path, err)
	}
	// Путь в стиле индекса плейсхолдеров (журнал inode → путь, design.md D6).
	indexPath := "/"
	if rel, rerr := filepath.Rel(root, path); rerr == nil && rel != "." {
		indexPath = "/" + filepath.ToSlash(rel)
	}
	indexLine := "n/a (pass --state-dir or --volume-name to look up)"
	stateDir := c.String("state-dir")
	if stateDir == "" && c.String("volume-name") != "" {
		stateDir = cfprovider.DefaultStateDir(c.String("volume-name"))
	}
	if stateDir != "" {
		if _, err := os.Stat(cfprovider.IndexPathJournal(stateDir)); err == nil {
			if idx, ierr := cfprovider.NewIndex(stateDir); ierr == nil {
				if ino, ok := idx.LookupPath(indexPath); ok {
					indexLine = fmt.Sprintf("inode %d", ino)
				} else {
					indexLine = "not found"
				}
				_ = idx.Close()
			} else {
				indexLine = fmt.Sprintf("unavailable (%v)", ierr)
			}
		} else {
			indexLine = "no journal"
		}
	}
	var parts []string
	if state&cfapi.PlaceholderStatePlaceholder != 0 {
		parts = append(parts, "placeholder")
		if state&cfapi.PlaceholderStateInSync != 0 {
			parts = append(parts, "in-sync")
		} else {
			parts = append(parts, "modified (not in-sync)")
		}
		if state&cfapi.PlaceholderStatePartial != 0 {
			parts = append(parts, "partial")
		}
	} else {
		// state=0 НЕ означает «не плейсхолдер»: фильтр чистит спец-атрибуты
		// у полностью синхронизированных записей (живой ретест этапа 5).
		// Уточняем через CF_PLACEHOLDER_STANDARD_INFO (OnDiskDataSize) и
		// логическую длину: ошибка запроса (STANDARD требует
		// плейсхолдер-хендл, plain-файл вне sync root) — честное
		// «not a placeholder», ошибка глотается.
		if fi, ferr := os.Stat(path); ferr == nil {
			if info, ierr := cfapi.PlaceholderStandardInfoByPath(path, fi.IsDir()); ierr == nil {
				inSync := "in-sync state unknown"
				switch info.InSyncState {
				case cfapi.InSyncStateInSync:
					inSync = "in-sync"
				case cfapi.InSyncStateNotInSync:
					inSync = "not in-sync"
				}
				switch {
				case fi.Size() > 0 && info.OnDiskDataSize == 0:
					parts = append(parts, "placeholder, dehydrated ("+inSync+")")
				case fi.Size() > 0:
					parts = append(parts, "placeholder, hydrated ("+inSync+")")
				default:
					parts = append(parts, "placeholder, empty ("+inSync+")")
				}
			} else {
				// Ошибка запроса не глотается молча (задача 5.8): у
				// плейсхолдера запрос STANDARD обязан проходить — честная
				// причина (sharing/права/исчезновение файла) вместо
				// вводящего в заблуждение «not a placeholder».
				parts = append(parts, "placeholder state query failed ("+ierr.Error()+")")
			}
		} else {
			parts = append(parts, "not a placeholder")
		}
	}
	if state&cfapi.PlaceholderStateSyncRoot != 0 {
		parts = append(parts, "sync-root")
	}
	fmt.Printf("Path:       %s\n", path)
	fmt.Printf("Index path: %s\n", indexPath)
	fmt.Printf("Index:      %s\n", indexLine)
	fmt.Printf("State:      %s (0x%08x)\n", strings.Join(parts, ", "), state)
	return nil
}

// cloudDriveServe — единственный процесс провайдера на sync root (спека
// «Второй экземпляр»): data path поверх meta+chunk (design.md Integration
// Points), CfConnectSyncRoot с таблицей callback'ов, наполнение верхнего
// уровня и маркер готовности (задачи 2.3, 2.5 — внутри провайдера),
// CfDisconnectSyncRoot при завершении.
func cloudDriveServe(c *cli.Context) error {
	setup(c, 1)
	root := c.String("root")
	metaURI := c.Args().Get(0)
	if metaURI == "" {
		return errors.New("serve requires META-URL, e.g. grpc://host:9900?tls=1")
	}

	// Предпроверка тома до проверки регистрации и до mutex/connect:
	// cldflt.sys работает только на NTFS (proposal, таблица рисков).
	fsName, err := fileSystemOf(root)
	if err != nil {
		return fmt.Errorf("cannot determine the file system of %s: %w", root, err)
	}
	if fsName != "NTFS" {
		return fmt.Errorf("sync root %s is on %s; the Cloud Files API requires NTFS", root, fsName)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Data path: недоступный hub не завершает serve (спека «Hub
	// недоступен») — попытки с нарастающей паузой до успеха или Ctrl+C.
	stack, err := connectWithRetry(ctx, metaURI, stsNodeOptionsFrom(c))
	if err != nil {
		return err
	}
	defer stack.Close()

	volume := c.String("volume-name")
	if volume == "" {
		volume = stack.format.Name
	}

	// Индекс плейсхолдеров (inode → путь) и журнал невыполненных операций
	// backend (этап 6, design.md D9) — в каталоге состояния
	// (design.md D6, proposal D-8).
	stateDir := c.String("state-dir")
	if stateDir == "" {
		stateDir = cfprovider.DefaultStateDir(volume)
	}
	idx, err := cfprovider.NewIndex(stateDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := idx.Close(); err != nil {
			logger.Warnf("close index journal: %v", err)
		}
	}()
	journal, err := cfprovider.NewJournal(stateDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := journal.Close(); err != nil {
			logger.Warnf("close ops journal: %v", err)
		}
	}()

	// Реальный диспетчер очередей: заглушка-логгер (dispatch=nil) не
	// обслуживает FETCH_PLACEHOLDERS — перечисление каталогов детьми
	// повисло бы до таймаута фильтра. Пул гидратации — --hydration-workers
	// (этап 3, замер M2); --read-only отключает выгрузку локальных
	// изменений и распространение операций в backend (задача 5.6).
	provider := cfprovider.New(root, volume, nil, cfapi.NewDispatcher(cfapi.DefaultQueueOptions())).WithData(stack.fsys, idx).WithJournal(journal).WithFetchWorkers(c.Int("hydration-workers")).WithReadOnly(c.Bool("read-only"))
	if err := provider.Start(ctx); err != nil {
		return err
	}
	logger.Infof("serving sync root %s (volume %s); press Ctrl+C to stop", root, volume)

	// Такт 2 (design.md D6): фоновый walk hub и пул выгрузки (design.md D8,
	// задачи 5.2–5.4) — старт строго после маркера готовности (такт 1
	// заселил верхний уровень), goroutine с ctx serve (signal.NotifyContext:
	// Ctrl+C отменяет). Остановка: cancel → воркеры дорабатывают текущие
	// задания, журнал сброшен → `walk cancelled:` — provider.Stop
	// дожидается через wg (walk.go). Полнота namespace — строка
	// `walk complete:` (design.md D6.2), прогресс — `walk:`.
	poolStarted := make(chan struct{})
	go func() {
		defer close(poolStarted)
		if !provider.WaitReady(ctx) {
			return
		}
		provider.StartIngest(ctx)
		provider.StartWalk(ctx)
	}()
	defer func() {
		<-poolStarted // Stop дождался пулов; сигнальная горутина вышла
	}()

	<-ctx.Done()
	logger.Info("shutting down: disconnecting sync root and releasing the instance mutex")
	return provider.Stop()
}

// cloudDriveStack — data path провайдера (design.md Integration Points,
// gc-паттерн cmd/gc.go и STS-выбор mount): meta.NewClient → Load(true) →
// getDefaultChunkConf → NewReloadableStorage (STS — общий startNodeSTS) →
// chunk.NewCachedStore → fs.NewFileSystem. WriteJournalPath не заполняется:
// очистка staging после падения — проба U11 (design.md Open Questions,
// задача 5.5).
type cloudDriveStack struct {
	format    *meta.Format
	metaCli   meta.Meta
	blob      object.ObjectStorage
	store     chunk.ChunkStore
	fsys      *fs.FileSystem
	refresher *stsRefresher // nil без --sts-enabled
}

// buildCloudDriveStack собирает data path. Последовательность и обработка
// ошибок повторяют mount: каждый отказ закрывает уже созданные ресурсы.
func buildCloudDriveStack(metaURI string, sts *stsNodeOptions) (*cloudDriveStack, error) {
	metaConf := meta.DefaultConf()
	metaCli := meta.NewClient(metaURI, metaConf)
	format, err := metaCli.Load(true)
	if err != nil {
		_ = metaCli.Shutdown()
		return nil, fmt.Errorf("load volume format: %w", err)
	}
	chunkConf := getDefaultChunkConf(format)
	vfsConf := &vfs.Config{
		Meta: metaConf,
		// Format — стартовый снимок (актуальный отдаёт Meta.GetFormat).
		Format:   *format,
		Security: &vfs.SecurityConfig{},
		Version:  version.Version(),
		Chunk:    chunkConf,
		Pid:      os.Getpid(),
		PPid:     os.Getppid(),
		UMask:    0xFFFF,
	}
	// Состояние connectivity hub — как в mount (журнал отложенных записей
	// включается провайдером на этапах 5+).
	if gm, ok := metaCli.(interface{ HubState() meta.HubState }); ok {
		vfsConf.HubState = gm.HubState
	}

	blob, err := NewReloadableStorage(format, metaCli, nil)
	if err != nil {
		_ = metaCli.Shutdown()
		return nil, fmt.Errorf("object storage: %w", err)
	}
	logger.Infof("Data use %s", blob)

	// STS-креды — пользовательский путь (как в mount, cmd/mount.go:743-793):
	// короткоживущие креды через StsProxyService самой meta-связи, identity
	// выводится прокси из OIDC-сессии (ADR-003, решение В2). Render-node путь
	// (KeyManager + IAM-токен узла) остаётся только у render-mount: узлу
	// рендер-фермы он нужен, workstation-клиенту — нет.
	var refresher *stsRefresher
	if sts != nil {
		if !format.EncryptionEnabled {
			object.Shutdown(blob)
			_ = metaCli.Shutdown()
			return nil, errors.New("--sts-enabled requires an encrypted volume")
		}
		if sts.companyID == "" {
			object.Shutdown(blob)
			_ = metaCli.Shutdown()
			return nil, errors.New("--company-id is required with --sts-enabled")
		}
		if !strings.HasPrefix(metaURI, "grpc://") {
			object.Shutdown(blob)
			_ = metaCli.Shutdown()
			return nil, errors.New("--sts-enabled requires a meta proxy (grpc) endpoint")
		}
		stsProxy, ok := metaCli.(interface {
			StsProxyClient() pb.StsProxyServiceClient
		})
		if !ok {
			object.Shutdown(blob)
			_ = metaCli.Shutdown()
			return nil, errors.New("--sts-enabled requires a meta proxy (grpc) endpoint")
		}
		holder, ok := blob.(*storageHolder)
		if !ok {
			object.Shutdown(blob)
			_ = metaCli.Shutdown()
			return nil, errors.New("sts: storage is not reloadable")
		}
		provider := &platformSTSProvider{
			stsProxy:  stsProxy.StsProxyClient(),
			companyID: sts.companyID,
		}
		// Маргон опроса привязан к meta-связи (design configure-sts-refresh-ttl
		// D4); не-gRPC клиенты держат дефолт.
		margin := meta.DefaultSTSRefreshMargin
		if g, ok := metaCli.(interface {
			STSRefreshMargin() float64
		}); ok {
			margin = g.STSRefreshMargin()
		}
		refresher = newSTSRefresher(provider, holder, margin)
		if err := refresher.Start(context.Background()); err != nil {
			object.Shutdown(blob)
			_ = metaCli.Shutdown()
			return nil, fmt.Errorf("sts: %w", err)
		}
		// Self-provision (implement-company-dir-self-provision): платформа
		// доставляет working_dir компании в первом ответе — best-effort,
		// отказ/отсутствие только Warn (как в mount).
		ensureCompanyWorkingDir(metaCli, provider.WorkingDir())
	}

	store := chunk.NewCachedStore(blob, *chunkConf, nil)
	registerMetaMsg(metaCli, store, chunkConf)
	if err := metaCli.NewSession(true); err != nil {
		if refresher != nil {
			refresher.Stop()
		}
		object.Shutdown(blob)
		_ = metaCli.Shutdown()
		return nil, fmt.Errorf("new session: %w", err)
	}
	metaCli.OnReload(func(fmt *meta.Format) error {
		store.UpdateLimit(fmt.UploadLimit, fmt.DownloadLimit)
		return nil
	})
	fsys, err := fs.NewFileSystem(vfsConf, metaCli, store, nil)
	if err != nil {
		if refresher != nil {
			refresher.Stop()
		}
		_ = metaCli.CloseSession()
		object.Shutdown(blob)
		_ = metaCli.Shutdown()
		return nil, fmt.Errorf("create filesystem: %w", err)
	}
	logger.Infof("JuiceFS version %s", version.Version())
	return &cloudDriveStack{
		format:    format,
		metaCli:   metaCli,
		blob:      blob,
		store:     store,
		fsys:      fsys,
		refresher: refresher,
	}, nil
}

// Close освобождает ресурсы стека в обратном порядке создания: refresher до
// сессии meta — как в mount.
func (s *cloudDriveStack) Close() {
	if s == nil {
		return
	}
	if s.fsys != nil {
		if err := s.fsys.Close(); err != nil {
			logger.Warnf("close filesystem: %v", err)
		}
	}
	if s.refresher != nil {
		s.refresher.Stop()
	}
	if err := s.metaCli.CloseSession(); err != nil {
		logger.Warnf("close session: %v", err)
	}
	object.Shutdown(s.blob)
	if err := s.metaCli.Shutdown(); err != nil {
		logger.Debugf("meta shutdown: %v", err)
	}
}

// connectWithRetry собирает data path, повторяя попытки с нарастающей
// паузой (2 c → 30 c): недоступный hub не завершает serve (спека «Hub
// недоступен»), из цикла выходят только успехом или отменой ctx (Ctrl+C).
func connectWithRetry(ctx context.Context, metaURI string, sts *stsNodeOptions) (*cloudDriveStack, error) {
	const maxDelay = 30 * time.Second
	delay := 2 * time.Second
	for attempt := 1; ; attempt++ {
		stack, err := buildCloudDriveStack(metaURI, sts)
		if err == nil {
			if attempt > 1 {
				logger.Infof("volume %s connected after %d attempts", stack.format.Name, attempt)
			}
			return stack, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		logger.Warnf("connecting to the volume (attempt %d): %v; retrying in %s", attempt, err, delay)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// withCloudDriveStack — общий каркас ls/hash: разбор META-URL, сборка
// data path (одна попытка — команды короткоживущие), гарантированное
// освобождение.
func withCloudDriveStack(c *cli.Context, fn func(*cloudDriveStack) error) error {
	metaURI := c.Args().Get(0)
	if metaURI == "" {
		return errors.New("META-URL is required, e.g. grpc://host:9900?tls=1")
	}
	stack, err := buildCloudDriveStack(metaURI, stsNodeOptionsFrom(c))
	if err != nil {
		return err
	}
	defer stack.Close()
	return fn(stack)
}

// normalizeFsPath приводит путь аргумента к стилю fs: разделитель "/",
// ведущий "/", без хвостового "/" (кроме корня).
func normalizeFsPath(p string) string {
	p = strings.TrimSpace(filepath.ToSlash(p))
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		return "/"
	}
	return p
}

// cloudDriveLs — задача 2.6: печать записей каталога backend (name/size/
// type/mime) через fs.FileSystem; путь — относительно корня тома.
func cloudDriveLs(c *cli.Context) error {
	setup(c, 2)
	return withCloudDriveStack(c, func(stack *cloudDriveStack) error {
		p := normalizeFsPath(c.Args().Get(1))
		entries, err := cfprovider.NewBackendFS(stack.fsys).List(context.Background(), p)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 1, 4, 2, ' ', 0)
		fmt.Fprintf(w, "NAME\tSIZE\tTYPE\tMIME\n")
		for _, e := range entries {
			typ, mimeType := "file", "-"
			if e.IsDir {
				typ, mimeType = "dir", "-"
			} else if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(e.Name))); t != "" {
				mimeType = t
			} else {
				mimeType = "application/octet-stream"
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", e.Name, e.Size, typ, mimeType)
		}
		return w.Flush()
	})
}

// cloudDriveHash — задача 2.6: SHA-256 содержимого файла backend, читается
// порциями через fs.Open/Pread; вывод — hex (сценарий «Содержимое совпадает
// с backend» сврявается с sha256sum файла в sync root).
func cloudDriveHash(c *cli.Context) error {
	setup(c, 2)
	return withCloudDriveStack(c, func(stack *cloudDriveStack) error {
		p := normalizeFsPath(c.Args().Get(1))
		// Контекст meta — как в BackendFS.List и pkg/fs/http.go (uid/gid
		// процесса, не root): права доступа backend применяются честно.
		mctx := meta.WrapWithCancel(context.Background(), uint32(os.Getpid()), uint32(utils.GetCurrentUID()), []uint32{uint32(utils.GetCurrentGID())})
		defer mctx.Cancel()
		// MODE_MASK_R (не 0): fs.open пропускает meta-Open при нулевых флагах —
		// на зашифрованном томе без него Open-ответ не несёт FEK и Pread падает
		// EIO (живой кейс 27.11).
		f, errno := stack.fsys.Open(mctx, p, vfs.MODE_MASK_R)
		if errno != 0 {
			return fmt.Errorf("open %s: %v", p, errno)
		}
		defer func() {
			if errno := f.Close(mctx); errno != 0 {
				logger.Warnf("close %s: %v", p, errno)
			}
		}()
		digest := sha256.New()
		buf := make([]byte, 4<<20)
		var off int64
		for {
			n, err := f.Pread(mctx, buf, off)
			if n > 0 {
				digest.Write(buf[:n])
				off += int64(n)
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return fmt.Errorf("read %s: %v", p, err)
			}
			if n == 0 {
				break
			}
		}
		fmt.Printf("%x  %s\n", digest.Sum(nil), p)
		return nil
	})
}

// cloudDrivePin — задача 4.1, спека «Pin и unpin»: CfSetPinState(Pinned)
// по локальной записи sync root. Сам pin-state фильтр хранит персистентно
// (переживает перезапуск провайдера); гидрация приходит FETCH_DATA к живому
// соединению — сценарий «Pin файла и чтение без провайдера» требует
// работающий serve на момент скачивания (после kill serve pinned-файл
// читается целиком из локальных данных).
func cloudDrivePin(c *cli.Context) error {
	return cloudDriveSetPinState(c, cfapi.PinStatePinned, "pin")
}

// cloudDriveUnpin — спека «Unpin»: CfSetPinState(Unpinned); фильтр
// дешидратирует плейсхолдер — занятый объём возвращается, запись остаётся
// в каталоге. Callback NOTIFY_DEHYDRATE не зарегистрирован (design.md D5 —
// не среди 9 OS-точек), поэтому уведомление протекает без ack (optional).
func cloudDriveUnpin(c *cli.Context) error {
	return cloudDriveSetPinState(c, cfapi.PinStateUnpinned, "unpin")
}

// cloudDriveSetPinState — общий каркас pin/unpin: локальный путь под корнем
// sync root, флаг --recursive → CF_SET_PIN_FLAG_RECURSE, SetPinStateByPath.
// Выполняется в CLI-процессе (не в serve): изоляция пулов design.md D3
// не затрагивается — она ограничивает только воркеров провайдера (воркер
// гидратации не должен сам вызывать CfSetPinState/CfHydratePlaceholder).
func cloudDriveSetPinState(c *cli.Context, pinState uint32, verb string) error {
	setup(c, 1)
	path, err := cloudDriveLocalPath(c)
	if err != nil {
		return err
	}
	pinFlags := uint32(cfapi.SetPinFlagNone)
	if c.Bool("recursive") {
		pinFlags = cfapi.SetPinFlagRecurse
	}
	// Предпроверка существования: без неё CfOpenFileWithOplock даёт голый
	// HRESULT 0x80070002 без указания, что путь разрешился ВНУТРЬ другого
	// корня (--root по умолчанию %USERPROFILE%\AgioDrive, а serve работал
	// на другом корне — живой кейс 27.11).
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s %s: file does not exist under sync root %s (resolved path %s; pass --root matching the running serve)", verb, path, c.String("root"), path)
	}
	if err := cfapi.SetPinStateByPath(path, pinState, pinFlags); err != nil {
		return fmt.Errorf("%s %s: %w (the path must be a placeholder inside the sync root; hydration requires a running `clouddrive serve`)", verb, path, err)
	}
	if pinFlags != cfapi.SetPinFlagNone && pinState == cfapi.PinStatePinned {
		// Проба U5: рекурсивный pin каталога фильтр выполняет сам —
		// FETCH_DATA приходит в serve пачкой на все файлы поддерева;
		// очередь обслуживает пул гидратации (--hydration-workers).
		logger.Infof("recursive pin %s issued: hydration arrives as FETCH_DATA batches to the running serve", path)
	} else {
		logger.Infof("%s %s (pin state %d)", verb, path, pinState)
	}
	return nil
}

// cloudDriveLocalPath разрешает аргумент pin/unpin в локальный путь
// (плейсхолдер под корнем sync root): относительный — относительно --root
// (ls/hash берут путь backend-тома, pin/unpin — локальную запись),
// полный — принимается, если лежит под корнем.
func cloudDriveLocalPath(c *cli.Context) (string, error) {
	return cloudDriveResolveLocalPath(c.String("root"), c.Args().Get(0))
}

// cloudDriveResolveLocalPath разрешает путь записи sync root: относительный
// — относительно root, полный — принимается, если лежит под корнем.
func cloudDriveResolveLocalPath(root, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("PATH is required, e.g. docs/report.pdf (relative to the sync root) or the full local path")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve sync root %s: %w", root, err)
	}
	pAbs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", p, err)
	}
	// Rel сравнивает компоненты пути без учёта регистра на Windows;
	// `..` означает выход за пределы корня.
	rel, err := filepath.Rel(rootAbs, pAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s is outside the sync root %s", p, root)
	}
	return pAbs, nil
}

// currentUserSIDStr — SID текущего пользователя (строкой), элемент имени
// ключа SyncRootManager `<providerId>!<SID>!<accountId>` (design.md D4).
func currentUserSIDStr() (string, error) {
	sid, err := win.GetCurrentUserSID()
	if err != nil {
		return "", err
	}
	return sid.String(), nil
}

// syncRootRegistryPath — путь ключа Explorer для sync root пользователя.
func syncRootRegistryPath(sid string) string {
	return syncRootManagerKeyPath + `\` + cloudDriveProviderID + `!` + sid + `!` + cloudDriveAccountID
}

// writeSyncRootRegistry создаёт ключ реестра Explorer: DisplayName,
// IconResource и UserSyncRoots\<SID> = путь корня.
func writeSyncRootRegistry(root, sid string) error {
	keyPath := syncRootRegistryPath(sid)
	key, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create key HKCU\\%s: %w", keyPath, err)
	}
	defer key.Close()
	if err := key.SetStringValue("DisplayName", cloudDriveDisplayName); err != nil {
		return fmt.Errorf("set DisplayName: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	if err := key.SetExpandStringValue("IconResource", exe+",0"); err != nil {
		return fmt.Errorf("set IconResource: %w", err)
	}
	userRoots, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath+`\UserSyncRoots`, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create UserSyncRoots: %w", err)
	}
	defer userRoots.Close()
	return userRoots.SetStringValue(sid, root)
}

// deleteSyncRootRegistry удаляет ключ реестра Explorer с под ключами
// (UserSyncRoots); отсутствие ключа — успех.
func deleteSyncRootRegistry(sid string) error {
	return registryDeleteTree(registry.CURRENT_USER, syncRootRegistryPath(sid))
}

// registryDeleteTree удаляет ключ реестра рекурсивно: registry.DeleteKey
// удаляет только пустые ключи.
func registryDeleteTree(k registry.Key, path string) error {
	key, err := registry.OpenKey(k, path, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	subkeys, err := key.ReadSubKeyNames(-1)
	key.Close()
	if err != nil {
		return err
	}
	for _, sk := range subkeys {
		if err := registryDeleteTree(k, path+`\`+sk); err != nil {
			return err
		}
	}
	return registry.DeleteKey(k, path)
}

// fileSystemOf возвращает имя файловой системы тома, содержащего путь
// (GetVolumeInformation по корню тома).
func fileSystemOf(path string) (string, error) {
	vol := filepath.VolumeName(path)
	if vol == "" {
		return "", fmt.Errorf("path %s has no volume", path)
	}
	volU16, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return "", err
	}
	var fsName [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeInformation(volU16, nil, 0, nil, nil, nil, &fsName[0], uint32(len(fsName))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(fsName[:]), nil
}
