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
				grpcServer.GracefulStop()
				_ = m.Shutdown()
			}()

			if err := grpcServer.Serve(lis); err != nil {
				loggerProxy.Fatalf("Failed to serve: %v", err)
			}

			loggerProxy.Info("gRPC server stopped")
			return nil
		},
	}
}
