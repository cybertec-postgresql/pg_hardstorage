# scim/

SCIM 2.0 user / group provisioning surface (RFC 7643 schema, RFC 7644 protocol).

## What lives here

An enterprise IdP (Okta, Azure AD, OneLogin, JumpCloud, ...) pushes user + group
lifecycle events to pg_hardstorage's SCIM endpoints. New operators are
auto-created; leavers are auto-deprovisioned. This package is the domain
primitive: `User`, `Group`, `Store`, a filter parser, and the subset of PATCH
operations IdPs actually use. HTTP wiring into `internal/server/` is a separate
concern.

## Key files

- `scim.go` — types (`User`, `Group`, `Meta`), filter parser, PATCH op
  evaluator, `Store` CRUD
- `scim_test.go` — schema round-trip, filter evaluation, PATCH op coverage

What is deliberately *not* here: bulk operations (RFC 7644 §3.7), complex
grouped filter expressions, schema extensions, SAML / OIDC binding.

## Read next

- `../jit/README.md` — JIT tokens resolve `principal` to a SCIM User
- `../approval/README.md` — approver allowlists are populated from SCIM Group
  membership
- `../audit/README.md` — SCIM CRUD events land in the audit chain
- `../server/README.md` — HTTP endpoint wiring
- `../README.md` — parent index

## Don't put X here

- HTTP routing — that's `internal/server/`.
- SAML / OIDC token validation — out of scope; identity is asserted at the API
  edge.
