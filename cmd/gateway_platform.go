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
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/urfave/cli/v2"
)

// Environment contract of the gateway platform mode (OpenSpec change
// add-gateway-platform-mode, Component Map): the deployment (agio-cloud)
// provides the values; secrets are injected from k8s Secrets and must never
// be logged — startup logs carry the key names only. AGIO_AUTHZ_*_TLS_* and
// AGIO_KEYMANAGER_TLS_* are optional (consumed by the minio fork) and are not
// validated here.
const (
	// envAgioGatewayPlatform - master switch consumed by the embedded minio
	// server in the same process ("1" enables platform mode).
	envAgioGatewayPlatform = "AGIO_GATEWAY_PLATFORM"

	// envAgioS3ServiceToken - raw service token presented to the meta proxy
	// (x-service-credential). Secret.
	envAgioS3ServiceToken = "AGIO_S3_SERVICE_TOKEN"

	// envAgioKeymanagerAddr - address of the platform keymanager service.
	envAgioKeymanagerAddr = "AGIO_KEYMANAGER_ADDR"

	// envAgioKeymanagerNodeCredential - raw node credential of the gateway
	// towards the keymanager platform. Secret.
	envAgioKeymanagerNodeCredential = "AGIO_KEYMANAGER_NODE_CREDENTIAL"

	// envAgioAuthzAddr - address of the platform authz service (mTLS).
	envAgioAuthzAddr = "AGIO_AUTHZ_ADDR"

	// envAgioAuthzVolumeName - volume name the authz service resolves paths for.
	envAgioAuthzVolumeName = "AGIO_AUTHZ_VOLUME_NAME"

	// envAgioFacilityID - facility this gateway serves (identifier, not a secret).
	envAgioFacilityID = "AGIO_FACILITY_ID"

	// envAgioS3Bucket - the single bucket name served in platform mode.
	envAgioS3Bucket = "AGIO_S3_BUCKET"

	// legacy root credentials of the regular gateway mode; their presence in
	// platform mode is a configuration error (a root key does not exist there).
	envMinioRootUser     = "MINIO_ROOT_USER"
	envMinioRootPassword = "MINIO_ROOT_PASSWORD"

	// platformProbeTimeout bounds the startup GetFormat probe: a proxy that
	// neither accepts nor refuses (black-holed network) must not hang the
	// startup indefinitely.
	platformProbeTimeout = 30 * time.Second
)

// platformEnvKeys lists the mandatory environment keys of the platform mode
// in reporting order (spec "Platform mode startup inputs": service token,
// keymanager address, node credential, authz address, authz volume name,
// facility id, bucket name).
var platformEnvKeys = []string{
	envAgioS3ServiceToken,
	envAgioKeymanagerAddr,
	envAgioKeymanagerNodeCredential,
	envAgioAuthzAddr,
	envAgioAuthzVolumeName,
	envAgioFacilityID,
	envAgioS3Bucket,
}

// validatePlatformStartup enforces the platform mode startup contract and
// returns an error naming the offending key without ever quoting values:
// --subdir and --refresh-iam-interval are refused (spec "Platform mode
// forbidden legacy surfaces"), MINIO_ROOT_* in the environment is refused
// (a root key does not exist in platform mode) and every mandatory AGIO_*
// key must be present.
func validatePlatformStartup(c *cli.Context) error {
	if c.IsSet("subdir") {
		return fmt.Errorf("--subdir is not allowed in platform mode: the authz path contract requires full paths from the volume root")
	}
	if c.IsSet("refresh-iam-interval") {
		return fmt.Errorf("--refresh-iam-interval is not allowed in platform mode: the built-in IAM subsystem is suppressed")
	}
	for _, key := range []string{envMinioRootUser, envMinioRootPassword} {
		if os.Getenv(key) != "" {
			return fmt.Errorf("%s is set: a root key does not exist in platform mode", key)
		}
	}
	for _, key := range platformEnvKeys {
		if os.Getenv(key) == "" {
			return fmt.Errorf("mandatory environment variable %s is empty or unset", key)
		}
	}
	return nil
}

// enablePlatformMasterSwitch turns the platform mode master switch on for the
// embedded minio server, which runs in this same process and reads it from the
// environment (the --platform flag is the CLI-level decision, the env var is
// the in-process one). Idempotent with the deployment-provided value.
func enablePlatformMasterSwitch() {
	_ = os.Setenv(envAgioGatewayPlatform, "1")
}

// withServiceCredentialEnv appends the service-credential-env query parameter
// to the meta URL so the grpc meta client attaches x-service-credential
// metadata on every outgoing RPC. Only the NAME of the env variable holding
// the token goes into the URL (it is logged and lands in the process title) —
// never the token itself. An unparsable URL is returned unchanged so the
// regular meta URL parsing reports it.
func withServiceCredentialEnv(metaUrl, envName string) string {
	u, err := url.Parse(metaUrl)
	if err != nil {
		return metaUrl
	}
	q := u.Query()
	q.Set("service-credential-env", envName)
	u.RawQuery = q.Encode()
	return u.String()
}

// probePlatformService runs the startup service connectivity check (spec
// "Platform mode service connectivity check"): a GetFormat RPC from the
// service identity before any S3 surface is opened. It requires a grpc meta
// client (any other meta engine fails the probe, so the startup aborts).
func probePlatformService(metaCli meta.Meta) error {
	prober, ok := metaCli.(interface {
		ProbeGetFormat(ctx context.Context) error
	})
	if !ok {
		return fmt.Errorf("meta engine %T does not support the GetFormat probe: platform mode requires a grpc meta URL", metaCli)
	}
	ctx, cancel := context.WithTimeout(context.Background(), platformProbeTimeout)
	defer cancel()
	if err := prober.ProbeGetFormat(ctx); err != nil {
		return fmt.Errorf("service connectivity check failed: %w", err)
	}
	return nil
}

// platformEnvKeyNames returns the mandatory key names for startup logging
// (names only — values must never be logged).
func platformEnvKeyNames() string {
	return strings.Join(platformEnvKeys, ", ")
}
