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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestCachingAuthzClient_ZeroTTL_Disabled(t *testing.T) {
	mockClient := new(mockAuthzClient)

	// TTL=0 should return the inner client as-is (no wrapping)
	result := NewCachingAuthzClient(mockClient, 0, 0)
	assert.Same(t, mockClient, result)

	// Negative TTL also disables
	result = NewCachingAuthzClient(mockClient, -time.Second, 0)
	assert.Same(t, mockClient, result)
}

func TestCachingAuthzClient_CachesResult(t *testing.T) {
	mockClient := new(mockAuthzClient)
	cached := NewCachingAuthzClient(mockClient, time.Hour, 0)

	mockClient.On("CheckPermission", mock.Anything, "user1", "/path/", AuthzPermissionView).
		Return(true, nil).Once()

	ctx := context.Background()

	// First call — hits the network
	allowed, err := cached.CheckPermission(ctx, "user1", "/path/", AuthzPermissionView)
	assert.NoError(t, err)
	assert.True(t, allowed)

	// Second call — served from cache (mock expects only 1 call total)
	allowed, err = cached.CheckPermission(ctx, "user1", "/path/", AuthzPermissionView)
	assert.NoError(t, err)
	assert.True(t, allowed)

	mockClient.AssertExpectations(t)
}

func TestCachingAuthzClient_DifferentUsers(t *testing.T) {
	mockClient := new(mockAuthzClient)
	cached := NewCachingAuthzClient(mockClient, time.Hour, 0)

	mockClient.On("CheckPermission", mock.Anything, "user1", "/path/", AuthzPermissionView).
		Return(true, nil).Once()
	mockClient.On("CheckPermission", mock.Anything, "user2", "/path/", AuthzPermissionView).
		Return(false, nil).Once()

	ctx := context.Background()

	allowed1, _ := cached.CheckPermission(ctx, "user1", "/path/", AuthzPermissionView)
	allowed2, _ := cached.CheckPermission(ctx, "user2", "/path/", AuthzPermissionView)

	assert.True(t, allowed1)
	assert.False(t, allowed2)
	mockClient.AssertExpectations(t)
}

func TestCachingAuthzClient_DifferentPermissions(t *testing.T) {
	mockClient := new(mockAuthzClient)
	cached := NewCachingAuthzClient(mockClient, time.Hour, 0)

	mockClient.On("CheckPermission", mock.Anything, "user1", "/path/", AuthzPermissionView).
		Return(true, nil).Once()
	mockClient.On("CheckPermission", mock.Anything, "user1", "/path/", AuthzPermissionWrite).
		Return(false, nil).Once()

	ctx := context.Background()

	viewAllowed, _ := cached.CheckPermission(ctx, "user1", "/path/", AuthzPermissionView)
	writeAllowed, _ := cached.CheckPermission(ctx, "user1", "/path/", AuthzPermissionWrite)

	assert.True(t, viewAllowed)
	assert.False(t, writeAllowed)
	mockClient.AssertExpectations(t)
}

func TestCachingAuthzClient_TTLExpiry(t *testing.T) {
	mockClient := new(mockAuthzClient)
	// Very short TTL — 1ms
	cached := NewCachingAuthzClient(mockClient, time.Millisecond, 0)

	mockClient.On("CheckPermission", mock.Anything, "user1", "/path/", AuthzPermissionView).
		Return(true, nil).Times(2)

	ctx := context.Background()

	// First call
	_, _ = cached.CheckPermission(ctx, "user1", "/path/", AuthzPermissionView)

	// Wait for TTL to expire
	time.Sleep(5 * time.Millisecond)

	// Second call — should hit network again
	allowed, err := cached.CheckPermission(ctx, "user1", "/path/", AuthzPermissionView)
	assert.NoError(t, err)
	assert.True(t, allowed)

	mockClient.AssertExpectations(t)
}

func TestCachingAuthzClient_ErrorNotCached(t *testing.T) {
	mockClient := new(mockAuthzClient)
	cached := NewCachingAuthzClient(mockClient, time.Hour, 0)

	testErr := errors.New("service unavailable")
	mockClient.On("CheckPermission", mock.Anything, "user1", "/path/", AuthzPermissionView).
		Return(false, testErr).Times(2)

	ctx := context.Background()

	// First call — error
	_, err := cached.CheckPermission(ctx, "user1", "/path/", AuthzPermissionView)
	assert.Error(t, err)

	// Second call — should hit network again (error not cached)
	_, err = cached.CheckPermission(ctx, "user1", "/path/", AuthzPermissionView)
	assert.Error(t, err)

	mockClient.AssertExpectations(t)
}

func TestCachingAuthzClient_BulkPermissions(t *testing.T) {
	mockClient := new(mockAuthzClient)
	cached := NewCachingAuthzClient(mockClient, time.Hour, 0)

	paths := []string{"/a/", "/b/", "/c/"}
	expected := []bool{true, false, true}

	mockClient.On("CheckBulkPermissions", mock.Anything, "user1", paths, AuthzPermissionView).
		Return(expected, nil).Once()

	ctx := context.Background()

	// First call — hits network
	results, err := cached.CheckBulkPermissions(ctx, "user1", paths, AuthzPermissionView)
	assert.NoError(t, err)
	assert.Equal(t, expected, results)

	// Second call — all from cache
	results, err = cached.CheckBulkPermissions(ctx, "user1", paths, AuthzPermissionView)
	assert.NoError(t, err)
	assert.Equal(t, expected, results)

	mockClient.AssertExpectations(t)
}

func TestCachingAuthzClient_BulkPartialCache(t *testing.T) {
	mockClient := new(mockAuthzClient)
	cached := NewCachingAuthzClient(mockClient, time.Hour, 0)

	ctx := context.Background()

	// Pre-cache /a/
	mockClient.On("CheckPermission", mock.Anything, "user1", "/a/", AuthzPermissionView).
		Return(true, nil).Once()
	_, _ = cached.CheckPermission(ctx, "user1", "/a/", AuthzPermissionView)

	// Bulk request: /a/ is cached, /b/ and /c/ need network
	mockClient.On("CheckBulkPermissions", mock.Anything, "user1", []string{"/b/", "/c/"}, AuthzPermissionView).
		Return([]bool{false, true}, nil).Once()

	results, err := cached.CheckBulkPermissions(ctx, "user1", []string{"/a/", "/b/", "/c/"}, AuthzPermissionView)
	assert.NoError(t, err)
	assert.Equal(t, []bool{true, false, true}, results)

	mockClient.AssertExpectations(t)
}

func TestCachingAuthzClient_OrgAdmin(t *testing.T) {
	mockClient := new(mockAuthzClient)
	cached := NewCachingAuthzClient(mockClient, time.Hour, 0)

	mockClient.On("CheckOrganizationAdmin", mock.Anything, "admin1").
		Return(true, nil).Once()

	ctx := context.Background()

	isAdmin, err := cached.CheckOrganizationAdmin(ctx, "admin1")
	assert.NoError(t, err)
	assert.True(t, isAdmin)

	// Second call — from cache
	isAdmin, err = cached.CheckOrganizationAdmin(ctx, "admin1")
	assert.NoError(t, err)
	assert.True(t, isAdmin)

	mockClient.AssertExpectations(t)
}

func TestCachingAuthzClient_MaxSizeEviction(t *testing.T) {
	mockClient := new(mockAuthzClient)
	// Max 2 entries
	cached := NewCachingAuthzClient(mockClient, time.Hour, 2)

	ctx := context.Background()

	// Fill cache to capacity
	for i, p := range []string{"/a/", "/b/"} {
		mockClient.On("CheckPermission", mock.Anything, "user1", p, AuthzPermissionView).
			Return(i == 0, nil).Once()
		_, _ = cached.CheckPermission(ctx, "user1", p, AuthzPermissionView)
	}

	// Third entry — cache is full, no expired entries to evict.
	// Should still work (just not cached).
	mockClient.On("CheckPermission", mock.Anything, "user1", "/c/", AuthzPermissionView).
		Return(true, nil).Times(2)

	allowed, err := cached.CheckPermission(ctx, "user1", "/c/", AuthzPermissionView)
	assert.NoError(t, err)
	assert.True(t, allowed)

	// Call again — still not cached (cache full), hits network
	allowed, err = cached.CheckPermission(ctx, "user1", "/c/", AuthzPermissionView)
	assert.NoError(t, err)
	assert.True(t, allowed)

	mockClient.AssertExpectations(t)
}
