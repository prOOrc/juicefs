# Delta: domain-meta-proxy

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
