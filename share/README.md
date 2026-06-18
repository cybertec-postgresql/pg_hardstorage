# share

Data files installed into `/usr/share/pg-hardstorage/` by every package: sample
config and the bundled LLM skill catalogue. These are read-only at runtime and
shipped verbatim.

## What lives here

Operator-visible defaults that need to be both in the source tree (so the build
can pick them up) and in the filesystem at install time (so the runtime can find
them via `../internal/paths/`). Anything copied into `/usr/share/...` by the
packaging recipes is staged here first.

## Key files / subdirs

- `pg_hardstorage.sample.yaml` — annotated example config, copied to
  `/etc/pg-hardstorage/config.yaml.sample` by packaging
- `skills/` — bundled LLM skills loaded by `../internal/llm/skills/` at
  startup
  - `ask.skill.yaml` — generic Q&A skill
  - `explain.skill.yaml` — explain-this-output skill

## Read next

- `../internal/config/` — the config loader that consumes
  `pg_hardstorage.sample.yaml` as a template
- `../internal/llm/skills/` — runtime that loads everything in `skills/`
- `../docs/reference/` — schema references for the sample config

## Don't put X here

- Generated artefacts — `../man/`, `../completions/` are the right homes.
- Binary blobs — keep this tree text-only so diffs stay reviewable.
- Per-user state — runtime data lives under the paths declared in
  `../internal/paths/`.
