# Why pg_hardstorage? — comparison with existing tools

This is an honest comparison — we highlight strengths *and* acknowledge
where other tools have more ecosystem maturity. The operators you hire
will already know pgBackRest or WAL-G. They need to know why to invest
in learning a new workflow. Here is the reasoning.

## pg_hardstorage vs pgBackRest

| Dimension | pgBackRest | pg_hardstorage |
|---|---|---|
| **WAL transport** | archive_command or async archive-push over SSH | replication-protocol streaming (START_REPLICATION SLOT) — no SSH or archive_command on the host |
| **Agent locality** | co-located; needs SSH to primary | remote — libpq replication connection; agent can run anywhere |
| **Dependency model** | C + Perl + libpq, requires OS access | single static Go binary, zero CGO (default), works without a compiler |
| **Backup chain** | incremental depends on chain ancestors — a corrupt increment breaks the chain | chunk-CAS — every backup is independent; no chain to maintain |
| **Dedup** | file-level delta compression, manifest-level | content-defined chunking (FastCDC, page-aligned); cross-backup deduplication |
| **Encryption** | optional, external | on by default, envelope encryption, named KMS integration |
| **Patroni-aware** | manual failover handling | automatic leader-following with permanent slots |
| **Coordination** | local state files | multi-host with PG advisory locks, K8s Leases; no extra daemon needed |
| **Compliance** | external toolchain | built-in WORM, FIPS, Merkle audit chain, cosign attestation |

## pg_hardstorage vs WAL-G

| Dimension | WAL-G | pg_hardstorage |
|---|---|---|
| **Backend selection** | separate binary per backend (wal-g, wal-g-s3, wal-g-gcs, …) | single binary, backend configured at runtime |
| **Kubernetes** | unofficial Kubernetes operator | built-in Helm charts, K8s-native coordination, operator-aware |
| **LLM assistant** | none | built-in 3am helper with MCP, tool surface, audit trail |
| **Test infrastructure** | unit + integration | multi-tier scenario framework with deterministic load generator and failure injection |
| **WAL streaming** | daemon mode or archive_command | replication-protocol streaming is the central data plane |

## pg_hardstorage vs Barman

| Dimension | Barman | pg_hardstorage |
|---|---|---|
| **Architecture** | barman-server model, cron-driven | binary agent, stream-driven, control-plane optional |
| **PG version support** | all PG-supported versions | PG 15–18 |
| **Backup model** | backup + incremental via rsync/SSH or streaming backup | streaming-only; chunk-CAS deduplication |
| **Multi-tenant** | one barman server per customer or careful config setup | per-tenant KEK, tenant-scoped RBAC, GDPR shred |
| **Operator integration** | none built-in | Helm charts, K8s CRDs, Patroni-aware, cloud-native recovery |
| **Migration from legacy** | — | drop-in CLI shims for Barman, WAL-G, and pgBackRest |

## When to use pg_hardstorage

- You are on PostgreSQL 15+ (the tool leans into PG 15+ features like non-exclusive backup, archive_library, PG 17 synced-slots).
- You want zero-agent-co-location — backup from a central host in the VPC.
- You are on Kubernetes or Patroni and want the tool to work with your auto-failover, not fight it.
- You care about compliance and don't want bolt-on third-party tooling.
- You want one binary that works from a 10 GB single-instance PG to a 100+ TB Patroni cluster.

## When to consider alternatives

- You are on PG 14 or earlier in production and cannot upgrade — use pgBackRest.
- You depend heavily on the operational runbooks your team has refined for WAL-G / pgBackRest — we have migration shims but adoption takes time.
- You need a lot of community examples — the younger tool has a smaller corpus.

## Acknowledge what we don't (yet) do

- No pgBackRest-style differential model (the pgBackRest "differential against a prior backup" approach with its own restart-point bookkeeping).  We **do** ship PG 17+ native incremental via `BASE_BACKUP INCREMENTAL` — the `--incremental-from <parent>` flag on `pg_hardstorage backup` produces a block-level delta against the parent's PG `backup_manifest`, and restore flattens an incremental chain through `pg_combinebackup`.  Requires `summarize_wal = on` on the source server (PG 17 requirement).
- No Barman-style WAL peg-out via pg_receivewal (we stream from a replication slot).
- The FIPS build variant exists but the PKCS#11 path is still in-progress.
- No pgaudit integration (yet).
- No auto-detection of TDE — operators with CYBERTEC PGEE / pg_tde / EDB TDE declare it via `tde.enabled: true` in deployment config.  Once declared, every code path that would otherwise parse PG byte layout off disk relaxes to "ciphertext, don't peek", manifests carry a `source_tde` block, and restore-time tooling skips checksum gates that would be meaningless against ciphertext.  See [TDE awareness](tde-awareness.md).

If those matter more for your compliance posture, we want to know — and they are on our roadmap.