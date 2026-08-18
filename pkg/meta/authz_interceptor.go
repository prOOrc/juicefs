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
	"strings"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/juicedata/juicefs/pkg/oidc"
	"github.com/juicedata/juicefs/pkg/utils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var authzLogger = utils.GetLogger("juicefs-authz")

// AuthzPermission represents the required permission level for an operation.
type AuthzPermission int

const (
	AuthzPermissionNone   AuthzPermission = iota // No authorization check needed
	AuthzPermissionView                          // stat, lookup, readlink
	AuthzPermissionRead                          // open for read, read data
	AuthzPermissionWrite                         // create, write, delete, rename
	AuthzPermissionAdmin                         // admin operations (CheckOrganizationAdmin)
	AuthzPermissionDenied                        // unknown method — deny everyone
)

// AuthzCheck describes a single authorization check (path + required permission).
type AuthzCheck struct {
	Path       string
	Permission AuthzPermission
}

// AuthzClient is the interface for calling the external authorization service.
type AuthzClient interface {
	CheckPermission(ctx context.Context, userID, filePath string, perm AuthzPermission) (bool, error)
	CheckBulkPermissions(ctx context.Context, userID string, paths []string, perm AuthzPermission) ([]bool, error)
	CheckOrganizationAdmin(ctx context.Context, userID string) (bool, error)
}

// HandleResolver resolves DirHandler handles to inodes.
type HandleResolver interface {
	ResolveHandle(handle uint64) (Ino, bool)
}

// AuthzInterceptor checks file-level authorization for gRPC MetaService calls.
type AuthzInterceptor struct {
	client          AuthzClient
	cache           *InodePathCache
	handleResolver  HandleResolver
	enabled         bool
	userIDExtractor func(ctx context.Context) string // injectable for testing
}

// NewAuthzInterceptor creates a new authorization interceptor.
func NewAuthzInterceptor(client AuthzClient, cache *InodePathCache, resolver HandleResolver) *AuthzInterceptor {
	return &AuthzInterceptor{
		client:          client,
		cache:           cache,
		handleResolver:  resolver,
		enabled:         client != nil,
		userIDExtractor: extractUserIDFromOIDC,
	}
}

// extractUserIDFromOIDC is the default userID extractor from OIDC claims.
func extractUserIDFromOIDC(ctx context.Context) string {
	claims := oidc.ClaimsFromContext(ctx)
	if claims == nil {
		return ""
	}
	if idToken, ok := claims.(*oidc.IDToken); ok {
		return idToken.Subject
	}
	return ""
}

// extractUserID extracts the user ID from context using the configured extractor.
func (ai *AuthzInterceptor) extractUserID(ctx context.Context) string {
	if ai.userIDExtractor == nil {
		return ""
	}
	return ai.userIDExtractor(ctx)
}

// UnaryInterceptor returns a gRPC unary server interceptor for authorization.
func (ai *AuthzInterceptor) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !ai.enabled {
			return handler(ctx, req)
		}

		// Determine required permission for this RPC
		permission := ai.requiredPermission(info.FullMethod)
		if permission == AuthzPermissionNone {
			return handler(ctx, req) // lifecycle methods — no check
		}

		// Unknown method — deny for everyone (including admins)
		if permission == AuthzPermissionDenied {
			return nil, status.Error(codes.PermissionDenied, "access denied: unknown method")
		}

		// Extract user identity from OIDC claims
		userID := ai.extractUserID(ctx)
		if userID == "" {
			return nil, status.Error(codes.Unauthenticated, "no authenticated user")
		}

		// Admin operations require organization admin status
		if permission == AuthzPermissionAdmin {
			authzLogger.Tracef("Authz check: method=%s user=%s path=- perm=Admin",
				info.FullMethod, userID)
			isAdmin, err := ai.client.CheckOrganizationAdmin(ctx, userID)
			if err != nil || !isAdmin {
				authzLogger.Warnf("Org admin check failed: user=%s method=%s err=%v",
					userID, info.FullMethod, err)
				return nil, status.Error(codes.PermissionDenied, "access denied: organization admin required")
			}
			return handler(ctx, req)
		}

		// File-level: resolve checks and authorize
		checks := ai.resolveChecks(req, info.FullMethod)
		if len(checks) == 0 {
			authzLogger.Debugf("Cannot resolve path for method=%s user=%s — denying",
				info.FullMethod, userID)
			return nil, status.Error(codes.PermissionDenied, "access denied: path not resolved")
		}

		// Check each required permission (deny if any fails)
		for _, chk := range checks {
			if chk.Path == "" {
				authzLogger.Debugf("Cannot resolve path for method=%s user=%s — denying",
					info.FullMethod, userID)
				return nil, status.Error(codes.PermissionDenied, "access denied: path not resolved")
			}

			if isAlwaysAllowed(chk.Path, chk.Permission) {
				authzLogger.Tracef("Authz skip (always allowed): method=%s user=%s path=%s perm=%d",
					info.FullMethod, userID, chk.Path, chk.Permission)
				continue
			}

			authzLogger.Tracef("Authz check: method=%s user=%s path=%s perm=%d",
				info.FullMethod, userID, chk.Path, chk.Permission)
			allowed, err := ai.client.CheckPermission(ctx, userID, chk.Path, chk.Permission)
			if err != nil || !allowed {
				authzLogger.Debugf("Authz denied: method=%s user=%s path=%s perm=%d err=%v",
					info.FullMethod, userID, chk.Path, chk.Permission, err)
				return nil, status.Error(codes.PermissionDenied, "access denied")
			}
		}

		return handler(ctx, req)
	}
}

// requiredPermission returns the authorization permission level for a gRPC method.
// Unknown methods return AuthzPermissionDenied (deny for everyone, including admins).
func (ai *AuthzInterceptor) requiredPermission(method string) AuthzPermission {
	name := method
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}

	switch name {
	// --- No check: internal / lifecycle ---
	case "Init", "Load",
		"NewSession", "CloseSession", "FlushSession",
		"GetSession", "ListSessions", "CleanStaleSessions",
		"Chroot",
		"DirHandlerList", "DirHandlerInsert", "DirHandlerDelete", "DirHandlerClose":
		return AuthzPermissionNone

		// --- Admin: require organization admin ---
	case "GetFormat", "Remove", "BatchUnlink",
		"GetSummary", "GetTreeSummary", "Clone", "GetPaths",
		"Check", "CompactAll", "Compact", "ListSlices",
		"HandleQuota", "ScanUserGroupUsage",
		"CleanupTrashBefore", "CleanupDetachedNodesBefore",
		"ScanDeletedObject", "ScanChangelog":
		return AuthzPermissionAdmin

		// --- Post-filter: Readdir (handled in handler, not interceptor) ---
	case "Readdir":
		return AuthzPermissionNone // handled by post-filter in handler

		// --- Admin: dump/load metadata (access to entire volume metadata) ---
	case "DumpMeta", "LoadMeta", "DumpMetaV2", "LoadMetaV2":
		return AuthzPermissionAdmin

		// --- Admin: token management (sensitive operations) ---
	case "StoreToken", "UpdateToken", "LoadToken", "DeleteTokens", "ListTokens":
		return AuthzPermissionAdmin

		// --- View: read metadata ---
	case "StatFS", "Lookup", "Resolve",
		"GetAttr", "ReadLink", "GetParents", "GetDirStat":
		return AuthzPermissionView

		// --- View: read extended attributes ---
	case "GetXattr", "ListXattr", "GetFacl":
		return AuthzPermissionView

		// --- Read: open for reading, read data ---
	case "Open", "Read":
		return AuthzPermissionRead

		// --- Read: access check (R_OK) ---
	case "Access":
		return AuthzPermissionRead

		// --- Write: modify metadata ---
	case "SetAttr", "CheckSetAttr", "Truncate", "Fallocate":
		return AuthzPermissionWrite

		// --- Write: create nodes ---
	case "Mknod", "Mkdir", "Create", "Symlink", "Link":
		return AuthzPermissionWrite

		// --- Write: delete / rename ---
	case "Unlink", "Rmdir", "Rename":
		return AuthzPermissionWrite

		// --- Write: modify data ---
	case "Write", "InvalidateChunkCache", "CopyFileRange", "NewSlice":
		return AuthzPermissionWrite

		// --- Write: modify extended attributes ---
	case "SetXattr", "RemoveXattr", "SetFacl":
		return AuthzPermissionWrite

		// --- Write: locks ---
	case "Flock", "Setlk":
		return AuthzPermissionWrite

		// --- View: query locks ---
	case "Getlk", "ListLocks":
		return AuthzPermissionView

		// --- View: close (read-only files should not require Write) ---
	case "Close":
		return AuthzPermissionView

		// --- DirHandler: NewDirHandler checks View; List/Insert/Delete/Close are lifecycle (handle already authorized) ---
	case "NewDirHandler":
		return AuthzPermissionView

	default:
		authzLogger.Warnf("Unknown gRPC method for authz: %s — denying for all", method)
		return AuthzPermissionDenied // deny everyone
	}
}

// resolveChecks returns the authorization checks needed for a request.
func (ai *AuthzInterceptor) resolveChecks(req interface{}, method string) []AuthzCheck {
	perm := ai.requiredPermission(method)

	switch r := req.(type) {
	// --- Parent-based: check parent permission ---
	case *pb.LookupRequest:
		p := ai.cache.Get(Ino(r.Parent))
		if p == "" {
			return nil
		}
		return []AuthzCheck{{Path: p, Permission: AuthzPermissionView}}

	case *pb.ResolveRequest:
		return nil // deny for PoC — multi-component paths not fully verified

	case *pb.CreateRequest:
		p := ai.cache.Get(Ino(r.Parent))
		if p == "" {
			return nil
		}
		return []AuthzCheck{{Path: p, Permission: AuthzPermissionWrite}}

	case *pb.MkdirRequest:
		p := ai.cache.Get(Ino(r.Parent))
		if p == "" {
			return nil
		}
		return []AuthzCheck{{Path: p, Permission: AuthzPermissionWrite}}

	case *pb.MknodRequest:
		p := ai.cache.Get(Ino(r.Parent))
		if p == "" {
			return nil
		}
		return []AuthzCheck{{Path: p, Permission: AuthzPermissionWrite}}

	case *pb.SymlinkRequest:
		p := ai.cache.Get(Ino(r.Parent))
		if p == "" {
			return nil
		}
		return []AuthzCheck{{Path: p, Permission: AuthzPermissionWrite}}

	case *pb.UnlinkRequest:
		p := ai.cache.Get(Ino(r.Parent))
		if p == "" {
			return nil
		}
		return []AuthzCheck{{Path: p, Permission: AuthzPermissionWrite}}

	case *pb.RmdirRequest:
		p := ai.cache.Get(Ino(r.Parent))
		if p == "" {
			return nil
		}
		return []AuthzCheck{{Path: p, Permission: AuthzPermissionWrite}}

		// --- Multi-check: Rename (source parent + destination parent) ---
	case *pb.RenameRequest:
		srcParentPath := ai.cache.Get(Ino(r.ParentSrc))
		dstParentPath := ai.cache.Get(Ino(r.ParentDst))

		if srcParentPath == "" || dstParentPath == "" {
			return nil // deny: cannot resolve both parents
		}
		return []AuthzCheck{
			{Path: srcParentPath, Permission: AuthzPermissionWrite},
			{Path: dstParentPath, Permission: AuthzPermissionWrite},
		}

		// --- Multi-check: Link (source inode Read + destination parent Write) ---
	case *pb.LinkRequest:
		srcPath := ai.cache.Get(Ino(r.InodeSrc))
		dstParentPath := ai.cache.Get(Ino(r.Parent))

		if srcPath == "" || dstParentPath == "" {
			return nil // deny: cannot resolve both paths
		}
		return []AuthzCheck{
			{Path: srcPath, Permission: AuthzPermissionRead},
			{Path: dstParentPath, Permission: AuthzPermissionWrite},
		}

		// --- Multi-check: CopyFileRange (source Read + destination Write) ---
	case *pb.CopyFileRangeRequest:
		srcPath := ai.cache.Get(Ino(r.Fin))
		dstPath := ai.cache.Get(Ino(r.Fout))

		if srcPath == "" || dstPath == "" {
			return nil // deny: cannot resolve both paths
		}
		return []AuthzCheck{
			{Path: srcPath, Permission: AuthzPermissionRead},
			{Path: dstPath, Permission: AuthzPermissionWrite},
		}

		// --- Open: permission from flags ---
	case *pb.OpenRequest:
		p := ai.cache.Get(Ino(r.Inode))
		if p == "" {
			return nil
		}
		openPerm := permissionFromFlags(r.Flags)
		return []AuthzCheck{{Path: p, Permission: openPerm}}

		// --- Access: permission from mask ---
	case *pb.AccessRequest:
		p := ai.cache.Get(Ino(r.Inode))
		if p == "" {
			return nil
		}
		accessPerm := permissionFromAccessMask(r.Modemask)
		return []AuthzCheck{{Path: p, Permission: accessPerm}}

		// --- DirHandler: resolve handle to inode ---
	case *pb.NewDirHandlerRequest:
		p := ai.cache.Get(Ino(r.Inode))
		if p == "" {
			return nil
		}
		return []AuthzCheck{{Path: p, Permission: AuthzPermissionView}}

	default:
		// --- Inode-based: look up inode in cache ---
		inode := ai.extractInode(req)
		if inode == 0 {
			return nil
		}
		p := ai.cache.Get(inode)
		if p == "" {
			return nil
		}
		return []AuthzCheck{{Path: p, Permission: perm}}
	}
}

// extractInode extracts the target inode from any request type.
func (ai *AuthzInterceptor) extractInode(req interface{}) Ino {
	switch r := req.(type) {
	case *pb.GetAttrRequest:
		return Ino(r.Inode)
	case *pb.SetAttrRequest:
		return Ino(r.Inode)
	case *pb.CheckSetAttrRequest:
		return Ino(r.Inode)
	case *pb.TruncateRequest:
		return Ino(r.Inode)
	case *pb.FallocateRequest:
		return Ino(r.Inode)
	case *pb.ReadLinkRequest:
		return Ino(r.Inode)
	case *pb.CloseRequest:
		return Ino(r.Inode)
	case *pb.ReadRequest:
		return Ino(r.Inode)
	case *pb.WriteRequest:
		return Ino(r.Inode)
	case *pb.NewSliceRequest:
		return 0 // NewSlice doesn't target a specific inode
	case *pb.InvalidateChunkCacheRequest:
		return Ino(r.Inode)
	case *pb.GetParentsRequest:
		return Ino(r.Inode)
	case *pb.GetDirStatRequest:
		return Ino(r.Inode)
	case *pb.GetXattrRequest:
		return Ino(r.Inode)
	case *pb.SetXattrRequest:
		return Ino(r.Inode)
	case *pb.RemoveXattrRequest:
		return Ino(r.Inode)
	case *pb.ListXattrRequest:
		return Ino(r.Inode)
	case *pb.FlockRequest:
		return Ino(r.Inode)
	case *pb.GetlkRequest:
		return Ino(r.Inode)
	case *pb.SetlkRequest:
		return Ino(r.Inode)
	case *pb.ListLocksRequest:
		return Ino(r.Inode)
	case *pb.SetFaclRequest:
		return Ino(r.Ino)
	case *pb.GetFaclRequest:
		return Ino(r.Ino)
	case *pb.StatFSRequest:
		return Ino(r.Ino)
	default:
		return 0
	}
}

// extractHandle extracts the DirHandler handle ID from a request.
func (ai *AuthzInterceptor) extractHandle(req interface{}) uint64 {
	switch r := req.(type) {
	case *pb.DirHandlerListRequest:
		if r.Handle != nil {
			return r.Handle.HandleId
		}
	case *pb.DirHandlerInsertRequest:
		if r.Handle != nil {
			return r.Handle.HandleId
		}
	case *pb.DirHandlerDeleteRequest:
		if r.Handle != nil {
			return r.Handle.HandleId
		}
	case *pb.DirHandlerCloseRequest:
		if r.Handle != nil {
			return r.Handle.HandleId
		}
	default:
		return 0
	}
	return 0
}

// permissionFromFlags determines the required permission from Open flags.
func permissionFromFlags(flags uint32) AuthzPermission {
	if flags&(1|2) != 0 { // O_WRONLY=1, O_RDWR=2
		return AuthzPermissionWrite
	}
	return AuthzPermissionRead
}

// permissionFromAccessMask maps Access mask to required permission.
func permissionFromAccessMask(mask uint32) AuthzPermission {
	if mask&2 != 0 { // W_OK
		return AuthzPermissionWrite
	}
	if mask&4 != 0 { // R_OK
		return AuthzPermissionRead
	}
	return AuthzPermissionView // F_OK (existence check) or X_OK
}

// isAlwaysAllowed returns true for paths that don't require an authz service call.
// Root "/" with View is always allowed: it's the container for company directories,
// and access control starts at the /company-code/ level.
func isAlwaysAllowed(path string, perm AuthzPermission) bool {
	if path == "/" && perm == AuthzPermissionView {
		return true
	}
	return false
}
