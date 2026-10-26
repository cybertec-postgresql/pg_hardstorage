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

## Not yet covered

`kms rotate`, `backup bundle` and `repair attestation` rewrite manifests and
still load them whole; they refuse a manifest over 1 GiB.
