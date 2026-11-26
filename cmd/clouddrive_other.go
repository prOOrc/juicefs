//go:build !windows
// +build !windows

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
	"errors"

	"github.com/urfave/cli/v2"
)

// cmdCloudDrive — заглушка для не-Windows платформ: Windows Cloud Files API
// (cldapi.dll) существует только на Windows. Реализация — clouddrive.go
// (//go:build windows).
func cmdCloudDrive() *cli.Command {
	return &cli.Command{
		Name:     "clouddrive",
		Category: "SERVICE",
		Usage:    "Manage the AgioDrive Windows Cloud Files sync root",
		Hidden:   true,
		Action: func(c *cli.Context) error {
			return errors.New("clouddrive is only supported on Windows")
		},
	}
}
