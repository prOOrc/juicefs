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

package meta

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/juicedata/juicefs/pkg/utils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var serviceIdentityLogger = utils.GetLogger("juicefs-service-identity")

// gRPC metadata keys of the service identity mode
// (add-meta-proxy-service-identity): inbound service credential presented by
// headless clients (S3 gateway), outbound node credential attached by the
// keymanager decorator to service-originated FEK calls.
const (
	MetadataServiceCredential = "x-service-credential"
	MetadataNodeCredential    = "x-node-credential"
)

// ServiceIdentityConfig is the parsed proxy-side configuration of the
// service-token auth mode. Constructed by NewServiceIdentityConfig from the
// --service-credential-hash / --node-credential flags.
type ServiceIdentityConfig struct {
	// credentialHash is the decoded SHA-256 of the service credential.
	credentialHash []byte
	// nodeCredential is the raw node token attached to service-originated
	// keymanager calls. Empty when not configured: service connections are
	// then rejected at the door (fail-closed, spec "Missing node credential
	// rejects service connections").
	nodeCredential string
}

// NodeCredential returns the raw node credential (never log it). Nil-safe.
func (c *ServiceIdentityConfig) NodeCredential() string {
	if c == nil {
		return ""
	}
	return c.nodeCredential
}

// NewServiceIdentityConfig validates the service credential hash and resolves
// the node credential from its source ("env:NAME" → environment variable,
// otherwise a file path holding the raw token). A nil config with a nil error
// means the service identity mode is disabled (no hash configured) — the
// x-service-credential header is then ignored and behavior stays OIDC-only.
// Neither the hash nor the resolved token value may be logged by callers.
func NewServiceIdentityConfig(credentialHashHex, nodeCredentialSource string) (*ServiceIdentityConfig, error) {
	if credentialHashHex == "" {
		if nodeCredentialSource != "" {
			return nil, fmt.Errorf("--node-credential is set without --service-credential-hash: the service identity mode is disabled")
		}
		return nil, nil
	}
	credentialHash, err := decodeServiceCredentialHash(credentialHashHex)
	if err != nil {
		return nil, err
	}
	nodeCredential := ""
	if nodeCredentialSource != "" {
		nodeCredential, err = loadNodeCredential(nodeCredentialSource)
		if err != nil {
			return nil, err
		}
	}
	return &ServiceIdentityConfig{
		credentialHash: credentialHash,
		nodeCredential: nodeCredential,
	}, nil
}

// decodeServiceCredentialHash decodes the hex SHA-256 digest of the service
// credential. The digest value is a secret-derived material: error messages
// name the flag, never the value.
func decodeServiceCredentialHash(hexHash string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(hexHash))
	if err != nil {
		return nil, fmt.Errorf("--service-credential-hash is not a valid hex string")
	}
	if len(raw) != sha256.Size {
		return nil, fmt.Errorf("--service-credential-hash must be a SHA-256 hex digest (%d hex characters)", sha256.Size*2)
	}
	return raw, nil
}

// loadNodeCredential resolves the node credential source: "env:NAME" reads an
// environment variable, anything else is a file path holding the raw token
// (design: "файл/env-путь с сырым токеном"). The returned value must never be
// logged; error messages carry only names/paths.
func loadNodeCredential(source string) (string, error) {
	if envName, ok := strings.CutPrefix(source, "env:"); ok {
		value := os.Getenv(envName)
		if value == "" {
			return "", fmt.Errorf("node credential environment variable %s is empty or unset", envName)
		}
		return strings.TrimSpace(value), nil
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return "", fmt.Errorf("read node credential file %s: %w", source, err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("node credential file %s is empty", source)
	}
	return value, nil
}

// serviceDecision is the outcome of the service method table.
type serviceDecision int

const (
	// serviceDecisionPass forwards the request to the handler.
	serviceDecisionPass serviceDecision = iota
	// serviceDecisionDeny rejects the request with PermissionDenied.
	serviceDecisionDeny
)

// ServiceIdentityInterceptor authenticates service-token requests (the first
// link of the interceptor chain, before OIDC) and enforces the service method
// table. Any request carrying x-service-credential is fully decided here:
// there is no fallback to the OIDC path, in either direction.
type ServiceIdentityInterceptor struct {
	cfg *ServiceIdentityConfig
}

// NewServiceIdentityInterceptor creates the interceptor for the given config.
func NewServiceIdentityInterceptor(cfg *ServiceIdentityConfig) *ServiceIdentityInterceptor {
	return &ServiceIdentityInterceptor{cfg: cfg}
}

// UnaryInterceptor returns the unary gRPC server interceptor.
func (si *ServiceIdentityInterceptor) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		authCtx, decision, err := si.authenticate(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		if decision == serviceDecisionDeny {
			return nil, status.Error(codes.PermissionDenied, "access denied: admin operations are not available for service identity")
		}
		return handler(authCtx, req)
	}
}

// StreamInterceptor returns the streaming gRPC server interceptor. The same
// method table applies, so the streaming admin surface (DumpMeta/LoadMeta and
// the scan streams) is denied for service identities too — closing the
// streaming authz gap for service connections.
func (si *ServiceIdentityInterceptor) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		authCtx, decision, err := si.authenticate(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		if decision == serviceDecisionDeny {
			return status.Error(codes.PermissionDenied, "access denied: admin operations are not available for service identity")
		}
		if authCtx != ss.Context() {
			// Authenticated as service: propagate the identity kind downstream.
			ss = &serviceIdentityServerStream{ServerStream: ss, ctx: authCtx}
		}
		return handler(srv, ss)
	}
}

// authenticate decides a single request: no service header → pass through
// untouched (legacy OIDC path); service header → authenticate it against the
// configured hash (both-headers and mismatch reject with Unauthenticated
// without fallback) and resolve the method table. A returned nil ctx always
// pairs with a non-nil error.
func (si *ServiceIdentityInterceptor) authenticate(ctx context.Context, fullMethod string) (context.Context, serviceDecision, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx, serviceDecisionPass, nil
	}
	svcVals := md.Get(MetadataServiceCredential)
	if len(svcVals) == 0 {
		// No service credential — the request keeps the OIDC path byte-identical.
		return ctx, serviceDecisionPass, nil
	}
	if si.cfg == nil || len(si.cfg.credentialHash) == 0 {
		// Service mode not configured: the header is ignored (spec) — behavior
		// identical to a proxy without this interceptor.
		return ctx, serviceDecisionPass, nil
	}
	// From here the request claims a service identity: every outcome is
	// decided below, there is no OIDC fallback in any failure case.
	if len(md.Get("authorization")) > 0 {
		serviceIdentityLogger.Warnf("Service credential rejected: both authorization and service credential present (method=%s)", fullMethod)
		return nil, serviceDecisionPass, status.Error(codes.Unauthenticated, "both authorization and service credential present")
	}
	if si.cfg.nodeCredential == "" {
		// Fail-closed: service mode without a node credential does not accept
		// service connections (spec "Missing node credential rejects service
		// connections").
		serviceIdentityLogger.Warnf("Service credential rejected: node credential is not configured (method=%s)", fullMethod)
		return nil, serviceDecisionPass, status.Error(codes.Unauthenticated, "service connections are not accepted")
	}
	if !serviceCredentialMatches(si.cfg.credentialHash, svcVals[0]) {
		serviceIdentityLogger.Warnf("Service credential mismatch (method=%s)", fullMethod)
		return nil, serviceDecisionPass, status.Error(codes.Unauthenticated, "invalid service credential")
	}
	decision := serviceMethodDecision(fullMethod)
	if decision == serviceDecisionDeny {
		serviceIdentityLogger.Warnf("Service identity denied on admin method %s", fullMethod)
	}
	return oidc.WithIdentityKind(ctx, oidc.IdentityKindService), decision, nil
}

// serviceCredentialMatches compares the SHA-256 of the presented credential
// with the configured digest in constant time.
func serviceCredentialMatches(configured []byte, presented string) bool {
	sum := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(sum[:], configured) == 1
}

// streamingAdminMethods lists the streaming admin RPCs explicitly (design D3):
// they bypass the unary authz interceptor, so the table must not rely on the
// authz classification alone. All of them are admin operations (metadata
// dump/load, slice/object/changelog scans); ScanSlices is not even classified
// by the authz interceptor (streams never reach it today).
var streamingAdminMethods = map[string]bool{
	"DumpMeta":          true,
	"LoadMeta":          true,
	"DumpMetaV2":        true,
	"LoadMetaV2":        true,
	"ScanSlices":        true,
	"ScanDeletedObject": true,
	"ScanChangelog":     true,
}

// serviceMethodDecision resolves the service access table: GetFormat is the
// only admin method a service identity may call (the gateway startup probe);
// the rest of the admin surface — the authz-classified admin set plus the
// streaming admin RPCs above — is denied; everything else passes through
// (per-user authz itself is skipped for service downstream). The authz method
// classification is the single source of truth of the unary admin surface:
// requiredPermission reads no receiver fields.
func serviceMethodDecision(fullMethod string) serviceDecision {
	name := fullMethod
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "GetFormat" {
		return serviceDecisionPass
	}
	if streamingAdminMethods[name] {
		return serviceDecisionDeny
	}
	if serviceClassifier.requiredPermission(name) == AuthzPermissionAdmin {
		return serviceDecisionDeny
	}
	return serviceDecisionPass
}

// serviceClassifier reuses the authz method classification
// (AuthzInterceptor.requiredPermission) as the admin-surface source of truth.
var serviceClassifier = &AuthzInterceptor{}

// serviceIdentityServerStream wraps a grpc.ServerStream with an enriched
// context (mirrors oidc.serverStream; unexported there).
type serviceIdentityServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *serviceIdentityServerStream) Context() context.Context { return s.ctx }
