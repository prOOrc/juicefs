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

	"github.com/juicedata/juicefs/pkg/meta/pb"
)

func (s *MetaProxyServer) Init(ctx context.Context, req *pb.InitRequest) (*pb.InitResponse, error) {
	format := ProtoToFormat(req.Format)
	logger.Debugf("Init called with format: name=%s, uuid=%s", format.Name, format.UUID)
	err := s.meta.Init(format, req.Force)
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
	logger.Debugf("Load called with checkVersion=%v", req.CheckVersion)
	format, err := s.meta.Load(req.CheckVersion)
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

func (s *MetaProxyServer) NewSession(ctx context.Context, req *pb.NewSessionRequest) (*pb.NewSessionResponse, error) {
	err := s.meta.NewSession(req.Record)
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &pb.NewSessionResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) CloseSession(ctx context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	err := s.meta.CloseSession()
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &pb.CloseSessionResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) FlushSession(ctx context.Context, req *pb.FlushSessionRequest) (*pb.FlushSessionResponse, error) {
	s.meta.FlushSession()
	return &pb.FlushSessionResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) Shutdown(ctx context.Context, req *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	err := s.meta.Shutdown()
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &pb.ShutdownResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) Reset(ctx context.Context, req *pb.ResetRequest) (*pb.ResetResponse, error) {
	err := s.meta.Reset()
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &pb.ResetResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.GetSessionResponse, error) {
	session, err := s.meta.GetSession(req.Sid, req.Detail)
	if err != nil {
		return &pb.GetSessionResponse{Errno: uint32(syscall.EIO)}, nil
	}
	return &pb.GetSessionResponse{
		Errno:   0,
		Session: SessionToProto(session),
	}, nil
}

func (s *MetaProxyServer) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	sessions, err := s.meta.ListSessions()
	if err != nil {
		return &pb.ListSessionsResponse{Errno: uint32(syscall.EIO)}, nil
	}
	protoSessions := make([]*pb.ProtoSession, len(sessions))
	for i, s := range sessions {
		protoSessions[i] = SessionToProto(s)
	}
	return &pb.ListSessionsResponse{
		Errno:    0,
		Sessions: protoSessions,
	}, nil
}

func (s *MetaProxyServer) CleanStaleSessions(ctx context.Context, req *pb.CleanStaleSessionsRequest) (*pb.CleanStaleSessionsResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	s.meta.CleanStaleSessions(mctx)
	return &pb.CleanStaleSessionsResponse{Errno: 0}, nil
}
