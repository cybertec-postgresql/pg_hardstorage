---
name: Feature request
about: A capability you'd like pg_hardstorage to gain
labels: enhancement
---

## What problem are you trying to solve?

<!-- The user-visible problem. NOT "I want feature X" — "I want to
accomplish Y, and currently I have to do Z which is awkward because…" -->

## What outcome would the ideal solution produce?

<!-- Be concrete: a CLI invocation, a config snippet, an output shape.
That's how we tell whether your problem maps onto an existing feature
we should explain better, vs a real gap in the surface. -->

## Have you looked at the SPEC?

`docs/SPEC.md` enumerates the full design surface; many "missing"
features are scheduled for v0.5 or v1.0. If the feature is already
there but on a later milestone, this issue is a great place to argue
for an earlier landing.

## Compatibility considerations

- Does this affect the on-disk manifest schema? (24-month back-compat
  commitment.)
- Does this affect the CLI / API contracts? (`schema: pg_hardstorage.v1`,
  same commitment.)
- Does this need a new dependency? (We try hard to stay pure-Go and
  CGO_ENABLED=0.)
