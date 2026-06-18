# Upgrading

`pg_hardstorage` ships with explicit compatibility commitments so
you can roll fleet upgrades without coordination flag-days. This
page is what an operator should read before bumping a binary.

---

## Version skew between agents

Forward-compat by **one minor version**. An agent at version
`v0.<n>.<x>` reads everything written by `v0.<n-1>.<y>` and
`v0.<n+1>.<y>`. That window is what makes rolling upgrades safe:
some hosts run the new binary, others still run the old, and they
share a repo without breakage.

Wider skew (more than one minor) is not tested. Plan rolling
upgrades inside that window. The control plane (when v0.5+ ships
it) negotiates capabilities at registration time and refuses agents
outside the window.

You can check what version produced a manifest:

```sh
pg_hardstorage show db1 <backup-id> | jq '.result.body.producer_version'
```

Mixed-version evidence in the same repo is a normal state. Migrate
clients before you migrate the central server, or vice versa — the
ordering doesn't matter as long as no two ends diverge by more than
one minor.

---

## Manifest schema 24-month back-compat

Every on-disk and on-the-wire schema carries a versioned `schema:`
field. The commitment is **24 months of backward compatibility for
readers**: a v0.1 manifest is readable by every release that ships
through 2028-04, and so on for every subsequent release.

The schemas covered:

| Schema                              | Current | First shipped |
| ----------------------------------- | ------- | ------------- |
| `pg_hardstorage.v1` (output)        | v1      | v0.1.0        |
| `pg_hardstorage.config.v1`          | v1      | v0.1.0        |
| `pg_hardstorage.repo.v1` (HSREPO)   | v1      | v0.1.0        |
| `pg_hardstorage.manifest.v1`        | v1      | v0.1.0        |
| `pg_hardstorage.wal_segment.v1`     | v1      | v0.1.0        |
| `pg_hardstorage.tombstone.v1`       | v1      | v0.1.0        |
| `pg_hardstorage.audit.v1`           | v1      | v0.1.0        |
| Chunk envelope                      | v0x02   | v0.1.0        |

What this means in practice:

- A backup taken on v0.1 is restorable on every release through
  v1.x without conversion.
- The CLI's JSON output won't drop fields you read; new fields may
  appear (consumers must ignore unknown fields).
- The chunk envelope has both v0x01 (legacy pre-encryption) and
  v0x02 (with encryption metadata) readers, so older backups in a
  long-lived repo continue to read clean.

When a schema does break (post v1.0, outside the 24-month window),
the major version of the binary increments and a one-shot conversion
tool ships in the same release.

---

## Repo metadata (HSREPO) versioning

The `HSREPO` file at the repo root is the schema anchor. Its layout:

```json
{
  "schema":     "pg_hardstorage.repo.v1",
  "id":         "8a7b...",
  "created_at": "2026-04-29T12:00:00Z",
  "tenants":    ["default"]
}
```

When you upgrade a binary that introduces new fields, the writer
appends them; older readers ignore unknowns. When the schema major
bumps (post v1.0 only), the binary refuses to operate against an
older HSREPO until you run the conversion tool that ships in that
same release.

`pg_hardstorage repo check <url>` prints the HSREPO state and warns
on any anomaly.

---

## KEK rotation

The local KEK at `<keyring>/kek.bin` is what wraps every backup's
DEK. Rotating it is two-step:

1. Generate a new KEK file beside the old one
   (e.g. `<keyring>/kek-2027.bin`).
2. Run `pg_hardstorage kms rotate --repo <url> --old-kek-ref local:default --old-kek-file kek.bin --new-kek-ref local:2027 --new-kek-file kek-2027.bin --apply`.

`kms rotate` walks every manifest, decrypts the wrapped DEK with
the old KEK, rewraps with the new KEK, atomically rewrites the
manifest, re-signs (chunks are NOT re-encrypted — this is the
point of the wrapping layer), emits an audit event per manifest.

`kms rotate` is deferred to v0.5. Until it ships, the practical
v0.1 path:

- Take the rotation as a generational boundary: keep the old KEK
  forever, start writing new backups under a new KEK by moving the
  keyring path. Old backups remain readable as long as the old
  keyring is preserved.
- After all backups under the old KEK have aged out per retention,
  the old KEK can be destroyed.

In FIPS-build profiles (v0.5+), the rotation also rewrites the
audit log marker so the chain shows continuity across the rotation.

---

## Audit chain continuity across upgrades

The audit log is a Merkle hash chain — each event's `hash` field
includes the previous event's hash, so any tamper or omission is
detectable.

Across an upgrade:

- The chain spans versions. Events written by v0.1 link cleanly to
  events written by v0.2.
- A new release adding new event types extends the type vocabulary;
  it does not break the hash format.
- If a `kms rotate` runs across an upgrade, the rotation events
  themselves are part of the chain; `audit verify-chain` after the
  upgrade should pass on the same repo it passed against before.

If `audit verify-chain` flags
`verify.audit_chain_broken` (exit 9) right after an upgrade,
do not roll forward: the upgrade has either lost an event, or
written one in a non-canonical form. File the diagnostic
(`pg_hardstorage audit search --since <upgrade-time>`) and bisect
against the previous binary.

The audit log lives at `audit/<yyyy>/<mm>/<dd>/<seq>-<id>.json`
under the repo root. Schema: `pg_hardstorage.audit.v1`. The chain
is ordered by `seq` within a day and across days lexicographically.

---

## Upgrade procedure (single-host)

1. Take a fresh backup before the upgrade. Practice good hygiene.
2. Stop the agent (`systemctl stop pg_hardstorage` or kill the
   `wal stream` and `agent` processes).
3. Replace the binary. The package upgrade does this for you.
4. Run `pg_hardstorage doctor` — it should pass clean. Any
   schema-skew warning surfaces here.
5. Run `pg_hardstorage repo check <repo>` and
   `pg_hardstorage audit verify-chain` — both should pass.
6. Restart the agent.

If step 4 or 5 fails, do not run further mutations. Roll back to
the previous binary, file the diagnostic, and contact the
maintainer (Hans-Jürgen Schönig <hs@cybertec.at>).

---

## Upgrade procedure (fleet)

Do it host-by-host inside the one-minor compat window:

1. Pick a non-leader agent. Stop it, replace, restart, verify
   `doctor` is clean.
2. Repeat for every non-leader.
3. For the leader (or active control plane), trigger a leader
   election (`pg_hardstorage agent --step-down` in v0.5+, or
   `systemctl restart` today) so a freshly-upgraded agent takes
   over.
4. Replace the old leader's binary, restart.

The whole sequence stays inside the one-minor compatibility window
because no agent ever sees a peer more than one minor away.
