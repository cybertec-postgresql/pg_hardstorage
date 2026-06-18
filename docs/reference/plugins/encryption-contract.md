---
title: Encryption (KMS) plugin contract
description: The kms.Provider interface — KEK custody, DEK wrapping, crypto-shred.
tags:
  - plugins
  - encryption
  - kms
  - reference
---

# Encryption (KMS) plugin contract

A KMS provider holds the Key-Encryption Key (KEK) — the
master key that wraps every backup's per-backup
Data-Encryption Key (DEK).  Each manifest stamps a
`KEKRef` string; resolving that string to a working
provider is what lets restore decrypt the chunks.

This is the **KEK-side** plugin tier.  The chunk-side
encryption codecs (AES-256-GCM today, AES-256-GCM-SIV in
v0.5+) live in `internal/plugin/encryption.Encryptor` —
a separate, lower-level contract; see
[Compression contract](compression-contract.md) for the
on-disk envelope they share.

!!! note "Reference implementations"
    - `internal/plugin/kms/awskms/awskms.go` —
      AWS KMS, FIPS-grade, supports
      `ScheduleKeyDeletion`-based crypto-shred.
    - `internal/plugin/kms/vaulttransit/vaulttransit.go` —
      HashiCorp Vault Transit; on-prem-friendly, FIPS via
      Vault Enterprise.
    The local-keystore provider at
    `internal/keystore/` is the dev / single-host default
    (`local:default`) and a useful minimal example of the
    interface shape.

## Interface

```go
// internal/kms/kms.go

package kms

type Provider interface {
    Name() string                                                 // "local", "aws-kms", "vault-transit"
    KEKRef() string                                               // round-trips through manifest
    WrapDEK(ctx context.Context, dek []byte) ([]byte, error)
    UnwrapDEK(ctx context.Context, wrapped []byte) ([]byte, error)
    Shred(ctx context.Context) error
    FIPSMode() bool
    Close() error
}
```

Implementations are **stateful** (they hold connection
state, refresh tokens, SDK clients) but every method is
**goroutine-safe**.  A single `Provider` is reused across
the lifetime of one repo session.

## KEKRef format

Every manifest carries the `kek_ref` string that
identifies which provider unlocks it.  Format conventions:

| Scheme | Example | Provider |
| --- | --- | --- |
| `local` | `local:default` | On-disk keystore |
| `aws-kms` | `aws-kms://arn:aws:kms:us-east-1:123456789012:key/abcd1234-...` | AWS KMS by ARN |
| `aws-kms` | `aws-kms://alias/pg-hardstorage-prod` | AWS KMS by alias |
| `aws-kms` | `aws-kms://12345678-1234-1234-1234-123456789012` | AWS KMS by key-id |
| `gcp-kms` | `gcp-kms://projects/p/locations/global/keyRings/r/cryptoKeys/k` | GCP KMS (planned) |
| `azure-kv` | `azure-kv://<vault>/<key>` | Azure Key Vault (planned) |
| `vault-transit` | `vault-transit://<vault-addr>/<key-name>` | HashiCorp Vault Transit |
| `pkcs11` | `pkcs11://<token>/<label>` | PKCS#11 HSM (planned) |

The scheme is everything before the first `:` or `://`.
`kms.SchemeOf(kekRef)` does the parsing; use it instead of
manual string-splitting.

`KEKRef()` round-trips: `provider.KEKRef() ==
manifest.Encryption.KEKRef`.  A provider that mints a
fresh KEK at `Open` MUST stamp the same string into both
places.

## Per-method contract

### `Name() string`

Lowercase scheme name (`"local"`, `"aws-kms"`,
`"vault-transit"`, …).  Stable across versions; goes into
audit-log `subject.kek_provider` fields and into
`pg_hardstorage doctor` output.

### `KEKRef() string`

The manifest-stamped reference this provider resolves.
Round-trips with `manifest.Encryption.KEKRef`.

### `WrapDEK(ctx, dek []byte) ([]byte, error)`

Encrypts the per-backup DEK with the cloud-side KEK and
returns the wrapped form.  The wrapped bytes go into
`manifest.Encryption.WrappedDEK`.

`dek` is 32 bytes (AES-256-key length, see
`encryption.KeyLen`).  The wrapped form is provider-
specific opaque bytes; pg_hardstorage never inspects
them.

The KEK material itself MUST NOT leave the cloud HSM /
on-prem HSM; only `Encrypt` / `Decrypt` ciphertext blobs
cross the wire.  This is the AWS KMS / GCP KMS posture
and the strongest production-grade KEK custody available
without bringing PKCS#11 into the binary.

### `UnwrapDEK(ctx, wrapped []byte) ([]byte, error)`

Decrypts a previously-wrapped DEK using the cloud-side
KEK.  Returns the 32-byte plaintext DEK.

Authentication failure (wrong KEK, deleted KEK, network
auth refused) surfaces as **`ErrUnwrap`**:

```go
return nil, fmt.Errorf("aws-kms: %w: %s", kms.ErrUnwrap, awsErr)
```

Callers `errors.Is(err, kms.ErrUnwrap)` to distinguish
"wrong key" from "network error" from "key scheduled for
deletion".

### `Shred(ctx) error`

**The most consequential operation in the binary.**
Schedules destruction of the cloud-side KEK.  Cloud KMS
providers typically schedule deletion with a cool-off
window (AWS KMS: 7-30 days; configurable via the
provider's `pending_window_days` config key).  After the
window elapses the key is destroyed; every backup whose
`wrapped_dek` depends on this KEK becomes
**permanently unrecoverable** — by design.

This is the GDPR Art. 17 / right-to-erasure primitive.
The CLI gates `kms shred` behind n-of-m approval +
typed-keyring confirmation + `--yes`; the audit chain
records the schedule + deletion-date for compliance.

`ErrShredFailed` wraps non-network failures.  Cloud KMS
often refuses Shred with structured errors (key already
pending deletion, key in different account, key still has
active grants); the provider returns these wrapped:

```go
return fmt.Errorf("aws-kms: %w: %s", kms.ErrShredFailed, awsErr)
```

### `FIPSMode() bool`

Reports whether this provider is operating in
FIPS-validated mode.  Used by `pg_hardstorage doctor` to
surface compliance posture and by the runtime
`--fips-strict` flag to refuse non-FIPS providers.

For AWS KMS, this means the operator pointed at a
FIPS-validated region (`us-gov-west-1`, `us-gov-east-1`,
or any commercial region with `aws_use_fips_endpoint=true`
in the config).  For Vault Transit, it means the Vault
deployment is running on FIPS-validated cryptographic
modules (Vault Enterprise + FIPS Inside).

`FIPSMode()` is a **must-tell-the-truth** method.  Lying
returns true here under non-FIPS operation will land
backups in the FIPS audit trail under false pretences.

### `Close() error`

Release provider-side resources — HTTP connections, SDK
clients, leased Vault tokens.  Idempotent.

## Registration

```go
// in your provider's package
func init() {
    kms.DefaultRegistry.Register("my-scheme",
        func(ctx context.Context, kekRef string, cfg map[string]any) (kms.Provider, error) {
            return New(ctx, kekRef, cfg)
        })
}
```

The `Builder` signature receives:

| Arg | Source |
| --- | --- |
| `ctx` | The caller's context (for SDK-init timeouts, AWS STS round-trips, …) |
| `kekRef` | The full manifest-stamped string (`aws-kms://arn:aws:kms:...`) |
| `cfg` | Provider-specific config from `pg_hardstorage.yaml` (`region`, `aws_use_fips_endpoint`, `pending_window_days`, etc.) |

The host's `kms.DefaultRegistry.Open(ctx, kekRef, cfg)`
extracts the scheme via `SchemeOf(kekRef)`, looks up the
builder, calls it, and returns the ready-to-use Provider.

**Re-registration is allowed and overwrites.**  This is
the idiom Tier-2 plugins use to override a Tier-1
default — `pg_hardstorage` calls
`DefaultRegistry.Register("aws-kms", tier2Builder)` after
the Tier-2 plugin discovery phase finishes, replacing the
in-tree implementation.

## Error sentinels

```go
var (
    ErrUnwrap        = errors.New("kms: DEK unwrap failed")
    ErrShredFailed   = errors.New("kms: shred failed")
    ErrUnknownScheme = errors.New("kms: unknown KEKRef scheme")
)
```

Use `errors.Is` for detection.  Wrap your provider
errors:

```go
return nil, fmt.Errorf("vault-transit: %w: %s", kms.ErrUnwrap, vaultErr)
```

## Concurrency contract

| Operation | Concurrent calls allowed? |
| --- | --- |
| `WrapDEK` / `UnwrapDEK` (different DEKs) | Yes |
| `WrapDEK` / `UnwrapDEK` (same DEK) | Yes — KEK ops are stateless w.r.t. the wrapped value |
| `Shred` | Effectively single-call; subsequent ops will fail |
| `Close` | Serial; host serializes against in-flight ops |

## Air-gap interaction

Cloud KMS resolves over the public internet (or via VPC
endpoint with private IP).  Operators running in air-gap
mode (`PG_HARDSTORAGE_AIRGAPPED=1`) MUST point at an
endpoint that resolves to an RFC 1918 address; the
air-gap policy honours the routable-private-IP allowlist.
Provider implementations consult `airgap.Default()` in
their constructor and refuse if the resolved endpoint
violates the policy — see `awskms.go` for the pattern.

## What providers MUST get right

1. **`UnwrapDEK` failures wrap `ErrUnwrap`.**  Callers
   distinguish auth-failure from network-failure.
2. **`KEKRef()` is stable.**  A provider that mints a
   fresh KEK at `Open` stamps the same string back into
   `KEKRef()` so the manifest writer captures the
   correct reference.
3. **`Shred` is irreversible.**  Provider implementations
   MUST NOT silently downgrade to a soft-delete; if the
   backend doesn't support irreversible destruction,
   return an error.
4. **`FIPSMode()` doesn't lie.**  False positives here
   pollute the compliance audit trail.

## Tier-2 mapping

The Tier-2 gRPC contract (see
`proto/plugin/v1/plugin.proto` `service EncryptionPlugin`)
exposes a slightly broader surface than `kms.Provider`:
the proto's `GenerateDEK` (mints a fresh DEK and returns
both plaintext and wrapped form) and `RotateKEK`
(re-wrap the same DEK under a new KEK) are convenience
operations the Tier-1 host implements outside the
Provider interface — the proto folds them in for
language-agnostic plugins.

A pure-`kms.Provider` Tier-2 plugin that doesn't
implement `GenerateDEK` / `RotateKEK` returns
`UNIMPLEMENTED` for those RPCs; the host falls back to
`WrapDEK` against a host-generated DEK.

## Further reading

- KEKRef catalogue: `reference/kekref-schemes.md`
  (auto-generated from `kms.DefaultRegistry.Schemes()`).
- `R2 — KMS key destroyed` runbook: response procedure
  when `Shred` fired against the wrong key.
- `R3 — Cold start from backups` runbook: what unwrap
  failure looks like during a cold-DR drill.
- The compliance posture pages
  (`compliance/`) on FIPS-strict mode and the audit chain.
