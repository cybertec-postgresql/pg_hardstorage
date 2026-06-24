---
title: Tier-2 plugin protocol
description: The wire contract for separately-shipped pg_hardstorage plugins — discovery, handshake, RPC.
tags:
  - plugins
  - tier-2
  - protocol
  - reference
---

# Tier-2 plugin protocol

Tier-2 plugins are *separate executables* that
`pg_hardstorage` discovers at startup and invokes per
operation.  Crash-isolated, language-agnostic, no shared
library ABI to break across Go versions.

This page documents the wire contract.  The Go-language
helper for plugin authors lives in
`internal/plugin/external/protocol.go` (host side and
plugin-side dispatcher both); the gRPC-shaped contract
that v1.1 will move to is at `proto/plugin/v1/plugin.proto`.

!!! note "Two protocol shapes"
    pg_hardstorage v1.0 ships a **stdio JSON-RPC**
    protocol (`pg_hardstorage.plugin.v1`) for Tier-2
    plugins.  The SPEC and the in-repo `.proto` file
    describe the gRPC-over-`hashicorp/go-plugin` contract
    that v1.1 will adopt.  Both are versioned `v1`
    intentionally — the gRPC shape is a strict superset
    and the host will negotiate which one a plugin
    speaks via the `--probe` response.

    This page documents the **shipped** stdio JSON-RPC
    protocol.  The gRPC contract is documented at the
    section heading below; reference your `.proto` file
    for the canonical message definitions.

## Discovery

The host walks every directory in `$HSPLUGIN_PATH` (or the
default `/usr/local/lib/pg_hardstorage/plugins:/usr/lib/pg_hardstorage/plugins`),
looks at every executable file whose name starts with
`pg-hardstorage-plugin-`, and invokes each with the
single argument `--probe` and the env var
`PG_HARDSTORAGE_PLUGIN=1`.

The plugin writes one JSON object on stdout and exits:

```json
{
  "protocol": "pg_hardstorage.plugin.v1",
  "name": "my-storage",
  "kind": "storage",
  "schemes": ["myproto"],
  "version": "1.2.3"
}
```

| Field | Required | Notes |
| --- | --- | --- |
| `protocol` | Yes | Must equal `"pg_hardstorage.plugin.v1"`.  Mismatched protocol = refusal. |
| `name` | Yes | Unique within `kind`.  Used by the registry; surfaces in `pg_hardstorage doctor`. |
| `kind` | Yes | One of `storage`, `sink`, `kms`, `compression`, `renderer`. |
| `schemes` | For `storage` and `kms` | URL schemes the plugin claims (`["myproto"]` for storage, `["my-kms"]` for kms). |
| `version` | Optional | Plugin's own SemVer.  Surfaces in audit events and `pg_hardstorage doctor`. |

### Probe-time guarantees

- **5-second timeout.**  A hung plugin can't stall
  startup; the host kills it after 5 s.
- **Probe failures are non-fatal.**  A bad plugin warns
  via the host's logger but never blocks startup.
- **One probe per process lifetime.**  The probe
  response is cached in the in-memory `external.Registry`
  for the rest of the run.

### Plugin-side helper

```go
// In your plugin's main():
import "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/external"

func main() {
    if !external.IsPluginInvocation() {
        fmt.Fprintln(os.Stderr, "this binary is a pg_hardstorage plugin; do not run directly")
        os.Exit(2)
    }
    if len(os.Args) > 1 && os.Args[1] == "--probe" {
        _ = external.EmitProbeResponse(os.Stdout,
            "my-storage", "storage", []string{"myproto"}, "1.2.3")
        return
    }
    // ... handle RPC ...
}
```

## RPC dispatch

For each operation the host wants to run, it spawns a
**fresh process** (no `--probe` flag) and speaks JSON-RPC
over stdio: one request line in, one response line out,
process exits.

### Request frame

```json
{"method":"Storage.Put","params":{"key":"...","data":"...","if_not_exists":true}}
```

A single line of JSON on stdin, terminated with `\n`.
Method names mirror the Tier-1 plugin interface methods:

| Tier | Methods |
| --- | --- |
| `Storage` | `Storage.Put`, `Storage.Get`, `Storage.Stat`, `Storage.List`, `Storage.Delete`, `Storage.Rename`, `Storage.SetRetention`, `Storage.FreeSpace` |
| `Sink` | `Sink.Open`, `Sink.Emit`, `Sink.Close` |
| `KMS` | `KMS.Wrap`, `KMS.Unwrap`, `KMS.Shred`, `KMS.FIPSMode` |
| `Compression` | `Compression.Compress`, `Compression.Decompress` |
| `Renderer` | `Renderer.Render` |

Method `params` shapes match the Tier-1 method
signatures, marshalled as JSON.  Bytes are
base64-encoded.  See `proto/plugin/v1/plugin.proto` for
the canonical message shapes — the JSON-RPC marshalling
follows the same field names.

### Response frame

```json
{"result":{"etag":"abc","already_existed":false}}
```

…or on error:

```json
{"error":{"code":"storage.not_found","message":"object not found: foo"}}
```

A single line of JSON on stdout, terminated with `\n`.
Exactly one of `result` / `error` is set.  The plugin
exits after writing the line.

### Error codes

`error.code` follows the v1 schema's `error.code`
namespace:

| Code | Meaning |
| --- | --- |
| `storage.not_found` | Key absent (mapped to `ErrNotFound`) |
| `storage.already_exists` | IfNotExists violated (mapped to `ErrAlreadyExists`) |
| `storage.checksum_mismatch` | ContentSHA256 disagreed |
| `storage.unsupported` | Backend doesn't support the requested op |
| `auth.permission_denied` | Backend refused auth |
| `kms.unwrap_failed` | DEK unwrap failed (mapped to `ErrUnwrap`) |
| `kms.shred_failed` | Shred refused / failed |
| `plugin.parse_request` | Plugin couldn't parse the request line |
| `plugin.unknown_method` | Plugin doesn't implement this method |
| `plugin.method_error` | Generic handler error |
| `plugin.marshal_result` | Plugin couldn't marshal its result |

### Plugin-side helper

```go
import (
    "encoding/json"
    "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/external"
)

func main() {
    if external.IsPluginInvocation() && len(os.Args) > 1 && os.Args[1] == "--probe" {
        _ = external.EmitProbeResponse(os.Stdout, "my-storage", "storage",
            []string{"myproto"}, "1.2.3")
        return
    }

    handlers := map[string]external.Handler{
        "Storage.Put": func(params json.RawMessage) (any, error) {
            var req PutRequest
            if err := json.Unmarshal(params, &req); err != nil {
                return nil, err
            }
            // ... do the work ...
            return PutResponse{ETag: etag, AlreadyExisted: false}, nil
        },
        "Storage.Get": func(params json.RawMessage) (any, error) { /* ... */ },
        // ... rest of the methods ...
    }
    if err := external.ServeRPC(os.Stdin, os.Stdout, handlers); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

`external.ServeRPC` reads one request, dispatches to the
named handler, writes one response, returns.

## Why one-shot processes (not long-lived daemons)

Long-lived plugin daemons need:

- a supervisor (lifecycle, restart-on-crash)
- a concurrency model (request multiplexing)
- a shutdown protocol

One-shot exec-per-call avoids all three.  Cost: TLS
handshake / SDK init runs once per call rather than once
per process.  Acceptable for the operations Tier-2
plugins do (init repo, refresh state, probe credentials);
the hot path (chunk I/O during a backup) stays Tier-1.

A future v1.1+ may layer a long-lived mode on top of this
protocol; the v1 contract is the one-shot shape.

## Per-RPC timeouts

Default RPC timeout is **30 seconds**.  Configurable per-
plugin via `pg_hardstorage.yaml`:

```yaml
plugins:
  - name: my-storage
    timeout: 5m
```

Slow plugins (cloud SDK init, network round-trip) push
this up.  No upper bound enforced by the host — the
per-plugin `timeout:` field (above) is the override path.

## gRPC contract (forward-looking, v1.1)

`proto/plugin/v1/plugin.proto` defines the gRPC contract
v1.1 will move to.  The shape:

- One service per plugin tier (`StoragePlugin`,
  `SinkPlugin`, `EncryptionPlugin`, ...).
- Every service has a `Handshake` RPC that exchanges
  `PluginInfo` + `Capabilities`.
- Streaming RPCs for `Get` and `List` (vs the JSON-RPC's
  send-everything-at-once shape).
- Transport: `hashicorp/go-plugin` (gRPC-over-stdin, with
  the lib's mTLS handshake).

Plugin authors who want forward compatibility can
generate stubs from the proto today and gate the
implementation behind a build tag; the host will accept
either protocol via `--probe`'s `protocol` field.

## Signing and trust

Tier-2 plugins are an **operator-trust decision**.  The
host:

- Logs the plugin path, name, version, and probe
  response in audit events at startup.
- Records the binary's SHA-256 in `pg_hardstorage doctor`
  output.
- Refuses to launch a plugin not in `$HSPLUGIN_PATH`
  (so a poisoned binary in `~/Downloads` can't be
  invoked accidentally).
- Honours `--no-tier2-plugins` to disable Tier-2
  discovery entirely (FIPS-strict environments).

`pg_hardstorage` does NOT verify plugin signatures
against a registry today.  When the public registry
(`registry.pghardstorage.org`) lands post-v1.0, the
binary will gain `--require-signed-plugins` to verify
cosign signatures against the registry root.

## Discovery diagnostics

```console
$ pg_hardstorage plugin list
NAME             KIND        VERSION    PATH
my-storage       storage     1.2.3      /usr/local/lib/pg_hardstorage/plugins/pg-hardstorage-plugin-my-storage
example-sink     sink        0.1.0      /usr/local/lib/pg_hardstorage/plugins/pg-hardstorage-plugin-example-sink
```

When no plugins are present (or `HSPLUGIN_PATH` is empty and the
default dirs don't exist), the command exits 0 with the body
`no Tier-2 plugins discovered (...)` — operators can wire it into
a doctor check without special-casing "nothing installed."

Probe failures (a binary on the path that exits non-zero or
emits a non-`v1` protocol) surface as warning events during the
host's discovery pass, so they appear in the agent's normal
event stream as well as in `audit query`.

## Further reading

- Per-tier interface contracts:
  [Storage](storage-contract.md), [Sink](sink-contract.md),
  [Encryption](encryption-contract.md),
  [Compression](compression-contract.md),
  [Renderer](renderer-contract.md).
- The gRPC contract: `proto/plugin/v1/plugin.proto`.
- Host-side discovery & client:
  `internal/plugin/external/protocol.go`.
- The [`pg_hardstorage plugin`](../cli/pg_hardstorage_plugin.md)
  CLI reference.
- [Tier-1 vs Tier-2: choosing a plugin tier](tier1-vs-tier2.md).
