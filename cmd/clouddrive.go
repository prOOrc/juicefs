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
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/urfave/cli/v2"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/juicedata/juicefs/pkg/cfapi"
	"github.com/juicedata/juicefs/pkg/cfprovider"
	"github.com/juicedata/juicefs/pkg/version"
	"github.com/juicedata/juicefs/pkg/win"
)

// Константы регистрации AgioDrive (design.md D4). id провайдера в имени
// ключа Explorer — строка, аккаунт — Agio. GUID провайдера — постоянный
// идентификатор CfAPI-регистрации. Ветка реестра — HKCU (проба U1; HKLM —
// только если проба покажет необходимость). pin/unpin/ls/hash добавляются
// задачами этапов 2+ (tasks.md 2.6, 4.1, 5.7).
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
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "root",
				Value: defaultCloudDriveRoot(),
				Usage: "sync root directory (must be on an NTFS volume)",
			},
			&cli.StringFlag{
				Name:  "volume-name",
				Usage: "volume name for readiness logging",
			},
		},
		Description: `
Manage a Windows Cloud Files API (CfAPI) sync root backed by a JuiceFS volume.

Examples:
# Register %USERPROFILE%\AgioDrive as the sync root (must be on an NTFS volume)
$ juicefs clouddrive register

# Show registration state of the sync root
$ juicefs clouddrive status

# Serve the sync root (single instance, Ctrl+C to stop)
$ juicefs clouddrive serve

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
				Name:   "status",
				Usage:  "Show sync root registration state",
				Action: cloudDriveStatus,
			},
			{
				Name:   "serve",
				Usage:  "Serve the sync root as the Cloud Files API provider",
				Action: cloudDriveServe,
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
	// Политики прототипа (design.md D4): гидрация on-demand (Progressive +
	// streaming), наполнение on-demand (Partial) — дети каталогов по первому
	// обращению (этап 2), атрибутный in-sync-трекинг выключен.
	policies := cfapi.NewSyncPolicies(
		cfapi.CFHydrationPolicy{Primary: cfapi.HydrationPolicyProgressive, Modifier: cfapi.HydrationPolicyModifierStreamingAllowed},
		cfapi.CFPopulationPolicy{Primary: cfapi.PopulationPolicyPartial, Modifier: cfapi.PopulationPolicyModifierNone},
		cfapi.InSyncPolicyTrackNone,
		cfapi.HardlinkPolicyNone,
		cfapi.PlaceholderManagementPolicyDefault,
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

	registered := "no"
	keyPath := ""
	if sid, err := currentUserSIDStr(); err == nil {
		keyPath = syncRootRegistryPath(sid)
		if k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE); err == nil {
			k.Close()
			registered = "yes"
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
	fmt.Printf("Directory:  %s\n", directory)
	return nil
}

// cloudDriveServe — спека «Второй экземпляр»: единственный процесс
// провайдера на sync root (именованный mutex), CfConnectSyncRoot с таблицей
// callback'ов, CfDisconnectSyncRoot при завершении. Шаг 1 MVP-лестницы:
// наполнение плейсхолдерами и маркер готовности — этап 2 (задачи 2.3, 2.5).
func cloudDriveServe(c *cli.Context) error {
	setup(c, 0)
	root := c.String("root")
	volume := c.String("volume-name")
	if volume == "" {
		volume = filepath.Base(root)
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

	provider := cfprovider.New(root, volume, nil, nil)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := provider.Start(ctx); err != nil {
		return err
	}
	logger.Infof("serving sync root %s (volume %s); press Ctrl+C to stop", root, volume)
	<-ctx.Done()
	logger.Info("shutting down: disconnecting sync root and releasing the instance mutex")
	return provider.Stop()
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
