//go:build windows && cfapionly
// +build windows,cfapionly

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

// Заглушки FUSE/WinFsp-маунта для cfapi-сборки движка (-tags cfapionly,
// CGO_ENABLED=0): mount_windows.go исключён из сборки (тянет pkg/winfsp →
// cgofuse), а mount.go и main.go ссылаются на его функции. Движок juicefs-
// cfapi обслуживает только команды clouddrive — mount в этой сборке
// функционально недоступен (заглушки возвращают ошибку, не паникуя, —
// команда остаётся в --help как в полной сборке).
package cmd

import (
	"errors"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/vfs"
	"github.com/urfave/cli/v2"
)

var errNotInCfapiBuild = errors.New("mount is not supported in the cfapi engine build (use the full juicefs binary)")

func mountFlags() []cli.Flag {
	return []cli.Flag{}
}

func makeDaemon(c *cli.Context, conf *vfs.Config) error {
	return errNotInCfapiBuild
}

func makeDaemonForSvc(c *cli.Context, m meta.Meta, metaUrl, listenAddr string) error {
	return errNotInCfapiBuild
}

func getDaemonStage() int {
	return 0
}

func mountMain(v *vfs.VFS, c *cli.Context) {
}

func checkMountpoint(name, mp, logPath string, background bool) {}

func prepareMp(mp string) {}

func setFuseOption(c *cli.Context, format *meta.Format, vfsConf *vfs.Config) {}

func launchMount(c *cli.Context, mp string, conf *vfs.Config) error {
	return errNotInCfapiBuild
}

func installHandler(m meta.Meta, mp string, v *vfs.VFS, blob object.ObjectStorage) {}

func tryToInstallMountExec() error { return nil }

func updateFstab(c *cli.Context) error { return nil }
