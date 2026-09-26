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
	"path/filepath"
	"strings"
	"testing"

	"github.com/juicedata/juicefs/pkg/meta"
)

// TestEnableEncryption_NonRedis_Refused (task 8.7): enable-encryption refuses
// non-Redis metadata engines before doing any work — the volume format is left
// untouched (EncryptionEnabled false, KEKVersion 0). Uses a local sqlite3
// volume, so no Redis server is required.
func TestEnableEncryption_NonRedis_Refused(t *testing.T) {
	metaURL := "sqlite3://" + filepath.Join(t.TempDir(), "test.db")
	conf := meta.DefaultConf()
	conf.NoBGJob = true
	m := meta.NewClient(metaURL, conf)
	format := &meta.Format{
		Name: "enc-guard-test", UUID: "33333333-4444-5555-6666-777777777777",
		Storage: "mem", Bucket: "test", BlockSize: 4096, Compression: "none",
		TrashDays: 1, MetaVersion: meta.MaxVersion,
	}
	if err := m.Init(format, true); err != nil {
		t.Fatalf("init: %s", err)
	}

	err := Main([]string{"", "enable-encryption", metaURL, "--company-id", "company-1", "--keymanager-service", "127.0.0.1:1"})
	if err == nil {
		t.Fatal("enable-encryption succeeded on a sqlite3 volume, want refusal")
	}
	if !strings.Contains(err.Error(), "Redis") {
		t.Fatalf("error %q does not mention the Redis requirement", err)
	}

	format2, err := m.Load(true)
	if err != nil {
		t.Fatalf("load: %s", err)
	}
	if format2.EncryptionEnabled {
		t.Fatal("EncryptionEnabled must stay false after a refused run")
	}
	if format2.KEKVersion != 0 {
		t.Fatalf("KEKVersion = %d, want 0 after a refused run", format2.KEKVersion)
	}
}

// TestMetaEngineScheme: the scheme extraction mirrors meta.NewClient — an
// address without "://" defaults to redis, otherwise the scheme is the part
// before "://" (query parameters do not affect it).
func TestMetaEngineScheme(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"redis://127.0.0.1:6379/1", "redis"},
		{"rediss://h", "rediss"},
		{"10.0.0.1:6379", "redis"},
		{"sqlite3:///tmp/f.db", "sqlite3"},
		{"mysql://h/db", "mysql"},
		{"postgres://h/x", "postgres"},
		{"tikv://h:2379/x", "tikv"},
		{"etcd://h:2379", "etcd"},
		{"badger:///tmp/b", "badger"},
		{"redis://127.0.0.1:6379/1?prefix=x", "redis"},
	}
	for _, tt := range tests {
		if got := metaEngineScheme(tt.url); got != tt.want {
			t.Errorf("metaEngineScheme(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// TestRequireRedisEngine: exactly the schemes meta.NewClient maps to redisMeta
// (redis, rediss, unix — pkg/meta/redis.go) pass the guard; everything else is
// refused with an error naming the scheme.
func TestRequireRedisEngine(t *testing.T) {
	allowed := []string{
		"redis://127.0.0.1:6379/1",
		"rediss://h",
		"unix:///tmp/redis.sock",
		"10.0.0.1:6379",
		"redis://127.0.0.1:6379/1?prefix=x",
	}
	for _, url := range allowed {
		if err := requireRedisEngine(url); err != nil {
			t.Errorf("requireRedisEngine(%q) = %v, want nil", url, err)
		}
	}

	refused := []string{
		"sqlite3:///tmp/f.db",
		"mysql://h/db",
		"postgres://h/x",
		"tikv://h:2379/x",
		"etcd://h:2379",
		"badger:///tmp/b",
	}
	for _, url := range refused {
		err := requireRedisEngine(url)
		if err == nil {
			t.Errorf("requireRedisEngine(%q) = nil, want error", url)
			continue
		}
		if !strings.Contains(err.Error(), "Redis") || !strings.Contains(err.Error(), metaEngineScheme(url)) {
			t.Errorf("error %q does not name the refused scheme", err)
		}
	}
}
