/*
 * JuiceFS, Copyright 2021 Juicedata, Inc.
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

package cmd

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	gRPC "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/urfave/cli/v2"
)

var loggerProxy = utils.GetLogger("juicefs-proxy")

// serverTLSConfig builds the listener TLS config for the --tls-cert/--tls-key
// pair (implement-grpc-tls, NFR-SEC-8). A nil config (both flags empty) means
// plaintext listening — the default, unchanged behavior. Exactly one flag is
// refused at startup with an error naming the missing one.
func serverTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	if certFile == "" {
		return nil, fmt.Errorf("--tls-cert is missing: TLS requires both --tls-cert and --tls-key")
	}
	if keyFile == "" {
		return nil, fmt.Errorf("--tls-key is missing: TLS requires both --tls-cert and --tls-key")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate pair (%s, %s): %w", certFile, keyFile, err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func cmdMetaProxy() *cli.Command {
	return &cli.Command{
		Name:  "meta-proxy",
		Usage: "Start a gRPC proxy for JuiceFS metadata backend",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "meta-backend",
				Usage: "Metadata backend URL (e.g., redis://localhost:6379/0, postgresql://...)",
				Value: "redis://localhost:6379/0",
			},
			&cli.StringFlag{
				Name:  "addr",
				Usage: "gRPC server address to listen on",
				Value: ":9561",
			},
			&cli.IntFlag{
				Name:  "grpc-max-send-msg-size",
				Usage: "Maximum gRPC send message size in MB",
				Value: 256,
			},
			&cli.IntFlag{
				Name:  "grpc-max-recv-msg-size",
				Usage: "Maximum gRPC receive message size in MB",
				Value: 256,
			},
			&cli.DurationFlag{
				Name:  "grpc-keepalive-time",
				Usage: "gRPC keepalive time",
				Value: time.Hour,
			},
			&cli.DurationFlag{
				Name:  "grpc-keepalive-timeout",
				Usage: "gRPC keepalive timeout",
				Value: time.Second * 20,
			},
			// OIDC flags
			&cli.StringFlag{
				Name:   "oidc-issuer",
				Usage:  "OIDC issuer URL (enables OIDC authentication when set, e.g., https://platform.agio.services/.ory/hydra/public)",
				Hidden: false,
			},
			&cli.StringFlag{
				Name:   "oidc-client-id",
				Usage:  "Expected client_id (aud claim) in OIDC token. If set, only tokens issued to this client are accepted. If omitted, any valid token is accepted.",
				Hidden: false,
			},
			&cli.StringFlag{
				Name:   "oidc-audience",
				Usage:  "Expected audience claim in OIDC token (optional)",
				Hidden: false,
			},
			// Authorization flags
			&cli.StringFlag{
				Name:   "authz-service",
				Usage:  "Authorization gRPC service address (e.g., localhost:9090). Enables file-level authorization when set.",
				Hidden: false,
			},
			&cli.StringFlag{
				Name:   "authz-volume-name",
				Usage:  "JuiceFS volume name for facility mapping in authz service (optional)",
				Hidden: false,
			},
			&cli.StringFlag{
				Name:   "authz-tls-cert",
				Usage:  "TLS client certificate for authz service connection (production use)",
				Hidden: true,
			},
			&cli.StringFlag{
				Name:   "authz-tls-key",
				Usage:  "TLS client private key for authz service connection (production use)",
				Hidden: true,
			},
			&cli.StringFlag{
				Name:   "authz-tls-ca",
				Usage:  "TLS CA certificate for authz service connection (production use)",
				Hidden: true,
			},
			&cli.StringFlag{
				Name:   "authz-server-name",
				Usage:  "Server name for TLS certificate validation (default: service hostname)",
				Hidden: true,
			},
			&cli.IntFlag{
				Name:   "authz-path-cache-size",
				Usage:  "Maximum number of inode→path mappings in cache (0 = unlimited, default 100000)",
				Value:  100000,
				Hidden: false,
			},
			&cli.DurationFlag{
				Name:   "authz-cache-ttl",
				Usage:  "TTL for cached authorization decisions (default 30s). Reduces repeated calls to the authz service.",
				Value:  30 * time.Second,
				Hidden: false,
			},
			// TLS listener flags (implement-grpc-tls)
			&cli.StringFlag{
				Name:  "tls-cert",
				Usage: "TLS server certificate PEM file (enables TLS when set together with --tls-key)",
			},
			&cli.StringFlag{
				Name:  "tls-key",
				Usage: "TLS server private key PEM file (enables TLS when set together with --tls-cert)",
			},
			// KeyManager flags (per-file FEK encryption)
			&cli.StringFlag{
				Name:   "keymanager-service",
				Usage:  "KeyManager gRPC service address (e.g., localhost:9091). Enables per-file FEK encryption when set.",
				Hidden: false,
			},
			&cli.StringFlag{
				Name:   "keymanager-tls-cert",
				Usage:  "TLS client certificate for KeyManager service connection (production use)",
				Hidden: true,
			},
			&cli.StringFlag{
				Name:   "keymanager-tls-key",
				Usage:  "TLS client private key for KeyManager service connection (production use)",
				Hidden: true,
			},
			&cli.StringFlag{
				Name:   "keymanager-tls-ca",
				Usage:  "TLS CA certificate for KeyManager service connection (production use)",
				Hidden: true,
			},
			&cli.StringFlag{
				Name:   "keymanager-server-name",
				Usage:  "Server name for TLS certificate validation (default: service hostname)",
				Hidden: true,
			},
		},
		Action: func(c *cli.Context) error {
			setup(c, 0)

			metaBackendUrl := c.String("meta-backend")
			addr := c.String("addr")
			maxSendMsgSize := c.Int("grpc-max-send-msg-size") * 1024 * 1024
			maxRecvMsgSize := c.Int("grpc-max-recv-msg-size") * 1024 * 1024
			keepaliveTime := c.Duration("grpc-keepalive-time")
			keepaliveTimeout := c.Duration("grpc-keepalive-timeout")

			loggerProxy.Info("Starting JuiceFS metadata proxy server")
			loggerProxy.Infof("Metadata backend URL: %s", utils.RemovePassword(metaBackendUrl))
			loggerProxy.Infof("gRPC address: %s", addr)

			m := meta.NewClient(metaBackendUrl, meta.DefaultConf())

			// Load the volume format so GetFormat (encryption checks) works from
			// the first request. A not-yet-formatted volume is not fatal: the
			// format is picked up by the first Init/Load RPC.
			var volFormat *meta.Format
			if format, err := m.Load(true); err != nil {
				loggerProxy.Warnf("Failed to load volume format (is the volume formatted?): %v", err)
			} else {
				volFormat = format
				loggerProxy.Infof("Volume %q loaded (per-file encryption enabled: %v)", format.Name, format.EncryptionEnabled)
			}

			cacheMaxSize := c.Int("authz-path-cache-size")
			server := meta.NewMetaProxyServer(m, cacheMaxSize)

			opts := []gRPC.ServerOption{
				gRPC.MaxRecvMsgSize(maxRecvMsgSize),
				gRPC.MaxSendMsgSize(maxSendMsgSize),
				gRPC.KeepaliveParams(keepalive.ServerParameters{
					Time:    keepaliveTime,
					Timeout: keepaliveTimeout,
				}),
				gRPC.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
					MinTime:             time.Second * 5,
					PermitWithoutStream: false,
				}),
			}

			// TLS listener (optional; without --tls-cert/--tls-key the server
			// stays plaintext — behavior unchanged)
			tlsCfg, err := serverTLSConfig(c.String("tls-cert"), c.String("tls-key"))
			if err != nil {
				loggerProxy.Fatalf("Invalid TLS configuration: %v", err)
			}
			if tlsCfg != nil {
				opts = append(opts, gRPC.Creds(credentials.NewTLS(tlsCfg)))
				loggerProxy.Infof("TLS enabled for the gRPC listener (minimum version 1.2)")
			}

			// Build interceptor chain: OIDC (authentication) → Authz (authorization)
			var unaryInterceptors []gRPC.UnaryServerInterceptor
			var streamInterceptors []gRPC.StreamServerInterceptor

			// OIDC authentication (strict — requires valid token when enabled)
			oidcIssuer := c.String("oidc-issuer")
			oidcClientID := c.String("oidc-client-id")
			if oidcIssuer != "" {
				if oidcClientID != "" {
					loggerProxy.Infof("OIDC authentication enabled (issuer: %s, client_id: %s, strict mode)", oidcIssuer, oidcClientID)
				} else {
					loggerProxy.Infof("OIDC authentication enabled (issuer: %s, any client accepted, strict mode)", oidcIssuer)
				}
				validator, err := oidc.NewValidator(context.Background(), oidcIssuer, oidcClientID)
				if err != nil {
					loggerProxy.Fatalf("Failed to create OIDC validator: %v", err)
				}
				unaryInterceptors = append(unaryInterceptors, oidc.StrictUnaryInterceptorWithValidator(validator))
				streamInterceptors = append(streamInterceptors, oidc.StrictStreamInterceptorWithValidator(validator))
			}

			// Authorization interceptor (optional, after OIDC)
			authzAddr := c.String("authz-service")
			if authzAddr != "" {
				volumeName := c.String("authz-volume-name")
				tlsCert := c.String("authz-tls-cert")
				tlsKey := c.String("authz-tls-key")
				tlsCA := c.String("authz-tls-ca")
				serverName := c.String("authz-server-name")

				loggerProxy.Infof("File authorization enabled (authz service: %s, volume: %s)", authzAddr, volumeName)

				authzClient, err := meta.NewPlatformAuthzClient(authzAddr, volumeName, tlsCert, tlsKey, tlsCA, serverName)
				if err != nil {
					loggerProxy.Fatalf("Failed to connect to authz service: %v", err)
				}

				cacheTTL := c.Duration("authz-cache-ttl")
				if cacheTTL > 0 {
					authzClient = meta.NewCachingAuthzClient(authzClient, cacheTTL, 0)
					loggerProxy.Infof("Authz decision cache enabled (TTL: %s)", cacheTTL)
				} else {
					loggerProxy.Infof("Authz decision cache disabled")
				}

				interceptor := meta.NewAuthzInterceptor(authzClient, server.InodePathCache(), server)
				server.SetAuthzInterceptor(interceptor)
				unaryInterceptors = append(unaryInterceptors, interceptor.UnaryInterceptor())

				loggerProxy.Warnf("Streaming DumpMeta/LoadMeta disabled (not covered by authz)")
			}

			// KeyManager (per-file FEK encryption, optional)
			server.SetVolumeName(c.String("authz-volume-name"))
			keymanagerAddr := c.String("keymanager-service")
			if keymanagerAddr != "" {
				tlsCert := c.String("keymanager-tls-cert")
				tlsKey := c.String("keymanager-tls-key")
				tlsCA := c.String("keymanager-tls-ca")
				serverName := c.String("keymanager-server-name")

				loggerProxy.Infof("Per-file FEK encryption enabled (keymanager service: %s)", keymanagerAddr)

				keyManager, err := meta.NewKeyManagerClient(keymanagerAddr, tlsCert, tlsKey, tlsCA, serverName)
				if err != nil {
					loggerProxy.Fatalf("Failed to connect to keymanager service: %v", err)
				}
				server.SetKeyManager(keyManager)
			} else if volFormat != nil && volFormat.EncryptionEnabled {
				loggerProxy.Warnf("Volume has encryption enabled but --keymanager-service is not set — encrypted files cannot be created or opened")
			} else {
				loggerProxy.Infof("KeyManager not configured — per-file encryption disabled for all volumes")
			}

			// Apply interceptor chain
			if len(unaryInterceptors) > 0 {
				opts = append(opts, gRPC.ChainUnaryInterceptor(unaryInterceptors...))
			}
			if len(streamInterceptors) > 0 {
				opts = append(opts, gRPC.ChainStreamInterceptor(streamInterceptors...))
			}

			grpcServer := gRPC.NewServer(opts...)
			pb.RegisterMetaServiceServer(grpcServer, server)
			// STS pass-through (task 7.12, ADR-003): same server, same OIDC/authz
			// interceptor chain; needs --keymanager-service to forward to the platform.
			pb.RegisterStsProxyServiceServer(grpcServer, server)

			lis, err := net.Listen("tcp", addr)
			if err != nil {
				loggerProxy.Fatalf("Failed to listen on %s: %v", addr, err)
			}
			loggerProxy.Infof("gRPC server listening on %s", lis.Addr())

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			go func() {
				<-sigCh
				loggerProxy.Info("Received shutdown signal, gracefully stopping server")
				cancel()
			}()

			go func() {
				<-ctx.Done()
				loggerProxy.Info("Stopping gRPC server...")
				// Use GracefulStop with timeout
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer stopCancel()
				done := make(chan struct{})
				go func() {
					grpcServer.GracefulStop()
					close(done)
				}()
				select {
				case <-done:
					loggerProxy.Info("gRPC server stopped gracefully")
				case <-stopCtx.Done():
					loggerProxy.Warn("GracefulStop timeout, forcing stop")
					grpcServer.Stop()
				}
				loggerProxy.Info("Shutting down metadata client...")
				if err := m.Shutdown(); err != nil {
					loggerProxy.Errorf("Error shutting down metadata client: %v", err)
				}
				loggerProxy.Info("Metadata client shutdown complete")
			}()

			if err := grpcServer.Serve(lis); err != nil {
				loggerProxy.Fatalf("Failed to serve: %v", err)
			}

			loggerProxy.Info("Meta proxy shutdown complete")
			return nil
		},
	}
}
