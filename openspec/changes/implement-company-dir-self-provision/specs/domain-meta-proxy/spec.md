# Delta: domain-meta-proxy

## MODIFIED Requirements

### Requirement: Authz decision cache

Authz decisions SHALL be cached per (user id, path, permission, operation context) key with a configurable TTL (`--authz-cache-ttl`, default 30s; TTL ≤ 0 disables caching) and a bounded size (default 10000 entries). Only successful decisions SHALL be cached; authz errors SHALL always trigger a live call. Bulk checks SHALL use partial caching (only cache misses are sent to the service in one batch). Organization-admin status SHALL be cached under a dedicated key with the same TTL. When the cache is full, expired entries SHALL be evicted first; if it remains full, new decisions SHALL not be cached.

The operation context (see "Authz operation context pass-through") SHALL be part of the cache key, so a decision granted for a directory creation on one path can never be served for a different operation on the same path.

Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_CachesResult
Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_TTLExpiry
Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_ErrorNotCached
Verified-by: pkg/meta/authz_cache_test.go::TestCachingAuthzClient_DifferentOps

#### Scenario: A mkdir grant is not served for another operation

- **WHEN** a directory creation on the companies namespace root was allowed and cached, and the same user then attempts a different operation on the same path within the cache TTL
- **THEN** the cached mkdir decision is not reused — the check goes live and the other operation is denied per its own rule

#### Scenario: Expired decision triggers a live call

- **WHEN** a cached decision older than the TTL is requested
- **THEN** the authz service SHALL be called again and the cache entry refreshed on success

## ADDED Requirements

### Requirement: Authz operation context pass-through

The meta proxy SHALL classify modifying metadata RPCs and forward an
operation context to the authorization service on every authz check:
directory-creation operations (Mkdir) SHALL be reported as `MKDIR`, all other
operations as the default. The classification SHALL NOT embed any storage- or
company-specific naming: it is a generic operation kind. Proxies built from
sources without the field (older clients against a newer authz service) and
newer proxies against an older authz service SHALL behave exactly as before
(the field is optional with a non-mkdir default).

Verified-by: pkg/meta/meta_proxy_authz_op_test.go::TestAuthzOpContext_MkdirClassified
Verified-by: pkg/meta/meta_proxy_authz_op_test.go::TestAuthzOpContext_OtherOpsDefault

#### Scenario: Mkdir reaches the authz service flagged as a directory creation

- **WHEN** a client issues Mkdir through the proxy and the proxy performs its authz check
- **THEN** the check request carries the mkdir operation context, and the service's decision (allow for the companies namespace root, deny otherwise) applies
