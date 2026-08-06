---
title: Environment variables
description: Every PG_HARDSTORAGE_* variable the binary reads, what it
              does, and when you would set it.
tags:
  - reference
  - configuration
---

# Environment variables

Every `PG_HARDSTORAGE_*` variable production code reads is listed
here. A variable that reaches production code and is missing from
this page fails
`TestOperatorEnvVarsAreDocumented` — an operator cannot set what
nobody wrote down, and for the SSH backends these variables are the
*only* configuration channel that exists.

Variables used solely by the test harness are deliberately absent;
they are not an operator surface.

## Configuration and paths

| Variable | Meaning |
| --- | --- |
| `PG_HARDSTORAGE_CONFIG` | YAML configuration supplied inline, as a literal document rather than a path. Merged like a file; useful in containers where mounting one is awkward. |
| `PG_HARDSTORAGE_CONFIG_FILE` | Path to `pg_hardstorage.yaml`, overriding the resolved default for the current mode. |
| `PG_HARDSTORAGE_ROOT` | Root directory the path resolver hangs config, state, cache, logs and runtime under. Mainly for a self-contained or relocated install. |
| `PG_HARDSTORAGE_URL` | Default repository URL when a command takes no `--repo`. |
| `PG_HARDSTORAGE_BIN` | Path to the `pg_hardstorage` binary to re-invoke for sub-steps. Defaults to the running executable; set it when that path is not re-executable (a wrapper script, a read-only layer). |
| `PG_HARDSTORAGE_AIRGAPPED` | Refuses every outbound network call not aimed at the repository or the database. |

## SSH backends (`scp://`, `sftp://`)

Nothing populates `StorageConfig.Extras` in production, so **these
variables are the only way to configure the SSH backends**. Each has
an `SCP` and an `SFTP` form; both plugins resolve the same settings in
the same order.

| Variable | Meaning |
| --- | --- |
| `PG_HARDSTORAGE_SCP_KNOWN_HOSTS` / `..._SFTP_KNOWN_HOSTS` | Path to a `known_hosts` file. **Required** — the plugins refuse to run without one rather than fall back to trusting any host key. |
| `PG_HARDSTORAGE_SCP_IDENTITY_FILE` / `..._SFTP_IDENTITY_FILE` | Private key for public-key authentication. |
| `PG_HARDSTORAGE_SCP_IDENTITY_PASSPHRASE` / `..._SFTP_IDENTITY_PASSPHRASE` | Passphrase, if the key is encrypted. |
| `PG_HARDSTORAGE_SCP_PASSWORD` / `..._SFTP_PASSWORD` | Password authentication. Discouraged — prefer a key. |

## LLM assistant

| Variable | Meaning |
| --- | --- |
| `PG_HARDSTORAGE_LLM_PROVIDER` | Provider to use (`openai`, `anthropic`, `ollama`, `mock`). |
| `PG_HARDSTORAGE_LLM_MODEL` | Model name for that provider. |
| `PG_HARDSTORAGE_LLM_API_KEY` / `PG_HARDSTORAGE_LLM_KEY` | Credential for the provider. |
| `PG_HARDSTORAGE_LLM_TEMPERATURE` | Sampling temperature, as a float. Overrides the provider default. |
| `PG_HARDSTORAGE_LLM_DEBUG_PROMPT` | Any non-empty value prints the assembled system prompt to stderr. For debugging what the assistant was actually told. |
| `PG_HARDSTORAGE_ON_ERROR_LLM` | Offers an assistant explanation when a command fails. |
| `PG_HARDSTORAGE_RUNBOOK_DIR` | Directory the assistant searches for runbooks, ahead of `/usr/share/pg_hardstorage/runbooks` and `docs/runbooks`. |
| `PG_HARDSTORAGE_SKILL_DIR` | Directory of assistant skill definitions. |

## See also

- [Repository URL parameters](../how-to/adding/repository-s3.md) —
  the other externally-settable surface, and subject to the same
  documentation guarantee.
- The YAML configuration schema, whose documented examples are parsed
  against the real loader by `configcheck`.
