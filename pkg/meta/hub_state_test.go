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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/stretchr/testify/require"
)

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
// exactly once per offline-connected → online transition (task 6.3), and not
// on repeated successes or after a terminal disconnect.
func TestHubStateMachine_ReconnectHook(t *testing.T) {
	m := &grpcMeta{
		fekCache:       expirable.NewLRU[uint64, *fekEntry](10, nil, time.Minute),
		offlineTimeout: 15 * time.Minute,
	}
	var reconnects int32
	m.SetOnReconnect(func() { atomic.AddInt32(&reconnects, 1) })

	m.markHubOnline() // already online: no transition, no callback
	require.Equal(t, int32(0), atomic.LoadInt32(&reconnects))

	m.markHubOffline()
	m.markHubOnline() // offline-connected → online: fires once (in a goroutine)
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&reconnects) == 1
	}, time.Second, time.Millisecond)

	m.markHubOnline() // repeated success: no additional callback
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), atomic.LoadInt32(&reconnects))

	// after a terminal disconnect the hook must not fire on a late recovery
	m.markHubOffline()
	m.hubMu.Lock()
	m.lastFail = time.Now().Add(-m.offlineTimeout - time.Second)
	m.hubMu.Unlock()
	m.checkOfflineTimeout()
	require.Equal(t, HubDisconnected, m.HubState())
	m.markHubOnline()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), atomic.LoadInt32(&reconnects))
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
