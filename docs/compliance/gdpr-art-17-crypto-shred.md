---
title: GDPR Article 17 — crypto-shred
description: Per-tenant KEK design and the kms shred flow that makes a tenant's backups bit-for-bit unrecoverable.
tags:
  - gdpr
  - crypto-shred
  - erasure
---

# GDPR Article 17 — crypto-shred

GDPR Article 17 ("right to erasure") requires the
controller to erase a data subject's personal data on
request. For backup data the conventional approach — find
every backup containing the subject's row and rewrite it —
is operationally expensive and never quite complete (chunk
dedup, replicas, archived snapshots).

`pg_hardstorage` implements **crypto-shred** instead:
destroy the per-tenant KEK that wraps every encrypted
backup's DEK. After shred, every backup encrypted under
that KEK is bit-for-bit unrecoverable. The audit log entry
is the compliance artefact.

---

## The three-layer envelope

Every encrypted chunk is sealed under a layered key system:

1. **Repository KEK (RKEK)** held in the configured KMS
   (AWS KMS, GCP KMS, Azure Key Vault, Vault Transit, or
   local AES-256-GCM with passphrase). Reference stored in
   `HSREPO`.
2. **Backup DEK (BDEK)** — 256-bit random per backup,
   wrapped under the RKEK. Stored in
   `manifest.json.encryption.wrapped_dek`.
3. **Per-chunk key** derived `Kc = HKDF-SHA256(BDEK,
   info=chunk_hash)`. Cipher: AES-256-GCM with a random
   96-bit nonce is shipping today; AES-256-GCM-SIV (RFC 8452,
   nonce-misuse resistant) is the planned default once a
   validated implementation lands.

A tenant's KEK wraps every BDEK for every backup of every
deployment under that tenant. Destroy that one key and
every dependent backup is unrestorable.

Per-tenant KEK is **mandatory architecture** — single-org
users get a default tenant. This is what makes shred a
one-line operation.

---

## The command

```sh
pg_hardstorage kms shred --repo <url> \
    --require-approval <approval-id> \
    --confirm-keyring /var/lib/pg_hardstorage/keyring \
    --reason "GDPR Art. 17 request #4421" \
    --yes
```

Three independent safety mechanisms gate the op:

| Gate | Purpose |
| --- | --- |
| `--require-approval` | n-of-m approved request whose `Op` is `kms.shred` and `Target` is the canonical keyring path. Refuses without it. |
| `--confirm-keyring` | Operator must repeat the literal keyring path. Defends against compromised credentials — a credential alone cannot drive shred without knowing the path. |
| `--yes` | Acknowledgement that the op is irreversible. |

The flag combination is **not optional**; the binary
refuses with a clear error if any one is missing.

---

## Dry-run preview

`--dry-run` skips every gate (no approval / typed keyring /
yes required) and just enumerates affected backups:

```sh
pg_hardstorage kms shred --repo <url> --dry-run
```

```console
kms shred (dry-run) — keyring /var/lib/pg_hardstorage/keyring
  Affected backups: 247

  db1.full.20260228T030001Z
  db1.full.20260301T030001Z
  ...
  db2.incr.20260427T030047Z
```

Operators preview the scope BEFORE asking for an n-of-m
approval. Without this preview, the workflow forces an
approval just to find out whether shred would even affect
anything.

---

## What happens on real shred

1. Verify the n-of-m approval is current and `Op = kms.shred`,
   `Target = <keyring-path>`.
2. Verify `--confirm-keyring` matches the resolved keyring
   path (defends against compromise).
3. Re-enumerate affected backups (canonical scope at shred
   time).
4. Write an audit event:

   ```json
   {
     "schema": "pg_hardstorage.audit.v1",
     "action": "kms.shred",
     "actor": "ops@acme",
     "tenant": "acme-prod",
     "subject": {"tenant": "acme-prod"},
     "body": {
       "reason": "GDPR Art. 17 request #4421",
       "approval_id": "appr-7f2a...",
       "keyring_dir": "/var/lib/pg_hardstorage/keyring",
       "affected_backup_count": 247,
       "affected_backup_ids": ["db1.full.20260228T030001Z", ...]
     }
   }
   ```

5. Atomically destroy the KEK file. The bytes are
   overwritten before unlink (best-effort defense against
   filesystem-level forensic recovery; the cryptographic
   guarantee comes from the AES-256-GCM-wrapped BDEKs being
   permanently unrecoverable, not from the bytes-on-disk
   step).
6. Return a structured result body:

   ```json
   {
     "schema": "pg_hardstorage.kms.shred.v1",
     "tenant": "acme-prod",
     "affected_count": 247,
     "audit_event_id": "01H7K9...",
     "completed_at": "2026-04-28T14:21:08Z"
   }
   ```

---

## Provider-managed keys

The `kms shred` command targets the **local keyring**.
KMS-provider plugins (AWS KMS, GCP KMS, Vault Transit) have
their own shred semantics — they typically schedule
destruction with a cooldown window of 7–30 days.

For provider-managed keys, drive destruction via the
provider's own primitives:

| Provider | Command |
| --- | --- |
| AWS KMS | `aws kms schedule-key-deletion --key-id <arn> --pending-window-in-days 7` |
| GCP KMS | `gcloud kms keys versions destroy <version> --location <region> --keyring <ring> --key <key>` |
| Vault Transit | `vault delete transit/keys/<name>` (destruction must be enabled per-key) |
| Azure Key Vault | `az keyvault key delete --vault-name <vault> --name <key>` |

Then write the audit event manually so the compliance
record is consistent:

```sh
pg_hardstorage audit append kms.shred \
    --repo <repo-url> \
    --tenant acme-prod \
    --reason "GDPR Art. 17 request #4421; AWS KMS schedule-deletion ID xyz"
```

---

## Mapping to GDPR

| GDPR clause | Product feature | Audit event |
| --- | --- | --- |
| Art. 17(1) — right to erasure | `kms shred` per-tenant KEK destruction | `kms.shred` |
| Art. 17(2) — communication of erasure to recipients | Cross-region replicas hold the same wrapped DEKs; KEK destruction propagates implicitly. Replica audit chain records `kms.shred`. | `kms.shred` (replicated) |
| Art. 30 — record of processing activities | Hash-chained audit log; `audit verify-chain` proves untampered. | `audit.*` |
| Art. 32(1)(a) — encryption of personal data | AES-256-GCM per chunk (AES-256-GCM-SIV planned; FIPS-validated GCM in FIPS build). | `backup.create` (records `encryption.scheme`) |
| Art. 32(1)(b) — confidentiality, integrity | Ed25519-signed manifests, Merkle audit chain. | `backup.create`, `audit.*` |

---

## The trade-off

Crypto-shred is **bit-level final** — you cannot un-shred.
Restore from a backup taken before the KEK was created is
unaffected (different KEK), but every backup of every
deployment under that tenant from KEK creation onward is
gone.

For controllers wanting a softer erasure (subject-row
removal without full backup destruction), pair with a
forward-going schedule:

1. Run a fresh backup with the row dropped (`pg_dump --exclude-table-data <table>` or schema redaction at the source).
2. Wait for retention to expire the older backups containing the row.
3. After retention, shred is no longer needed for THIS
   subject; the older backups already aged out.

The full-tenant shred is for the cases where retention
isn't fast enough — a subject demands erasure within 30
days and the GFS schedule keeps yearly snapshots for 5
years.

---

## Further reading

- [Audit evidence bundles](audit-evidence-bundles.md) — the
  forensic-grade export that includes `kms.shred` events.
- [Operator guide: encryption + KMS](../operations/operator-guide.md#7-encryption-kms)
  — the operational view of the same flow.
- [Runbook R2: KMS key destroyed](../reference/runbooks/R2-kms-key-destroyed.md)
  — what to do when shred fires accidentally.
- [SOC 2 control mapping](soc2-control-mapping.md) — CC6.1
  encryption + erasure mapping.
