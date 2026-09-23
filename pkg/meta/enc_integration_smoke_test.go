//go:build encintegration

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
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// TestEncIntegrationEnv (stage 9, task 9.3): smoke check that the compose suite
// services are reachable at REDIS_ADDR / S3_ENDPOINT before the real tests run.
// Only built with -tags=encintegration (make test.enc.integration).
func TestEncIntegrationEnv(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	require.NotEmpty(t, addr, "REDIS_ADDR must be set by the compose suite")
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	require.NoError(t, rdb.Ping(Background()).Err(), "Redis must answer at "+addr)
	_ = rdb.Close()

	endpoint := os.Getenv("S3_ENDPOINT")
	require.NotEmpty(t, endpoint, "S3_ENDPOINT must be set by the compose suite")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + endpoint + "/minio/health/live")
	require.NoError(t, err, "MinIO must answer at "+endpoint)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "MinIO health check")
}
