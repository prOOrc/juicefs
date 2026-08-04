/*
 * JuiceFS, Copyright 2024 Juicedata, Inc.
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/metadata"
)

// mockTokenProvider simulates a token provider for testing withAuth.
type mockTokenProvider struct {
	token     string
	callCount int32
	delay     func() // optional delay to test singleflight coalescing
}

func (m *mockTokenProvider) BearerToken(ctx context.Context) string {
	atomic.AddInt32(&m.callCount, 1)
	if m.delay != nil {
		m.delay()
	}
	return m.token
}

func (m *mockTokenProvider) Stop() {}

// slowTokenProvider blocks for a fixed duration to let goroutines pile up in singleflight.Do.
type slowTokenProvider struct {
	token     string
	callCount int32
	duration  time.Duration
}

func (s *slowTokenProvider) BearerToken(ctx context.Context) string {
	atomic.AddInt32(&s.callCount, 1)
	time.Sleep(s.duration) // block long enough for other goroutines to reach Do
	return s.token
}

func (s *slowTokenProvider) Stop() {}

func TestWithAuthWithoutOIDC(t *testing.T) {
	m := &grpcMeta{sid: 42}
	ctx := m.withAuth(context.Background())

	md, ok := metadata.FromOutgoingContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, []string{"42"}, md["x-session-id"])
	assert.Empty(t, md["authorization"])
}

func TestWithAuthWithOIDC(t *testing.T) {
	mock := &mockTokenProvider{token: "Bearer abc123"}
	m := &grpcMeta{sid: 7, tokenManager: mock}
	ctx := m.withAuth(context.Background())

	md, ok := metadata.FromOutgoingContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, []string{"7"}, md["x-session-id"])
	assert.Equal(t, []string{"Bearer abc123"}, md["authorization"])
	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.callCount))
}

func TestWithAuthEmptyToken(t *testing.T) {
	mock := &mockTokenProvider{token: ""}
	m := &grpcMeta{sid: 1, tokenManager: mock}
	ctx := m.withAuth(context.Background())

	md, ok := metadata.FromOutgoingContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, []string{"1"}, md["x-session-id"])
	assert.Empty(t, md["authorization"])
	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.callCount))
}

func TestWithAuthNilContext(t *testing.T) {
	m := &grpcMeta{sid: 5}
	ctx := m.withAuth(nil)

	md, ok := metadata.FromOutgoingContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, []string{"5"}, md["x-session-id"])
}

func TestWithAuthSingleflightCoalescing(t *testing.T) {
	mock := &slowTokenProvider{token: "Bearer slow", duration: 50 * time.Millisecond}
	m := &grpcMeta{sid: 10, tokenManager: mock}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = m.withAuth(context.Background())
		}()
	}
	wg.Wait()

	// All 10 goroutines should have been coalesced into a single BearerToken call.
	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.callCount),
		"singleflight should coalesce all concurrent calls into one")
}
