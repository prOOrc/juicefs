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

import (
	"time"
)

// EventType represents the type of filesystem event
type EventType string

const (
	// File operations
	FileCreated EventType = "FileCreated"
	FileDeleted EventType = "FileDeleted"
	FileMoved   EventType = "FileMoved"
	FileWritten EventType = "FileWritten"
	// Directory operations
	DirCreated EventType = "DirCreated"
	DirDeleted EventType = "DirDeleted"
)

// JuiceFsEvent represents a filesystem event to be published to the outbox
type JuiceFsEvent struct {
	// Event metadata
	Type      EventType `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Volume    string    `json:"volume"`
	// Filesystem context
	Uid    uint32 `json:"uid"`
	Gid    uint32 `json:"gid"`
	Subdir string `json:"subdir,omitempty"`
	// Inode information
	Inode  Ino    `json:"inode"`
	Parent Ino    `json:"parent"`
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	// Operation-specific fields
	OldPath string `json:"old_path,omitempty"` // For FileMoved (source path)
	Size    uint64 `json:"size,omitempty"`     // For FileWritten
	Mode    uint16 `json:"mode,omitempty"`     // For FileCreated/DirCreated
}
