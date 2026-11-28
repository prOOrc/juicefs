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

// Классификация занятых имён по идентичности (fix-cfapi-followups A1):
// identity-first — решающий признак FileIdentity STANDARD-записи, а не
// атрибутный флаг (урок f78e14cd: фильтр чистит спец-атрибуты у in-sync
// записей — U20). Идентичность провайдера — inode записи backend, 8 байт
// LE, записанная createEntry (design.md D6); FileId — платформенный
// идентификатор и для сверки не используется.
package cfprovider

// identityKind — вид занятого локального имени по STANDARD-запросу
// идентичности.
type identityKind int

const (
	identityOwn     identityKind = iota // наш плейсхолдер: FileIdentity == inode
	identityForeign                     // чужая идентичность или длина ≠ 8
	identityPlain                       // не плейсхолдер (обычная запись пользователя)
	identityUnknown                     // STANDARD-запрос не дал определённого ответа
)

// String — причина классификации для строки `walk classify:` (Debug).
func (k identityKind) String() string {
	switch k {
	case identityOwn:
		return "own"
	case identityForeign:
		return "foreign"
	case identityPlain:
		return "plain"
	default:
		return "unknown"
	}
}

// classifyIdentity — вид занятого имени через STANDARD-запрос идентичности
// (store.StandardInfoOf). «Не плейсхолдер» — plain; ошибка запроса —
// unknown (консервативно конфликт: без createEntry, без idx.Put, без
// дегидратации; повтор на следующем walk — design.md A1.5). Идентичность —
// FileIdentity хвоста STANDARD-ответа: равенство inode 8B LE — own, чужая
// идентичность или длина ≠ 8 — foreign.
func classifyIdentity(store placeholderStore, localPath string, inode uint64) identityKind {
	info, isPlaceholder, err := store.StandardInfoOf(localPath)
	if err != nil {
		return identityUnknown
	}
	if !isPlaceholder || info == nil {
		return identityPlain
	}
	identity := info.FileIdentityBytes()
	if len(identity) != 8 || info.InodeIdentity() != inode {
		return identityForeign
	}
	return identityOwn
}
