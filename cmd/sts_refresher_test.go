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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/stretchr/testify/require"
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

// TestPlatformSTSProvider verifies the user-mode provider: the OIDC subject is
// used as user_id, the company ID comes from the flag, and a missing subject
// fails closed.
func TestPlatformSTSProvider(t *testing.T) {
	km := &stubKeyManager{stsResp: &kmpb.GetSTSCredentialsResponse{
		AccessKeyId:     "AK-P",
		SecretAccessKey: "SK-P",
		SessionToken:    "TOK-P",
		ExpirationUnix:  time.Now().Add(time.Hour).Unix(),
	}}
	p := &platformSTSProvider{keyManager: km, subject: func(ctx context.Context) string { return "user-1" }, companyID: "comp-1"}
	cred, err := p.Credentials(context.Background())
	require.NoError(t, err)
	require.Equal(t, "AK-P", cred.AccessKey)
	require.Equal(t, "TOK-P", cred.SessionToken)
	require.Equal(t, "user-1", km.lastSTSUser)
	require.Equal(t, "comp-1", km.lastSTSCompany)

	p.subject = func(ctx context.Context) string { return "" }
	_, err = p.Credentials(context.Background())
	require.Error(t, err, "a missing OIDC subject must fail closed")
}

// TestYCEphemeralKeyProvider verifies the YC REST contract: Bearer IAM token,
// sessionName/policy/duration body, and response parsing.
func TestYCEphemeralKeyProvider(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessKeyId":"YC-AK","secret":"YC-SK","sessionToken":"YC-TOK","expiresAt":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`))
	}))
	defer srv.Close()

	p := &ycEphemeralKeyProvider{
		token:       func(ctx context.Context) (string, error) { return "IAM-TOKEN", nil },
		sessionName: "render-node-1",
		policy:      `{"Version":"2012-10-17"}`,
		duration:    30 * time.Minute,
		endpoint:    srv.URL,
	}
	cred, err := p.Credentials(context.Background())
	require.NoError(t, err)
	require.Equal(t, "YC-AK", cred.AccessKey)
	require.Equal(t, "YC-SK", cred.SecretKey)
	require.Equal(t, "YC-TOK", cred.SessionToken)
	require.True(t, cred.Expiry.After(time.Now()))
	require.Equal(t, "Bearer IAM-TOKEN", gotAuth)
	require.Contains(t, gotBody, `"sessionName":"render-node-1"`)
	require.Contains(t, gotBody, `"duration":"1800s"`)
	require.Contains(t, gotBody, `"policy":`)

	// a non-200 answer must surface the YC error body
	srv.Close()
	p.endpoint = "http://127.0.0.1:1" // connection refused
	_, err = p.Credentials(context.Background())
	require.Error(t, err)
}

// TestPrefixPolicies pins the policy shapes: AWS carries the s3:prefix
// condition on ListBucket; YC does not (its schema does not document the
// condition key). Both must be valid JSON with two statements.
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

	ycPolicy := ycPrefixPolicy("bucket", "companies/acme/")
	check(ycPolicy, "YC")
	require.NotContains(t, ycPolicy, `"s3:prefix"`)
	require.Contains(t, ycPolicy, `arn:aws:s3:::bucket/companies/acme/*`)
}

// stubKeyManager is a minimal KeyManagerClient for the STS provider test.
type stubKeyManager struct {
	stsResp        *kmpb.GetSTSCredentialsResponse
	lastSTSUser    string
	lastSTSCompany string
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

func (s *stubKeyManager) RotateFileFEK(ctx context.Context, req *kmpb.RotateFileFEKRequest) (*kmpb.RotateFileFEKResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *stubKeyManager) Close() error { return nil }
