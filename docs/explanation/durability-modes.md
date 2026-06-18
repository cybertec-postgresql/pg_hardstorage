---
title: Durability modes — when bytes hit stable storage
description: Why the WAL streamer and base backup batch their fsyncs, what per-segment vs per-chunk trade, and why pg_hardstorage is not a synchronous standby.
tags:
  - wal
  - durability
  - fsync
  - performance
---

# Durability modes — when bytes hit stable storage

Every chunk `pg_hardstorage` writes to a filesystem repository is a
file. The question this page answers is a narrow one: **between
"the bytes are in the page cache" and "the bytes survive a power
cut", who calls `fsync`, and how often?**

The answer used to be "every chunk, always". That was correct and
catastrophically slow. This page explains what replaced it, what
the replacement gives up, and — honestly — what it still does not
do.

---

## The problem: one fsync per chunk

The content-addressed store splits every backup and every WAL
segment into chunks of roughly 64 KiB (FastCDC, 4/64/256 KiB
min/avg/max). A 50 GB database is on the order of **a million
chunks**.

The original filesystem-plugin write path fsynced each chunk file
*and* its parent directory immediately after writing it. That is
~2 million serial `fsync(2)` calls for one base backup. Each
`fsync` is a full round-trip to stable storage; on a spinning
disk or a network block device that is single-digit milliseconds
apiece. The arithmetic is unforgiving — the write path spent
almost all of its wall-clock time parked in `fsync`, not
chunking, compressing, or transferring.

The observable symptoms, from the 50 GB streaming-backup test
that triggered this work:

- Base backup ran at **~6.5 MB/s** with the CPU ~13% busy — the
  process was I/O-wait-bound on `fsync`, not compute-bound.
- The WAL streamer, sharing the same per-chunk-fsync path, could
  not keep pace with a `VACUUM FULL` WAL burst and fell **~49 GB
  behind**. A table created after the base backup was therefore
  not yet in the repository when the test killed PostgreSQL, and
  was unrecoverable.

Per-chunk fsync is the *strongest* durability you can offer. It is
also the wrong default, because the strength is wasted: a backup
is not valuable half-written. What matters is that the **whole**
backup — or the **whole** WAL segment — is durable before its
manifest is committed. Durability at any finer grain buys nothing
and costs everything.

---

## The fix: defer the writes, barrier once

The write path now separates *writing* a chunk from *making it
durable*.

A chunk written with `DurabilityDeferred` lands in the page cache
and returns immediately — no `fsync`. The plugin remembers it. A
later **`Barrier`** call flushes every deferred write in one
operation, and only then are the bytes guaranteed durable.

On Linux the barrier is a single **`syncfs(2)`** — one syscall
that flushes the entire filesystem. A million deferred chunks cost
*one* `syncfs`, not a million `fsync`s. On non-Linux platforms the
barrier falls back to fsyncing each staged file plus its
directory; still batched, just without the single-syscall
shortcut.

The caller's contract is simple and is the **core invariant** of
the whole feature:

> Never commit a manifest — never advance an LSN PostgreSQL is
> told is flushed — until a `Barrier` has returned for every
> chunk that manifest references.

A base backup writes all its chunks deferred, calls `Barrier`
once, *then* fsyncs and commits the manifest. The WAL streamer
does the same per 16 MiB segment.

### Crash-safety: staged, then linked

Deferring raises an obvious hazard. If a deferred chunk were
written directly at its final content-addressed key and the
process crashed before the `Barrier`, a later run would find a
**truncated file sitting at a real content key**. The CAS dedup
check (`O_EXCL` create — "does this key exist? then skip it")
would trust that corrupt file and a future backup would commit a
manifest pointing at unrecoverable bytes.

So a deferred chunk is never written at its final key. It is
written to a staging name — `<key>.deferred-<random>` — and the
`Barrier` is what links staging to the final key, *after* the
`syncfs` has made the staged bytes durable. The link itself is
`O_EXCL`, so concurrent writers and dedup stay correct.

The consequence, which the storage-plugin tests pin explicitly: a
deferred chunk is **not visible at its content key until the
`Barrier` returns**. A crash before the barrier leaves only
harmless `.deferred-*` debris — never a poisoned content key. This
is the property that makes deferral safe for a backup tool rather
than merely fast.

---

## The two modes

The WAL streamer exposes the choice as `wal stream --durability`:

| Mode | fsync behaviour | Use it when |
| --- | --- | --- |
| **`per-segment`** *(default)* | Chunks deferred; **one barrier per processor batch** — adaptively a single 16 MiB segment under a light trickle, up to 16 segments under a sustained burst — before that batch's manifests commit. | Normal operation. An async WAL archiver's durability unit is the segment; batching amortises the whole-filesystem `syncfs` without ever advancing the flush LSN past un-barrier'd WAL. |
| **`per-chunk`** | Every chunk fsynced inline, as before. | A compliance regime that demands each object be independently durable, and only on **small** databases — the throughput cost is real. |

The base backup always uses the deferred-plus-barrier path: all
chunks deferred, one barrier, then the manifest. There is no
per-chunk knob for base backup because a half-fsynced base backup
was never a meaningful durability state.

`per-segment` is the default because it is what the durability
*model* already was — PostgreSQL's own archiver, `pg_receivewal`
in its default mode, and every gap-free async-replication design
treat the 16 MiB segment as the atomic unit. The old per-chunk
behaviour was finer-grained than the guarantee it supported.

What `per-segment` costs: a crash between a segment's barrier and
the next segment's barrier loses at most the in-flight segment's
work — which PostgreSQL re-sends from the slot on reconnect
anyway. The confirmed-flush LSN is advanced only *after* the
barrier, so PostgreSQL is never told a segment is durable before
it is. The crash window does not lose acknowledged data; it only
re-does unacknowledged work.

### A complementary fix: parallel chunking

Removing the fsync stall exposed a second, smaller bottleneck —
chunking and compressing each unit was single-threaded. Both the
base backup's per-file chunker and the WAL streamer's per-segment
chunker now fan the compress+encrypt+upload of their chunks across
a worker pool (`min(GOMAXPROCS, 8)` workers), reassembling results
in offset order. This is orthogonal to durability — it speeds up
the compute the deferred-write change stopped hiding behind I/O
wait.

---

## The WAL streamer is a pipeline, and an *async* archiver

The WAL streamer earns its own honest treatment, because two
distinct things were wrong with it and only one is a simple "fix".

**It used to process segments inline on the receive goroutine.**
`replication.Stream` calls the sink's `OnRecord` on its single
socket-reading goroutine, whose contract is "return promptly". The
old sink did not: when a 16 MiB segment filled, `OnRecord` ran the
whole chunk → compress → barrier → manifest-commit *inline* before
returning. WAL receipt and WAL processing never overlapped, and
the per-segment whole-filesystem `syncfs` — issued on a disk a
busy primary is also writing to — stalled the receive loop for
seconds at a time. Measured against a 50 GB database under a
`VACUUM FULL` storm, the streamer archived at ~11 MB/s.

The sink is now a **two-stage pipeline**: `OnRecord` only memcpy's
into a buffer and hands filled segments to a background processor
over a bounded channel, returning to the socket immediately. The
processor adaptively batches segments, issues one Barrier per
batch, chunks through the worker pool, and commits manifests in
ascending order. That removed the serialization and the per-
segment `syncfs`; the same workload now archives several times
faster.

**But faster is not the same as instant — and this is the part
that must not be oversold.** The WAL streamer is an *asynchronous*
archiver. When PostgreSQL emits WAL faster than the streamer can
compress and write it — a `VACUUM FULL`, a bulk `COPY`, an index
build — the streamer falls behind *during the burst*. That is not
a bug: PostgreSQL's replication slot retains the un-acked WAL, and
the streamer drains the backlog over the following minutes once
the burst subsides. The guarantee a streaming backup makes is:

> **Every committed WAL byte eventually reaches the repository,
> provided the streamer is allowed to keep running.**

It is *not* "the repository is always within N seconds of the
primary". An operator (or a test) that stops the streamer
moments after a multi-gigabyte burst, and then restores, will
find the burst's WAL not yet archived — and recovery to an LSN
inside that gap will correctly fail. The fix for that is to let
the streamer catch up, not to expect zero lag.

### Honest stop reporting

Because of the above, `wal stream` does not claim more than it
delivers. When the streamer stops with its synced LSN still
behind the primary's flush position, the result reports
`clean_stop: false` and emits a `wal.stream.stopped_with_unarchived_wal`
warning carrying the `lag_bytes`. A stop that left WAL
un-archived is a real, surfaced condition — never a silent
`clean_stop: true`. The operator is told, in the audit trail,
exactly how much WAL is not yet safe.

### Verified end-to-end

The 50 GB streaming `t_check` scenario is the proof: seed a 50 GB
database, take a streaming base backup, run a `VACUUM FULL` (which
emits ~88 GiB of WAL), create a table *after* the backup, let the
streamer drain, kill PostgreSQL, restore, and assert the
post-backup table is present on the restored cluster. With the
async pipeline the streamer archives the full ~88 GiB, the restore
recovers cleanly to the post-table LSN, and the table is there —
`pass: true`. The table existed only in WAL written after the base
backup, so the pass is a direct proof that streaming + replay
carried it through.

---

## What this is *not*: a synchronous standby

It is tempting to read "durability modes" and ask for a `sync`
mode — one where `pg_hardstorage` joins `synchronous_standby_names`
and PostgreSQL blocks each `COMMIT` until we acknowledge the WAL,
the way `pg_receivewal --synchronous` does. **That mode does not
exist, and this section explains why honestly.**

`pg_receivewal --synchronous` can be a synchronous standby because
it flushes and acknowledges WAL at **record granularity** — it can
tell PostgreSQL "flushed up to LSN X" for an X in the middle of a
segment, microseconds after receiving it.

`pg_hardstorage`'s WAL streamer is **segment-granular by
construction**. It assembles a full 16 MiB segment in memory,
chunks it through the CAS, commits a per-segment manifest, and
*then* advances `SyncedLSN`. It cannot honestly report a
flush LSN in the middle of a segment, because the segment's chunks
are not yet barriered and its manifest is not yet committed —
reporting it would violate the core invariant above. Making the
streamer record-granular is a `walsink` architecture change, not a
configuration flag.

So a real synchronous-standby mode is **not implemented**. What
*is* implemented (see [the WAL pipeline preflight](wal-pipeline.md#stream-startup-preflight))
is **detection** of an operator who configured one anyway:

- The streamer pins its `application_name` to the replication
  slot name, so it is identifiable in `pg_stat_replication`.
- Preflight reads `synchronous_standby_names`. If `pg_hardstorage`
  is named in it, preflight emits:
  - **`sync_standby.named`** — a *warning*. PostgreSQL will wait
    for our acknowledgement, which arrives only at 16 MiB segment
    boundaries. Commit latency on the primary will be spiky and
    coarse. Streaming still works; the operator is told the
    latency cost is real.
  - **`sync_standby.remote_apply`** — *fatal*, the stream refuses
    to start. With `synchronous_commit = remote_apply` PostgreSQL
    waits for the standby to *apply* (replay) WAL. A WAL archiver
    never replays anything, so it can never report an apply LSN,
    so **every `COMMIT` on the primary would hang forever**.
    Refusing to start is the only safe answer.

The honest summary: `pg_hardstorage` is an excellent *asynchronous*
WAL archiver with a tight, gap-free RPO measured in seconds. It is
not a zero-RPO synchronous replica, and it now tells you loudly if
it has been mistaken for one. The `sync-target` row in the
[streaming-modes table](wal-pipeline.md#streaming-modes-auto-selected-by-topology)
describes a spec aspiration, not shipped behaviour — track its
status in `SPEC.md`.

---

## Further reading

- [WAL pipeline](wal-pipeline.md) — the streaming data plane the
  `per-segment` barrier sits inside, and the stream-startup
  preflight that carries the sync-standby checks.
- [Content-addressed storage](content-addressed-storage.md) — the
  chunker whose `Put` calls the durability options thread through.
- [`wal stream` CLI reference](../reference/cli/pg_hardstorage_wal_stream.md)
  — the `--durability` flag.
