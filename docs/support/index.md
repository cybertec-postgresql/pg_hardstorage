---
title: Support
description: Filing bugs, security disclosure, community.
---

# Support

## Filing a bug

Best bug reports land with a **runnable testkit scenario
file** that reproduces the failure.  The testkit
(`pg_hardstorage_testkit`) ships with the binary; bug
reproductions take the form of `*.scenario.yaml` files we
can run locally.

The
[`docs/reference/runbooks/`](../reference/runbooks/index.md)
runbooks index covers operator-side recovery for the
named disaster scenarios (R1-R7); they often resolve the
issue without needing a bug report.

When in doubt, open a
[GitHub issue](https://github.com/cybertec-postgresql/pg_hardstorage/issues/new/choose)
and we'll triage from there.

## Security disclosure

Vulnerabilities go through the disclosure process documented
in [`SECURITY.md`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/SECURITY.md).
Please do **not** open public issues for security findings.

## Community

`pg_hardstorage` is Apache 2.0.  Contributions, plugins,
and Tier-2 plugin authors welcome.  Start with
[`CONTRIBUTING.md`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/CONTRIBUTING.md).
