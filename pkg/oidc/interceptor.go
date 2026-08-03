/*
 * JuiceFS, Copyright 2024 Juicedata, Inc.
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

package oidc

import (
	"context"

	"github.com/juicedata/juicefs/pkg/utils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var interceptorLogger = utils.GetLogger("juicefs")

// ValidatorInterface is the interface for token validation (for testability).
type ValidatorInterface interface {
	Validate(ctx context.Context, token string) (interface{}, error)
}

// validatorAdapter wraps *Validator to implement ValidatorInterface.
type validatorAdapter struct{ *Validator }

func (a *validatorAdapter) Validate(ctx context.Context, token string) (interface{}, error) {
	return a.Validator.Validate(ctx, token)
}

// ctxKeyClaims is the context key for verified ID token claims.
type ctxKeyClaims struct{}

// ClaimsFromContext returns the verified ID token from the context (set by interceptors).
// Returns nil if no token was validated.
func ClaimsFromContext(ctx context.Context) interface{} {
	if v := ctx.Value(ctxKeyClaims{}); v != nil {
		return v
	}
	return nil
}

// ExtractBearerToken extracts "Bearer <token>" from incoming gRPC metadata.
// Returns empty string if no authorization header is present.
func ExtractBearerToken(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}

	vals := md.Get("authorization")
	if len(vals) == 0 {
		return ""
	}

	auth := vals[0]
	const prefix = "Bearer "
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}

	return ""
}

// UnaryInterceptor returns a gRPC unary server interceptor that validates
// bearer tokens. If no token is present, the request passes through (backward compatible).
// If a token is present but invalid, the request is rejected with Unauthenticated.
func UnaryInterceptor(v ValidatorInterface) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		tokenStr := ExtractBearerToken(ctx)
		if tokenStr == "" {
			// No token — pass through for backward compatibility
			return handler(ctx, req)
		}

		idToken, err := v.Validate(ctx, tokenStr)
		if err != nil {
			interceptorLogger.Warnf("OIDC: invalid token: %v", err)
			return nil, status.Error(codes.Unauthenticated, "invalid or expired token")
		}

		// Inject verified claims into context for downstream handlers
		ctx = context.WithValue(ctx, ctxKeyClaims{}, idToken)
		return handler(ctx, req)
	}
}

// StreamInterceptor returns a gRPC stream server interceptor that validates
// bearer tokens on stream setup. Same semantics as UnaryInterceptor.
func StreamInterceptor(v ValidatorInterface) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		tokenStr := ExtractBearerToken(ctx)
		if tokenStr == "" {
			// No token — pass through for backward compatibility
			return handler(srv, ss)
		}

		idToken, err := v.Validate(ctx, tokenStr)
		if err != nil {
			interceptorLogger.Warnf("OIDC: invalid stream token: %v", err)
			return status.Error(codes.Unauthenticated, "invalid or expired token")
		}

		// Inject verified claims into context
		ctx = context.WithValue(ctx, ctxKeyClaims{}, idToken)
		// Wrap the ServerStream to use the enriched context
		wrapped := &serverStream{ServerStream: ss, ctx: ctx}
		return handler(srv, wrapped)
	}
}

// StrictUnaryInterceptor returns a gRPC unary server interceptor that requires
// a valid bearer token. Requests without a token are rejected with Unauthenticated.
func StrictUnaryInterceptor(v ValidatorInterface) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		tokenStr := ExtractBearerToken(ctx)
		if tokenStr == "" {
			interceptorLogger.Warnf("OIDC: no token provided")
			return nil, status.Error(codes.Unauthenticated, "authentication required")
		}

		idToken, err := v.Validate(ctx, tokenStr)
		if err != nil {
			interceptorLogger.Warnf("OIDC: invalid token: %v", err)
			return nil, status.Error(codes.Unauthenticated, "invalid or expired token")
		}

		ctx = context.WithValue(ctx, ctxKeyClaims{}, idToken)
		return handler(ctx, req)
	}
}

// StrictStreamInterceptor returns a gRPC stream server interceptor that requires
// a valid bearer token. Requests without a token are rejected with Unauthenticated.
func StrictStreamInterceptor(v ValidatorInterface) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		tokenStr := ExtractBearerToken(ctx)
		if tokenStr == "" {
			interceptorLogger.Warnf("OIDC: no token provided")
			return status.Error(codes.Unauthenticated, "authentication required")
		}

		idToken, err := v.Validate(ctx, tokenStr)
		if err != nil {
			interceptorLogger.Warnf("OIDC: invalid stream token: %v", err)
			return status.Error(codes.Unauthenticated, "invalid or expired token")
		}

		ctx = context.WithValue(ctx, ctxKeyClaims{}, idToken)
		wrapped := &serverStream{ServerStream: ss, ctx: ctx}
		return handler(srv, wrapped)
	}
}

// UnaryInterceptorWithValidator is a convenience wrapper that accepts *Validator directly.
func UnaryInterceptorWithValidator(v *Validator) grpc.UnaryServerInterceptor {
	return UnaryInterceptor(&validatorAdapter{v})
}

// StreamInterceptorWithValidator is a convenience wrapper that accepts *Validator directly.
func StreamInterceptorWithValidator(v *Validator) grpc.StreamServerInterceptor {
	return StreamInterceptor(&validatorAdapter{v})
}

// StrictUnaryInterceptorWithValidator is a convenience wrapper for strict mode.
func StrictUnaryInterceptorWithValidator(v *Validator) grpc.UnaryServerInterceptor {
	return StrictUnaryInterceptor(&validatorAdapter{v})
}

// StrictStreamInterceptorWithValidator is a convenience wrapper for strict mode.
func StrictStreamInterceptorWithValidator(v *Validator) grpc.StreamServerInterceptor {
	return StrictStreamInterceptor(&validatorAdapter{v})
}

// serverStream wraps grpc.ServerStream to override Context().
type serverStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *serverStream) Context() context.Context { return s.ctx }
