#!/usr/bin/env bash
# coverage-deadcorners.sh — the zero-execution report.
#
# Why this exists
# ---------------
# `logs --since 24h` was broken for a year and the dead `notfound.unit`
# detection for longer, and in both cases the root cause of the SILENCE
# (as opposed to the bug) was the same: no test — unit, integration or
# scenario — ever executed those functions, and nothing reported that
# fact. A green suite over unexecuted code is indistinguishable from a
# green suite over correct code.
#
# This script produces the list that removes the ambiguity: shipped
# functions with ZERO executions across BOTH
#
#   (a) the package test suite (`go test -cover` textfmt profile), and
#   (b) an end-to-end run of the coverage-instrumented CLI binary
#       (GOCOVERDIR covdata from `make coverage-e2e`).
#
# A function on the list is not necessarily buggy — it is UNWITNESSED,
# which is the state every one of the bugs above lived in. The list is
# a work queue, and the ratchet is: it may only shrink (compare against
# the committed baseline with --diff).
#
# Usage:
#   scripts/coverage-deadcorners.sh <unit.cov> <e2e-covdata-dir> [--diff baseline.txt]
set -euo pipefail

UNIT_PROFILE="${1:?usage: coverage-deadcorners.sh <unit.cov> <e2e-covdata-dir> [--diff baseline]}"
E2E_DIR="${2:?need the GOCOVERDIR from the instrumented e2e run}"
BASELINE=""
if [ "${3:-}" = "--diff" ]; then BASELINE="${4:?--diff needs a baseline file}"; fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# covdata -> textfmt so both sides speak the same language.
go tool covdata textfmt -i="$E2E_DIR" -o "$TMP/e2e.cov"

zeros() { # profile -> sorted "file:line: Func" lines at 0.0%
  go tool cover -func="$1" 2>/dev/null \
    | awk '$3 == "0.0%" && $2 != "(statements)" {print $1" "$2}' \
    | sort -u
}
zeros "$UNIT_PROFILE" > "$TMP/unit.zero"
zeros "$TMP/e2e.cov"  > "$TMP/e2e.zero"

# Dead corners = zero in BOTH. Functions the e2e profile has never
# heard of (packages the binary does not link) fall back to the unit
# verdict alone — absence from a profile is not execution.
comm -12 "$TMP/unit.zero" "$TMP/e2e.zero" > "$TMP/dead.txt"

# Test-only and generated helpers are noise, not dead product code.
grep -vE '_test\.go|/testkit/|/cmd/pg_hardstorage_testkit/' "$TMP/dead.txt" > "$TMP/dead-shipped.txt" || true

echo "== dead corners (zero executions in unit AND e2e): $(wc -l < "$TMP/dead-shipped.txt") functions =="
cat "$TMP/dead-shipped.txt"

# The ratchet comparison lives in its own script so it can be tested
# against plain fixture files; see scripts/coverage-ratchet-diff.sh for
# why it matches on file+function rather than file:line.
if [ -n "$BASELINE" ]; then
  echo
  "$(dirname "$0")/coverage-ratchet-diff.sh" "$BASELINE" "$TMP/dead-shipped.txt"
fi
