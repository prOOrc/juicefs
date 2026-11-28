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
	"context"
	"strings"
	"syscall"

	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"google.golang.org/protobuf/proto"
)

func (s *MetaProxyServer) Init(ctx context.Context, req *pb.InitRequest) (*pb.InitResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	format := ProtoToFormat(req.Format)
	logger.Debugf("Init called with format: name=%s, uuid=%s", format.Name, format.UUID)
	err = eng.Init(format, req.Force)
	var errno uint32
	if err != nil {
		logger.Errorf("Init failed: %v", err)
		if e, ok := err.(syscall.Errno); ok {
			errno = uint32(e)
		} else {
			errno = uint32(syscall.EIO)
		}
	}
	return &pb.InitResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) Load(ctx context.Context, req *pb.LoadRequest) (*pb.LoadResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	logger.Debugf("Load called with checkVersion=%v", req.CheckVersion)
	format, err := eng.Load(req.CheckVersion)
	if err != nil {
		logger.Debugf("Load failed: %v (type: %T)", err, err)
		if e, ok := err.(syscall.Errno); ok {
			logger.Debugf("Load error is syscall.Errno: %d", e)
			return &pb.LoadResponse{Errno: uint32(e)}, nil
		}
		if strings.HasPrefix(err.Error(), "database is not formatted") {
			logger.Debugf("Load error: database is not formatted, returning ENOENT")
			return &pb.LoadResponse{Errno: uint32(syscall.ENOENT)}, nil
		}
		logger.Debugf("Load error is not syscall.Errno, returning EIO")
		return &pb.LoadResponse{Errno: uint32(syscall.EIO)}, nil
	}
	logger.Debugf("Load succeeded")
	return &pb.LoadResponse{
		Errno:  0,
		Format: FormatToProto(*format),
	}, nil
}

// NewSession answers with the engine session that was created once at engine
// creation (D4): every client of the engine group receives a clone of the same
// saved response. The client's own record argument is not re-issued (D5).
func (s *MetaProxyServer) NewSession(ctx context.Context, _ *pb.NewSessionRequest) (*pb.NewSessionResponse, error) {
	e, err := s.sessionEntryFor(ctx) // fail-closed + lazy root engine
	if err != nil {
		return nil, err
	}
	return proto.Clone(e.newSessResp).(*pb.NewSessionResponse), nil
}

// CloseSession ends only the sender's session (D6); the engine CloseSession
// runs exactly once at pool teardown, when the last referencing session is
// gone (I5, I6).
func (s *MetaProxyServer) CloseSession(ctx context.Context, _ *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	sess, err := s.sessionFor(ctx)
	if err != nil {
		return nil, err
	}
	s.endSession(sess)
	return &pb.CloseSessionResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) FlushSession(ctx context.Context, req *pb.FlushSessionRequest) (*pb.FlushSessionResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	eng.FlushSession()
	resp := &pb.FlushSessionResponse{Errno: 0}
	// Permission generation (task 7.3): the heartbeat is the revocation channel —
	// the platform bumps drivepermgen:{userID} on every role change and the client
	// wipes its keys when the value increases. Any failure here (no KeyManager, no
	// verified identity, RPC error) leaves the field at 0: clients react only to an
	// increase, so a KeyManager outage must not wipe keys of authorized users.
	if s.keyManager != nil {
		if userID, err := extractUserIDFromOIDC(ctx); err == nil {
			if genResp, err := s.keyManager.GetPermissionGeneration(ctx, &kmpb.GetPermissionGenerationRequest{UserId: userID}); err == nil {
				resp.PermissionGeneration = genResp.Generation
			} else {
				logger.Warnf("FlushSession: GetPermissionGeneration failed for user %s: %v", userID, err)
			}
		}
	}
	return resp, nil
}

func (s *MetaProxyServer) GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.GetSessionResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	session, err := eng.GetSession(req.Sid, req.Detail)
	if err != nil {
		return &pb.GetSessionResponse{Errno: uint32(syscall.EIO)}, nil
	}
	return &pb.GetSessionResponse{
		Errno:   0,
		Session: SessionToProto(session),
	}, nil
}

func (s *MetaProxyServer) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	sessions, err := eng.ListSessions()
	if err != nil {
		return &pb.ListSessionsResponse{Errno: uint32(syscall.EIO)}, nil
	}
	protoSessions := make([]*pb.ProtoSession, len(sessions))
	for i, sess := range sessions {
		protoSessions[i] = SessionToProto(sess)
	}
	return &pb.ListSessionsResponse{
		Errno:    0,
		Sessions: protoSessions,
	}, nil
}

func (s *MetaProxyServer) CleanStaleSessions(ctx context.Context, req *pb.CleanStaleSessionsRequest) (*pb.CleanStaleSessionsResponse, error) {
	eng, err := s.engineFor(ctx)
	if err != nil {
		return nil, err
	}
	mctx := s.metaCtx(ctx, req.Ctx)
	eng.CleanStaleSessions(mctx)
	return &pb.CleanStaleSessionsResponse{Errno: 0}, nil
}
