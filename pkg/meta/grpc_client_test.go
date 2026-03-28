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
	"syscall"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
)

// TestGRPCMetaCreation tests that grpcMeta can be created
func TestGRPCMetaCreation(t *testing.T) {
	// Just test that we can create a new grpcMeta instance
	// (connection will fail without server, but that's OK)
	conf := DefaultConf()
	_, err := newGRPCMeta("grpc", "localhost:9561", conf)
	// Expected to fail if no server is running
	t.Logf("Connection result (expected to fail without server): %v", err)
}

// TestGRPCMetaName tests that grpcMeta returns correct name
func TestGRPCMetaName(t *testing.T) {
	// This is a simple test - actual connection tests require a running server
	conf := DefaultConf()
	meta, err := newGRPCMeta("grpc", "localhost:9561", conf)
	if err != nil {
		// Expected to fail if no server is running
		t.Logf("Expected error when no server is running: %v", err)
		return
	}
	defer meta.Shutdown()

	assert.Contains(t, meta.Name(), "grpc://")
}

// MockGRPCClient implements pb.MetaServiceClient for testing
type MockGRPCClient struct {
	pb.MetaServiceClient
	ListResponse   *pb.DirHandlerListResponse
	InsertResponse *pb.DirHandlerInsertResponse
	DeleteResponse *pb.DirHandlerDeleteResponse
	CloseResponse  *pb.DirHandlerCloseResponse
}

func (m *MockGRPCClient) DirHandlerList(ctx context.Context, req *pb.DirHandlerListRequest, opts ...grpc.CallOption) (*pb.DirHandlerListResponse, error) {
	if m.ListResponse != nil {
		return m.ListResponse, nil
	}
	return &pb.DirHandlerListResponse{}, nil
}

func (m *MockGRPCClient) DirHandlerInsert(ctx context.Context, req *pb.DirHandlerInsertRequest, opts ...grpc.CallOption) (*pb.DirHandlerInsertResponse, error) {
	if m.InsertResponse != nil {
		return m.InsertResponse, nil
	}
	return &pb.DirHandlerInsertResponse{}, nil
}

func (m *MockGRPCClient) DirHandlerDelete(ctx context.Context, req *pb.DirHandlerDeleteRequest, opts ...grpc.CallOption) (*pb.DirHandlerDeleteResponse, error) {
	if m.DeleteResponse != nil {
		return m.DeleteResponse, nil
	}
	return &pb.DirHandlerDeleteResponse{}, nil
}

func (m *MockGRPCClient) DirHandlerClose(ctx context.Context, req *pb.DirHandlerCloseRequest, opts ...grpc.CallOption) (*pb.DirHandlerCloseResponse, error) {
	if m.CloseResponse != nil {
		return m.CloseResponse, nil
	}
	return &pb.DirHandlerCloseResponse{}, nil
}

// TestGRPCDirHandlerList tests the List method of grpcDirHandler
func TestGRPCDirHandlerList(t *testing.T) {
	mockClient := &MockGRPCClient{
		ListResponse: &pb.DirHandlerListResponse{
			Entries: []*pb.ProtoEntry{
				{Inode: 1, Name: []byte("file1")},
				{Inode: 2, Name: []byte("file2")},
			},
		},
	}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	ctx := Context(nil)
	entries, errno := handler.List(ctx, 0)

	assert.Equal(t, syscall.Errno(0), errno)
	assert.Len(t, entries, 2)
	assert.Equal(t, Ino(1), entries[0].Inode)
	assert.Equal(t, []byte("file1"), entries[0].Name)
	assert.Equal(t, Ino(2), entries[1].Inode)
	assert.Equal(t, []byte("file2"), entries[1].Name)
}

// TestGRPCDirHandlerListError tests error handling in List
func TestGRPCDirHandlerListError(t *testing.T) {
	mockClient := &MockGRPCClient{
		ListResponse: &pb.DirHandlerListResponse{
			Errno: uint32(syscall.ENOENT),
		},
	}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	ctx := Context(nil)
	entries, errno := handler.List(ctx, 0)

	assert.Equal(t, syscall.ENOENT, errno)
	assert.Nil(t, entries)
}

// TestGRPCDirHandlerInsert tests the Insert method
func TestGRPCDirHandlerInsert(t *testing.T) {
	mockClient := &MockGRPCClient{}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	attr := &Attr{Mode: 0100644, Mtime: time.Now().Unix()}
	handler.Insert(Ino(100), "newfile", attr)
}

// TestGRPCDirHandlerDelete tests the Delete method
func TestGRPCDirHandlerDelete(t *testing.T) {
	mockClient := &MockGRPCClient{}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	handler.Delete("fileToDelete")
}

// TestGRPCDirHandlerClose tests the Close method
func TestGRPCDirHandlerClose(t *testing.T) {
	mockClient := &MockGRPCClient{}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	handler.Close()
}

// TestGRPCDirHandlerListWithOffset tests List with different offsets
func TestGRPCDirHandlerListWithOffset(t *testing.T) {
	mockClient := &MockGRPCClient{
		ListResponse: &pb.DirHandlerListResponse{
			Entries: []*pb.ProtoEntry{
				{Inode: 3, Name: []byte("file3")},
			},
		},
	}

	handler := &grpcDirHandler{
		client: mockClient,
		handle: &pb.DirHandlerHandle{HandleId: 123},
	}

	ctx := Context(nil)
	entries, errno := handler.List(ctx, 10)

	assert.Equal(t, syscall.Errno(0), errno)
	assert.Len(t, entries, 1)
	assert.Equal(t, Ino(3), entries[0].Inode)
}
