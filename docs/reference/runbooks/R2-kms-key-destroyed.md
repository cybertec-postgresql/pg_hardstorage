# R2 — KMS key destroyed

The KEK that wraps DEKs for one or more backups is gone. This is a
crypto-shred event — affected backups are bit-for-bit unrecoverable.
Your job is to triage what's lost, prove the audit trail, and stop
new backups from being written under a missing key.

## Symptoms

- `pg_hardstorage restore` or `verify` exits with `kms.unreachable`
  or `kms.key_missing` (exit 8). The body carries the manifest's
  `KEKRef`.
- `pg_hardstorage kms inspect` shows the keyring file missing,
  truncated, or owned by a different user.
- A `kms.shred` audit event exists if this was deliberate; absent
  if not.

## Pre-flight

- Confirm the KEK is genuinely gone. Check backups, snapshots,
  HSM, or wherever your key custody layer keeps it. Do this before
  declaring data loss.
- Identify which `KEKRef` values reference the missing key. Every
  manifest carries one in `encryption.kek_ref`.
- Confirm whether this was an authorised crypto-shred (compliance
  request, GDPR Article 17, etc.) — if so, the audit event already
  exists.

## Procedure

1. **Stop new backups under the missing key.** Move or rename the
   keyring path so `init` doesn't silently regenerate a fresh KEK
   that masks the loss:

   ```sh
   mv ~/.config/pg_hardstorage/keyring ~/.config/pg_hardstorage/keyring.lost.$(date +%s)
   ```

2. **Inventory affected backups.** The `KEKRef` lives in each
   manifest under `encryption.kek_ref`:

   `list` does not carry the `encryption` block; the per-backup
   manifest does. Enumerate backups, then read each manifest with
   `manifest show` (whose body embeds the full manifest, including
   `encryption.kek_ref` and `backup_id`):

   ```sh
   for d in $(pg_hardstorage deployment list -o json | jq -r '.result.body.deployments[].name'); do
     for b in $(pg_hardstorage list "$d" -o json | jq -r '.result.body.backups[].backup_id'); do
       pg_hardstorage manifest show "$d" "$b" -o json | jq -r --arg key "<missing-kek-ref>" \
         'select(.result.body.encryption.kek_ref == $key) | "\(.result.body.backup_id)"' \
         | sed "s|^|$d |"
     done
   done
   ```

3. **Tombstone affected backups** so retention sweeps and listings
   don't surface unrecoverable data as restorable. Use a hold for
   forensic preservation if needed:

   ```sh
   pg_hardstorage hold add <deployment> <backup-id> \
       --holder <responsible-party> \
       --reason "KEK lost, audit reference <ticket>"
   ```

4. **If this was an authorised shred,** append the compliance event:

   ```sh
   pg_hardstorage audit append kms.shred \
       --repo <repo-url> \
       --reason "GDPR Art 17 #<ticket>; kek-ref <missing-kek-ref>"
   ```

   `audit verify-chain` afterwards must remain clean.

5. **Generate a fresh KEK** and rotate the deployment forward:

   ```sh
   mkdir -p ~/.config/pg_hardstorage/keyring
   pg_hardstorage init --pg-connection ... --repo ... --deployment <d> --yes
   ```

6. **Take a fresh backup** under the new KEK as a safe restore
   floor.

## Verification

- `pg_hardstorage doctor` is clean.
- `pg_hardstorage kms inspect` shows the new KEK present, mode 0600,
  matching public-key fingerprint on the new manifest.
- `pg_hardstorage audit verify-chain` passes.
- A `verify` against any new backup succeeds (exit 0).

## Rollback

There is no rollback for a destroyed KEK. The point of crypto-shred
is irreversibility. If the KEK turns up later (was just on a
different host), drop it back at the keyring path; previously
unrecoverable backups become readable again.

## Post-incident

- Document scope: how many backups, which deployments, which time
  range.
- Notify per organisational policy. If this was unintentional and
  customer-impacting data is lost, that is a breach event.
- Tighten key custody: HSM/PKCS#11/TPM-sealed keys (v0.5+) or
  cloud-KMS-backed KEKs prevent single-host key loss.
- Add a check to `doctor`-driven alerting that flags missing
  keyring files immediately rather than at next read.
