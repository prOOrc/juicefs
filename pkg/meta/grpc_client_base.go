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

package meta

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

// GRPCClient implements meta.Meta interface using gRPC
type GRPCClient struct {
	*baseMeta
	client pb.MetaServiceClient
	conn   *grpc.ClientConn
	addr   string
	opts   *GRPCOptions
}

// GRPCOptions for gRPC client
type GRPCOptions struct {
	Timeout     time.Duration
	MaxRetries  int
	DialOptions []grpc.DialOption
}

// DefaultGRPCOptions returns default client options
func DefaultGRPCOptions() *GRPCOptions {
	return &GRPCOptions{
		Timeout: 30 * time.Second,
	}
}

// NewGRPCClient creates a new gRPC client
func NewGRPCClient(addr string, conf *Config, opts *GRPCOptions) (*GRPCClient, error) {
	if opts == nil {
		opts = DefaultGRPCOptions()
	}

	m := &GRPCClient{
		baseMeta: newBaseMeta(addr, conf),
		addr:     addr,
		opts:     opts,
	}

	if addr != "" {
		dialOpts := opts.DialOptions
		if len(dialOpts) == 0 {
			dialOpts = []grpc.DialOption{
				grpc.WithTransportCredentials(insecure.NewCredentials()),
			}
		}

		conn, err := grpc.Dial(addr, dialOpts...)
		if err != nil {
			return nil, err
		}

		m.conn = conn
		m.client = pb.NewMetaServiceClient(conn)
	}

	m.impl = m

	return m, nil
}

// CloseConn closes the gRPC connection
func (c *GRPCClient) CloseConn() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Name returns the name of the meta backend
func (c *GRPCClient) Name() string {
	return "grpc"
}

// getBase returns the baseMeta (required by Meta interface)
func (c *GRPCClient) getBase() *baseMeta {
	return c.baseMeta
}

// chroot sets the root inode (required by Meta interface)
func (c *GRPCClient) chroot(inode Ino) {
	c.baseMeta.chroot(inode)
}

// Compile-time check that GRPCClient implements Meta interface
var _ Meta = (*GRPCClient)(nil)
