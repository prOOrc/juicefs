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
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	gRPC "google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/urfave/cli/v2"
)

var loggerProxy = utils.GetLogger("juicefs-proxy")

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
			&cli.BoolFlag{
				Name:  "debug",
				Usage: "Enable debug logging",
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
		},
		Action: func(c *cli.Context) error {
			if c.Bool("debug") {
				utils.SetLogLevel(0)
			}

			metaBackendUrl := c.String("meta-backend")
			addr := c.String("addr")
			maxSendMsgSize := c.Int("grpc-max-send-msg-size") * 1024 * 1024
			maxRecvMsgSize := c.Int("grpc-max-recv-msg-size") * 1024 * 1024
			keepaliveTime := c.Duration("grpc-keepalive-time")
			keepaliveTimeout := c.Duration("grpc-keepalive-timeout")

			loggerProxy.Info("Starting JuiceFS metadata proxy server")
			loggerProxy.Infof("Metadata backend URL: %s", metaBackendUrl)
			loggerProxy.Infof("gRPC address: %s", addr)

			m := meta.NewClient(metaBackendUrl, meta.DefaultConf())

			server := meta.NewMetaProxyServer(m)

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
				opts = append(opts,
					gRPC.UnaryInterceptor(oidc.StrictUnaryInterceptorWithValidator(validator)),
					gRPC.StreamInterceptor(oidc.StrictStreamInterceptorWithValidator(validator)),
				)
			}

			grpcServer := gRPC.NewServer(opts...)
			pb.RegisterMetaServiceServer(grpcServer, server)

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
