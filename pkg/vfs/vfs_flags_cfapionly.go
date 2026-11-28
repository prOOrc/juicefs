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

// Флаги-заглушки для cfapi-сборки движка (-tags cfapionly, CGO_ENABLED=0):
// vfs_windows.go исключён из сборки вместе с cgofuse/WinFsp, а платформо-
// нейтральные файлы vfs ссылаются на эти константы. Значения повторяют
// FUSE-определения cgofuse (fsop_cgo.go: O_ACCMODE = O_RDONLY|O_WRONLY|
// O_RDWR = 3; F_UNLCK = 1) — путь clouddrive их не использует в cgo-смысле.
package vfs

const O_ACCMODE = 0x3
const F_UNLCK = 0x01
