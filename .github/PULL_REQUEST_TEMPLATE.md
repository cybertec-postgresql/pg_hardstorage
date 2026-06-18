<!-- Please keep PRs focused. One concern per PR makes review tractable. -->

## Summary

<!-- 1-3 sentence description of what changes and why. -->

## Type

- [ ] Bug fix
- [ ] New feature
- [ ] Refactor (no behaviour change)
- [ ] Documentation
- [ ] Test infrastructure
- [ ] Packaging / release

## Tests

- [ ] `make check` passes locally (vet + race tests)
- [ ] New tests added for the changed behaviour, or there's a clear reason none exist
- [ ] Integration tests pass (`make test-integration`) where touched

## Compatibility

- [ ] No on-disk manifest schema changes (or: schema bumped + 24-month back-read preserved)
- [ ] No CLI / API contract changes (or: documented + bumped)
- [ ] No new external dependencies (or: justified in the description)

## Checklist

- [ ] Maintainer-attribution authoring (`Author: Hans-Jürgen Schönig <hs@cybertec.at>`)
- [ ] No AI-attribution lines anywhere
- [ ] CHANGELOG.md updated under the unreleased section
- [ ] Comments explain WHY (not WHAT) where the code isn't self-evident
