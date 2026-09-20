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

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// mockOpenClient implements pb.MetaServiceClient and records Open requests.
type mockOpenClient struct {
	pb.MetaServiceClient
	openReqs []*pb.OpenRequest
	openResp func(req *pb.OpenRequest) (*pb.OpenResponse, error)
}

func (m *mockOpenClient) Open(ctx context.Context, req *pb.OpenRequest, opts ...grpc.CallOption) (*pb.OpenResponse, error) {
	m.openReqs = append(m.openReqs, req)
	return m.openResp(req)
}

func newTestGRPCMeta(t *testing.T, client pb.MetaServiceClient, fekTTL time.Duration) *grpcMeta {
	t.Helper()
	fekCache := expirable.NewLRU[uint64, *fekEntry](100, func(_ uint64, e *fekEntry) {
		for i := range e.fek {
			e.fek[i] = 0
		}
	}, fekTTL)
	return &grpcMeta{
		client:    client,
		attrCache: expirable.NewLRU[uint64, *Attr](100, nil, time.Second),
		dirCache:  expirable.NewLRU[uint64, []*Entry](100, nil, time.Second),
		fekCache:  fekCache,
	}
}

func TestFekCache(t *testing.T) {
	testFEK := []byte("0123456789abcdef0123456789abcdef")

	t.Run("cache miss fetches FEK from server", func(t *testing.T) {
		mock := &mockOpenClient{
			openResp: func(req *pb.OpenRequest) (*pb.OpenResponse, error) {
				assert.EqualValues(t, 0, req.CachedFekVersion)
				return &pb.OpenResponse{
					Errno:      0,
					Attr:       &pb.ProtoAttr{Typ: 1},
					Fek:        testFEK,
					FekVersion: 1,
					Encrypted:  true,
				}, nil
			},
		}
		m := newTestGRPCMeta(t, mock, time.Minute)

		var attr Attr
		require.Equal(t, syscall.Errno(0), m.Open(Background(), Ino(42), 0, &attr))
		assert.Equal(t, testFEK, attr.Fek)
		assert.EqualValues(t, 1, attr.FekVersion)
		e, ok := m.fekCache.Get(42)
		require.True(t, ok)
		assert.Equal(t, testFEK, e.fek)
	})

	t.Run("cache hit sends cached_fek_version and reuses LRU FEK", func(t *testing.T) {
		mock := &mockOpenClient{
			openResp: func(req *pb.OpenRequest) (*pb.OpenResponse, error) {
				if req.CachedFekVersion == 1 {
					// Server skips KeyManager on a cache hit — no FEK in the response.
					return &pb.OpenResponse{Errno: 0, Attr: &pb.ProtoAttr{Typ: 1}, FekVersion: 1, Encrypted: true}, nil
				}
				return &pb.OpenResponse{Errno: 0, Attr: &pb.ProtoAttr{Typ: 1}, Fek: testFEK, FekVersion: 1, Encrypted: true}, nil
			},
		}
		m := newTestGRPCMeta(t, mock, time.Minute)

		var attr Attr
		require.Equal(t, syscall.Errno(0), m.Open(Background(), Ino(42), 0, &attr))
		require.Equal(t, syscall.Errno(0), m.Open(Background(), Ino(42), 0, &attr))

		require.Len(t, mock.openReqs, 2)
		assert.EqualValues(t, 0, mock.openReqs[0].CachedFekVersion)
		assert.EqualValues(t, 1, mock.openReqs[1].CachedFekVersion) // cache hit — server skips KeyManager
		assert.Equal(t, testFEK, attr.Fek)
	})

	t.Run("TTL expiry forces re-fetch", func(t *testing.T) {
		mock := &mockOpenClient{
			openResp: func(req *pb.OpenRequest) (*pb.OpenResponse, error) {
				return &pb.OpenResponse{Errno: 0, Attr: &pb.ProtoAttr{Typ: 1}, Fek: testFEK, FekVersion: 1, Encrypted: true}, nil
			},
		}
		m := newTestGRPCMeta(t, mock, 50*time.Millisecond)

		var attr Attr
		require.Equal(t, syscall.Errno(0), m.Open(Background(), Ino(42), 0, &attr))
		time.Sleep(150 * time.Millisecond) // let the entry expire

		var attr2 Attr
		require.Equal(t, syscall.Errno(0), m.Open(Background(), Ino(42), 0, &attr2))
		require.Len(t, mock.openReqs, 2)
		assert.EqualValues(t, 0, mock.openReqs[1].CachedFekVersion) // expired — full re-fetch
		assert.Equal(t, testFEK, attr2.Fek)
	})

	t.Run("eviction zeroes plaintext FEK", func(t *testing.T) {
		mock := &mockOpenClient{
			openResp: func(req *pb.OpenRequest) (*pb.OpenResponse, error) {
				return &pb.OpenResponse{Errno: 0, Attr: &pb.ProtoAttr{Typ: 1}, Fek: []byte(testFEK), FekVersion: 1, Encrypted: true}, nil
			},
		}
		m := newTestGRPCMeta(t, mock, time.Minute)
		// Shrink the cache so the next Add evicts the first entry.
		m.fekCache = expirable.NewLRU[uint64, *fekEntry](1, func(_ uint64, e *fekEntry) {
			for i := range e.fek {
				e.fek[i] = 0
			}
		}, time.Minute)

		var attr Attr
		require.Equal(t, syscall.Errno(0), m.Open(Background(), Ino(1), 0, &attr))
		firstFek := attr.Fek
		require.Equal(t, []byte(testFEK), firstFek)

		var attr2 Attr
		require.Equal(t, syscall.Errno(0), m.Open(Background(), Ino(2), 0, &attr2))
		// The first entry was evicted and its FEK zeroed in place.
		assert.Equal(t, make([]byte, len(testFEK)), firstFek)
	})

	t.Run("non-encrypted open does not touch FEK cache", func(t *testing.T) {
		mock := &mockOpenClient{
			openResp: func(req *pb.OpenRequest) (*pb.OpenResponse, error) {
				return &pb.OpenResponse{Errno: 0, Attr: &pb.ProtoAttr{Typ: 1}}, nil
			},
		}
		m := newTestGRPCMeta(t, mock, time.Minute)

		var attr Attr
		require.Equal(t, syscall.Errno(0), m.Open(Background(), Ino(42), 0, &attr))
		assert.Nil(t, attr.Fek)
		assert.Zero(t, m.fekCache.Len())
	})
}
