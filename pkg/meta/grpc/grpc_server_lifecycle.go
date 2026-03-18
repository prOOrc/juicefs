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
)

func (s *MetaProxyServer) Init(ctx context.Context, req *InitRequest) (*InitResponse, error) {
	format := ProtoToFormat(req.Format)
	err := s.meta.Init(format, req.Force)
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &InitResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) Load(ctx context.Context, req *LoadRequest) (*LoadResponse, error) {
	format, err := s.meta.Load(req.CheckVersion)
	if err != nil {
		return &LoadResponse{Errno: uint32(syscall.EIO)}, nil
	}
	return &LoadResponse{
		Errno:  0,
		Format: FormatToProto(*format),
	}, nil
}

func (s *MetaProxyServer) NewSession(ctx context.Context, req *NewSessionRequest) (*NewSessionResponse, error) {
	err := s.meta.NewSession(req.Record)
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &NewSessionResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) CloseSession(ctx context.Context, req *CloseSessionRequest) (*CloseSessionResponse, error) {
	err := s.meta.CloseSession()
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &CloseSessionResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) FlushSession(ctx context.Context, req *FlushSessionRequest) (*FlushSessionResponse, error) {
	s.meta.FlushSession()
	return &FlushSessionResponse{Errno: 0}, nil
}

func (s *MetaProxyServer) Shutdown(ctx context.Context, req *ShutdownRequest) (*ShutdownResponse, error) {
	err := s.meta.Shutdown()
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &ShutdownResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) Reset(ctx context.Context, req *ResetRequest) (*ResetResponse, error) {
	err := s.meta.Reset()
	var errno uint32
	if err != nil {
		errno = uint32(syscall.EIO)
	}
	return &ResetResponse{Errno: errno}, nil
}

func (s *MetaProxyServer) GetSession(ctx context.Context, req *GetSessionRequest) (*GetSessionResponse, error) {
	session, err := s.meta.GetSession(req.Sid, req.Detail)
	if err != nil {
		return &GetSessionResponse{Errno: uint32(syscall.EIO)}, nil
	}
	return &GetSessionResponse{
		Errno:   0,
		Session: SessionToProto(session),
	}, nil
}

func (s *MetaProxyServer) ListSessions(ctx context.Context, req *ListSessionsRequest) (*ListSessionsResponse, error) {
	sessions, err := s.meta.ListSessions()
	if err != nil {
		return &ListSessionsResponse{Errno: uint32(syscall.EIO)}, nil
	}
	protoSessions := make([]*ProtoSession, len(sessions))
	for i, s := range sessions {
		protoSessions[i] = SessionToProto(s)
	}
	return &ListSessionsResponse{
		Errno:    0,
		Sessions: protoSessions,
	}, nil
}

func (s *MetaProxyServer) CleanStaleSessions(ctx context.Context, req *CleanStaleSessionsRequest) (*CleanStaleSessionsResponse, error) {
	mctx := s.metaCtx(ctx, req.Ctx)
	s.meta.CleanStaleSessions(mctx)
	return &CleanStaleSessionsResponse{Errno: 0}, nil
}
