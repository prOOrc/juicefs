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
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeHeartbeatClient is a pb.MetaServiceClient stub for heartbeat tests:
// FlushSession returns the configured error or response.
type fakeHeartbeatClient struct {
	pb.MetaServiceClient
	mu   sync.Mutex
	err  error
	resp *pb.FlushSessionResponse
}

func (c *fakeHeartbeatClient) set(err error, resp *pb.FlushSessionResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
	c.resp = resp
}

func (c *fakeHeartbeatClient) FlushSession(ctx context.Context, req *pb.FlushSessionRequest, opts ...grpc.CallOption) (*pb.FlushSessionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if c.resp != nil {
		return c.resp, nil
	}
	return &pb.FlushSessionResponse{}, nil
}

// TestHubStateMachine verifies the hub connectivity state machine (task 6.2,
// NFR-OFF-1..4): online → offline-connected on failure, back online on
// recovery, and disconnected with key wipe once the offline window elapses.
func TestHubStateMachine(t *testing.T) {
	m := &grpcMeta{
		fekCache:       expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		offlineTimeout: 15 * time.Minute,
	}
	fek := bytes.Repeat([]byte{0xAB}, 32)
	m.fekCache.Add(1, &fekEntry{fek: fek, version: 1})
	var wiped int32
	m.SetOnWipe(func() { atomic.StoreInt32(&wiped, 1) })

	require.Equal(t, HubOnline, m.HubState())

	// failure → offline-connected; keys stay cached (reads from cache work)
	m.markHubOffline()
	require.Equal(t, HubOfflineConnected, m.HubState())
	require.Equal(t, 1, m.fekCache.Len())

	// recovery before the timeout → online
	m.markHubOnline()
	require.Equal(t, HubOnline, m.HubState())

	// failure + window exceeded → disconnected: keys wiped, onWipe called
	m.markHubOffline()
	m.hubMu.Lock()
	m.lastFail = time.Now().Add(-m.offlineTimeout - time.Second) // simulate a long outage
	m.hubMu.Unlock()
	m.checkOfflineTimeout()

	require.Equal(t, HubDisconnected, m.HubState())
	require.Zero(t, m.fekCache.Len(), "FEK cache must be empty after disconnect")
	require.Equal(t, make([]byte, 32), fek, "FEK backing array must be zeroed on disconnect")
	require.Equal(t, int32(1), atomic.LoadInt32(&wiped), "onWipe callback must fire exactly once")

	// disconnect is one-shot and terminal (design 6.6)
	m.checkOfflineTimeout()
	m.markHubOnline() // a late recovery cannot resurrect a disconnected client
	require.Equal(t, HubDisconnected, m.HubState())
	require.Equal(t, int32(1), atomic.LoadInt32(&wiped))
}

// TestHubStateMachine_ReconnectHook verifies that the reconnect callback fires
// exactly once per offline-connected → online transition (task 6.3), driven by
// a successful heartbeat beat, and not on repeated successes or after a
// terminal disconnect.
func TestHubStateMachine_ReconnectHook(t *testing.T) {
	fc := &fakeHeartbeatClient{}
	m := &grpcMeta{
		fekCache:       expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		offlineTimeout: 15 * time.Minute,
		client:         fc,
	}
	var reconnects int32
	m.SetOnReconnect(func() { atomic.AddInt32(&reconnects, 1) })

	// already online: a successful beat is not a transition, no callback
	m.doHeartbeat()
	require.Equal(t, int32(0), atomic.LoadInt32(&reconnects))

	// failing beat → offline-connected; recovery beat → online: fires once
	fc.set(errors.New("hub down"), nil)
	m.doHeartbeat()
	require.Equal(t, HubOfflineConnected, m.HubState())
	fc.set(nil, &pb.FlushSessionResponse{})
	m.doHeartbeat()
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&reconnects) == 1
	}, time.Second, time.Millisecond)

	// repeated success: no additional callback
	m.doHeartbeat()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), atomic.LoadInt32(&reconnects))

	// after a terminal disconnect the hook must not fire on a late recovery
	fc.set(errors.New("hub down"), nil)
	m.doHeartbeat()
	m.hubMu.Lock()
	m.lastFail = time.Now().Add(-m.offlineTimeout - time.Second)
	m.hubMu.Unlock()
	m.checkOfflineTimeout()
	require.Equal(t, HubDisconnected, m.HubState())
	fc.set(nil, &pb.FlushSessionResponse{})
	m.doHeartbeat()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), atomic.LoadInt32(&reconnects))
}

// TestHubReconnect_GenerationBump_NoReplay verifies design A2 (task 6.8): on a
// reconnect beat that also carries a permission generation increase, the key
// wipe wins — onWipe fires and onReconnect does NOT (replaying under destroyed
// FEKs would commit undecryptable data; the journal survives for the next
// mount). A later offline window closed by a beat with an unchanged
// generation then fires the reconnect hook.
func TestHubReconnect_GenerationBump_NoReplay(t *testing.T) {
	fc := &fakeHeartbeatClient{}
	m := &grpcMeta{
		fekCache:       expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		offlineTimeout: 15 * time.Minute,
		client:         fc,
		lastPermGen:    -1, // as set by newGRPCMeta before the first heartbeat
	}
	fek := bytes.Repeat([]byte{0xAB}, 32)
	m.fekCache.Add(1, &fekEntry{fek: fek, version: 1})
	var wiped, reconnects int32
	m.SetOnWipe(func() { atomic.AddInt32(&wiped, 1) })
	m.SetOnReconnect(func() { atomic.AddInt32(&reconnects, 1) })

	// First beat: baseline generation, no wipe, no reconnect (already online).
	fc.set(nil, &pb.FlushSessionResponse{PermissionGeneration: 5})
	m.doHeartbeat()
	require.EqualValues(t, 5, m.lastPermGen)
	require.Equal(t, 1, m.fekCache.Len())
	require.Equal(t, int32(0), atomic.LoadInt32(&wiped))
	require.Equal(t, int32(0), atomic.LoadInt32(&reconnects))

	// Force offline with a failing beat.
	fc.set(errors.New("hub down"), nil)
	m.doHeartbeat()
	require.Equal(t, HubOfflineConnected, m.HubState())

	// Reconnect beat carrying an INCREASED generation: the wipe fires and the
	// replay must NOT.
	fc.set(nil, &pb.FlushSessionResponse{PermissionGeneration: 6})
	m.doHeartbeat()
	require.Equal(t, HubOnline, m.HubState())
	require.Zero(t, m.fekCache.Len(), "FEK cache must be wiped on the generation bump")
	require.Equal(t, make([]byte, 32), fek, "FEK backing array must be zeroed")
	require.Equal(t, int32(1), atomic.LoadInt32(&wiped), "onWipe must fire")
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(0), atomic.LoadInt32(&reconnects), "no replay on a beat that wiped the keys")

	// Next offline window closed by a beat with the SAME generation: the
	// reconnect hook fires (nothing was wiped this time).
	fc.set(errors.New("hub down"), nil)
	m.doHeartbeat()
	require.Equal(t, HubOfflineConnected, m.HubState())
	fc.set(nil, &pb.FlushSessionResponse{PermissionGeneration: 6})
	m.doHeartbeat()
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&reconnects) == 1
	}, time.Second, time.Millisecond)
	require.Equal(t, int32(1), atomic.LoadInt32(&wiped), "no second wipe")
}

// TestHubStateMachine_WindowFromFirstFailure verifies that repeated failures do
// not extend the offline window: it is measured from the first failed RPC.
func TestHubStateMachine_WindowFromFirstFailure(t *testing.T) {
	m := &grpcMeta{
		fekCache:       expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		offlineTimeout: 15 * time.Minute,
	}

	m.markHubOffline()
	m.hubMu.Lock()
	firstFail := m.lastFail
	m.hubMu.Unlock()
	require.False(t, firstFail.IsZero())

	// repeated failures must not move lastFail
	m.markHubOffline()
	m.markHubOffline()
	m.hubMu.Lock()
	require.Equal(t, firstFail, m.lastFail)
	m.hubMu.Unlock()
}

// TestOfflineTimeout_Disconnected verifies NFR-OFF-4 with a real (short)
// timeout: while the window is open the client stays offline-connected with
// keys intact; once it elapses since the first failed RPC the client
// disconnects (terminal) and wipes all cached keys.
func TestOfflineTimeout_Disconnected(t *testing.T) {
	m := &grpcMeta{
		fekCache:       expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		offlineTimeout: 150 * time.Millisecond,
	}
	fek := bytes.Repeat([]byte{0xCD}, 32)
	m.fekCache.Add(7, &fekEntry{fek: fek, version: 1})
	var wiped int32
	m.SetOnWipe(func() { atomic.StoreInt32(&wiped, 1) })

	m.markHubOffline()
	require.Equal(t, HubOfflineConnected, m.HubState())
	require.Equal(t, 1, m.fekCache.Len(), "keys stay cached while offline-connected")

	// within the window: still offline-connected
	time.Sleep(50 * time.Millisecond)
	m.checkOfflineTimeout()
	require.Equal(t, HubOfflineConnected, m.HubState())
	require.Equal(t, 1, m.fekCache.Len())

	// after the window: disconnected + wiped (terminal)
	time.Sleep(200 * time.Millisecond)
	m.checkOfflineTimeout()
	require.Equal(t, HubDisconnected, m.HubState())
	require.Zero(t, m.fekCache.Len(), "FEK cache must be empty after disconnect")
	require.Equal(t, make([]byte, 32), fek, "FEK backing array must be zeroed on disconnect")
	require.Equal(t, int32(1), atomic.LoadInt32(&wiped), "onWipe callback must fire exactly once")

	// terminal: a later recovery does not resurrect the session
	m.markHubOnline()
	require.Equal(t, HubDisconnected, m.HubState())
}

// TestOpenFailClosedOffline verifies NFR-OFF-2: a new Open fails closed with
// EIO while the hub is unreachable, before any RPC is attempted (m.client is
// nil here — without the fast-fail this call would panic).
func TestOpenFailClosedOffline(t *testing.T) {
	m := &grpcMeta{
		fekCache: expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
	}
	m.markHubOffline()
	require.Equal(t, HubOfflineConnected, m.HubState())
	require.Equal(t, syscall.EIO, m.Open(Background(), 1, 0, &Attr{}))
}

// TestHeartbeat_GenerationChange_WipesKeys verifies task 7.3 (FR-REV-2/7): the
// heartbeat's permission generation is a monotonic platform counter — the first
// beat initializes the baseline without wiping (a user with historical role
// changes must keep freshly issued keys), an equal or lower value (including 0,
// the KeyManager-unavailable sentinel) is a no-op, and an increase wipes all
// cached FEKs and fires the onWipe hook (VFS.InvalidateAllKeys).
func TestHeartbeat_GenerationChange_WipesKeys(t *testing.T) {
	m := &grpcMeta{
		fekCache:    expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		lastPermGen: -1, // as set by newGRPCMeta before the first heartbeat
	}
	fek := bytes.Repeat([]byte{0xEF}, 32)
	m.fekCache.Add(1, &fekEntry{fek: fek, version: 1})
	var wiped int32
	m.SetOnWipe(func() { atomic.AddInt32(&wiped, 1) })

	// first beat: baseline initialization, no wipe (generation may already be >0
	// from role changes that predate this mount)
	m.applyPermissionGeneration(5)
	require.EqualValues(t, 5, m.lastPermGen)
	require.Equal(t, 1, m.fekCache.Len(), "first beat must not wipe")
	require.Equal(t, int32(0), atomic.LoadInt32(&wiped))

	// unchanged generation: no-op
	m.applyPermissionGeneration(5)
	require.Equal(t, 1, m.fekCache.Len())
	require.Equal(t, int32(0), atomic.LoadInt32(&wiped))

	// KeyManager unavailable (proxy reports 0): must not wipe authorized users
	m.applyPermissionGeneration(0)
	require.Equal(t, 1, m.fekCache.Len(), "a 0 generation is a no-op, not a revocation")
	require.Equal(t, int32(0), atomic.LoadInt32(&wiped))

	// role change on the platform (INCR): increase → wipe + InvalidateAllKeys
	m.applyPermissionGeneration(6)
	require.EqualValues(t, 6, m.lastPermGen)
	require.Zero(t, m.fekCache.Len(), "FEK cache must be empty after a generation increase")
	require.Equal(t, make([]byte, 32), fek, "FEK backing array must be zeroed")
	require.Equal(t, int32(1), atomic.LoadInt32(&wiped), "onWipe (InvalidateAllKeys) must fire")

	// the wipe is not sticky: a later equal generation stays quiet
	m.applyPermissionGeneration(6)
	require.Equal(t, int32(1), atomic.LoadInt32(&wiped))
}
