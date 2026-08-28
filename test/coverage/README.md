# Coverage dead-corner baseline

`deadcorners-baseline.txt` is the committed list of shipped functions
with zero executions across both the package test suite — INCLUDING
integration-tagged tests, which the release gate runs every soak and
which are therefore legitimate witnesses — and the
coverage-instrumented end-to-end scenario run (`make coverage-e2e`).
The first baseline was cut without the integration tag and
over-reported (jobs_pg and postverify, both integration-covered,
appeared dead); a function on this list now has genuinely nothing
anywhere that executes it.

It is a **ratchet**: `make coverage-ratchet` fails if any function is
unwitnessed now that was not on the baseline. Shrinking the list is
always welcome; growing it needs the baseline updated in the same
commit as an explanation of why the new code cannot be exercised.

Regenerate with:

    make coverage-e2e   # writes the report to stdout
    # paste the function list into deadcorners-baseline.txt

Why: `logs --since` shipped broken for a year and the dead
`notfound.unit` detection for longer. Both were functions no test ever
executed, and nothing reported that fact. A green suite over
unexecuted code is indistinguishable from a green suite over correct
code — this list is the difference made visible.
