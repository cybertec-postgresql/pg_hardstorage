# approval/

n-of-m approval workflow for destructive operations: `kms shred`, `repo gc
--delete`, `backup delete --force`, `repo wipe`.

## What lives here

An initiator writes a `Request` (op, target, reason, TTL, threshold N,
ed25519-PEM approver allowlist) to `approvals/<id>.json`. Each approver fetches
the request and posts a signed `Approval` envelope, which is appended via
read-modify-write against the same key. `Status()` counts unique-approver
signatures over canonical request bytes and returns one of pending / approved /
expired / revoked. Destructive commands consult `Status()` before mutating
anything.

## Key files

- `approval.go` — `Request`, `Approval`, `Status`, canonical signing, request
  lifecycle
- `approval_test.go` — request lifecycle, signature uniqueness, TTL,
  revocation
- `concurrency_test.go` — read-modify-write race coverage for concurrent
  approves
- `workflow_integration_test.go` — full request → approve → consume flow

## Read next

- `../threshold/README.md` — similar shape; threshold is for *attesting
  subjects*, approval is for *gating ops*
- `../jit/README.md` — JIT can be gated behind an approval workflow for
  high-blast-radius scopes
- `../scim/README.md` — approver allowlists are sourced from SCIM Groups
- `../audit/README.md` — every create + every approve is hash-chained
- `../README.md` — parent index

## Don't put X here

- The actual destructive op — `kms.shred`, `repo.gc` live in their own
  packages; approval gates them.
- Operator key management — same ed25519 keypair as manifest signing
  (`internal/backup/sign.go`).
