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
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	kmpb "github.com/juicedata/juicefs/pkg/meta/keymanager_pb"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/utils"
)

func parseRenderFlags(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	var ctx *cli.Context
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "attr-cache", Value: "1.0s"},
			&cli.StringFlag{Name: "entry-cache", Value: "1.0s"},
			&cli.StringFlag{Name: "dir-entry-cache", Value: "1.0s"},
		},
		Action: func(c *cli.Context) error {
			ctx = c
			return nil
		},
	}
	require.NoError(t, app.Run(append([]string{"juicefs"}, args...)))
	return ctx
}

// TestForceRenderCacheTimeouts_DefaultsRaised (task 4.3): without explicit
// operator values the kernel-side cache timeouts are raised to at least 60s.
func TestForceRenderCacheTimeouts_DefaultsRaised(t *testing.T) {
	c := parseRenderFlags(t)
	forceRenderCacheTimeouts(c)
	for _, name := range []string{"attr-cache", "entry-cache", "dir-entry-cache"} {
		require.GreaterOrEqual(t, utils.Duration(c.String(name)), renderCacheTimeout, name)
	}
}

// TestForceRenderCacheTimeouts_ExplicitRespected: an explicit operator value is
// kept as-is (even below 60s); the other flags are still raised.
func TestForceRenderCacheTimeouts_ExplicitRespected(t *testing.T) {
	c := parseRenderFlags(t, "--attr-cache", "5s")
	forceRenderCacheTimeouts(c)
	require.Equal(t, 5*time.Second, utils.Duration(c.String("attr-cache")))
	require.GreaterOrEqual(t, utils.Duration(c.String("entry-cache")), renderCacheTimeout)
	require.GreaterOrEqual(t, utils.Duration(c.String("dir-entry-cache")), renderCacheTimeout)
}

// nodeTokenCaptureServer is a bufconn KeyManager that answers the node-token
// bootstrap RPCs and records the raw credential metadata of each call.
type nodeTokenCaptureServer struct {
	kmpb.UnimplementedDriveKeyManagerServiceServer
	kek          []byte
	pair         *kmpb.GetNodeStorageCredentialsResponse
	gotFetchCred string
	gotNodeCred  string
}

func (s *nodeTokenCaptureServer) metadataCred(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(meta.NodeCredentialMetadataKey); len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

func (s *nodeTokenCaptureServer) FetchCompanyKEK(ctx context.Context, req *kmpb.FetchCompanyKEKRequest) (*kmpb.FetchCompanyKEKResponse, error) {
	s.gotFetchCred = s.metadataCred(ctx)
	return &kmpb.FetchCompanyKEKResponse{Kek: s.kek, KekVersion: 3}, nil
}

func (s *nodeTokenCaptureServer) GetNodeStorageCredentials(ctx context.Context, req *kmpb.GetNodeStorageCredentialsRequest) (*kmpb.GetNodeStorageCredentialsResponse, error) {
	s.gotNodeCred = s.metadataCred(ctx)
	return s.pair, nil
}

// TestRenderMountNodeCredentialBootstrap (add-render-selectel-company-keys,
// task 2.2): the node-token bootstrap helpers authenticate both RPCs with the
// raw node credential in the x-node-credential metadata (no Bearer scheme —
// the platform interceptor reads the bare value) and map KEK and static pair.
func TestRenderMountNodeCredentialBootstrap(t *testing.T) {
	kek := make([]byte, renderKEKSize)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	srv := &nodeTokenCaptureServer{
		kek: kek,
		pair: &kmpb.GetNodeStorageCredentialsResponse{
			AccessKeyId:     "AK-SEL",
			SecretAccessKey: "SK-SEL",
			VolumeName:      "render-vol",
			Bucket:          "render-bucket",
		},
	}
	lis := bufconn.Listen(1024 * 1024)
	defer lis.Close()
	gs := grpc.NewServer()
	kmpb.RegisterDriveKeyManagerServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	dialer := func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(dialer), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	km := &bufconnKeyManager{client: kmpb.NewDriveKeyManagerServiceClient(conn), conn: conn}

	tokenFile := filepath.Join(t.TempDir(), "node-credential")
	require.NoError(t, os.WriteFile(tokenFile, []byte("test-node-token\n"), 0o600))
	p, err := newNodeCredentialFileProvider(tokenFile)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	gotKEK, kekVersion, err := fetchCompanyKEKNodeToken(ctx, km, p, "comp-1", "render-node-1")
	require.NoError(t, err)
	require.Equal(t, kek, gotKEK)
	require.EqualValues(t, 3, kekVersion)

	creds, err := getNodeStorageCredentials(ctx, km, p, "comp-1", "render-node-1")
	require.NoError(t, err)
	require.Equal(t, "AK-SEL", creds.accessKey)
	require.Equal(t, "SK-SEL", creds.secretKey)
	require.Equal(t, "render-vol", creds.volumeName)
	require.Equal(t, "render-bucket", creds.bucket)

	require.Equal(t, "test-node-token", srv.gotFetchCred, "the node credential must reach FetchCompanyKEK as the raw x-node-credential value")
	require.Equal(t, "test-node-token", srv.gotNodeCred, "the node credential must reach GetNodeStorageCredentials as the raw x-node-credential value")
}

// TestRenderMountNodeCredentialFileProvider pins the fail-closed file checks:
// the credential file must exist and be owner-readable only (0600).
func TestRenderMountNodeCredentialFileProvider(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{"owner only", 0o600, false},
		{"group readable", 0o640, true},
		{"world readable", 0o644, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name)
			require.NoError(t, os.WriteFile(path, []byte("test-node-token"), c.mode))
			p, err := newNodeCredentialFileProvider(path)
			if c.wantErr {
				require.Error(t, err)
				require.Nil(t, p)
			} else {
				require.NoError(t, err)
				token, err := p.Token(context.Background())
				require.NoError(t, err)
				require.Equal(t, "test-node-token", token)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		_, err := newNodeCredentialFileProvider(filepath.Join(dir, "absent"))
		require.Error(t, err)
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(dir, "empty")
		require.NoError(t, os.WriteFile(path, []byte("  \n"), 0o600))
		p, err := newNodeCredentialFileProvider(path)
		require.NoError(t, err)
		_, err = p.Token(context.Background())
		require.Error(t, err)
	})
}

// TestRenderMountNodeCredentialRejected pins the fail-closed bootstrap behavior:
// a platform Unauthenticated (unknown/revoked node credential) surfaces with its
// gRPC status class intact through the wrapped bootstrap error.
func TestRenderMountNodeCredentialRejected(t *testing.T) {
	srv := &nodeTokenUnauthenticatedServer{}
	lis := bufconn.Listen(1024 * 1024)
	defer lis.Close()
	gs := grpc.NewServer()
	kmpb.RegisterDriveKeyManagerServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	dialer := func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(dialer), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	km := &bufconnKeyManager{client: kmpb.NewDriveKeyManagerServiceClient(conn), conn: conn}

	tokenFile := filepath.Join(t.TempDir(), "node-credential")
	require.NoError(t, os.WriteFile(tokenFile, []byte("revoked-token"), 0o600))
	p, err := newNodeCredentialFileProvider(tokenFile)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err = fetchCompanyKEKNodeToken(ctx, km, p, "comp-1", "render-node-1")
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, grpcstatus.Code(err), "the Unauthenticated class must survive the bootstrap wrapping")
}

// nodeTokenUnauthenticatedServer rejects every guarded RPC like the platform
// interceptor does for an unknown/revoked credential.
type nodeTokenUnauthenticatedServer struct {
	kmpb.UnimplementedDriveKeyManagerServiceServer
}

func (s *nodeTokenUnauthenticatedServer) FetchCompanyKEK(ctx context.Context, req *kmpb.FetchCompanyKEKRequest) (*kmpb.FetchCompanyKEKResponse, error) {
	return nil, grpcstatus.Error(codes.Unauthenticated, "invalid or revoked node credential")
}

// TestRenderStaticKeysFromPlatform (add-render-selectel-company-keys,
// domain-encrypt "Render path"): the platform-delivered static pair replaces
// the volume-format credentials, the delivered volume/bucket win over the
// Format, and the patch composition handed to NewReloadableStorage re-stamps
// them on every format reload — a refresh from Redis (which carries no platform
// keys) must not clobber the mount credentials.
func TestRenderStaticKeysFromPlatform(t *testing.T) {
	cases := []struct {
		name   string
		format *meta.Format
		creds  *nodeStorageCredentials
		want   meta.Format
	}{
		{
			// (а) the platform pair is the complete credential set
			name:   "pair replaces format keys and resets stale session token",
			format: &meta.Format{Name: "fmt-vol", Bucket: "fmt-bucket", AccessKey: "fmt-ak", SecretKey: "fmt-sk", SessionToken: "fmt-token"},
			creds:  &nodeStorageCredentials{accessKey: "plat-ak", secretKey: "plat-sk", volumeName: "fmt-vol", bucket: "fmt-bucket"},
			want:   meta.Format{Name: "fmt-vol", Bucket: "fmt-bucket", AccessKey: "plat-ak", SecretKey: "plat-sk", SessionToken: ""},
		},
		{
			// (б) a platform answer without the facility scope keeps the Format's
			name:   "empty volume and bucket fall back to the format",
			format: &meta.Format{Name: "fmt-vol", Bucket: "fmt-bucket", AccessKey: "fmt-ak", SecretKey: "fmt-sk"},
			creds:  &nodeStorageCredentials{accessKey: "plat-ak", secretKey: "plat-sk"},
			want:   meta.Format{Name: "fmt-vol", Bucket: "fmt-bucket", AccessKey: "plat-ak", SecretKey: "plat-sk"},
		},
		{
			// (в) the client does not name volume/bucket
			name:   "platform volume and bucket win over the format",
			format: &meta.Format{Name: "fmt-vol", Bucket: "fmt-bucket", AccessKey: "fmt-ak", SecretKey: "fmt-sk"},
			creds:  &nodeStorageCredentials{accessKey: "plat-ak", secretKey: "plat-sk", volumeName: "plat-vol", bucket: "plat-bucket"},
			want:   meta.Format{Name: "plat-vol", Bucket: "plat-bucket", AccessKey: "plat-ak", SecretKey: "plat-sk"},
		},
		{
			// stage acceptance: legacy encrypted storage creds (created by
			// juicefs format with --encrypt-algo) must be sanitized away —
			// Format.Decrypt would FATAL on a render node (no format secret)
			// and a leftover EncryptKey would enable the legacy RSA data
			// encryptor on top of the AGIO FEK/CEK data path
			name: "format with legacy encrypted storage creds is sanitized",
			format: &meta.Format{
				Name: "fmt-vol", Bucket: "fmt-bucket",
				AccessKey: "enc-ak-ciphertext", SecretKey: "enc-sk-ciphertext", SessionToken: "enc-token-ciphertext",
				EncryptKey: "encrypted-format-secret", EncryptAlgo: "aes-256-gcm", KeyEncrypted: true,
			},
			creds: &nodeStorageCredentials{accessKey: "plat-ak", secretKey: "plat-sk", volumeName: "fmt-vol", bucket: "fmt-bucket"},
			want: meta.Format{
				Name: "fmt-vol", Bucket: "fmt-bucket",
				AccessKey: "plat-ak", SecretKey: "plat-sk",
			},
		},
		{
			// negative: without platform credentials the legacy fields must not
			// be touched (the guard only fires when the pair is delivered)
			name: "nil credentials keep the format untouched",
			format: &meta.Format{
				Name: "fmt-vol", Bucket: "fmt-bucket",
				AccessKey: "enc-ak-ciphertext", SecretKey: "enc-sk-ciphertext", SessionToken: "enc-token-ciphertext",
				EncryptKey: "encrypted-format-secret", EncryptAlgo: "aes-256-gcm", KeyEncrypted: true,
			},
			creds: nil,
			want: meta.Format{
				Name: "fmt-vol", Bucket: "fmt-bucket",
				AccessKey: "enc-ak-ciphertext", SecretKey: "enc-sk-ciphertext", SessionToken: "enc-token-ciphertext",
				EncryptKey: "encrypted-format-secret", EncryptAlgo: "aes-256-gcm", KeyEncrypted: true,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyNodeStorageCredentials(c.format, c.creds)
			require.Equal(t, c.want, *c.format)
		})
	}

	// (г) reload composition: render-mount hands staticCredentialsPatch to
	// NewReloadableStorage, which applies it to every format delivered by
	// OnReload before the holder comparison. A fresh Redis format (no keys)
	// must come out identical to the holder copy — the storage is not rebuilt
	// with stale Redis credentials and the platform keys survive.
	t.Run("reload from redis does not clobber platform credentials", func(t *testing.T) {
		c := parseRenderFlags(t)
		creds := &nodeStorageCredentials{accessKey: "plat-ak", secretKey: "plat-sk", volumeName: "plat-vol", bucket: "plat-bucket"}
		patch := staticCredentialsPatch(updateFormat(c), creds)

		// a format as Load(true) delivers it on the stage: identity and scope,
		// legacy encrypted storage credentials from the platform-side format,
		// no plain keys; Tiers non-nil like a real volume format
		redisFormat := func() *meta.Format {
			return (&meta.Format{
				Name: "redis-vol", Bucket: "redis-bucket", Tiers: object.Tiers{},
				AccessKey: "enc-ak-ciphertext", SecretKey: "enc-sk-ciphertext",
				EncryptKey: "encrypted-format-secret", EncryptAlgo: "aes-256-gcm", KeyEncrypted: true,
			}).Clone()
		}

		bootstrap := redisFormat()
		patch(bootstrap) // the holder's fmt after NewReloadableStorage

		reload := redisFormat()
		patch(reload) // what OnReload stamps onto a fresh Load(true)
		require.Equal(t, *bootstrap, *reload, "a patched reload must match the holder copy: no storage swap, platform credentials intact")
		require.Equal(t, "plat-ak", reload.AccessKey)
		require.Equal(t, "plat-sk", reload.SecretKey)
		require.Empty(t, reload.SessionToken)
		require.Equal(t, "plat-vol", reload.Name)
		require.Equal(t, "plat-bucket", reload.Bucket)
		require.False(t, reload.KeyEncrypted, "Format.Decrypt must never trigger on a render node (no format secret)")
		require.Empty(t, reload.EncryptKey, "no legacy data encryptor on top of the AGIO FEK/CEK data path")
		require.Empty(t, reload.EncryptAlgo)

		// Without the platform patch the same reload keeps the legacy encrypted
		// state (the stage FATAL: Format.Decrypt with no format secret on the
		// node) and never delivers plain credentials.
		plain := redisFormat()
		updateFormat(c)(plain)
		require.True(t, plain.KeyEncrypted)
		require.Equal(t, "enc-ak-ciphertext", plain.AccessKey)
		require.Equal(t, "encrypted-format-secret", plain.EncryptKey)
	})
}

// parseRenderModeFlags builds a cli.Context from the real render-mount flag
// definitions so the mode validation sees the actual flag names.
func parseRenderModeFlags(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	var ctx *cli.Context
	app := &cli.App{
		Flags: renderMountFlags(),
		Action: func(c *cli.Context) error {
			ctx = c
			return nil
		},
	}
	require.NoError(t, app.Run(append([]string{"juicefs"}, args...)))
	return ctx
}

// TestRenderMountModeFlags pins the fail-closed mode selection (design D3): the
// node-token mode never mixes with the YC IAM file or STS.
func TestRenderMountModeFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"node token alone", []string{"--node-credential-file", "/tmp/node-cred"}, ""},
		{"iam file alone", []string{"--iam-token-file", "/tmp/iam"}, ""},
		{"sts alone", []string{"--sts-enabled"}, ""},
		{"nothing set", nil, ""},
		{"node token with sts", []string{"--node-credential-file", "/tmp/node-cred", "--sts-enabled"}, "--node-credential-file cannot be combined with --sts-enabled"},
		{"node token with iam file", []string{"--node-credential-file", "/tmp/node-cred", "--iam-token-file", "/tmp/iam"}, "--node-credential-file cannot be combined with --iam-token-file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateRenderModeFlags(parseRenderModeFlags(t, c.args...))
			if c.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, c.wantErr)
			}
		})
	}
}
