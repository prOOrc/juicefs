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

// Тесты identity-first классификации занятых имён (fix-cfapi-followups A1,
// задачи 1.3): таблица видов identity на подменном store без cldapi.dll.

package cfprovider

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/cfapi"
)

// TestClassifyIdentity — таблица видов занятого имени (design.md A1.3):
// own (FileIdentity == inode 8B LE), plain (не плейсхолдер), foreign (чужой
// inode, длина ≠ 8, нулевая длина), unknown (ошибка STANDARD-запроса).
func TestClassifyIdentity(t *testing.T) {
	const ino = uint64(0x1122334455667788)
	path := filepath.Join(`R:\root`, "entry.bin")

	cases := []struct {
		name  string
		store func() placeholderStore
		want  identityKind
	}{
		{
			name: "own",
			store: func() placeholderStore {
				return &fakeStore{standard: map[string]*cfapi.CFPlaceholderStandardInfo{
					path: fakeStdInfoWithIdentity(ino),
				}}
			},
			want: identityOwn,
		},
		{
			name: "own after convert",
			store: func() placeholderStore {
				// Convert запоминает identity — модель фильтра отдаёт её в
				// STANDARD-запросе (наш плейсхолдер после createEntry).
				s := &fakeStore{}
				if err := s.Convert(path, false, mustIdentityLE(ino), cfapi.ConvertFlagMarkInSync); err != nil {
					t.Fatalf("convert: %v", err)
				}
				return s
			},
			want: identityOwn,
		},
		{
			name: "plain",
			store: func() placeholderStore {
				return &fakeStore{localReal: map[string]bool{path: true}}
			},
			want: identityPlain,
		},
		{
			name: "foreign other inode",
			store: func() placeholderStore {
				return &fakeStore{standard: map[string]*cfapi.CFPlaceholderStandardInfo{
					path: fakeStdInfoWithIdentity(ino + 1),
				}}
			},
			want: identityForeign,
		},
		{
			name: "foreign short identity",
			store: func() placeholderStore {
				info := fakeStdInfoWithIdentity(ino)
				info.FileIdentityLength = 4 // чужая запись с укороченной identity
				return &fakeStore{standard: map[string]*cfapi.CFPlaceholderStandardInfo{path: info}}
			},
			want: identityForeign,
		},
		{
			name: "foreign zero length",
			store: func() placeholderStore {
				return &fakeStore{standard: map[string]*cfapi.CFPlaceholderStandardInfo{
					path: {},
				}}
			},
			want: identityForeign,
		},
		{
			name: "unknown standard error",
			store: func() placeholderStore {
				return &fakeStore{standardOfErr: map[string]error{path: errFake("fake: sharing violation")}}
			},
			want: identityUnknown,
		},
		{
			name: "unknown open failure",
			store: func() placeholderStore {
				return &fakeStore{}
			},
			want: identityUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyIdentity(tc.store(), path, ino); got != tc.want {
				t.Errorf("classifyIdentity = %v, ожидается %v", got, tc.want)
			}
		})
	}
}

// mustIdentityLE — identity записи backend: inode LE 8 байт (сериализация
// makeCreateInfo).
func mustIdentityLE(ino uint64) []byte {
	b := make([]byte, 8)
	for i := 0; i < 8; i++ {
		b[i] = byte(ino >> (8 * i))
	}
	return b
}

// TestClassifyOccupiedIdentityFirst — исходы классификации занятого имени по
// идентичности (fix-cfapi-followups A1, задача 1.4; U20-регрессия): своя
// идентичность усыновляет (дегидратация — только при положительном
// атрибутном флаге), plain/foreign/ошибка STANDARD — конфликт без мутаций.
func TestClassifyOccupiedIdentityFirst(t *testing.T) {
	const ino = uint64(300)
	rel := "/u20.bin"
	entry := BackendEntry{Name: "u20.bin", Inode: ino, Size: 5, MtimeSec: 1}

	cases := []struct {
		name          string
		store         func(localPath string) placeholderStore
		wantInIndex   bool
		wantAdopted   int
		wantConflicts int
		wantUpdates   int // дегидратация при усыновлении — только при положительном атрибутном флаге
	}{
		{
			// U20: атрибуты очищены фильтром (StateOf «не плейсхолдер»), но
			// STANDARD-запрос подтверждает свою идентичность — усыновление
			// БЕЗ дегидратации (спека «Усыновление синхронизированного
			// плейсхолдера»).
			name: "u20 adopts without dehydrate",
			store: func(localPath string) placeholderStore {
				return &fakeStore{
					localClean: map[string]bool{localPath: true},
					standard: map[string]*cfapi.CFPlaceholderStandardInfo{
						localPath: fakeStdInfoWithIdentity(ino),
					},
				}
			},
			wantInIndex: true,
			wantAdopted: 1,
			wantUpdates: 0,
		},
		{
			// Гидратированный свой плейсхолдер (атрибутный флаг положителен,
			// OnDiskDataSize > 0) — усыновление с нормализацией: StatOf
			// подтверждает равные версии (свежесть equal — дегидратация
			// допустима, fix-cfapi-followups A1/A2).
			name: "hydrated own adopts with dehydrate",
			store: func(localPath string) placeholderStore {
				return &fakeStore{
					local: map[string]bool{localPath: true},
					standard: map[string]*cfapi.CFPlaceholderStandardInfo{
						localPath: ownStdInfo(ino, 5),
					},
					stats: map[string]fakeStat{
						localPath: {size: 5, mtime: time.Unix(1, 0)},
					},
				}
			},
			wantInIndex: true,
			wantAdopted: 1,
			wantUpdates: 1,
		},
		{
			name: "plain conflicts without mutations",
			store: func(localPath string) placeholderStore {
				return &fakeStore{localReal: map[string]bool{localPath: true}}
			},
			wantConflicts: 1,
		},
		{
			name: "foreign conflicts without mutations",
			store: func(localPath string) placeholderStore {
				return &fakeStore{
					local: map[string]bool{localPath: true},
					standard: map[string]*cfapi.CFPlaceholderStandardInfo{
						localPath: fakeStdInfoWithIdentity(ino + 1),
					},
				}
			},
			wantConflicts: 1,
		},
		{
			name: "standard error conflicts without mutations",
			store: func(localPath string) placeholderStore {
				return &fakeStore{
					local:         map[string]bool{localPath: true},
					standardOfErr: map[string]error{localPath: errFake("fake: sharing violation")},
				}
			},
			wantConflicts: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			localPath := filepath.Join(root, "u20.bin")
			p, idx := newTestProvider(t, root, &fakeBackend{}, tc.store(localPath))
			res := &dirPopulation{dir: "/"}
			examples := []string{}

			classifyOccupied(p, res, entry, rel, localPath, true, &examples)

			_, inIndex := idx.Path(ino)
			if inIndex != tc.wantInIndex {
				t.Errorf("запись в индексе = %v, ожидается %v", inIndex, tc.wantInIndex)
			}
			if res.adopted != tc.wantAdopted {
				t.Errorf("adopted = %d, ожидается %d", res.adopted, tc.wantAdopted)
			}
			if res.skipped != tc.wantAdopted {
				t.Errorf("skipped = %d, ожидается %d (усыновлённые считаются пропущенными, конфликты — нет)", res.skipped, tc.wantAdopted)
			}
			if res.conflicts != tc.wantConflicts {
				t.Errorf("conflicts = %d, ожидается %d", res.conflicts, tc.wantConflicts)
			}
			if fs, ok := p.store.(interface{ updateCalls() []updateCall }); ok {
				if n := len(fs.updateCalls()); n != tc.wantUpdates {
					t.Errorf("Update вызовов = %d, ожидается %d", n, tc.wantUpdates)
				}
			}
			if tc.wantConflicts > 0 {
				if fs, ok := p.store.(interface{ convertCalls() []convertCall }); ok {
					if n := len(fs.convertCalls()); n != 0 {
						t.Errorf("конфликт сконвертирован: %d вызовов", n)
					}
				}
				if len(res.conflictExamples) == 0 {
					t.Error("конфликт без примера имени для Warn каталога")
				}
			}
		})
	}
}

// TestClassifyOccupiedKnownNoStandardQuery — known-ветка (inode известен
// индексу): skip БЕЗ STANDARD-запроса идентичности (ноль новых syscalls на
// тёплом пути, fix-cfapi-followups A1.1).
func TestClassifyOccupiedKnownNoStandardQuery(t *testing.T) {
	const ino = uint64(301)
	root := t.TempDir()
	localPath := filepath.Join(root, "known.bin")
	rel := "/known.bin"
	// Ошибка STANDARD была бы, классификация на неё выйти не должна.
	store := &fakeStore{
		local:         map[string]bool{localPath: true},
		standardOfErr: map[string]error{localPath: errFake("fake: must not be queried")},
	}
	p, idx := newTestProvider(t, root, &fakeBackend{}, store)
	if err := idx.Put(ino, rel); err != nil {
		t.Fatalf("idx.Put: %v", err)
	}
	res := &dirPopulation{dir: "/"}
	examples := []string{}

	classifyOccupied(p, res, BackendEntry{Name: "known.bin", Inode: ino, IsDir: true}, rel, localPath, true, &examples)

	if got, ok := idx.Path(ino); !ok || got != rel {
		t.Errorf("idx.Path(%d) = (%q, %v), ожидается %q", ino, got, ok, rel)
	}
	if res.skipped != 1 || res.conflicts != 0 {
		t.Errorf("skipped=%d conflicts=%d, ожидается skip без конфликта", res.skipped, res.conflicts)
	}
	if n := store.standardOfCalls; n != 0 {
		t.Errorf("STANDARD-запросов = %d, ожидается 0 (known-ветка без syscalls)", n)
	}
}

// TestClassifyOccupiedAbsentCreates — имя исчезло между перечислением и
// проверкой: при allowCreate — createEntry (запись создана, в индексе);
// без allowCreate (EEXIST-переклассификация) — конфликт без циклов.
func TestClassifyOccupiedAbsentCreates(t *testing.T) {
	const ino = uint64(302)
	root := t.TempDir()
	localPath := filepath.Join(root, "absent.bin")
	entry := BackendEntry{Name: "absent.bin", Inode: ino, Size: 2, MtimeSec: 1}

	p, idx := newTestProvider(t, root, &fakeBackend{}, &fakeStore{})
	res := &dirPopulation{dir: "/"}
	examples := []string{}
	classifyOccupied(p, res, entry, "/absent.bin", localPath, true, &examples)

	if got, ok := idx.Path(ino); !ok || got != "/absent.bin" {
		t.Errorf("idx.Path(%d) = (%q, %v), ожидается /absent.bin", ino, got, ok)
	}
	if res.createdFiles != 1 || res.skipped != 0 || res.conflicts != 0 {
		t.Errorf("createdFiles=%d skipped=%d conflicts=%d, ожидается создание", res.createdFiles, res.skipped, res.conflicts)
	}

	// allowCreate=false: имя занято (EEXIST), состояние уже пусто — двойная
	// гонка, конфликт без повторного создания (чистый индекс: известный inode
	// ушёл бы в known-ветку).
	p2, _ := newTestProvider(t, root, &fakeBackend{}, &fakeStore{})
	res2 := &dirPopulation{dir: "/"}
	examples2 := []string{}
	classifyOccupied(p2, res2, BackendEntry{Name: "absent.bin", Inode: ino, Size: 2, MtimeSec: 1}, "/absent.bin", localPath, false, &examples2)
	if res2.conflicts != 1 {
		t.Errorf("повторная классификация: conflicts = %d, ожидается 1 (двойная гонка)", res2.conflicts)
	}
}

// TestBuildPopulatedDir — путь FETCH-наполнения использует ту же
// identity-классификацию, что и walk (fix-cfapi-followups A1.4): свой
// плейсхолдер без повторного создания (existing/infos по includeExisting),
// plain/foreign — конфликт без записи в infos.
func TestBuildPopulatedDir(t *testing.T) {
	const ino = uint64(310)
	entry := BackendEntry{Name: "fetch.bin", Inode: ino, Size: 4, MtimeSec: 2}

	// Случаи с общим store-фикстурой: путь локальной записи — <root>/fetch.bin.
	t.Run("own existing create path", func(t *testing.T) {
		root := t.TempDir()
		local := filepath.Join(root, "fetch.bin")
		store := &fakeStore{
			local:    map[string]bool{local: true},
			standard: map[string]*cfapi.CFPlaceholderStandardInfo{local: fakeStdInfoWithIdentity(ino)},
		}
		p, _ := newTestProvider(t, root, &fakeBackend{}, store)
		out, err := buildPopulatedDir(context.Background(), p, "/", []BackendEntry{entry}, false)
		if err != nil {
			t.Fatalf("buildPopulatedDir: %v", err)
		}
		if len(out.existing) != 1 || len(out.infos) != 0 || out.conflicts != 0 {
			t.Errorf("existing=%d infos=%d conflicts=%d, ожидается existing=1 без конфликтов (усыновление create-пути)", len(out.existing), len(out.infos), out.conflicts)
		}
	})
	t.Run("own in batch answer", func(t *testing.T) {
		root := t.TempDir()
		local := filepath.Join(root, "fetch.bin")
		store := &fakeStore{
			local:    map[string]bool{local: true},
			standard: map[string]*cfapi.CFPlaceholderStandardInfo{local: fakeStdInfoWithIdentity(ino)},
		}
		p, _ := newTestProvider(t, root, &fakeBackend{}, store)
		out, err := buildPopulatedDir(context.Background(), p, "/", []BackendEntry{entry}, true)
		if err != nil {
			t.Fatalf("buildPopulatedDir: %v", err)
		}
		if len(out.infos) != 1 || out.conflicts != 0 {
			t.Errorf("infos=%d conflicts=%d, ожидается infos=1 (batch complete несёт и существующие)", len(out.infos), out.conflicts)
		}
	})
	t.Run("plain conflicts", func(t *testing.T) {
		root := t.TempDir()
		local := filepath.Join(root, "fetch.bin")
		store := &fakeStore{localReal: map[string]bool{local: true}}
		p, _ := newTestProvider(t, root, &fakeBackend{}, store)
		out, err := buildPopulatedDir(context.Background(), p, "/", []BackendEntry{entry}, true)
		if err != nil {
			t.Fatalf("buildPopulatedDir: %v", err)
		}
		if out.conflicts != 1 || len(out.infos) != 0 || len(out.existing) != 0 {
			t.Errorf("conflicts=%d infos=%d existing=%d, ожидается конфликт без передачи записи", out.conflicts, len(out.infos), len(out.existing))
		}
	})
	t.Run("foreign conflicts", func(t *testing.T) {
		root := t.TempDir()
		local := filepath.Join(root, "fetch.bin")
		store := &fakeStore{
			local:    map[string]bool{local: true},
			standard: map[string]*cfapi.CFPlaceholderStandardInfo{local: fakeStdInfoWithIdentity(ino + 5)},
		}
		p, _ := newTestProvider(t, root, &fakeBackend{}, store)
		out, err := buildPopulatedDir(context.Background(), p, "/", []BackendEntry{entry}, true)
		if err != nil {
			t.Fatalf("buildPopulatedDir: %v", err)
		}
		if out.conflicts != 1 || len(out.infos) != 0 {
			t.Errorf("conflicts=%d infos=%d, ожидается конфликт чужой идентичности", out.conflicts, len(out.infos))
		}
	})
	t.Run("known inode no standard query", func(t *testing.T) {
		// Известный inode — шорткат перед classifyIdentity (fix-cfapi-
		// followups A1, спека «При известном inode SHALL NOT выполняться
		// STANDARD-запрос идентичности»): запись ведётся как existing
		// без STANDARD; мина standardOfErr уронила бы классификацию.
		root := t.TempDir()
		local := filepath.Join(root, "fetch.bin")
		store := &fakeStore{
			local:         map[string]bool{local: true},
			standardOfErr: map[string]error{local: errFake("fake: must not be queried")},
		}
		p, idx := newTestProvider(t, root, &fakeBackend{}, store)
		if err := idx.Put(ino, "/fetch.bin"); err != nil {
			t.Fatalf("idx.Put: %v", err)
		}

		out, err := buildPopulatedDir(context.Background(), p, "/", []BackendEntry{entry}, false)
		if err != nil {
			t.Fatalf("buildPopulatedDir: %v", err)
		}
		if len(out.existing) != 1 || out.conflicts != 0 || len(out.infos) != 0 {
			t.Errorf("existing=%d infos=%d conflicts=%d, ожидается existing=1 без STANDARD (как в walk-ветке known)", len(out.existing), len(out.infos), out.conflicts)
		}
		if n := store.standardOfCalls; n != 0 {
			t.Errorf("FETCH-путь при известном inode сделал STANDARD-запросов: %d, ожидается 0", n)
		}
	})
	t.Run("known inode renamed updates index", func(t *testing.T) {
		// Путь в индексе разошёлся (переименование): idx.Put(newPath) и
		// дальше как existing — без STANDARD и без конфликта.
		root := t.TempDir()
		local := filepath.Join(root, "fetch.bin")
		store := &fakeStore{
			local:         map[string]bool{local: true},
			standardOfErr: map[string]error{local: errFake("fake: must not be queried")},
		}
		p, idx := newTestProvider(t, root, &fakeBackend{}, store)
		if err := idx.Put(ino, "/fetch.old.bin"); err != nil {
			t.Fatalf("idx.Put: %v", err)
		}

		out, err := buildPopulatedDir(context.Background(), p, "/", []BackendEntry{entry}, false)
		if err != nil {
			t.Fatalf("buildPopulatedDir: %v", err)
		}
		if len(out.existing) != 1 || out.conflicts != 0 {
			t.Errorf("existing=%d conflicts=%d, ожидается existing=1 (переименованная запись)", len(out.existing), out.conflicts)
		}
		if got, ok := idx.Path(ino); !ok || got != "/fetch.bin" {
			t.Errorf("idx.Path(%d) = (%q, %v), ожидается /fetch.bin (индекс обновлён)", ino, got, ok)
		}
		if n := store.standardOfCalls; n != 0 {
			t.Errorf("переименованная известная запись сделала STANDARD-запросов: %d, ожидается 0", n)
		}
	})
}
