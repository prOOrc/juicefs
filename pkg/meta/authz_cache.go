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
	"sync"
	"time"
)

const defaultAuthzCacheSize = 10_000

// authzCacheEntry stores a cached permission decision.
type authzCacheEntry struct {
	allowed   bool
	expiresAt time.Time
}

// cachingAuthzClient wraps an AuthzClient with a TTL-based decision cache.
// Repeated checks for the same (userID, path, perm) tuple within the TTL
// window are served from cache without hitting the network.
type cachingAuthzClient struct {
	inner   AuthzClient
	ttl     time.Duration
	maxSize int

	mu      sync.Mutex
	entries map[string]authzCacheEntry
}

// NewCachingAuthzClient wraps client with a TTL decision cache.
// If ttl <= 0, caching is disabled and inner is returned as-is.
// If maxSize <= 0, defaultAuthzCacheSize is used.
func NewCachingAuthzClient(inner AuthzClient, ttl time.Duration, maxSize int) AuthzClient {
	if ttl <= 0 {
		return inner
	}
	if maxSize <= 0 {
		maxSize = defaultAuthzCacheSize
	}
	return &cachingAuthzClient{
		inner:   inner,
		ttl:     ttl,
		maxSize: maxSize,
		entries: make(map[string]authzCacheEntry),
	}
}

func (c *cachingAuthzClient) cacheKey(userID, path string, perm AuthzPermission) string {
	return userID + "\x00" + path + "\x00" + string(rune(perm))
}

func (c *cachingAuthzClient) CheckPermission(ctx context.Context, userID, filePath string, perm AuthzPermission) (bool, error) {
	key := c.cacheKey(userID, filePath, perm)

	// Fast path: check cache
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok {
		if time.Now().Before(entry.expiresAt) {
			c.mu.Unlock()
			authzLogger.Tracef("Authz cache hit: user=%s path=%s perm=%d allowed=%v",
				userID, filePath, perm, entry.allowed)
			return entry.allowed, nil
		}
		// Expired — remove and fall through
		delete(c.entries, key)
	}
	c.mu.Unlock()

	// Slow path: call the underlying client
	allowed, err := c.inner.CheckPermission(ctx, userID, filePath, perm)
	if err != nil {
		return false, err
	}

	// Store in cache (only successful decisions; errors are not cached)
	c.mu.Lock()
	if len(c.entries) >= c.maxSize {
		c.evictExpiredLocked()
		if len(c.entries) >= c.maxSize {
			// Still full — skip caching to avoid unbounded growth
			c.mu.Unlock()
			return allowed, nil
		}
	}
	c.entries[key] = authzCacheEntry{
		allowed:   allowed,
		expiresAt: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()

	return allowed, nil
}

func (c *cachingAuthzClient) CheckBulkPermissions(ctx context.Context, userID string, paths []string, perm AuthzPermission) ([]bool, error) {
	// Try to serve all from cache first
	results := make([]bool, len(paths))
	missIdx := make([]int, 0)
	missPaths := make([]string, 0)

	now := time.Now()
	c.mu.Lock()
	for i, p := range paths {
		key := c.cacheKey(userID, p, perm)
		if entry, ok := c.entries[key]; ok && now.Before(entry.expiresAt) {
			results[i] = entry.allowed
		} else {
			if ok {
				delete(c.entries, key)
			}
			missIdx = append(missIdx, i)
			missPaths = append(missPaths, p)
		}
	}
	c.mu.Unlock()

	if len(missIdx) == 0 {
		return results, nil
	}

	// Fetch missing from network
	fetched, err := c.inner.CheckBulkPermissions(ctx, userID, missPaths, perm)
	if err != nil {
		return nil, err
	}

	// Fill results and cache
	c.mu.Lock()
	for j, idx := range missIdx {
		results[idx] = fetched[j]
		key := c.cacheKey(userID, missPaths[j], perm)
		if len(c.entries) < c.maxSize {
			c.entries[key] = authzCacheEntry{
				allowed:   fetched[j],
				expiresAt: now.Add(c.ttl),
			}
		}
	}
	c.mu.Unlock()

	return results, nil
}

func (c *cachingAuthzClient) CheckOrganizationAdmin(ctx context.Context, userID string) (bool, error) {
	// Admin status is less frequent; use a shorter-lived cache entry
	key := c.cacheKey(userID, "__admin__", AuthzPermissionAdmin)

	c.mu.Lock()
	if entry, ok := c.entries[key]; ok {
		if time.Now().Before(entry.expiresAt) {
			c.mu.Unlock()
			return entry.allowed, nil
		}
		delete(c.entries, key)
	}
	c.mu.Unlock()

	isAdmin, err := c.inner.CheckOrganizationAdmin(ctx, userID)
	if err != nil {
		return false, err
	}

	c.mu.Lock()
	if len(c.entries) < c.maxSize {
		c.entries[key] = authzCacheEntry{
			allowed:   isAdmin,
			expiresAt: time.Now().Add(c.ttl),
		}
	}
	c.mu.Unlock()

	return isAdmin, nil
}

// evictExpiredLocked removes all expired entries. Caller must hold c.mu.
func (c *cachingAuthzClient) evictExpiredLocked() {
	now := time.Now()
	for k, v := range c.entries {
		if now.After(v.expiresAt) {
			delete(c.entries, k)
		}
	}
}

// Close delegates to the underlying client.
func (c *cachingAuthzClient) Close() error {
	if closer, ok := c.inner.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}
