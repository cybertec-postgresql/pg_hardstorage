# Contributing to pg_hardstorage

Thanks for your interest. pg_hardstorage is owned by CYBERTEC PostgreSQL
International GmbH and licensed Apache-2.0. We welcome external
contributions, but the bar is "this would land in the project anyway"
— scope and design discipline matter.

## Before you start

1. **Read `docs/SPEC.md`.** It's the single source of truth for what
   the system is and isn't supposed to do. Most of what looks like a
   missing feature is on a later milestone (v0.5 or v1.0); arguing for
   an earlier landing is a great use of an issue.
2. **Open an issue first** for anything more than a one-line typo fix.
   "Should we even do this?" is a faster conversation in an issue than
   in a PR review.
3. **Check the maintainer profile.** Hans-Jürgen Schönig prefers tight,
   technical responses; describe your change like you'd brief a senior
   colleague.

## Development setup

```sh
git clone https://github.com/cybertec-postgresql/pg_hardstorage.git
cd pg_hardstorage
make build            # produces bin/pg_hardstorage
make build-testkit    # produces bin/pg_hardstorage_testkit
make check            # vet + race tests; the pre-PR sanity gate
```

Go 1.26+, CGO_ENABLED=0 by default. The integration suite needs a
running Docker daemon (testcontainers spins up real PostgreSQL):

```sh
make test-integration
```

## Code style

- **Comments explain WHY, not WHAT.** If a reader can see what the code
  does from the code, the comment tells them why it's that way. That's
  the project's prevailing comment style; PRs that drift from it get
  pushed back.
- **No AI attribution anywhere.** Not in commits, not in code comments,
  not in PR descriptions. Author commits as Hans-Jürgen Schönig
  <hs@cybertec.at>.
- **Vocabulary discipline.** Always *deployment*, never *stanza*.
  Always *backup*, never *snapshot* (which has a different meaning in
  our `internal/plugin/source/snapshot/`). Always *repository* / *repo*,
  never *vault* / *bucket* (those are backend-specific).
- **gofmt -s -w .** before commit. CI fails on unformatted code.
- **go vet ./...** must be clean.
- **No `panic` outside `init()` functions** in production code.
  Goroutine top-levels recover; errors get returned.

## Tests

- **Race detector on every test run.** No exceptions.
- **Property-based tests** are encouraged for parsers, the chunker,
  the retention engine, the manifest schema, and natural-time parsing.
- **Differential tests** against `pg_basebackup` / `pg_verifybackup`
  for any change touching the backup/restore byte stream.
- **Integration tests** for anything that talks to PostgreSQL or to a
  real storage backend.

A bug report is best filed with a runnable testkit scenario file:

```sh
pg_hardstorage_testkit scenario reproduce --bug-report bug-1234.scenario.yaml
```

That turns "weird intermittent thing" into "here's the green-then-red
commit" within hours.

## Commit messages

```
component: short imperative summary

Longer body explaining WHY this change. Reference the issue (#123) if
there is one. Wrap at ~72 columns.

The maintainer reads commits in `git log --oneline` first; the
short summary should be informative on its own.
```

No AI-attribution trailers. The author line carries the maintainer's
name and email.

## Sign-off

We use DCO (Developer Certificate of Origin) sign-off rather than a
CLA. Add `Signed-off-by: Your Name <you@example.com>` to commits with
`git commit -s`. By signing off, you certify the contribution is
yours and you're licensing it under Apache-2.0.

## Security

Don't open public issues for security findings. Use the GitHub
Security Advisories tab on the repository; an advisory is a private
draft until disclosure.

## License

Apache-2.0. The LICENSE file at the repo root is the authoritative
text. Every file in this repository is covered by it (we don't use
per-file headers).
