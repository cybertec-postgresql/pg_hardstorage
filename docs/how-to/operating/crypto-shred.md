---
title: Crypto-shred (GDPR Art. 17)
description: Irreversibly destroy the KEK; every backup whose DEK
              was wrapped under it becomes bit-for-bit unrecoverable.
              The compliance artefact is the audit-chain entry.
tags:
  - kms
  - shred
  - gdpr
  - compliance
---

# Crypto-shred

!!! danger
    `pg_hardstorage kms shred` is **irreversible**. After it
    completes, every backup whose DEK was wrapped under the
    destroyed KEK is permanently unrecoverable. There is no
    "undo." That's the GDPR Art. 17 / right-to-be-forgotten
    contract operating exactly as designed.

## What you need

- Approval to destroy the KEK. The command requires three
  independent gates (see *Safety belts* below).
- For local-keyring shred: `--confirm-keyring` set to the
  literal keyring directory path.
- For cloud-KMS shred: the provider-side destroy permission
  (`kms:ScheduleKeyDeletion` on AWS, `roles/cloudkms.admin`
  on GCP, "Crypto Officer" on Azure, `delete` capability in
  Vault Transit, `C_DestroyObject` on PKCS#11).
- An [n-of-m approval](n-of-m-approvals.md) request whose
  `op = kms.shred` and whose `target` is the canonical
  keyring directory path.

## Steps

### 1. Dry-run: enumerate the affected backups

```bash
pg_hardstorage kms shred --dry-run --repo file:///srv/pg_hardstorage/repo
```

```console
✓ kms shred --dry-run — preview only, KEK NOT destroyed
  Keyring:  /etc/pg_hardstorage/keyring
  Affected: 0 backup(s) would become unrecoverable (none — no encrypted backups in this repo were wrapped with this KEK)
  Note:     re-run without --dry-run plus --require-approval / --confirm-keyring / --yes to actually destroy the KEK
```

Dry-run never writes to the audit chain (no state change worth
recording) and never touches the KEK file. Use this output to
size the consequences before requesting approval.

### 2. Open an n-of-m approval request

```bash
pg_hardstorage approval request \
    --repo file:///srv/pg_hardstorage/repo \
    --op kms.shred \
    --target /etc/pg_hardstorage/keyring \
    --reason "GDPR Art 17 #4421 — subject deletion request" \
    --threshold 2 \
    --approver-key /etc/pg_hardstorage/approvers/alice.pub \
    --approver-key /etc/pg_hardstorage/approvers/bob.pub \
    --approver-key /etc/pg_hardstorage/approvers/carol.pub
```

```console
✓ approval request created
  ID:        appr-6a4bb4064d13c6f0
  Op:        kms.shred
  Target:    /etc/pg_hardstorage/keyring
  Threshold: 2 of 3 allowlisted approvers
  Expires:   2026-07-07T13:56:22Z
  Approve:   pg_hardstorage approval approve appr-6a4bb4064d13c6f0 --repo <url>
```

The request is signed against the operator's keypair; tampering
with the request body invalidates every existing approval.
Each approver fetches the request, decides yes / no, and signs
with their ed25519 private key:

```bash
# Alice
pg_hardstorage approval approve apr-2026-04-28-7f3a1b2c \
    --repo file:///srv/pg_hardstorage/repo \
    --approver alice@acme.example.com \
    --key /home/alice/.ssh/pg_hardstorage_alice.pem \
    --reason "I confirm subject 4421's deletion is in flight"

# Bob does the same; the request flips to "approved" once the
# threshold is reached.
```

See [n-of-m approvals](n-of-m-approvals.md) for the full flow.

### 3. Run the shred

```bash
pg_hardstorage kms shred \
    --repo file:///srv/pg_hardstorage/repo \
    --require-approval appr-6a4bb4064d13c6f0 \
    --confirm-keyring /etc/pg_hardstorage/keyring \
    --reason "GDPR Art 17 #4421" \
    --yes
```

The command:

1. Re-validates the n-of-m approval against the repo.
2. Re-checks `--confirm-keyring` against the resolved keyring path.
3. Enumerates the affected backups one more time (matches the
   dry-run output).
4. **Destroys** the KEK.
5. Emits an audit-chain entry recording the shred + the
   affected scope.

```console
✓ kms shred — KEK irreversibly destroyed
  Keyring:  /etc/pg_hardstorage/keyring
  Reason:   GDPR Art 17 #4421
  Affected: 0 backup(s) now unrecoverable (none — no encrypted backups in this repo were wrapped with this KEK)
  Approval: appr-6a4bb4064d13c6f0
  Note:     every backup wrapped with this KEK is now permanently unrecoverable
```

The audit event is your compliance artefact: it records
*who*, *when*, *why*, *what*, and the cryptographic chain
linking that event to the rest of the audit log.

## Safety belts

Three independent mechanisms guard the destructive op:

1. **n-of-m approval** with `op = kms.shred` and `target`
   matching the resolved keyring path. Refused at the gate
   otherwise.
2. **Typed-confirmation flag** (`--confirm-keyring`) where the
   operator must repeat the literal keyring directory path.
   A compromised credential alone cannot drive shred without
   knowing the exact path.
3. **Acknowledgement flag** (`--yes`) for non-interactive
   use.

Dry-run skips all three. Production shred requires all three.

## Cloud-KMS shred

When the KEK lives in a cloud provider, the provider's own
destruction primitive is what actually destroys the bytes.
pg_hardstorage delegates and records:

| Provider | Mechanism | Cooldown |
| --- | --- | --- |
| AWS KMS | `ScheduleKeyDeletion` | 7-30 days |
| GCP KMS | `DestroyCryptoKeyVersion` | configured at key creation (default 24h) |
| Azure Key Vault | `DeleteKey` (soft-delete) | configured at vault creation (default 90 days) |
| Vault Transit | `DELETE transit/keys/<name>` | immediate (after `deletion_allowed=true`) |
| PKCS#11 | `C_DestroyObject` | immediate (HSM may impose ACS quorum) |

Use the provider's own destruction primitives — those have
their own cooldown windows + audit trails that this binary
doesn't attempt to reproduce.

## Recovery during cooldown

For providers with a cooldown window, you can cancel the
destruction within the window:

```bash
# AWS
aws kms cancel-key-deletion --key-id <key-uuid>
```

```bash
# GCP — restore a destroyed-scheduled version
gcloud kms keys versions restore 3 \
    --key db-kek --keyring pg-hardstorage \
    --location europe-west1
```

After the cooldown, recovery is impossible — that's the
contract.

## What survives a shred

- The audit chain (the shred event itself + every event before
  it) — by design, the chain stays intact for forensic and
  compliance purposes.
- Manifest metadata (deployment names, byte counts, timestamps,
  KEK references). The manifest body is unencrypted; only the
  wrapped DEK was protected by the KEK.
- The repo's chunk bytes. They're now ciphertext nobody holds
  the key for. `pg_hardstorage repo gc` reclaims them on the
  next pass.

## Troubleshooting

**`usage.missing_flag`** — `--require-approval` was omitted;
`kms shred` refuses to run without an approved n-of-m gate.
Open an approval request for `op=kms.shred, target=<keyring>`,
get it approved, then pass its ID.

**`usage.confirmation_mismatch`** — `--confirm-keyring`
doesn't match the resolved keyring path. Compare against
`pg_hardstorage kms inspect`.

**`usage.confirmation_required`** — `--confirm-keyring` or
`--yes` was omitted. Both are required for a live shred; add
whichever is missing.

**Provider-side destroy refused (HSM)** — the HSM has policy
gates (operator card, ACS quorum) beyond pg_hardstorage's
control. Coordinate with whoever holds the operator cards.

## Next steps

- [n-of-m approvals](n-of-m-approvals.md) — the consent gate
- [Legal hold](legal-hold.md) — pin a backup against
  retention; held backups are visible to shred (the wrap is
  identical) but the manifest entries record the prior hold
  for audit
- [`kms shred` CLI reference](../../reference/cli/pg_hardstorage_kms_shred.md)
- [Runbook R2: KMS key destroyed](../../reference/runbooks/R2-kms-key-destroyed.md)
