/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
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

import "strings"

// FilterEvent applies filtering and transformation rules to an outbox event.
// Returns nil if the event should be skipped (not published to Kafka).
// May return a transformed event (e.g., FileMoved from tmp → FileCreated).
//
// Rules are applied in order — first match is the final decision:
//  1. SKIP: MinIO internal paths (/.minio.sys/, /.sys/.minio.sys/)
//  2. SKIP: tmp operations except FileMoved (/.sys/tmp/)
//  3. TRANSFORM: FileMoved from tmp → FileCreated (MinIO S3 PUT pattern)
//  4. PASS: everything else
func FilterEvent(event *JuiceFsEvent) *JuiceFsEvent {
	// --- Temporary rules (MinIO Gateway) — remove when migrating to Meta Proxy ---

	if isMinioInternal(event.Path) {
		return nil
	}

	if isTmpOperation(event.Path, event.Type) {
		return nil
	}

	// --- Temporary rule (MinIO Gateway) ---

	if isMovedFromTmp(event.OldPath, event.Type) {
		return &JuiceFsEvent{
			Type:      FileCreated,
			Timestamp: event.Timestamp,
			Volume:    event.Volume,
			Uid:       event.Uid,
			Gid:       event.Gid,
			Subdir:    event.Subdir,
			Inode:     event.Inode,
			Parent:    event.Parent,
			Name:      event.Name,
			Path:      event.Path,
			Mode:      event.Mode,
		}
	}

	// --- Default: pass through ---
	return event
}

// isMinioInternal returns true for MinIO's internal filesystem paths.
// Temporary — remove when migrating away from MinIO Gateway.
func isMinioInternal(path string) bool {
	return strings.HasPrefix(path, "/.minio.sys/") ||
		strings.HasPrefix(path, "/.sys/.minio.sys/")
}

// isTmpOperation returns true for non-FileMoved operations inside MinIO's tmp directory.
// MinIO writes to tmp files then renames; Create/Write/Delete in tmp are intermediate noise.
// Temporary — remove when migrating away from MinIO Gateway.
func isTmpOperation(path string, eventType EventType) bool {
	return strings.HasPrefix(path, "/.sys/tmp/") && eventType != FileMoved
}

// isMovedFromTmp returns true for FileMoved events where the source is MinIO's tmp directory.
// This represents the actual user S3 PUT — MinIO writes to tmp then renames to final path.
// Temporary — remove when migrating away from MinIO Gateway.
func isMovedFromTmp(oldPath string, eventType EventType) bool {
	return eventType == FileMoved && strings.HasPrefix(oldPath, "/.sys/tmp/")
}
