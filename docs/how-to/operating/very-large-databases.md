---
title: Very large databases
description: What bounds memory and disk when a backup has billions of chunks, and how to size the spool.
tags:
  - operations
  - scale
  - manifest
---

# Very large databases

A backup's manifest lists every chunk of every file. At the default 64 KiB
average chunk size that is about 115 bytes of manifest per 64 KiB of
database: roughly **0.18 % of the database size**. A 200 TB database has
about 3.3 billion chunks and a manifest of about 380 GB.

pg_hardstorage handles manifests of that size without holding them in
memory. The on-disk format is unchanged.

## What is bounded, and by what

| What | How it is kept off the heap |
|---|---|
| Manifest being written | file entries are spooled to disk as the tar stream is chunked (`SpoolDir`) |
| Signature | Ed25519 needs the whole canonical document as one buffer, so it is written to a temp file and memory-mapped; it costs disk and page cache, not heap |
| Manifest being read | streamed one file entry at a time; a manifest under 64 MiB is still read in one piece |
| Sets of chunk hashes (GC marks, integrity, verify, dedup counts, adoption) | exact, disk-spilling sets (`internal/spill`) with a Bloom filter per on-disk run; the heap budget is `spill.DefaultMemEntries` (4 M hashes) per set |
| Dedup hints | a Bloom filter (~1.25 bytes per chunk); a false positive costs one `Stat` |
| S3 manifest upload | multipart, so the 5 GiB single-PUT limit does not apply |

## Disk you need

- **Spool**: about the manifest size, twice while the signature is
  computed (entries file + canonical copy). Set it with the runner's
  `SpoolDir` (defaults to the system temp dir).
- **Spill files** for the hash sets: about 32 bytes per distinct chunk
  while a command runs (verify, integrity, GC, restore plan, list).
  They go to the system temp dir, i.e. `$TMPDIR`; point it at a large
  disk-backed volume. Merging runs transiently needs the merged size again
  on top, so a set checks the free space first and fails with a clear
  "not enough free space ... set TMPDIR" error instead of hitting ENOSPC
  mid-run. A warning is printed once when the directory is on tmpfs/ramfs,
  because spilled hashes would then live in RAM.

## Memory sizing

Heap does not grow with the database, but three terms do grow slowly with
the number of distinct chunks `N` in a set. Per hash set (a command can hold
two or three at once, for example GC's reference set plus its orphan scan):

| Term | Size | Bounded by |
|---|---|---|
| Unflushed buffer | up to `spill.DefaultMemEntries` = 4 Mi hashes, about 300 MiB as a Go map | constant |
| Bloom filter, one per on-disk run | about 1.25 bytes per key (10 bits) | each run's filter is capped at 128 MiB (`DefaultMaxBloomBytes`, full precision up to about 107 M keys per run); past the cap the filter is leakier and a few more blocks are read |
| Sparse index | one 32-byte key per 1024 on-disk keys, about 0.03 bytes per key | grows with `N` |

For a 100 TB database at the default 64 KiB average chunk, `N` is about 1.6
billion: plan on the 300 MiB buffer plus roughly 2 GB of Bloom filters and
about 50 MB of sparse index per set, on top of the process baseline. With
`--chunk-size 1MiB` (see below) the same database has about 100 million
chunks and the Bloom and index terms are 16 times smaller. These are
derived from the structures above, not a measured guarantee; run a
`verify` or a dry-run `repo gc` against your own repository and watch RSS
before relying on a number.

Disk grows with `N` as well (see the next section), so size `$TMPDIR` first.

## Garbage collection at scale

`repo gc` (dry-run or `--apply`) marks every referenced chunk into a
disk-spilling set, streaming the manifests, so it needs `$TMPDIR` space of
about 32 bytes per distinct referenced chunk (plus the same again
transiently while runs merge) and no heap proportional to the repository.

`repo gc --apply` is fenced against concurrent writers (see
[gc safety](../../operations/operator-guide.md#gc-safety-running-apply-next-to-live-writers)).
Each delete batch ends in a fence checkpoint: a run-record bump, a pin
read, a backup-lease check and a rescan of manifests committed since the
snapshot. Writers that deduplicate against chunks wait for the run's
in-flight checkpoint, for at most 5 minutes (`GCFenceMaxWait`), so a
checkpoint must not take longer than that. By default one checkpoint covers
as many delete slices as fit in about a fifth of that wait (the adaptive
window), so a repository with hundreds of millions of orphans pays for the
rescan once per window rather than once per 4096 chunks. A live backup lease
stops an `--apply` run (`conflict.gc_backup_in_flight`, exit 7); the run is
safe to repeat. Before it may delete anything an `--apply` run waits a
60-second settle period after publishing its run record (overlapped with
the initial walk, so on a large repository it costs nothing extra).

## Restoring a large database: resume

A restore that is interrupted (killed, network loss, full disk) can be
continued by running the **same command again** against the same `--target`.
No flag is needed; do not remove the target and do not add `--force`.

While a restore runs it keeps two bookkeeping files in the target
directory:

- `.pg_hardstorage_restore_state.json`, the checkpoint: backup ID,
  deployment, byte and chunk totals. It is rewritten (atomically) about
  every 1 GiB written and at the end.
- `.pg_hardstorage_restore_done.log`, an append-only log with one completed
  file per line. It holds the completed set instead of the checkpoint, so
  neither the writer nor a resuming restore holds or re-sorts a list that
  grows with the file count (millions of files on a 100 TB cluster). The
  checkpoint records how many log bytes are committed; a torn tail left by
  a crash is ignored and overwritten. Checkpoints written by older versions
  with an inline `completed_files` list are still read and migrated.

On resume the completed files are skipped and only files that were not
fully written are fetched again (granularity is the whole file, so a
partly written 1 GiB relation segment starts over). The restore emits a
`restore/resumed` event with `resumed_bytes` and `resumed_chunks`. Both
files are removed when the data directory is fully materialised.

If the target holds a checkpoint for a **different** backup the restore
refuses with `conflict.checkpoint_mismatch`; the message names the backup ID
to re-run with. A chain (incremental) restore keeps its link staging under
`--chain-staging-root` and preserves it across retries; `--reset-chain-staging`
discards it.

## Fewer, larger chunks

`pg_hardstorage backup --chunk-size 1MiB` makes the average chunk 1 MiB
(minimum 1/16, maximum 4×), which cuts chunk objects, manifest entries and
dedup hashes by 16×. Dedup is coarser, and because chunk boundaries move
with the size, a backup taken with a different size shares few chunks with
earlier ones. Choose one value per deployment before the first backup.

## What is still slow

Everything that verifies a manifest reads all of it, because the signature
covers the whole document: expect on the order of 40 minutes per 380 GB
manifest at 160 MiB/s, per pass. `list`, `show`, `verify` and `restore`
each do one or two passes per backup. There is no per-file index; "restore
one table" still scans the manifest.

## Size limits

No command refuses a manifest because of its size. Every reader of a backup
manifest (`list`, `show`, `status`, `verify` and `verify --full`,
`repo check`, `repo scrub`, `restore` and its planning, `backup delete` /
`undelete`, `rotate`, `integrity`, `doctor`, `recovery drill`, `kms rotate`
and `kms shred`, `repo bundle export` / `import`, `repo replicate`, `repo gc`
and `wal prune`) streams it, and the two repair commands that rewrite one,
`repair manifest` and `repair attestation`, copy and re-sign it as a stream
through a temp file. `repair attestation` needs disk for the re-signed copy
(about the manifest size) in `$TMPDIR`.

The 1 GiB constants that earlier releases applied to manifests
(`MaxManifestBytes`, `MaxMetadataBytes`) now only bound objects that are read
whole on purpose: tombstones, holds, WAL segment manifests, key records, and
backup manifests under 64 MiB. A manifest above 64 MiB is always streamed,
so an existing multi-GB manifest written by 1.5.0 is read as it is; nothing
has to be re-taken. The streaming readers bound one file entry (256 MiB of
JSON, a 1 GiB relation segment is about 2 MiB) and the whole document at
16 TiB.

## Scheduled backups

`backup --chunk-size` has an agent equivalent, because a scheduled backup
has no command line:

```yaml
deployments:
  db1:
    schedule:
      backup: { every: "24h" }
    backup:
      chunk_size: 1MiB   # average; min = 1/16, max = 4x. One value per deployment, set before the first backup.
      timeout: 36h       # wall-clock ceiling for one run
```

Without `backup.timeout` the agent stops a run after twice the schedule
interval, clamped to 1 h .. 48 h (so `every: 6h` gives 12 h). A full backup of
a multi-TB database that needs longer than that must either set
`backup.timeout` or use a longer interval.
