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
	"syscall"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/prometheus/client_golang/prometheus"
)

// fekCacheHits/fekCacheMisses count client FEK resolutions served from the
// local LRU vs fetched from the server (KeyManager) on Open (task 10.4).
var (
	fekCacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "fek_cache_hits",
		Help: "number of FEK resolutions served from the local LRU cache",
	})
	fekCacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "fek_cache_misses",
		Help: "number of FEK resolutions that had to fetch the key (KeyManager or local unwrap)",
	})
)

// openOnce performs a single Open RPC with the given cached FEK version.
func (m *grpcMeta) openOnce(ctx Context, inode Ino, flags uint32, cachedFekVersion uint32) (*pb.OpenResponse, syscall.Errno) {
	c := m.grpcContext(ctx)
	req := &pb.OpenRequest{
		Ctx:              c,
		Inode:            uint64(inode),
		Flags:            flags,
		CachedFekVersion: cachedFekVersion,
	}
	resp, err := m.client.Open(m.withSessionID(ctx), req)
	if err != nil {
		// Map through the shared status table (implement-grpc-tls): the FEK
		// refusal over a plaintext connection arrives as Unauthenticated and
		// surfaces to the caller as EACCES.
		return nil, grpcStatusToErrno(err)
	}
	if resp.GetErrno() != 0 {
		return nil, syscall.Errno(resp.GetErrno())
	}
	return resp, 0
}

// fekForOpen returns the plaintext FEK for an encrypted open response: from the
// response itself (cache miss), or from the local LRU (cache hit).
func (m *grpcMeta) fekForOpen(resp *pb.OpenResponse, inode Ino, cachedVer uint32) ([]byte, uint32) {
	if len(resp.GetFek()) > 0 {
		fekCacheMisses.Inc() // fetched through the server (KeyManager)
		return resp.GetFek(), uint32(resp.GetFekVersion())
	}
	if cachedVer != 0 && cachedVer == uint32(resp.GetFekVersion()) {
		if e, ok := m.fekCache.Get(uint64(inode)); ok {
			fekCacheHits.Inc()
			return e.fek, e.version
		}
	}
	return nil, 0
}
