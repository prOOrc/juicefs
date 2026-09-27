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
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	stssdk "github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
)

// stsCredentials is a short-lived cloud storage credential set (task 7.4).
type stsCredentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Expiry       time.Time
}

func (c stsCredentials) valid(now time.Time) bool {
	return now.Before(c.Expiry)
}

// stsCredentialsProvider fetches a fresh credential set.
type stsCredentialsProvider interface {
	Credentials(ctx context.Context) (stsCredentials, error)
}

// stsProxyClient is the minimal surface of the meta proxy's StsProxyService
// the user-mode provider needs (the generated pb.StsProxyServiceClient and the
// authenticated wrapper from grpcMeta.StsProxyClient both satisfy it).
type stsProxyClient interface {
	GetSTSCredentials(ctx context.Context, req *pb.StsCredentialsRequest, opts ...grpc.CallOption) (*pb.StsCredentialsResponse, error)
}

// platformSTSProvider obtains user-scoped STS credentials through the meta
// proxy's StsProxyService pass-through (task 7.12, ADR-003). The proxy fills
// user_id from the authenticated OIDC session — the client never names the
// identity; the company ID comes from --company-id.
type platformSTSProvider struct {
	stsProxy  stsProxyClient
	companyID string
}

func (p *platformSTSProvider) Credentials(ctx context.Context) (stsCredentials, error) {
	resp, err := p.stsProxy.GetSTSCredentials(ctx, &pb.StsCredentialsRequest{
		CompanyId: p.companyID,
	})
	if err != nil {
		return stsCredentials{}, fmt.Errorf("sts: GetSTSCredentials: %w", err)
	}
	return stsCredentials{
		AccessKey:    resp.GetAccessKeyId(),
		SecretKey:    resp.GetSecretAccessKey(),
		SessionToken: resp.GetSessionToken(),
		Expiry:       time.Unix(resp.GetExpirationUnix(), 0),
	}, nil
}

// awsAssumeRoleProvider obtains render-node credentials by assuming an IAM role
// with the node's instance profile (render mode, AWS). The inline session
// policy restricts the role to the volume prefix of the bucket (A6).
type awsAssumeRoleProvider struct {
	roleARN     string
	sessionName string
	bucket      string
	prefix      string
	duration    time.Duration
	loadConfig  func(ctx context.Context, opts ...func(*awsconfig.LoadOptions) error) (aws.Config, error)
}

func (p *awsAssumeRoleProvider) Credentials(ctx context.Context) (stsCredentials, error) {
	load := p.loadConfig
	if load == nil {
		load = awsconfig.LoadDefaultConfig
	}
	cfg, err := load(ctx)
	if err != nil {
		return stsCredentials{}, fmt.Errorf("sts: load AWS config: %w", err)
	}
	client := stssdk.NewFromConfig(cfg)
	provider := stscreds.NewAssumeRoleProvider(client, p.roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = p.sessionName
		o.Duration = p.duration
		o.Policy = aws.String(awsPrefixPolicy(p.bucket, p.prefix))
	})
	cred, err := provider.Retrieve(ctx)
	if err != nil {
		return stsCredentials{}, fmt.Errorf("sts: AssumeRole %s: %w", p.roleARN, err)
	}
	expiry := time.Now().Add(defaultSTSTTL)
	if cred.CanExpire && !cred.Expires.IsZero() {
		expiry = cred.Expires
	}
	return stsCredentials{
		AccessKey:    cred.AccessKeyID,
		SecretKey:    cred.SecretAccessKey,
		SessionToken: cred.SessionToken,
		Expiry:       expiry,
	}, nil
}

// awsPrefixPolicy builds an AWS inline session policy restricting the role to
// object operations under bucket/prefix and ListBucket on the bucket (mirrors
// the platform's BuildSessionPolicy).
func awsPrefixPolicy(bucket, prefix string) string {
	policy := map[string]interface{}{
		"Version": "2012-10-17",
		"Statement": []map[string]interface{}{
			{
				"Effect":   "Allow",
				"Action":   []string{"s3:GetObject", "s3:PutObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploads", "s3:ListParts"},
				"Resource": []string{fmt.Sprintf("arn:aws:s3:::%s/%s*", bucket, prefix)},
			},
			{
				"Effect":   "Allow",
				"Action":   []string{"s3:ListBucket"},
				"Resource": []string{fmt.Sprintf("arn:aws:s3:::%s", bucket)},
				"Condition": map[string]interface{}{
					"StringLike": map[string][]string{"s3:prefix": {prefix + "*"}},
				},
			},
		},
	}
	b, _ := json.Marshal(policy)
	return string(b)
}

// defaultSTSTTL is the refresh period for STS credentials; the platform issues
// 60-minute sessions (task 7.2), so refreshing at half of that keeps a margin.
const defaultSTSTTL = 30 * time.Minute

// stsRefreshFailures counts failed STS refresh calls (task 10.4, FR-REV-3).
var stsRefreshFailures = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "sts_refresh_failures",
	Help: "number of failed STS credential refresh attempts",
})

// stsRefresher keeps the storage credential set fresh: it fetches on start
// (fail-fast — the mount must not fall back to static credentials) and then
// refreshes every ttl/2. On a refresh failure the previous credentials stay in
// place until they expire (fail-safe, FR-REV-3); once they are expired the
// situation is logged loudly exactly once.
type stsRefresher struct {
	provider  stsCredentialsProvider
	storage   *storageHolder
	ttl       time.Duration
	now       func() time.Time
	newTicker func(d time.Duration) *time.Ticker
	stop      chan struct{}
	done      chan struct{}

	mu      sync.Mutex
	current stsCredentials
	expired bool
}

func newSTSRefresher(provider stsCredentialsProvider, storage *storageHolder, ttl time.Duration) *stsRefresher {
	if ttl <= 0 {
		ttl = defaultSTSTTL
	}
	return &stsRefresher{
		provider:  provider,
		storage:   storage,
		ttl:       ttl,
		now:       time.Now,
		newTicker: time.NewTicker,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Start fetches the initial credential set (fail-fast on error) and starts the
// refresh loop.
func (r *stsRefresher) Start(ctx context.Context) error {
	cred, err := r.provider.Credentials(ctx)
	if err != nil {
		return fmt.Errorf("sts: initial credentials: %w", err)
	}
	if err := r.swap(cred); err != nil {
		return err
	}
	go r.loop()
	return nil
}

// Stop terminates the refresh loop.
func (r *stsRefresher) Stop() {
	close(r.stop)
	<-r.done
}

func (r *stsRefresher) loop() {
	defer close(r.done)
	ticker := r.newTicker(r.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.refresh()
		}
	}
}

func (r *stsRefresher) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cred, err := r.provider.Credentials(ctx)
	if err != nil {
		stsRefreshFailures.Inc()
		logger.Warnf("STS refresh failed: %v (keeping current credentials until expiry)", err)
		r.checkExpiry()
		return
	}
	if err := r.swap(cred); err != nil {
		logger.Warnf("STS credential swap failed: %v", err)
		r.checkExpiry()
		return
	}
	r.mu.Lock()
	r.expired = false
	r.mu.Unlock()
	r.checkExpiry() // a provider returning already-expired credentials still alarms
}

// checkExpiry reports once that the current credential set is expired and no
// refresh has succeeded since.
func (r *stsRefresher) checkExpiry() {
	r.mu.Lock()
	valid := r.current.valid(r.now())
	r.mu.Unlock()
	if !valid {
		r.logExpired()
	}
}

// swap installs a fresh credential set into the storage holder.
func (r *stsRefresher) swap(cred stsCredentials) error {
	if err := r.storage.SetCredentials(cred.AccessKey, cred.SecretKey, cred.SessionToken); err != nil {
		return err
	}
	r.mu.Lock()
	r.current = cred
	r.mu.Unlock()
	logger.Infof("STS credentials refreshed (expiry %s)", cred.Expiry.Format(time.RFC3339))
	return nil
}

// logExpired reports once that the current credential set is expired and no
// refresh has succeeded since.
func (r *stsRefresher) logExpired() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.expired {
		return
	}
	r.expired = true
	logger.Errorf("STS credentials are EXPIRED and no refresh has succeeded: storage I/O will fail until the next successful refresh")
}
