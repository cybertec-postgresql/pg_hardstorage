---
title: Envelope encryption — local KEK and AWS KMS
description: Wrap chunks with AES-256-GCM, anchored either to a
              local KEK file or an AWS KMS key.
tags:
  - encryption
  - kms
  - kek
---

# Envelope encryption — local KEK and AWS KMS

> Walks through both legs of envelope encryption: a local-KEK setup
> (one file on disk, no cloud) and an AWS KMS setup that wraps the
> per-backup DEK with a customer-managed key. About 15 minutes if
> you have AWS credentials handy; the local-KEK leg works offline.

`pg_hardstorage` encrypts every chunk with a per-backup AES-256-GCM
DEK. The DEK is wrapped by a Key Encryption Key (KEK) and the wrapped
copy lands in the manifest. There are two KEK schemes shipped today:

- **`local:default`** — a 32-byte file at `<keyring>/kek.bin`.
  Default when the file exists. Good for tutorials, single-host
  deployments, and air-gapped sites.
- **`aws-kms://<key-id>`** — every wrap and unwrap calls
  `KMS:Encrypt` / `KMS:Decrypt` against the configured key. Good
  for fleets that already centralise key custody.

Other cloud KMS providers (`gcp-kms://`, `azure-key-vault://`,
`vault-transit://`) are slated for v0.5+; `kms shred`,
`kms rotate`, and PKCS#11 / TPM2 binding land in the same window.

---

## Part A — Local KEK

### What you need

- A reachable PostgreSQL 15+ instance and `pg_hardstorage` v0.2 or
  later (see [getting-started](getting-started.md)).
- 200 MB free disk for the sandbox repo.

### Steps

#### 1. Initialise the repo

```bash
# RUNNABLE
pg_hardstorage repo init file:///tmp/hs-encrypt-repo
```

#### 2. Run `init` so it generates a KEK

The `init` wizard generates a signing keypair and a `kek.bin` on
first run. The default is encryption *on*:

```bash
# RUNNABLE
pg_hardstorage init \
    --pg-connection "${PG_CONNECTION:-postgres://postgres:postgres@127.0.0.1/postgres}" \
    --repo file:///tmp/hs-encrypt-repo \
    --deployment db1 \
    --skip-backup \
    --yes
```

Inspect the keystore (no secret bytes are printed):

```bash
# RUNNABLE
pg_hardstorage kms inspect
```

```console
Keyring: /home/you/.config/pg_hardstorage/keyring
  signing-public.pem    ed25519  fp=ab12...ef34
  signing-private.pem   0600
  kek.bin               0600  · 32 bytes
```

If you already have a keyring without a KEK, generate one with:

```bash
head -c 32 /dev/urandom > ~/.config/pg_hardstorage/keyring/kek.bin
chmod 0600 ~/.config/pg_hardstorage/keyring/kek.bin
```

#### 3. Take an encrypted backup

`backup` chooses encryption automatically when a KEK is present:

```bash
# RUNNABLE
pg_hardstorage backup db1 \
    --pg-connection "${PG_CONNECTION:-postgres://postgres:postgres@127.0.0.1/postgres}" \
    --repo file:///tmp/hs-encrypt-repo
```

Force encryption (refuse if no KEK present) with `--encrypt`. Force
plaintext (escape hatch for mixed-posture fleets) with `--no-encrypt`.

#### 4. Confirm the manifest carries an envelope

```bash
# RUNNABLE
pg_hardstorage show db1 latest \
    --repo file:///tmp/hs-encrypt-repo \
    -o json | jq '.encryption'
```

```console
{
  "kek_ref": "local:default",
  "wrapped_dek_b64": "...",
  "envelope_version": 1
}
```

The chunk bodies on disk are AES-GCM ciphertext keyed by a fresh
DEK; without the KEK, neither the manifest's `wrapped_dek` nor the
chunks tell you anything.

#### 5. Verify the envelope across every backup in the repo

```bash
# RUNNABLE
pg_hardstorage kms verify --repo file:///tmp/hs-encrypt-repo
```

`kms verify` walks every manifest and exercises the `UnwrapDEK` path
without restoring. A failure here means a KEK mismatch or
manifest corruption — fix it before you find out at restore time.

#### 6. Restore (no extra flags needed)

```bash
# RUNNABLE skip-in-ci="postverify pg_ctl-start needs continuous WAL in the repo; this tutorial does not run `wal stream`, so recovery hangs reading trailing segments. The restore + decrypt path is exercised end-to-end by the restore-correctness CI matrix."
pg_hardstorage restore db1 latest \
    --repo file:///tmp/hs-encrypt-repo \
    --target /tmp/hs-encrypt-restored
```

The restore reads the KEKRef from the manifest, opens the matching
provider, unwraps the DEK, and decrypts chunks in-process. Nothing
changes from the operator's point of view; that is the whole goal of
envelope design.

---

## Part B — AWS KMS

### What you need

- AWS credentials in scope — environment variables, IMDS, or
  `~/.aws/credentials` profile.  The role needs `kms:Encrypt`,
  `kms:Decrypt`, and `kms:DescribeKey` on the chosen key.
- A customer-managed key in the same region as the operator host.
  Aliases (`alias/pg-hardstorage-prod`) and full ARNs both work; do
  not use AWS-managed keys (you cannot rotate them on your schedule).

### Steps

#### 1. Create a CMK (one-shot, AWS CLI)

If you do not already have one:

```bash
aws kms create-key --description "pg_hardstorage demo KEK" \
    --key-spec SYMMETRIC_DEFAULT --key-usage ENCRYPT_DECRYPT
aws kms create-alias --alias-name alias/pg-hardstorage-demo \
    --target-key-id <key-id-from-above>
```

#### 2. Take a KMS-wrapped backup

Pass `--kek` with the `aws-kms://` scheme. Region (and any FIPS or
endpoint override) goes through `--kms-config`:

```bash
pg_hardstorage backup db1 \
    --pg-connection "${PG_CONNECTION:-postgres://postgres:postgres@127.0.0.1/postgres}" \
    --repo file:///tmp/hs-encrypt-repo \
    --kek aws-kms://alias/pg-hardstorage-demo \
    --kms-config region=eu-central-1
```

The runner opens the matching `kms.Provider`, asks it to wrap the
per-backup DEK, and stamps the KEKRef into the manifest. Each chunk
PUT carries the `x-amz-checksum-sha256` of the *plaintext* — the
backend rejects mismatches.

A FIPS-mode endpoint:

```bash
--kms-config region=us-east-1,use_fips_endpoint=true
```

#### 3. Confirm the envelope is anchored to the KMS key

```bash
pg_hardstorage show db1 latest \
    --repo file:///tmp/hs-encrypt-repo \
    -o json | jq '.encryption'
```

```console
{
  "kek_ref": "aws-kms://arn:aws:kms:eu-central-1:123456789012:key/abc-...",
  "wrapped_dek_b64": "...",
  "envelope_version": 1
}
```

The wrapped DEK is unwrappable only by an IAM principal with
`kms:Decrypt` on that key. A copy of the repo on a different account,
or in a region without that key, is plaintext-equivalent worthless.

#### 4. Restore from the KMS-wrapped backup

```bash
pg_hardstorage restore db1 latest \
    --repo file:///tmp/hs-encrypt-repo \
    --target /tmp/hs-encrypt-restored
```

No extra flags. The restore reads the KEKRef, opens the matching
provider, and asks AWS KMS to unwrap. If the calling principal lacks
`kms:Decrypt`, you get `kms.unauthorized` (exit 8) with the suggested
IAM statement to add.

#### 5. Verify the envelope across every backup

```bash
pg_hardstorage kms verify --repo file:///tmp/hs-encrypt-repo
```

Same command as the local-KEK leg. The verifier walks every manifest
and exercises the unwrap path; AWS KMS calls are sequential and rate-
limited, so a large repo takes longer than the local-KEK case.

---

## Crypto-shred and rotation (preview)

Two operations are wired in v0.2 but locked behind n-of-m approval:

```bash
pg_hardstorage kms shred --confirm-keyring <keyring-dir> --require-approval ...
pg_hardstorage kms rotate --repo "$REPO" \
    --old-kek-ref local:default --old-kek-file old-kek.bin \
    --new-kek-ref aws-kms://alias/pg-hardstorage-demo-2026 --new-kek-file new-kek.bin --apply
```

- **`kms shred`** destroys the local KEK irreversibly. Every backup
  wrapped under that KEK is then plaintext-unrecoverable (this is the
  point — GDPR right-to-erasure). Refuses without an approval token.
- **`kms rotate`** re-wraps every encrypted manifest's DEK with a new
  KEK, in place. The old KEK can then be retired.

Land neither in production until you have read
[R2 — KMS key destroyed](../reference/runbooks/R2-kms-key-destroyed.md).

---

## What just happened

You exercised both halves of envelope encryption. Local KEK gave you
a hermetic, single-file setup; AWS KMS handed key custody to your
cloud control plane while the wire format and operator commands
stayed identical. The manifest's `kek_ref` is the only thing that
changes between the two.

The design rule: **the repo is encrypted at rest by virtue of its
own contents — not by virtue of where it is stored.** A leaked S3
bucket, a copied disk, or a rsync to a third party leaks ciphertext
only.

---

## Next steps

- [LLM incident walkthrough](llm-incident-walkthrough.md) — using the
  helper to triage a failed restore (often a missing KMS permission).
- [R2 — KMS key destroyed](../reference/runbooks/R2-kms-key-destroyed.md) —
  recovery (or not) when the KEK is gone.
- [Operator guide — Encryption](../operations/operator-guide.md) —
  day-2 KMS operations.
