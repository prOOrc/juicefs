/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
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
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeSTSProvider delegates to a test-controlled closure.
type fakeSTSProvider struct {
	next  func() (stsCredentials, error)
	calls int
}

func (f *fakeSTSProvider) Credentials(ctx context.Context) (stsCredentials, error) {
	f.calls++
	return f.next()
}

// newTestStorageHolder builds a storageHolder around the in-memory object
// storage so SetCredentials can be exercised without a real cloud.
func newTestStorageHolder(t *testing.T) *storageHolder {
	t.Helper()
	format := &meta.Format{
		Name:      "sts-test",
		Storage:   "mem",
		Bucket:    "test-bucket",
		AccessKey: "static-ak",
		SecretKey: "static-sk",
	}
	blob, err := createStorage(*format)
	require.NoError(t, err)
	return &storageHolder{ObjectStorage: blob, fmt: *format}
}

// TestSTSRefresher_StartAndRefresh (FR-REV-3): Start fetches the initial set
// and swaps it into the storage holder; each refresh installs the next set.
func TestSTSRefresher_StartAndRefresh(t *testing.T) {
	holder := newTestStorageHolder(t)
	now := time.Now()
	seq := []stsCredentials{
		{AccessKey: "AK-1", SecretKey: "SK-1", SessionToken: "TOK-1", Expiry: now.Add(time.Hour)},
		{AccessKey: "AK-2", SecretKey: "SK-2", SessionToken: "TOK-2", Expiry: now.Add(2 * time.Hour)},
	}
	i := 0
	provider := &fakeSTSProvider{next: func() (stsCredentials, error) {
		if i < len(seq) {
			c := seq[i]
			i++
			return c, nil
		}
		return stsCredentials{}, errors.New("no more queued credentials")
	}}
	ref := newSTSRefresher(provider, holder, time.Hour)
	ref.now = func() time.Time { return now }

	require.NoError(t, ref.Start(context.Background()))
	defer ref.Stop()
	require.Equal(t, "AK-1", holder.fmt.AccessKey, "initial fetch must swap credentials")
	require.Equal(t, "TOK-1", holder.fmt.SessionToken)

	// refresh at ttl/2 installs the next set
	now = now.Add(30 * time.Minute)
	ref.refresh()
	require.Equal(t, "AK-2", holder.fmt.AccessKey)
	require.Equal(t, "SK-2", holder.fmt.SecretKey)
}

// TestSTSRefresher_FailSafeUntilExpiry (FR-REV-3): a failed refresh keeps the
// previous credentials in place until they expire; once expired the situation
// is reported exactly once and cleared by the next successful refresh.
func TestSTSRefresher_FailSafeUntilExpiry(t *testing.T) {
	holder := newTestStorageHolder(t)
	now := time.Now()
	var fail bool
	provider := &fakeSTSProvider{next: func() (stsCredentials, error) {
		if fail {
			return stsCredentials{}, errors.New("keymanager unavailable")
		}
		fail = true // only the first call succeeds
		return stsCredentials{AccessKey: "AK-1", Expiry: now.Add(time.Hour)}, nil
	}}
	ref := newSTSRefresher(provider, holder, time.Hour)
	ref.now = func() time.Time { return now }

	require.NoError(t, ref.Start(context.Background()))
	defer ref.Stop()

	// refresh failure: old credentials stay in place
	now = now.Add(30 * time.Minute)
	ref.refresh()
	require.Equal(t, "AK-1", holder.fmt.AccessKey, "failed refresh must keep the current credentials")
	ref.mu.Lock()
	expired := ref.expired
	ref.mu.Unlock()
	require.False(t, expired, "credentials are still valid — no expiry alarm yet")

	// past the expiry with no successful refresh: alarm fires exactly once
	now = now.Add(90 * time.Minute)
	ref.refresh()
	ref.refresh()
	ref.mu.Lock()
	expired = ref.expired
	ref.mu.Unlock()
	require.True(t, expired, "expired credentials without a successful refresh must be reported")

	// recovery: the next success swaps and clears the alarm
	fail = false
	ref.refresh()
	require.Equal(t, "AK-1", holder.fmt.AccessKey)
	ref.mu.Lock()
	expired = ref.expired
	ref.mu.Unlock()
	require.False(t, expired, "a successful refresh must clear the expiry alarm")
}

// TestSTSRefresher_Stop verifies the loop terminates cleanly.
func TestSTSRefresher_Stop(t *testing.T) {
	holder := newTestStorageHolder(t)
	provider := &fakeSTSProvider{next: func() (stsCredentials, error) {
		return stsCredentials{AccessKey: "AK-1", Expiry: time.Now().Add(time.Hour)}, nil
	}}
	ref := newSTSRefresher(provider, holder, time.Hour)
	require.NoError(t, ref.Start(context.Background()))
	done := make(chan struct{})
	go func() { ref.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return")
	}
}

// TestPlatformSTSProvider verifies the user-mode provider (task 7.12, ADR-003):
// the request goes through the proxy's StsProxyService carrying only the
// company ID — the proxy fills user_id from the authenticated session itself —
// and the response fields are mapped into stsCredentials. Proxy errors are
// propagated (fail-fast).
func TestPlatformSTSProvider(t *testing.T) {
	proxy := &fakeStsProxyClient{resp: &pb.StsCredentialsResponse{
		AccessKeyId:     "AK-P",
		SecretAccessKey: "SK-P",
		SessionToken:    "TOK-P",
		ExpirationUnix:  time.Now().Add(time.Hour).Unix(),
	}}
	p := &platformSTSProvider{stsProxy: proxy, companyID: "comp-1"}
	cred, err := p.Credentials(context.Background())
	require.NoError(t, err)
	require.Equal(t, "AK-P", cred.AccessKey)
	require.Equal(t, "SK-P", cred.SecretKey)
	require.Equal(t, "TOK-P", cred.SessionToken)
	require.True(t, cred.Expiry.After(time.Now()))
	require.Equal(t, "comp-1", proxy.lastCompanyID)

	proxy.err = errors.New("proxy down")
	_, err = p.Credentials(context.Background())
	require.Error(t, err, "a proxy failure must surface (fail-fast)")
}

// TestPlatformNodeSTSProvider verifies the render-node provider (task 7.11,
// ADR-003): it proxies CompanyId/NodeId to GetNodeSTSCredentials and maps the
// response; a failing node IAM token fails closed before the RPC.
func TestPlatformNodeSTSProvider(t *testing.T) {
	km := &stubKeyManager{stsResp: &kmpb.GetSTSCredentialsResponse{
		AccessKeyId:     "AK-N",
		SecretAccessKey: "SK-N",
		SessionToken:    "TOK-N",
		ExpirationUnix:  time.Now().Add(time.Hour).Unix(),
	}}
	p := &platformNodeSTSProvider{
		keyManager: km,
		token:      &staticTokenProvider{token: "IAM-TOKEN"},
		nodeID:     "render-node-1",
		companyID:  "comp-1",
	}
	cred, err := p.Credentials(context.Background())
	require.NoError(t, err)
	require.Equal(t, "AK-N", cred.AccessKey)
	require.Equal(t, "SK-N", cred.SecretKey)
	require.Equal(t, "TOK-N", cred.SessionToken)
	require.True(t, cred.Expiry.After(time.Now()))
	require.Equal(t, "comp-1", km.lastNodeCompany)
	require.Equal(t, "render-node-1", km.lastNodeID)

	// a failing IAM token must fail closed
	p.token = &staticTokenProvider{err: errors.New("metadata service unreachable")}
	_, err = p.Credentials(context.Background())
	require.Error(t, err)
}

// staticTokenProvider is a fixed IAM token source for provider tests.
type staticTokenProvider struct {
	token string
	err   error
}

func (p *staticTokenProvider) Token(ctx context.Context) (string, error) {
	return p.token, p.err
}

// fakeStsProxyClient is a minimal StsProxyService client for the user-mode
// provider test.
type fakeStsProxyClient struct {
	resp          *pb.StsCredentialsResponse
	err           error
	lastCompanyID string
}

func (f *fakeStsProxyClient) GetSTSCredentials(ctx context.Context, req *pb.StsCredentialsRequest, opts ...grpc.CallOption) (*pb.StsCredentialsResponse, error) {
	f.lastCompanyID = req.GetCompanyId()
	return f.resp, f.err
}

// TestPrefixPolicies pins the AWS policy shape: it carries the s3:prefix
// condition on ListBucket and must be valid JSON with two statements.
func TestPrefixPolicies(t *testing.T) {
	check := func(policy, label string) map[string]interface{} {
		t.Helper()
		var m map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(policy), &m), "%s policy must be valid JSON", label)
		stmts, ok := m["Statement"].([]interface{})
		require.True(t, ok, "%s policy must have a Statement array", label)
		require.Len(t, stmts, 2, "%s policy must have two statements", label)
		return m
	}

	awsPolicy := awsPrefixPolicy("bucket", "companies/acme/")
	check(awsPolicy, "AWS")
	require.Contains(t, awsPolicy, `"s3:prefix"`)
	require.Contains(t, awsPolicy, `arn:aws:s3:::bucket/companies/acme/*`)
}

// stubKeyManager is a minimal KeyManagerClient for the STS provider tests.
type stubKeyManager struct {
	stsResp         *kmpb.GetSTSCredentialsResponse
	lastSTSUser     string
	lastSTSCompany  string
	lastNodeID      string
	lastNodeCompany string
}

func (s *stubKeyManager) CreateFileKey(ctx context.Context, req *kmpb.CreateFileKeyRequest) (*kmpb.CreateFileKeyResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubKeyManager) GetFileFEK(ctx context.Context, req *kmpb.GetFileFEKRequest) (*kmpb.GetFileFEKResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubKeyManager) FetchCompanyKEK(ctx context.Context, req *kmpb.FetchCompanyKEKRequest) (*kmpb.FetchCompanyKEKResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubKeyManager) ProvisionCompanyKEK(ctx context.Context, req *kmpb.ProvisionCompanyKEKRequest) (*kmpb.ProvisionCompanyKEKResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubKeyManager) GetPermissionGeneration(ctx context.Context, req *kmpb.GetPermissionGenerationRequest) (*kmpb.GetPermissionGenerationResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubKeyManager) GetSTSCredentials(ctx context.Context, req *kmpb.GetSTSCredentialsRequest) (*kmpb.GetSTSCredentialsResponse, error) {
	s.lastSTSUser = req.UserId
	s.lastSTSCompany = req.CompanyId
	return s.stsResp, nil
}

func (s *stubKeyManager) GetNodeSTSCredentials(ctx context.Context, req *kmpb.GetNodeSTSCredentialsRequest) (*kmpb.GetSTSCredentialsResponse, error) {
	s.lastNodeCompany = req.CompanyId
	s.lastNodeID = req.NodeId
	return s.stsResp, nil
}

func (s *stubKeyManager) RotateFileFEK(ctx context.Context, req *kmpb.RotateFileFEKRequest) (*kmpb.RotateFileFEKResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubKeyManager) Close() error { return nil }
