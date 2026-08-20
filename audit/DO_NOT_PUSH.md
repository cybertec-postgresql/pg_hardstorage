# DO NOT PUSH — audit branch

**Branch**: `h200_fixes`
**Owner of this guard**: audit session 2026-08-20
**Status**: branch exists only locally. `git ls-remote origin refs/heads/h200_fixes` returns empty.

## Why this file exists

The user explicitly ordered:

> stop all github actions for this forever

GitHub Actions only fire on push, PR, tag, schedule, or manual `workflow_dispatch`.
The only trigger reachable from this harness is **push** (and downstream: opening a PR).

This branch is the workspace for a comprehensive audit whose findings are still being
collected. Nothing on this branch is ready for review, and pushing it would:

1. Trigger `ci.yml` on the `h200_fixes` branch (push to a non-`main` branch does **not**
   match `ci.yml`'s `branches: [main]` filter, so this one is naturally inert — but
   if anyone broadens that filter, the workflow fires).
2. Trigger `docs-doctest.yml` and `docs.yml` — same caveat as above, both filtered to `main`.
3. NOT trigger `chaos-soak.yml` / `packaging-weekly.yml` / `release.yml` regardless of branch.

## Hard rules for any session operating on this branch

- **NEVER run `git push` while `h200_fixes` is checked out.** This includes
  `git push --set-upstream origin h200_fixes`, `git push origin h200_fixes`,
  or any force-push / mirror-push variant.
- **NEVER open a PR from `h200_fixes` to `main`.** The user has said the next model
  may open a PR — but only AFTER findings have been reviewed and the user gives
  explicit approval in that future session. Until then, no PR.
- **NEVER tag this branch.** `release.yml` triggers on `v*` tag pushes.
- **NEVER run `gh workflow run`.** Manual triggers fire from the `main` ref by default,
  but the user has said stop all actions — don't pull the trigger either.

## When the user does approve a PR

The PR description MUST include a `skip-ci` label OR the workflow files for that PR
must temporarily add a `paths-ignore: ['audit/**']` rule. Otherwise `ci.yml` will fire
on the PR open and consume CI minutes.

## To fully disable Actions on this branch from the GitHub side

Two repo settings the user can apply from the GitHub web UI (Settings → Actions →
General):

1. **Allow select actions** with empty allowlist, OR
2. **Disable actions** for the whole repository, OR
3. **Rulesets** (branches → `h200_fixes`) → "Require status checks to pass" with
   a check that always fails would force-push audits back, but that's a sledgehammer.

None of these are reachable from this harness. The user must apply them out-of-band.
