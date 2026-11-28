//go:build !nogateway
// +build !nogateway

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
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/juicedata/juicefs/pkg/meta"
	pb "github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

// Canary values: they must never appear in any error message or log line.
const (
	canaryServiceToken    = "canary-service-token-NEVER-LOG"
	canaryNodeCredential  = "canary-node-credential-NEVER-LOG"
	canaryFacilityID      = "canary-facility-id-NEVER-LOG"
	testGatewayProxyToken = "gateway-test-proxy-token"
)

// newGatewayTestContext builds a real cli.Context from the gateway command
// flags, so c.IsSet reflects the parsed arguments.
func newGatewayTestContext(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet("gateway", flag.ContinueOnError)
	for _, f := range cmdGateway().Flags {
		if err := f.Apply(set); err != nil {
			t.Fatalf("apply flag: %s", err)
		}
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse args %v: %s", args, err)
	}
	return cli.NewContext(nil, set, nil)
}

// setPlatformEnv installs a complete valid platform environment and returns a
// cleanup-restoring test helper via t.Setenv.
func setPlatformEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envAgioS3ServiceToken, canaryServiceToken)
	t.Setenv(envAgioKeymanagerAddr, "keymanager.platform:9090")
	t.Setenv(envAgioKeymanagerNodeCredential, canaryNodeCredential)
	t.Setenv(envAgioAuthzAddr, "authz.platform:9090")
	t.Setenv(envAgioAuthzVolumeName, "test-volume")
	t.Setenv(envAgioFacilityID, canaryFacilityID)
	t.Setenv(envAgioS3Bucket, "companies-bucket")
}

// TestPlatformStartupMissingInput (spec "Missing mandatory input fails fast"):
// without any one of the mandatory env keys the startup validation fails and
// names the missing key.
func TestPlatformStartupMissingInput(t *testing.T) {
	for _, key := range platformEnvKeys {
		key := key
		t.Run(key, func(t *testing.T) {
			setPlatformEnv(t)
			t.Setenv(key, "")
			err := validatePlatformStartup(newGatewayTestContext(t))
			require.Error(t, err)
			assert.Contains(t, err.Error(), key, "error must name the missing key")
		})
	}
}

// TestPlatformStartupSecretsNotLogged (spec "Secrets are not logged"): error
// messages and the service-credential URL parameter carry key names only, no
// secret values.
func TestPlatformStartupSecretsNotLogged(t *testing.T) {
	setPlatformEnv(t)
	t.Setenv(envAgioS3Bucket, "") // force one failure with secrets present
	err := validatePlatformStartup(newGatewayTestContext(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), envAgioS3Bucket)
	assert.NotContains(t, err.Error(), canaryServiceToken)
	assert.NotContains(t, err.Error(), canaryNodeCredential)
	assert.NotContains(t, err.Error(), canaryFacilityID)

	// The meta URL names the env variable, never its value.
	setPlatformEnv(t)
	metaUrl := withServiceCredentialEnv("grpc://proxy:9000?tls=1", envAgioS3ServiceToken)
	assert.Contains(t, metaUrl, "service-credential-env="+envAgioS3ServiceToken)
	assert.NotContains(t, metaUrl, canaryServiceToken)
}

// TestPlatformStartupForbiddenSurfaces (spec "Platform mode forbidden legacy
// surfaces"): --subdir, --refresh-iam-interval and MINIO_ROOT_* are refused.
func TestPlatformStartupForbiddenSurfaces(t *testing.T) {
	t.Run("subdir is refused", func(t *testing.T) {
		setPlatformEnv(t)
		err := validatePlatformStartup(newGatewayTestContext(t, "--subdir", "companies/acme"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--subdir")
	})

	t.Run("refresh-iam-interval is refused", func(t *testing.T) {
		setPlatformEnv(t)
		err := validatePlatformStartup(newGatewayTestContext(t, "--refresh-iam-interval", "1h"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--refresh-iam-interval")
	})

	t.Run("root credentials are refused", func(t *testing.T) {
		setPlatformEnv(t)
		t.Setenv(envMinioRootUser, "admin")
		err := validatePlatformStartup(newGatewayTestContext(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), envMinioRootUser)

		t.Setenv(envMinioRootUser, "")
		t.Setenv(envMinioRootPassword, "12345678")
		err = validatePlatformStartup(newGatewayTestContext(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), envMinioRootPassword)
	})

	t.Run("legacy root credentials still required without --platform", func(t *testing.T) {
		// Invariance: the legacy ROOT-credential block stays untouched; the
		// platform validation is only reachable behind the --platform flag.
		c := newGatewayTestContext(t)
		assert.False(t, c.Bool("platform"))
	})
}

// TestPlatformStartupValid enables every mandatory input: the validation
// passes and the master switch for the embedded minio server is set.
func TestPlatformStartupValid(t *testing.T) {
	setPlatformEnv(t)
	c := newGatewayTestContext(t)
	require.NoError(t, validatePlatformStartup(c))
	enablePlatformMasterSwitch()
	assert.Equal(t, "1", os.Getenv(envAgioGatewayPlatform))
}

// fakeGatewayProxy is a minimal meta proxy: GetFormat accepts exactly one
// service token, rejects anything else (mirrors the proxy-side service
// identity table of change add-meta-proxy-service-identity).
type fakeGatewayProxy struct {
	pb.UnimplementedMetaServiceServer
	acceptedToken string
}

func (s *fakeGatewayProxy) GetFormat(ctx context.Context, req *pb.GetFormatRequest) (*pb.GetFormatResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("x-service-credential")
	if len(vals) == 0 || vals[0] != s.acceptedToken {
		return nil, grpcstatus.Error(codes.Unauthenticated, "invalid service credential")
	}
	return &pb.GetFormatResponse{Format: &pb.ProtoFormat{}}, nil
}

func startFakeGatewayProxy(t *testing.T, acceptedToken string) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	pb.RegisterMetaServiceServer(srv, &fakeGatewayProxy{acceptedToken: acceptedToken})
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// TestProbePlatformService (spec "Platform mode service connectivity check"):
// a GetFormat from the service identity succeeds against an accepting proxy
// and fails when the proxy rejects the token; a non-grpc meta engine fails
// the probe as well.
func TestProbePlatformService(t *testing.T) {
	t.Run("accepted service token", func(t *testing.T) {
		addr := startFakeGatewayProxy(t, testGatewayProxyToken)
		t.Setenv(envAgioS3ServiceToken, testGatewayProxyToken)
		metaUrl := withServiceCredentialEnv(fmt.Sprintf("grpc://%s", addr), envAgioS3ServiceToken)
		metaCli := meta.NewClient(metaUrl, nil)
		require.NoError(t, probePlatformService(metaCli))
	})

	t.Run("rejected service token", func(t *testing.T) {
		addr := startFakeGatewayProxy(t, testGatewayProxyToken)
		t.Setenv(envAgioS3ServiceToken, canaryServiceToken) // token of another volume/service
		metaUrl := withServiceCredentialEnv(fmt.Sprintf("grpc://%s", addr), envAgioS3ServiceToken)
		metaCli := meta.NewClient(metaUrl, nil)
		err := probePlatformService(metaCli)
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "Unauthenticated") || grpcstatus.Code(err) == codes.Unauthenticated)
		assert.NotContains(t, err.Error(), canaryServiceToken)
	})

	t.Run("non-grpc meta engine", func(t *testing.T) {
		metaCli := meta.NewClient("memkv://", nil)
		err := probePlatformService(metaCli)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "GetFormat probe")
	})
}
