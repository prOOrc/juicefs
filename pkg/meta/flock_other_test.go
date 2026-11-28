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

package meta

import "syscall"

// flockRDLCK/flockWRLCK/flockUNLCK — нетипизированные алиасы syscall.F_* для
// тестов локов (base_test.go, subdir_test.go); Windows-значения см. в
// flock_windows_test.go.
const (
	flockRDLCK = syscall.F_RDLCK
	flockWRLCK = syscall.F_WRLCK
	flockUNLCK = syscall.F_UNLCK
)
