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
	"testing"
	"time"
)

func TestFilterEvent(t *testing.T) {
	ts := time.Now()

	cases := []struct {
		name         string
		input        *JuiceFsEvent
		expectNil    bool
		expectedType EventType // only checked when expectNil is false
	}{
		{
			name: "MinIO internal path",
			input: &JuiceFsEvent{
				Type: DirCreated, Path: "/.minio.sys/config/config.json", Subdir: "test", Timestamp: ts,
			},
			expectNil: true,
		},
		{
			name: "MinIO sys internal path",
			input: &JuiceFsEvent{
				Type: FileCreated, Path: "/.sys/.minio.sys/tmp/something", Subdir: "test", Timestamp: ts,
			},
			expectNil: true,
		},
		{
			name: "Tmp dir created",
			input: &JuiceFsEvent{
				Type: DirCreated, Path: "/.sys/tmp/d09/", Subdir: "test", Timestamp: ts,
			},
			expectNil: true,
		},
		{
			name: "Tmp file created",
			input: &JuiceFsEvent{
				Type: FileCreated, Path: "/.sys/tmp/d09/550e8400-e29b-41d4-a716-446655440000", Subdir: "test", Timestamp: ts,
			},
			expectNil: true,
		},
		{
			name: "Tmp file written",
			input: &JuiceFsEvent{
				Type: FileWritten, Path: "/.sys/tmp/d09/550e8400-e29b-41d4-a716-446655440000", Size: 36, Subdir: "test", Timestamp: ts,
			},
			expectNil: true,
		},
		{
			name: "FileWritten outside tmp",
			input: &JuiceFsEvent{
				Type: FileWritten, Path: "/data/users/alice/report.pdf", Size: 1024, Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: FileWritten,
		},
		{
			name: "FileMoved from tmp — transform to FileCreated",
			input: &JuiceFsEvent{
				Type:      FileMoved,
				Path:      "/data/users/alice/document.pdf",
				OldPath:   "/.sys/tmp/d09/550e8400-e29b-41d4-a716-446655440000",
				Inode:     5001,
				Parent:    5000,
				Name:      "document.pdf",
				Mode:      438,
				Uid:       1000,
				Gid:       1000,
				Subdir:    "test",
				Timestamp: ts,
			},
			expectNil:    false,
			expectedType: FileCreated,
		},
		{
			name: "Real DirCreated",
			input: &JuiceFsEvent{
				Type: DirCreated, Path: "/data/users/alice/photos/", Inode: 100, Parent: 99, Name: "photos", Mode: 493, Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: DirCreated,
		},
		{
			name: "Real FileMoved (rename, not from tmp)",
			input: &JuiceFsEvent{
				Type: FileMoved, Path: "/data/users/alice/renamed.txt", OldPath: "/data/users/alice/original.txt", Inode: 200, Parent: 99, Name: "renamed.txt", Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: FileMoved,
		},
		{
			name: "Real FileDeleted",
			input: &JuiceFsEvent{
				Type: FileDeleted, Path: "/data/users/alice/old-file.txt", Inode: 300, Parent: 99, Name: "old-file.txt", Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: FileDeleted,
		},
		{
			name: "Real DirDeleted",
			input: &JuiceFsEvent{
				Type: DirDeleted, Path: "/data/users/alice/photos/", Inode: 400, Parent: 99, Name: "photos", Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: DirDeleted,
		},
		{
			name: "Unknown path — pass through",
			input: &JuiceFsEvent{
				Type: FileCreated, Path: "/other/path/file.txt", Inode: 500, Parent: 499, Name: "file.txt", Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: FileCreated,
		},
		{
			name: "Real DirCreated",
			input: &JuiceFsEvent{
				Type: DirCreated, Path: "/data/users/alice/photos/", Inode: 100, Parent: 99, Name: "photos", Mode: 493, Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: DirCreated,
		},
		{
			name: "Real FileMoved (rename, not from tmp)",
			input: &JuiceFsEvent{
				Type: FileMoved, Path: "/data/users/alice/renamed.txt", OldPath: "/data/users/alice/original.txt", Inode: 200, Parent: 99, Name: "renamed.txt", Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: FileMoved,
		},
		{
			name: "Real FileDeleted",
			input: &JuiceFsEvent{
				Type: FileDeleted, Path: "/data/users/alice/old-file.txt", Inode: 300, Parent: 99, Name: "old-file.txt", Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: FileDeleted,
		},
		{
			name: "Real DirDeleted",
			input: &JuiceFsEvent{
				Type: DirDeleted, Path: "/data/users/alice/photos/", Inode: 400, Parent: 99, Name: "photos", Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: DirDeleted,
		},
		{
			name: "Unknown path — pass through",
			input: &JuiceFsEvent{
				Type: FileCreated, Path: "/other/path/file.txt", Inode: 500, Parent: 499, Name: "file.txt", Subdir: "test", Timestamp: ts,
			},
			expectNil:    false,
			expectedType: FileCreated,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := FilterEvent(c.input)
			if c.expectNil {
				if result != nil {
					t.Fatalf("expected nil (SKIP), got event with type %s", result.Type)
				}
				return
			}
			if result == nil {
				t.Fatalf("expected event with type %s, got nil (SKIP)", c.expectedType)
			}
			if result.Type != c.expectedType {
				t.Fatalf("expected type %s, got %s", c.expectedType, result.Type)
			}
		})
	}
}

func TestFilterEvent_TransformPreservesFields(t *testing.T) {
	ts := time.Now()
	input := &JuiceFsEvent{
		Type:      FileMoved,
		Timestamp: ts,
		Volume:    "test-vol",
		Uid:       1000,
		Gid:       1000,
		Inode:     5001,
		Parent:    5000,
		Name:      "document.pdf",
		Path:      "/data/users/alice/document.pdf",
		OldPath:   "/.sys/tmp/d09/550e8400-e29b-41d4-a716-446655440000",
		Mode:      438,
		Subdir:    "test",
	}

	result := FilterEvent(input)
	if result == nil {
		t.Fatal("expected transformed event, got nil")
	}
	if result.Type != FileCreated {
		t.Fatalf("expected FileCreated, got %s", result.Type)
	}
	if result.Path != input.Path {
		t.Fatalf("expected path %s, got %s", input.Path, result.Path)
	}
	if result.OldPath != "" {
		t.Fatalf("expected empty old_path, got %s", result.OldPath)
	}
	if result.Inode != input.Inode {
		t.Fatalf("expected inode %d, got %d", input.Inode, result.Inode)
	}
	if result.Uid != input.Uid {
		t.Fatalf("expected uid %d, got %d", input.Uid, result.Uid)
	}
	if result.Mode != input.Mode {
		t.Fatalf("expected mode %d, got %d", input.Mode, result.Mode)
	}
	if result.Timestamp != ts {
		t.Fatalf("expected timestamp %v, got %v", ts, result.Timestamp)
	}
	if result.Volume != input.Volume {
		t.Fatalf("expected volume %s, got %s", input.Volume, result.Volume)
	}
	if result.Subdir != input.Subdir {
		t.Fatalf("expected subdir %s, got %s", input.Subdir, result.Subdir)
	}
}
