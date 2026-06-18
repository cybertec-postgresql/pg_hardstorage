// runbook_templates.go — disaster-runbook scenario titles, SPEC refs, and text/template bodies.
package cli

// scenarioTitles is the human-readable label per scenario name. Adding
// a new entry here without a matching template in scenarioTemplates is
// a usage error caught at runtime — the table below is the source of
// truth for what `runbook list` shows.
var scenarioTitles = map[string]string{
	"corruption": "Repository corruption detected (chunk bit-rot or signature failure)",
	"dr":         "Disaster recovery — provision a new host, restore from backups",
	"failover":   "Patroni failover handled — slot dropped, possible WAL gap",
	"kms-loss":   "KEK destroyed — identify which backups become unrecoverable",
	"repo-loss":  "Primary repository region is gone — promote replica region",
	"upgrade":    "Major-version PostgreSQL upgrade — verify backups round-trip",
}

// scenarioSPECRefs maps scenarios to the R1–R7 disaster-runbook IDs
// in docs/SPEC.md. A scenario without an entry here is a v0.1
// template that doesn't yet have a SPEC counterpart; that's fine.
var scenarioSPECRefs = map[string]string{
	"corruption": "R4",
	"dr":         "R3",
	"failover":   "R6",
	"kms-loss":   "R2",
	"repo-loss":  "R1",
	// "upgrade" doesn't have a SPEC R-id yet (it's a routine
	// operation, not a disaster).
}

// scenarioTemplates is the body of each runbook. Go text/template
// syntax; available context fields:
//
//	{{ .Scenario }}      slug
//	{{ .Title }}         human title
//	{{ .Deployment }}    e.g. "db1"
//	{{ .RepoURL }}       e.g. "s3://acme-pg-backups/"
//	{{ .PGConnection }}  redacted libpq URI
//	{{ .Tenant }}        operator-set tenant scope (may be empty)
//	{{ .KeyringDir }}    where the local KEK + signing key live
//	{{ .GeneratedAt }}   RFC3339 timestamp
//	{{ .BinaryName }}    "pg_hardstorage" — kept as a variable so a
//	                     downstream rebrand doesn't re-template every body.
var scenarioTemplates = map[string]string{
	"corruption": runbookCorruption,
	"dr":         runbookDR,
	"failover":   runbookFailover,
	"kms-loss":   runbookKMSLoss,
	"repo-loss":  runbookRepoLoss,
	"upgrade":    runbookUpgrade,
}

// Each template is one short Markdown document. They share a common
// header (title + frontmatter) and a tail (where to escalate). Body
// is scenario-specific. < 1 page each — the 3am operator must read
// it top-to-bottom without scrolling.

const runbookCorruption = `# Runbook: Repository corruption — {{ .Deployment }}

> Generated {{ .GeneratedAt }} for deployment **{{ .Deployment }}**.
> Scenario ID: ` + "`{{ .Scenario }}`" + ` (SPEC R4).

## Symptom

` + "`{{ .BinaryName }} repair scrub`" + ` reported chunks whose stored bytes
do not match their key, OR ` + "`{{ .BinaryName }} repo check`" + ` reported
manifests with missing chunks. Restores depending on those chunks
will fail at chunk-fetch or SHA-verify time.

## Steps

1. **Stop the bleeding — pause backups.** A new full backup may
   reuse the corrupt chunks (deduplication is content-addressed).
   Pause your scheduled-backup job for ` + "`{{ .Deployment }}`" + `:

       {{ .BinaryName }} schedule {{ .Deployment }} off

2. **Quantify the damage.**

       {{ .BinaryName }} repo check {{ .RepoURL }}
       {{ .BinaryName }} repair chunks --missing --repo {{ .RepoURL }}
       {{ .BinaryName }} repair scrub --repo {{ .RepoURL }}

   Note the affected hashes; they're the recovery scope.

3. **Identify which backups depend on the bad chunks.**

       {{ .BinaryName }} list {{ .Deployment }} --repo {{ .RepoURL }} -o json | \
         jq '.result.body.backups[] | {backup_id, stop_lsn}'

   Cross-reference with the failing-hash list. Backups that
   reference any failing hash are unrecoverable from this repo.

4. **Re-fetch from a replica region**, if you have one. Otherwise,
   take a fresh full backup and accept the loss of any PITR
   window depending on the corrupt chunks:

       {{ .BinaryName }} backup {{ .Deployment }} --repo {{ .RepoURL }}

5. **Resume scheduled backups** once a fresh full has committed:

       {{ .BinaryName }} schedule {{ .Deployment }} "every 6h"

## Escalate when

- The corruption is in WAL chunks (PITR window unrecoverable).
- ` + "`repo check`" + ` reports the same hashes mismatching after a
  re-fetch from a replica.
- Multiple deployments share the affected chunk-prefix range
  (suggests storage-side bit-rot, not a single-backup glitch).
`

const runbookDR = `# Runbook: Disaster recovery — {{ .Deployment }}

> Generated {{ .GeneratedAt }} for deployment **{{ .Deployment }}**.
> Scenario ID: ` + "`{{ .Scenario }}`" + ` (SPEC R3).

## Symptom

The PostgreSQL instance backing **{{ .Deployment }}** is gone. The
repository is intact. You need to provision a new host and restore.

## Pre-flight

- A new Linux host with PostgreSQL of the original major version
  (or newer — the runner pins major version to the manifest).
- ` + "`{{ .BinaryName }}`" + ` installed on the new host.
- Network reachability to ` + "`{{ .RepoURL }}`" + `.
- KEK present at ` + "`{{ .KeyringDir }}`" + ` if backups are encrypted
  (check ` + "`{{ .BinaryName }} doctor`" + ` for ` + "`KEK: ✓ present`" + `).

## Steps

1. **Stage the data dir.**

       sudo install -d -o postgres -g postgres -m 0700 /var/lib/postgresql/restored

2. **Preview the restore.** Confirm pre-flight checks pass before
   committing:

       {{ .BinaryName }} restore {{ .Deployment }} latest \
         --target /var/lib/postgresql/restored \
         --repo {{ .RepoURL }} \
         --preview

3. **Execute the restore.** Drop ` + "`--preview`" + `:

       {{ .BinaryName }} restore {{ .Deployment }} latest \
         --target /var/lib/postgresql/restored \
         --repo {{ .RepoURL }}

   This invokes ` + "`pg_verifybackup`" + ` automatically. Failure aborts
   the restore.

4. **Start PostgreSQL.** Recovery applies WAL up to the latest
   archived segment, then the cluster reaches the new timeline:

       sudo -u postgres pg_ctlcluster <ver> main start

5. **Verify the cluster is consistent.**

       psql -c 'SELECT pg_is_in_recovery();'           # → f, eventually
       psql -c 'SELECT version();'

6. **Resume backups against the new host.** Edit
   ` + "`{{ .Deployment }}`" + ` to point at the new connection:

       {{ .BinaryName }} deployment edit {{ .Deployment }} \
         --connection 'postgres://pgbackup@new-host/postgres'
       {{ .BinaryName }} backup {{ .Deployment }}

## Escalate when

- ` + "`pg_verifybackup`" + ` fails after restore (corrupt manifest).
- Recovery stops with "FATAL: requested timeline X does not match"
  — see the **failover** runbook.
- The repository itself is unreachable — see **repo-loss**.
`

const runbookFailover = `# Runbook: Patroni failover handled — {{ .Deployment }}

> Generated {{ .GeneratedAt }} for deployment **{{ .Deployment }}**.
> Scenario ID: ` + "`{{ .Scenario }}`" + ` (SPEC R6).

## Symptom

A Patroni leader change happened. Our replication slot may have been
dropped on the old primary; the new primary started a new timeline.
` + "`{{ .BinaryName }} doctor {{ .Deployment }}`" + ` may report
` + "`wal.slot_missing`" + ` or ` + "`wal_gap_detected`" + `.

## Steps

1. **Check the timeline + slot state.**

       {{ .BinaryName }} wal list {{ .Deployment }} --repo {{ .RepoURL }} --gaps-only

   If gaps span a timeline boundary, that's the failover signature —
   continue. If gaps are mid-timeline, see **corruption**.

2. **Recreate the slot on the new primary.** ` + "`wal repair`" + ` is
   idempotent (does nothing if the slot is healthy):

       {{ .BinaryName }} wal repair {{ .Deployment }} \
         --pg-connection '{{ .PGConnection }}' \
         --repo {{ .RepoURL }}

   Note the reported gap (slot.restart_lsn − highest_archived). A
   non-zero positive gap means we lost some WAL across the failover.

3. **Take a fresh full backup** to anchor the new timeline. PITR
   into the gap window is no longer possible from streaming alone:

       {{ .BinaryName }} backup {{ .Deployment }} --repo {{ .RepoURL }}

4. **Verify restorability** before declaring victory:

       {{ .BinaryName }} verify {{ .Deployment }} latest --repo {{ .RepoURL }}

## Escalate when

- The reported gap exceeds your RPO budget — investigate whether
  the old primary's WAL directory is still mounted somewhere
  (` + "`pg_waldump`" + ` may recover bytes the repo never saw).
- Repeated failovers in short order — Patroni's DCS health is
  the upstream concern.
`

const runbookKMSLoss = `# Runbook: KEK destroyed — {{ .Deployment }}

> Generated {{ .GeneratedAt }} for deployment **{{ .Deployment }}**.
> Scenario ID: ` + "`{{ .Scenario }}`" + ` (SPEC R2).

## Symptom

The KEK at ` + "`{{ .KeyringDir }}`" + ` is destroyed (or a remote KMS
key has been deleted). Backups for tenant **{{ .Tenant }}** can no
longer be decrypted.

## Steps

1. **Confirm the loss.**

       {{ .BinaryName }} doctor

   Look for ` + "`KEK: ✗ absent`" + `. If a backup KEK file exists
   under a different filename, it may be salvageable — STOP here
   and recover that file before proceeding.

2. **Identify affected backups.** Every backup whose
   ` + "`encryption.kek_ref`" + ` matches the destroyed key is now
   unrecoverable. Build the list:

       {{ .BinaryName }} list {{ .Deployment }} --repo {{ .RepoURL }} -o json | \
         jq '.result.body.backups[] | {backup_id}'

3. **Document the loss as an audit event.** The compliance artifact
   IS the record that these bytes can no longer be read — that's
   the GDPR Art. 17 / PCI-DSS / SOC2 outcome of crypto-shred.

4. **Establish a new KEK** for the deployment. The next backup
   will use the new wrapping key:

       # Move the old (broken) keyring aside, then re-init.
       {{ .BinaryName }} init --pg-connection '{{ .PGConnection }}' \
         --repo {{ .RepoURL }} \
         --deployment {{ .Deployment }} \
         --encrypt --yes

5. **Take a fresh full backup** under the new KEK:

       {{ .BinaryName }} backup {{ .Deployment }} --repo {{ .RepoURL }}

## Escalate when

- The destruction was unintentional and the destruction window is
  shorter than your KMS provider's grace period — recovery may
  be possible.
- You're seeing this on a tier-0 deployment without an
  out-of-band KEK escrow.
`

const runbookRepoLoss = `# Runbook: Primary repository gone — {{ .Deployment }}

> Generated {{ .GeneratedAt }} for deployment **{{ .Deployment }}**.
> Scenario ID: ` + "`{{ .Scenario }}`" + ` (SPEC R1).

## Symptom

` + "`{{ .RepoURL }}`" + ` is unreachable or contents are gone. If you
have a replica region configured, switch to it; otherwise, the
deployment is in the **DR — provision new host** scenario.

## Steps

1. **Confirm the primary is truly gone, not just unreachable.**

       {{ .BinaryName }} repo check {{ .RepoURL }}
       # Network errors with notfound.repo on retry → really gone

2. **If a replica region exists** (e.g. ` + "`s3://acme-pg-backups-eu/`" + `),
   switch the deployment to it:

       {{ .BinaryName }} deployment edit {{ .Deployment }} \
         --repo s3://acme-pg-backups-eu/

   Verify the replica is structurally healthy:

       {{ .BinaryName }} repo check s3://acme-pg-backups-eu/

3. **Take a fresh full backup against the replica** so it becomes
   the new primary record:

       {{ .BinaryName }} backup {{ .Deployment }}

4. **Re-establish redundancy** by configuring a NEW replica
   region. A single repo with no replica is one outage from
   total loss.

5. **Audit the period of unavailability.** Note the gap in the
   audit log; the dead-man's-switch alert (if configured) should
   already have fired.

## Escalate when

- No replica region was configured — see the **dr** runbook for
  the cold-start case (PG plus repo are both lost = total loss
  outside of off-site backups).
- The replica region's manifests fail signature verification
  (` + "`signature_failures > 0`" + ` in ` + "`repo check`" + `) —
  that's a separate trust break; do not restore from it
  without re-validating the signing keypair provenance.
`

const runbookUpgrade = `# Runbook: Major-version upgrade — {{ .Deployment }}

> Generated {{ .GeneratedAt }} for deployment **{{ .Deployment }}**.
> Scenario ID: ` + "`{{ .Scenario }}`" + `.

## Goal

Verify that the existing backups round-trip into a new-major
PostgreSQL before committing the upgrade.

## Steps

1. **Take a fresh full backup BEFORE the upgrade.**

       {{ .BinaryName }} backup {{ .Deployment }} --repo {{ .RepoURL }}
       {{ .BinaryName }} verify {{ .Deployment }} latest --repo {{ .RepoURL }}

2. **Provision a sandbox host** running the NEW PG major.

3. **Restore the just-taken backup** into the sandbox:

       {{ .BinaryName }} restore {{ .Deployment }} latest \
         --target /var/lib/postgresql/sandbox \
         --repo {{ .RepoURL }} \
         --preview

   Note the source PG version reported by ` + "`--preview`" + `.

4. **Start the sandbox**, run ` + "`pg_amcheck`" + ` and your smoke
   queries. A successful boot + ` + "`pg_amcheck`" + ` clean is your
   green light.

5. **Schedule the production upgrade.** During the cutover:

   - pause backups: ` + "`{{ .BinaryName }} schedule {{ .Deployment }} off`" + `
   - perform the upgrade (` + "`pg_upgrade`" + ` or logical-replication path)
   - take a fresh full backup post-upgrade:
     ` + "`{{ .BinaryName }} backup {{ .Deployment }}`" + `
   - resume the schedule:
     ` + "`{{ .BinaryName }} schedule {{ .Deployment }} \"every 6h\"`" + `

6. **Verify** the new full backup is restorable into a fresh
   sandbox before declaring the upgrade complete.

## Escalate when

- ` + "`pg_amcheck`" + ` reports inconsistencies in the sandbox
  restore — possibly a pre-existing corruption masked by
  routine reads. Investigate via ` + "`repair scrub`" + `.
- The new PG major's recovery refuses our timeline history.
  The replay-time error message names the missing
  ` + "`<TLI>.history`" + ` file; verify the repo has it.
`
