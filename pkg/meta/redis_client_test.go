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
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestCreateRedisClient(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
		check   func(t *testing.T, c redis.UniversalClient)
	}{
		{
			name: "single node",
			url:  "redis://host:6379",
			check: func(t *testing.T, c redis.UniversalClient) {
				client, ok := c.(*redis.Client)
				if !ok {
					t.Fatalf("expected *redis.Client, got %T", c)
				}
				if got := client.Options().Addr; got != "host:6379" {
					t.Fatalf("expected Addr host:6379, got %q", got)
				}
			},
		},
		{
			name: "sentinel",
			url:  "master,s1:26379,s2:26379",
			check: func(t *testing.T, c redis.UniversalClient) {
				// go-redis v9 returns *Client for both single and sentinel modes;
				// the sentinel client is identified by its Addr marker.
				client, ok := c.(*redis.Client)
				if !ok {
					t.Fatalf("expected *redis.Client, got %T", c)
				}
				if got := client.Options().Addr; got != "FailoverClient" {
					t.Fatalf("expected sentinel Addr FailoverClient, got %q", got)
				}
			},
		},
		{
			name: "cluster",
			url:  "redis://h1:7000,h2:7000",
			check: func(t *testing.T, c redis.UniversalClient) {
				if _, ok := c.(*redis.ClusterClient); !ok {
					t.Fatalf("expected *redis.ClusterClient, got %T", c)
				}
			},
		},
		{
			name: "client-cache params are consumed",
			url:  "redis://host:6379?client-cache=true&client-cache-size=100&client-cache-expire=30s&client-cache-preload=10",
			check: func(t *testing.T, c redis.UniversalClient) {
				client, ok := c.(*redis.Client)
				if !ok {
					t.Fatalf("expected *redis.Client, got %T", c)
				}
				if got := client.Options().Addr; got != "host:6379" {
					t.Fatalf("expected Addr host:6379, got %q", got)
				}
			},
		},
		{
			name:    "invalid tls cert file",
			url:     "rediss://host:6379?tls-cert-file=/nonexistent",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := CreateRedisClient(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, c)
			}
			_ = c.Close()
		})
	}
}
