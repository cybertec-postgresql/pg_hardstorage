# jit/

Just-in-time access tokens: time-bound, scoped, ed25519-signed grants for
break-glass operations.

## What lives here

An operator with standing authority issues a token (`jit issue <principal>
--scope kms.shred --duration 1h --reason ...`). The token is signed with the
operator's ed25519 manifest-signing key, persisted at `jit/<id>.json` in the
repo, and stamped into the audit chain. `jit verify` checks signature + expiry
+ scope + tenant + revocation marker (a tenant-bound token never verifies for
an operation in another tenant or in no tenant). Revocation drops a
`<id>.json.revoked` sentinel that verification checks.

Not yet wired: destructive commands do not accept a `--jit-token` or call
`VerifyAt`; they are gated by approvals (`../approval/`). Until that lands a
token is advisory, checked by `jit verify` in operator tooling.

## Key files

- `jit.go` — `Token`, `Issue`, `Verify`, `Revoke`, scope matching
- `jit_test.go` — issuance, verification, expiry, scope mismatch, revocation

## Read next

- `../approval/README.md` — n-of-m approval for the *issuance* of
  high-blast-radius JITs
- `../scim/README.md` — IdP-driven operator identity; resolves the `principal`
  in a JIT
- `../audit/README.md` — every issue / use / revoke is hash-chained
- `../README.md` — parent index

## Don't put X here

- Standing RBAC (role / permission tables) — JIT is the break-glass overlay,
  not the base policy.
- SAML / OIDC binding — out of scope; `scim/` handles user lifecycle, identity
  is asserted at the CLI / API boundary.
