#!/usr/bin/env bash
# coverage-ratchet-diff.sh — the ratchet comparison, on its own.
#
# Split out of coverage-deadcorners.sh so the gate's own logic can be
# tested with plain fixture files instead of a GOCOVERDIR full of
# covdata. A gate whose comparison has never been executed is the same
# unwitnessed-code problem the gate exists to report.
#
# Both inputs are lists of "path/file.go:LINE: FuncName". Comparison
# ignores LINE and is a MULTISET diff on (file, func):
#
#   * ignoring LINE, because editing anything above a function shifts
#     it, so a commit that only ADDED a test used to present every
#     untouched function below as "newly unwitnessed" and fail the
#     gate. The line bought no precision the report does not already
#     print, and cost false reds.
#   * multiset, because one file legitimately holds several functions
#     with the same name — internal/cli/approval.go has five distinct
#     WriteText methods. Witnessing one must read as a gain (4 left),
#     and regressing back to five must read as a violation.
#
# Exit 1 on a violation, 0 otherwise.
#
# Usage: coverage-ratchet-diff.sh <baseline.txt> <current.txt>
set -euo pipefail

BASELINE="${1:?usage: coverage-ratchet-diff.sh <baseline> <current>}"
CURRENT="${2:?need the current dead-corner list}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# sed, not `grep -v`, for the blank-line filter: grep exits 1 when it
# selects nothing, and under `set -e -o pipefail` that aborts the script
# with no output — so on the day the dead-corner list finally reaches
# zero, the gate would fail silently instead of reporting the win.
strip_line() { sed 's/:[0-9][0-9]*:/:/; /^[[:space:]]*$/d'; }

strip_line < "$BASELINE" | sort > "$TMP/base.keys"
strip_line < "$CURRENT"  | sort > "$TMP/cur.keys"

NEW="$(comm -13 "$TMP/base.keys" "$TMP/cur.keys" || true)"
if [ -n "$NEW" ]; then
  echo "RATCHET VIOLATION — functions newly unwitnessed vs baseline:"
  echo "$NEW"
  echo
  echo "(matched on file + function; see the dead-corner report for line numbers)"
  exit 1
fi

FIXED="$(comm -23 "$TMP/base.keys" "$TMP/cur.keys" || true)"
if [ -n "$FIXED" ]; then
  echo "ratchet ok — and $(printf '%s\n' "$FIXED" | wc -l | tr -d ' ') baseline entries are now witnessed:"
  echo "$FIXED"
  echo "update test/coverage/deadcorners-baseline.txt to lock the gain in."
else
  echo "ratchet ok: no new dead corners vs baseline"
fi
