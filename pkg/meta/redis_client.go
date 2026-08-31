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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// NOTE: parseRedisClientOptions is a deliberate duplicate of the Redis option
// parsing in newRedisMeta (redis.go). It is kept in this separate file so that
// CreateRedisClient can be added without modifying the upstream redis.go, which
// avoids merge conflicts when rebasing onto upstream JuiceFS. Keep the two in
// sync when rebasing.

// redisClientOptions holds the parsed options for CreateRedisClient.
type redisClientOptions struct {
	Options         *redis.Options
	MinRetryBackoff time.Duration
	MaxRetryBackoff time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
}

// parseRedisClientOptions parses a Redis URL into configured redis.Options with
// the same defaults as the Redis meta engine (timeouts, retries, TLS, password
// from environment variables).
func parseRedisClientOptions(redisURL string) (*redisClientOptions, error) {
	if !strings.Contains(redisURL, "://") {
		redisURL = "redis://" + redisURL
	}
	u, err := url.Parse(redisURL)
	if err != nil {
		return nil, fmt.Errorf("url parse %s: %s", redisURL, err)
	}
	values := u.Query()
	query := queryMap{&values}
	minRetryBackoff := query.duration("min-retry-backoff", "min_retry_backoff", time.Millisecond*20)
	maxRetryBackoff := query.duration("max-retry-backoff", "max_retry_backoff", time.Second*10)
	readTimeout := query.duration("read-timeout", "read_timeout", time.Second*30)
	writeTimeout := query.duration("write-timeout", "write_timeout", time.Second*5)
	skipVerify := query.pop("insecure-skip-verify")
	certFile := query.pop("tls-cert-file")
	keyFile := query.pop("tls-key-file")
	caCertFile := query.pop("tls-ca-cert-file")
	tlsServerName := query.pop("tls-server-name")
	// Client-side caching options: parsed and consumed for parity with
	// newRedisMeta (so the URL is cleaned identically and the duplicate stays
	// in sync on rebase). The raw client returned by CreateRedisClient does not
	// use the JuiceFS metadata cache (redisCache), which is a redisMeta-specific
	// layer tied to the meta prefix.
	query.pop("client-cache")
	query.getInt("client-cache-size", "client_cache_size", 12800)
	query.duration("client-cache-expire", "client_cache_expire", time.Minute)
	query.getInt("client-cache-preload", "client_cache_preload", 0)
	u.RawQuery = values.Encode()

	opt, err := redis.ParseURL(u.String())
	if err != nil {
		return nil, fmt.Errorf("redis parse %s: %s", redisURL, err)
	}
	if opt.TLSConfig != nil {
		opt.TLSConfig.ServerName = tlsServerName
		opt.TLSConfig.InsecureSkipVerify = skipVerify != ""
		if certFile != "" {
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, fmt.Errorf("get certificate error certFile:%s keyFile:%s error:%s", certFile, keyFile, err)
			}
			opt.TLSConfig.Certificates = []tls.Certificate{cert}
		}
		if caCertFile != "" {
			caCert, err := os.ReadFile(caCertFile)
			if err != nil {
				return nil, fmt.Errorf("read ca cert file error path:%s error:%s", caCertFile, err)
			}
			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)
			opt.TLSConfig.RootCAs = caCertPool
		}
	}
	if opt.Password == "" {
		opt.Password = os.Getenv("REDIS_PASSWORD")
	}
	if opt.Password == "" {
		opt.Password = os.Getenv("META_PASSWORD")
	}
	if opt.Password == "" {
		if passwordFile := os.Getenv("META_PASSWORD_FILE"); passwordFile != "" {
			password, err := readPasswordFromFile(passwordFile)
			if err != nil {
				logger.Errorf("%v", err)
			} else {
				opt.Password = password
			}
		}
	}
	opt.MaxRetries = -1 // Redis uses -1 to disable retries
	opt.MinRetryBackoff = minRetryBackoff
	opt.MaxRetryBackoff = maxRetryBackoff
	opt.ReadTimeout = readTimeout
	opt.WriteTimeout = writeTimeout
	opt.MaintNotificationsConfig = &maintnotifications.Config{Mode: maintnotifications.ModeDisabled}

	return &redisClientOptions{
		Options:         opt,
		MinRetryBackoff: minRetryBackoff,
		MaxRetryBackoff: maxRetryBackoff,
		ReadTimeout:     readTimeout,
		WriteTimeout:    writeTimeout,
	}, nil
}

// CreateRedisClient creates a Redis client from a URL with the same connection
// semantics as the Redis meta engine: it detects single-node, sentinel (failover)
// and cluster modes from the URL form and applies the same timeouts, retries, TLS
// and password-from-environment handling. It is safe for use as a library: errors
// are returned instead of terminating the process.
func CreateRedisClient(redisURL string) (redis.UniversalClient, error) {
	result, err := parseRedisClientOptions(redisURL)
	if err != nil {
		return nil, err
	}
	opt := result.Options

	if !strings.Contains(redisURL, "://") {
		redisURL = "redis://" + redisURL
	}
	u, err := url.Parse(redisURL)
	if err != nil {
		return nil, err
	}
	hosts := u.Host

	// Sentinel mode: the first comma comes before the first colon.
	if strings.Contains(hosts, ",") && strings.Index(hosts, ",") < strings.Index(hosts, ":") {
		var fopt redis.FailoverOptions
		ps := strings.Split(hosts, ",")
		fopt.MasterName = ps[0]
		fopt.SentinelAddrs = ps[1:]
		_, port, _ := net.SplitHostPort(fopt.SentinelAddrs[len(fopt.SentinelAddrs)-1])
		if port == "" {
			port = "26379"
		}
		for i, addr := range fopt.SentinelAddrs {
			h, p, e := net.SplitHostPort(addr)
			if e != nil {
				fopt.SentinelAddrs[i] = net.JoinHostPort(addr, port)
			} else if p == "" {
				fopt.SentinelAddrs[i] = net.JoinHostPort(h, port)
			}
		}
		fopt.SentinelPassword = os.Getenv("SENTINEL_PASSWORD")
		fopt.DB = opt.DB
		fopt.Username = opt.Username
		fopt.Password = opt.Password
		fopt.TLSConfig = opt.TLSConfig
		fopt.MaxRetries = opt.MaxRetries
		fopt.MinRetryBackoff = opt.MinRetryBackoff
		fopt.MaxRetryBackoff = opt.MaxRetryBackoff
		fopt.DialTimeout = opt.DialTimeout
		fopt.ReadTimeout = opt.ReadTimeout
		fopt.WriteTimeout = opt.WriteTimeout
		fopt.PoolFIFO = opt.PoolFIFO
		fopt.PoolSize = opt.PoolSize
		fopt.PoolTimeout = opt.PoolTimeout
		fopt.MinIdleConns = opt.MinIdleConns
		fopt.MaxIdleConns = opt.MaxIdleConns
		fopt.MaxActiveConns = opt.MaxActiveConns
		fopt.ConnMaxIdleTime = opt.ConnMaxIdleTime
		fopt.ConnMaxLifetime = opt.ConnMaxLifetime
		return redis.NewFailoverClient(&fopt), nil
	}

	// Cluster mode: multiple hosts separated by commas.
	if strings.Contains(hosts, ",") {
		var copt redis.ClusterOptions
		copt.Addrs = strings.Split(hosts, ",")
		copt.MaxRedirects = 1
		copt.Username = opt.Username
		copt.Password = opt.Password
		copt.TLSConfig = opt.TLSConfig
		copt.MaxRetries = opt.MaxRetries
		copt.MinRetryBackoff = opt.MinRetryBackoff
		copt.MaxRetryBackoff = opt.MaxRetryBackoff
		copt.DialTimeout = opt.DialTimeout
		copt.ReadTimeout = opt.ReadTimeout
		copt.WriteTimeout = opt.WriteTimeout
		copt.PoolFIFO = opt.PoolFIFO
		copt.PoolSize = opt.PoolSize
		copt.PoolTimeout = opt.PoolTimeout
		copt.MinIdleConns = opt.MinIdleConns
		copt.MaxIdleConns = opt.MaxIdleConns
		copt.MaxActiveConns = opt.MaxActiveConns
		copt.ConnMaxIdleTime = opt.ConnMaxIdleTime
		copt.ConnMaxLifetime = opt.ConnMaxLifetime
		return redis.NewClusterClient(&copt), nil
	}

	// Single-node mode.
	return redis.NewClient(opt), nil
}
