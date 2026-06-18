# pg_hardstorage Documentation Plan (v1.0)

> **Status:** approved 2026-05-04. MkDocs Material, parallel
> writing, no public domain yet (configure for one-line
> swap-in later).

This plan lives in-tree so anyone touching docs can see the
shape we're building toward.  The plan is intentionally
opinionated: the system is large enough that an unstructured
"write whatever's missing" approach burns months and
produces drift.  Every decision below has a "why" attached.

---

## Goals (priority order)

1. **3am operator can succeed without reading docs first** —
   but when they do, every command, error, and runbook is
   one click away.
2. **Tier-2 plugin authors can write a working plugin from
   documentation alone** — no source-reading required.
3. **Compliance auditor finds every claim verifiable** —
   schemas, controls mapping, attestation chain.
4. **Every published example actually runs in CI.** No
   bit-rotted code blocks.
5. **24-month back-compat** on docs version-pinning per
   release.

## Non-goals (deferred)

- Customer case studies / marketing pages
- i18n (DE/FR/JA) — defer to v1.0+ doc release; plan tooling
  so it drops in
- Video tutorials
- Blog
- Cookbook of advanced patterns — defer to v1.5

## Audiences (drives IA)

| Audience | First question | First page they hit |
| --- | --- | --- |
| 3am operator | "How do I restore right now?" | `tutorials/restore-now`, `runbooks/` |
| Evaluator (DBA) | "Why this not pgBackRest?" | `explanation/comparison`, `tutorials/` |
| Steady-state operator | "How do I add a deployment / KMS / repo?" | `how-to/`, `reference/config` |
| K8s operator team | "How does this fit my CNPG cluster?" | `how-to/kubernetes/` |
| Tier-2 plugin author | "Storage plugin reference contract" | `reference/plugins/`, `tutorials/build-a-plugin` |
| Compliance auditor | "Where's the audit chain?" | `explanation/security`, `reference/audit-events` |
| CI / build engineer | "How do I package this?" | `how-to/packaging/`, `reference/build-flavours` |

## Information architecture (Diátaxis)

Four-quadrant Diátaxis split:

- **Tutorials** — learn by doing, fixed paths
- **How-to** — task-oriented recipes
- **Reference** — exhaustive, machine-comparable
- **Explanation** — the "why"

Plus operations (handbook), compliance (control mappings),
and support pages.  Full page tree:

```
docs/
├── index.md
├── tutorials/                            # ~8 pages
├── how-to/                               # ~40 pages
│   ├── adding/
│   ├── operating/
│   ├── kubernetes/
│   ├── air-gapped/
│   ├── packaging/
│   ├── migration/
│   └── verify/
├── reference/                            # ~50 pages, half auto-generated
│   ├── cli/                              # AUTO from Cobra
│   ├── api/                              # AUTO from openapi.yaml
│   ├── grpc/                             # AUTO from .proto
│   ├── config/                           # AUTO from internal/config schema
│   ├── plugins/
│   ├── manifest-schema.md                # AUTO
│   ├── audit-event-schema.md             # AUTO
│   ├── output-event-schema.md            # AUTO
│   ├── metric-catalog.md                 # AUTO from /metrics scrape
│   ├── error-codes.md                    # AUTO from grep
│   ├── exit-codes.md                     # AUTO
│   ├── kekref-schemes.md                 # AUTO
│   ├── storage-url-schemes.md            # AUTO
│   ├── skill-schema.md                   # AUTO
│   ├── runbooks/                         # MOVED from current docs/runbooks/
│   ├── crd/                              # AUTO from api/crd/
│   ├── build-flavours.md
│   ├── filesystem-layout.md
│   └── compatibility-matrix.md           # AUTO from test/matrix.yaml
├── explanation/                          # ~14 pages
├── operations/                           # day-2 handbook
├── compliance/                           # SOC2/ISO/HIPAA/PCI/FedRAMP/GDPR
├── faq.md
├── glossary.md
├── support/
├── changelog.md                          # symlinked to repo CHANGELOG.md
└── release-notes/
```

~150 pages total.  Roughly **half are auto-generated** from
single sources of truth; the other half are hand-written
content distributed across parallel writing tracks.

## Auto-generation map

| Reference page | Source of truth | Tooling |
| --- | --- | --- |
| `reference/cli/*.md` | Cobra command tree | `cobra/doc.GenMarkdownTree` |
| `reference/api/openapi.html` | `api/openapi.yaml` | `redoc-cli build` |
| `reference/api/_index.md` | `api/openapi.yaml` | small Go tool |
| `reference/grpc/*.md` | `proto/**/*.proto` | `protoc-gen-doc` |
| `reference/config/*` | `internal/config.Schema()` reflection | new `cmd/docsgen` Go tool |
| `reference/manifest-schema.md` | `internal/backup/manifest.go` struct tags | reflection |
| `reference/audit-event-schema.md` | `internal/audit/events` registry | reflection |
| `reference/output-event-schema.md` | `internal/output/event.go` Op registry | reflection |
| `reference/metric-catalog.md` | live `/metrics` scrape during `make docs-regen` | scrape + format |
| `reference/error-codes.md` | grep `output.NewError("…")` over the tree | one-shot script |
| `reference/exit-codes.md` | `internal/output/Exit*` constants | reflection |
| `reference/kekref-schemes.md` | `kms.DefaultRegistry.Schemes()` | runtime emitter |
| `reference/storage-url-schemes.md` | `storage.Schemes()` | runtime emitter |
| `reference/skill-schema.md` | `internal/llm/skills.SchemaV1` | reflection |
| `reference/crd/*.md` | `api/crd/*.yaml` | `crd-ref-docs` |
| `reference/compatibility-matrix.md` | `test/matrix.yaml` | yq + jq → markdown |
| `man/man1/pg_hardstorage*.1` | Cobra | `cobra/doc.GenManTree` |
| `changelog.md` | repo `CHANGELOG.md` | symlink |

`make docs-regen` rebuilds everything; CI fails on
non-empty `git diff` after regen.

## Tooling stack

| Layer | Pick | Why |
| --- | --- | --- |
| Static-site generator | **MkDocs Material** | best-in-class search, native Mermaid + admonitions, mkdocs-versioning + mkdocs-static-i18n drop-in, Python tooling already in CI |
| OpenAPI render | **Redoc** static HTML | single-file, no server, embeds cleanly |
| gRPC render | **protoc-gen-doc** Markdown | mature |
| API mock for examples | **prism** (stoplight) | validates docs' example bodies against spec; CI gate |
| Linkcheck | **lychee** | fast, parallel |
| Code-block testing | **markdown-test-runner** custom | `# RUNNABLE` blocks executed in containers |
| i18n (deferred) | **mkdocs-static-i18n** | drops into the same site |
| Hosting | **GitHub Pages** + Cloudflare later | versioned via `gh-pages` branch |

## Domain & hosting policy

**v1.0 ships without a public domain.**  The site builds
to a portable `site/` directory and is publishable to
GitHub Pages (or any static host) with one config flip.

`mkdocs.yml` is configured with:

  - `site_url: ""` left blank by default
  - `repo_url` populated (the GitHub repo)
  - All internal links **relative** (no absolute URLs)
  - All extra-asset paths relative

Adding a domain later is **one line in `mkdocs.yml`**
(`site_url: "https://docs.…"`) plus a DNS record + a
GitHub Pages CNAME.  No content rewrite needed.

## Quality gates (CI-enforced)

1. **`make docs-regen` is clean** on every PR — auto-gen
   can't drift.
2. **`mkdocs build --strict`** — broken cross-link or
   missing nav entry fails the build.
3. **`lychee docs/`** — every external link reachable.
4. **`markdown-test-runner docs/`** — every `# RUNNABLE`
   code block executes against a containerised
   `pg_hardstorage` + a fake repo.
5. **OpenAPI gate** — every published path has at least
   one example request + response.  Vacuum + Spectral.
6. **`buf lint` + `buf breaking`** — proto changes can't
   break the v1.0 contract.
7. **Schema parity** — config keys in
   `reference/config/` exist in `internal/config.Schema()`,
   and vice versa.
8. **`pg_hardstorage <cmd> --help` round-trip** — text in
   `reference/cli/pg_hardstorage_<cmd>.md` is byte-equal
   to live `--help` output.
9. **Spell-check** — `cspell` with project allowlist.

## Phasing

Parallel-writer plan; effort numbers assume two writers.

**Phase 1 — Foundations (1 week)** — DONE-ish in this commit.
- mkdocs scaffold, IA migration, CONTRIBUTING-DOCS, Makefile
  targets, basic CI, CLI auto-gen wired.

**Phase 2 — Tutorials (3-4 days, parallel)**
- 8 tutorials, each with `# RUNNABLE` code blocks.

**Phase 3 — How-to library (1 week, parallel)**
- ~40 task-oriented pages.  The "adding/" branch
  parallelises per plugin.

**Phase 4 — Explanation deep-dives (4 days, parallel)**
- ~14 conceptual pages.  Adapt heavily from `SPEC.md`.

**Phase 5 — Compliance & operations (4 days)**
- Control-mapping pages + operations handbook.

**Phase 6 — Plugin author guide (3 days)**
- Tier-1 contracts + Tier-2 walkthrough + reference
  example plugin in `examples/`.

**Phase 7 — Polish (3 days)**
- FAQ, glossary, search-tuning, screenshots, v1.0 release
  notes.

**Total:** 4-5 weeks with two parallel writers.

## Out-of-scope follow-ups (engineering tasks, not docs)

These are real gaps the doc audit surfaced; they are
**engineering work** not doc work and will be tracked
separately:

1. **OpenAPI completeness gap** — `api/openapi.yaml` covers
   15 paths; the SPEC promises ~25.  Missing paths:
   `/v1/deployments/{d}/backups/{id}/verify`, `/v1/wal/*`,
   `/v1/repos/{r}/gc`, `/v1/repos/{r}/usage`, `/v1/kms/*`,
   `/v1/audit`, `/v1/search`, `/v1/doctor`.  Either
   implement the missing routes or tighten the SPEC to
   match what's shipped.
2. **gRPC service definitions** — `proto/pg_hardstorage/v1/`
   has 3 files; some services in the SPEC aren't in the
   protos yet.
3. **CRD schema files** — `api/crd/` referenced in the
   plugin model section but the directory is sparse.

These don't block the doc project; we document what's there
and link to issues for what's coming.

## Authoring conventions

See `docs/CONTRIBUTING-DOCS.md`.

## Open decisions deferred to writing time

1. **Versioning policy** — every minor (v1.0, v1.1) gets
   its own preserved doc set, or latest-wins with archived
   snapshots only at majors?  `mike` plugin supports
   either.  Default until decided: **latest-wins**.
2. **Feedback widget** — "this page wasn't helpful" / GitHub
   issue link?  Default until decided: **no widget**, link
   to "How to file a bug" in footer.
3. **Screenshot policy** — none for v1.0 (every screenshot
   bit-rots) is the default.  TUI / status output rendered
   as text blocks instead.
