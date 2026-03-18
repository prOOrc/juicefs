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

package grpc

import (
	"context"
	"syscall"

	"github.com/juicedata/juicefs/pkg/meta"
)

// --- Directory operations ---

// GetParents gets parent inodes
func (c *Client) GetParents(ctx meta.Context, ino meta.Ino) map[meta.Ino]int {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetParents(grpcCtx, &GetParentsRequest{
		Ctx:   toProtoContext(ctx),
		Inode: uint64(ino),
	})
	if err != nil || resp == nil {
		return nil
	}
	if resp.GetErrno() != 0 {
		return nil
	}
	parents := make(map[meta.Ino]int, len(resp.GetParents()))
	for k, v := range resp.GetParents() {
		parents[meta.Ino(k)] = int(v)
	}
	return parents
}

// GetDirStat gets directory statistics
func (c *Client) GetDirStat(ctx meta.Context, ino meta.Ino) (stat interface{}, st syscall.Errno) {
	return nil, syscall.ENOSYS
}
