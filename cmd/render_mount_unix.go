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
	"golang.org/x/sys/unix"

	"github.com/juicedata/juicefs/pkg/utils"
)

// mlockKey pins the key in RAM so it cannot be swapped to disk (FR-RND-3).
// Best effort: some platforms (macOS, overcommit restrictions) refuse Mlock.
func mlockKey(key []byte) {
	if err := unix.Mlock(key); err != nil {
		logger.Warnf("mlock key (best effort): %s", err)
	}
}

// renderPreMountSetup applies the process tuning that launchMount performs in
// the normal mount flow. render-mount runs foreground-only (no daemon stages),
// so it must do this itself before serving.
func renderPreMountSetup() {
	increaseRlimit()
	utils.AdjustOOMKiller(-1000)
	utils.SetIOFlusher()
}
