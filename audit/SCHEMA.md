# Audit Finding Schema

Single source of truth for finding shape. Every line in every `*.jsonl` file under `audit/` MUST parse as one JSON object matching the schema below.

## Object schema

| field          | type     | required | notes                                                                                          |
|----------------|----------|----------|------------------------------------------------------------------------------------------------|
| `id`           | string   | yes      | `F-NNNN` assigned by aggregator; raw scouts may omit (aggregator fills)                         |
| `severity`     | string   | yes      | one of `critical`, `high`, `medium`, `low`, `info`                                              |
| `lens`         | string[] | yes      | subset of {`bug`, `unsafe`, `corruption`, `perf`, `docs`, `mem`, `security`, `false-claim`}    |
| `scope`        | string   | yes      | one of the 13 scout scope keys from `SCOPE.md`                                                  |
| `file`         | string   | yes      | repo-relative path                                                                             |
| `lines`        | string   | no       | `start-end` or `start`; omit when not applicable (e.g. doc-wide claim)                         |
| `snippet`      | string   | yes      | verbatim quoted code/text, ≤400 chars; never paraphrased                                        |
| `issue`        | string   | yes      | one-sentence statement of what's wrong                                                          |
| `evidence`     | string   | yes      | why the conclusion is grounded (caller chain, docs reference, execution path)                   |
| `fix_sketch`   | string   | yes      | concrete change — function/symbol, what to do, no hand-waving                                  |
| `risk`         | string   | yes      | what breaks (correctness/perf/compat/security) if the fix is applied without care              |
| `verification` | string   | yes      | the test/command/scenario that proves the fix; for doc claims, the source location that matches |
| `confidence`   | string   | no       | `high` \| `medium` \| `low` — high = reproducer obvious, low = requires further analysis         |

## Severity rubric

- **critical** — data loss / silent corruption / auth bypass / RCE / vault unlock by attacker
- **high**     — likely bug in hot path, fix likely straightforward, no obvious workaround
- **medium**   — correctness doubt in cold path, ergonomic/performance issue with real cost, doc-code drift that misleads
- **low**      — code-smell / dead path / minor inconsistency / doc typo
- **info**     — observation worth recording but not a defect (e.g. `// XXX: review` comment, intentional but surprising behaviour)

## Lenses

- `bug`         — incorrect behaviour under normal use
- `unsafe`      — Go `unsafe` / CGo / shell injection / unchecked type assertion / nil deref
- `corruption`  — risk of producing or accepting a corrupt backup/restore
- `perf`        — avoidable allocation / O(n²) / unbounded goroutine / leak
- `docs`        — incorrect, missing, or stale documentation relative to code
- `mem`         — unbounded slice/map growth, missing bounds, large struct value copy
- `security`    — secret leak, weak crypto, missing authn/authz, SSRF, command injection
- `false-claim` — README/SPEC/changelog claims something the code does not do

## JSONL rules

- One object per line. No trailing comma. No surrounding `[]`.
- No markdown fences. The file is parsed line-by-line; fences will break parsers.
- Use `\n` inside string values (literal backslash-n, two characters).
- Sort by `(file, lines)` for stable diffs across runs.

## Example

```jsonl
{"id":"F-0042","severity":"high","lens":["bug","corruption"],"scope":"restore-verify-recovery","file":"internal/restore/stream.go","lines":"412-438","snippet":"n, err := r.Read(buf[:n])\nif err != nil { return err }","issue":"Checksum is not verified when the read returns fewer bytes than requested.","evidence":"Loop at line 425 calls Read into `buf[:n]` without re-checking the trailing partial block; pgBackRest-style WAL streams allow short reads.","fix_sketch":"In `internal/restore/stream.go:StreamBlock`, wrap Read in a loop accumulating until len(buf) bytes are filled or io.EOF, then verify SHA256 before returning.","risk":"May surface latent corruption that was previously masked; ensure tests cover partial-read path.","verification":"Add a table-driven test that streams a 3-byte block via a fake reader returning (1, nil) then (1, nil) then (1, nil) — verify checksum fails when middle byte is mutated.","confidence":"high"}
```
