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

package winfsp

import (
	"testing"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/vfs"
	"github.com/juicedata/juicefs/pkg/win"
	"github.com/winfsp/cgofuse/fuse"
)

// D4: WinFsp synthesizes security descriptors from the presented uid:gid:mode.
// With --owner-override every entry must present the override identity;
// without it the legacy uid-0 → SYSTEM mapping must stay intact.
func TestAttrToStatOwnerOverride(t *testing.T) {
	attr := &meta.Attr{Typ: meta.TypeDirectory, Mode: 0755, Uid: 0, Gid: 0}

	var j juice
	j.conf = &vfs.Config{OwnerOverride: &vfs.AnonymousAccount{Uid: 197608, Gid: 197121}}
	var st fuse.Stat_t
	j.attrToStat(1, attr, &st)
	if st.Uid != 197608 || st.Gid != 197121 {
		t.Fatalf("override presentation: got uid=%d gid=%d, want 197608/197121", st.Uid, st.Gid)
	}

	// attr from vfs.GetAttr already carries the override — application must be idempotent
	overridden := j.conf.OwnerOverride
	attr.Uid = overridden.Uid
	attr.Gid = overridden.Gid
	st = fuse.Stat_t{}
	j.attrToStat(1, attr, &st)
	if st.Uid != 197608 || st.Gid != 197121 {
		t.Fatalf("idempotent override: got uid=%d gid=%d, want 197608/197121", st.Uid, st.Gid)
	}

	// without override: uid 0 → SYSTEM presentation (legacy behavior)
	j.conf = &vfs.Config{}
	attr.Uid = 0
	attr.Gid = 0
	st = fuse.Stat_t{}
	j.attrToStat(1, attr, &st)
	if st.Uid != win.SystemUIDFromFUSE {
		t.Fatalf("legacy presentation: got uid=%d, want SystemUIDFromFUSE=%d", st.Uid, win.SystemUIDFromFUSE)
	}
}
