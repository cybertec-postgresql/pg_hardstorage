# fips/

Reports the binary's FIPS-mode posture so the runtime knows which crypto stack
it is on.

## What lives here

pg_hardstorage ships in two flavours:

- **default** — pure-Go `crypto/...` from the standard library. Runs anywhere.
  No FIPS claim.
- **fips** — built with `make build-fips` (`GOEXPERIMENT=boringcrypto`,
  `CGO_ENABLED=1`, build tag `fips`). All crypto routes through Google's
  BoringSSL — a FIPS 140-2 validated module. linux/amd64 only.

`Enabled()` is the single public symbol consumers care about. Consumers:
`pg_hardstorage doctor` ("FIPS: yes/no"), `--fips-strict` start-gate, the audit
log's `fips:true` event attribute.

## Key files

- `fips.go` — public `Enabled()` symbol
- `enabled_default.go` — `//go:build !fips` — returns false
- `enabled_fips.go` — `//go:build fips` — returns true
- `fips_test.go` — sanity tests under both build tags

## Read next

- `../../docs/reference/build-flavours.md` — operator-facing description of
  the two flavours
- `../plugin/kms/README.md` if present — FIPS posture per KMS provider
- `../README.md` — parent index

## Don't put X here

- Crypto algorithm selection — that's the call site's job; this package only
  *reports* posture.
