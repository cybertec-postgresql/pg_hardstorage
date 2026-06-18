<!-- AUTO-GEN candidate: reflect over internal/llm/skills.Skill struct tags; per docs/DOC_PLAN.md auto-generation map. -->
---
title: Skill schema
description: YAML schema for LLM skill files — top-level keys, RBAC, guardrails, tool allowlist, and locales.
tags:
  - reference
  - llm
  - skills
---

# Skill schema

A **skill** is a single YAML file describing one
operator-facing LLM capability ("ask a question", "run the
restore wizard", "draft an incident postmortem").  Skills
are loaded by `internal/llm/skills`; the schema string is
`pg_hardstorage.skill.v1`.  24-month back-compat applies.

## Loader precedence

```
~/.config/pg_hardstorage/skills/    (user-private; highest)
/etc/pg_hardstorage/skills/         (operator overrides)
/usr/share/pg_hardstorage/skills/   (shipped, read-only)
internal/llm/skills/builtin/*.skill.yaml  (embedded floor)
```

Later directories override earlier ones by `name`.  The
production agent layers builtins as the floor and the
three on-disk paths as override layers; missing directories
are skipped silently.

`Parse` decodes with `KnownFields=true` — typos become
loud errors, not silent dropped fields.

## Top-level keys

| Key | Type | Required | Notes |
| --- | --- | --- | --- |
| `schema` | string | yes | must equal `pg_hardstorage.skill.v1` |
| `name` | string | yes | Stable machine name; the override-precedence key |
| `display_name` | string | no | Operator-visible label |
| `version` | string | yes | Skill version (semver-shaped, but not parsed) |
| `description` | string | no | One-line summary; surfaced by `llm skill list` |
| `prompt_template` | string | yes | The body the model receives |
| `trigger` | mapping | no | When the skill fires |
| `permissions` | mapping | no | RBAC and read/write posture |
| `context` | mapping | no | Preloaded tools and available-tool allowlist |
| `guardrails` | sequence | no | Token / cost / safety belts |
| `locales` | mapping | no | Per-language overrides |

## `trigger`

Two distinct firing paths.  Either may be empty.

```yaml
trigger:
  manual:
    - "review-backup"
    - "rb"
  auto_on_error:
    - "verify.checksum_mismatch"
    - "wal.slot_missing"
```

| Key | Type | Notes |
| --- | --- | --- |
| `manual` | sequence of strings | Aliases that fire the skill from `pg_hardstorage llm <alias>` |
| `auto_on_error` | sequence of strings | [Error-code](error-codes.md) prefixes; the dispatcher offers the skill when a matching error surfaces |

## `permissions`

```yaml
permissions:
  read_only: true
  required_rbac:
    - role:operator
    - role:incident-responder
```

| Key | Type | Notes |
| --- | --- | --- |
| `read_only` | bool | v0.1 only supports read-only skills.  `Lint()` flags `read_only: false` until `advise+execute` lands |
| `required_rbac` | sequence of strings | RBAC roles that gate the invocation |

## `context`

Declares what the skill needs preloaded and which tools
it can call.

```yaml
context:
  preload_tools:
    - read_doctor
    - read_runbook: { id: R5 }
  available_tools:
    - search_audit
    - read_manifest
    - execute_command         # v1.0+; refused in v0.1
  allowed_executes:
    - "pg_hardstorage doctor"
    - "pg_hardstorage list"
    - "pg_hardstorage status"
    - "pg_hardstorage show"
```

| Key | Type | Notes |
| --- | --- | --- |
| `preload_tools` | sequence | Each entry is either a bare tool name (default args) or a single-key map `<name>: { <arg>: <val>, … }` |
| `available_tools` | sequence of strings | The `tools:` allowlist — model sees only these |
| `allowed_executes` | sequence of strings | **Required** when `available_tools` includes `execute_command`. Each entry is a command-prefix; `execute_command` refuses any invocation that does not start with one of them. Empty list refuses every execution |

`Lint()` warns on:

- `available_tools` containing `execute_command` in v0.1
  (the tool isn't shipped yet);
- missing `allowed_executes` paired with `execute_command`.

## `guardrails`

Single-key-map sequence; the key names a guardrail kind, the
value is its body (free-form per kind).

```yaml
guardrails:
  - max_token_budget:        { tokens: 8000 }
  - max_seconds:             30
  - require_human_for:
      - "DROP"
      - "DELETE"
      - "TRUNCATE"
```

| Common kind | Body shape | Effect |
| --- | --- | --- |
| `max_token_budget` | `{ tokens: <int> }` | Cap on prompt + completion tokens; `Lint()` warns when absent |
| `max_seconds` | `<int>` | Wall-clock cap |
| `require_human_for` | sequence of substrings | Refuses a tool call whose argv contains any string |

## `locales`

Per-locale overrides for `display_name`, `description`,
and `prompt_template`.  Empty fields fall back to the
default English values.

```yaml
locales:
  de:
    display_name: "Backup-Status prüfen"
    description: "Aktuellen Backup-Stand zusammenfassen."
  de-AT:
    display_name: "Backup-Status checken"
  ja:
    display_name: "バックアップ状態を確認"
```

Lookup is exact-match first, then language-prefix:

- request `de-AT` → tries `de-AT` → falls back to `de` →
  falls back to default;
- request `ja-JP` → tries `ja-JP` → falls back to `ja` →
  falls back to default;
- request `klingon` → falls back to default.

Locale codes are lowercased before lookup; whitespace is
trimmed.

## `Source` (read-only)

`Skill.Source` is set by the loader to the file path the
skill came from (or `"builtin:<embedded-path>"` for the
embedded set).  Surfaced by `llm skill show`; not present
on YAML output.

## Minimal example

```yaml
schema: pg_hardstorage.skill.v1
name: status-summary
version: 1.0.0
description: One-paragraph summary of the deployment's backup posture.
permissions:
  read_only: true
context:
  preload_tools:
    - read_status
  available_tools:
    - search_audit
guardrails:
  - max_token_budget: { tokens: 4000 }
prompt_template: |
  Summarise the deployment's backup posture in one paragraph.
  Use the preloaded status block.  Do not invent missing
  fields.
```

## Linter

`Skill.Lint()` returns operator-readable issues, not
errors.  Today it warns on:

- empty `description`;
- `permissions.read_only: false` (v0.1 limitation);
- `available_tools` containing `execute_command`;
- absence of any `max_token_budget` guardrail.

`pg_hardstorage llm skill lint` surfaces the same list.

## See also

- [Plugins → LLM provider contract](plugins/llm-provider-contract.md)
  — the `Provider` interface skills run against.
- [Explanation → LLM safety stack](../explanation/llm-safety-stack.md) —
  why guardrails and `allowed_executes` exist.
