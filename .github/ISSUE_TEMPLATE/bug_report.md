---
name: Bug report
about: A reproducible failure in pg_hardstorage
labels: bug
---

## What happened

<!-- Symptom in one sentence: command, expected outcome, actual outcome. -->

## To reproduce

```
# Exact commands.
```

## pg_hardstorage version

Run `pg_hardstorage version` and paste the output.

## Doctor report

If the failure is operational (not a crash), paste the output of
`pg_hardstorage doctor [<deployment>] -o json`. The `health.issues`
array is the part that points the maintainer at the failure mode.

## Logs

The last ~50 NDJSON events from the agent or the failing CLI invocation.
Use `-o ndjson` to capture them in a script-friendly form. Redact any
DSN passwords / tokens before pasting.

## Environment

- OS: <!-- distro + version -->
- Architecture: <!-- amd64 / arm64 -->
- PG: <!-- 15.4 / 16.2 / 17.1 -->
- Repo backend: <!-- s3 / fs / azure / gcs -->
- Coordination: <!-- single-host / pg-advisory / k8s-lease -->

## Anything else

<!-- Bug report scenario file from the testkit, if you have one — that
makes the failure reproducible in CI:
   pg_hardstorage_testkit reproduce --bug-report attached.scenario.yaml -->
