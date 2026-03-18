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

	aclAPI "github.com/juicedata/juicefs/pkg/acl"
	"github.com/juicedata/juicefs/pkg/meta"
)

// --- ACL operations ---

// SetFacl sets ACL
func (c *Client) SetFacl(ctx meta.Context, ino meta.Ino, aclType uint32, rule *aclAPI.Rule) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.SetFacl(grpcCtx, &SetFaclRequest{
		Ctx:     toProtoContext(ctx),
		Ino:     uint64(ino),
		AclType: aclType,
		Rule:    toProtoACLRule(rule),
	})
	if err != nil {
		return syscall.EIO
	}
	return syscall.Errno(resp.GetErrno())
}

// GetFacl gets ACL
func (c *Client) GetFacl(ctx meta.Context, ino meta.Ino, aclType uint32, rule *aclAPI.Rule) syscall.Errno {
	grpcCtx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
	defer cancel()

	resp, err := c.client.GetFacl(grpcCtx, &GetFaclRequest{
		Ctx:     toProtoContext(ctx),
		Ino:     uint64(ino),
		AclType: aclType,
	})
	if err != nil {
		return syscall.EIO
	}
	if resp.GetErrno() != 0 {
		return syscall.Errno(resp.GetErrno())
	}
	if rule != nil {
		*rule = *fromProtoACLRule(resp.GetRule())
	}
	return 0
}
