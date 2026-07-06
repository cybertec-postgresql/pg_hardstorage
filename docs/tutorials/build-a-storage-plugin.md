---
title: Build a Tier-2 storage plugin
description: Author and ship an external storage plugin — discovery,
              probe handshake, JSON-RPC over stdio, putting bytes.
tags:
  - plugins
  - tier-2
  - extensibility
---

# Build a Tier-2 storage plugin

> Walks through writing a working external storage plugin from
> scratch, in Go: probe handshake, JSON-RPC over stdio, registering
> a custom URL scheme, smoke-testing it against a real
> `pg_hardstorage` install. About 45 minutes if you have Go on
> your machine; the same protocol works in any language that can
> read stdin and write stdout.

`pg_hardstorage` ships with two plugin tiers:

- **Tier-1** — in-tree Go interfaces, statically linked. The fast
  path: chunk I/O during a backup never crosses a process boundary.
  S3, FS, Azure, GCS all live here.
- **Tier-2** — separate executables discovered at startup, talking
  one-shot JSON-RPC over stdio. Crash-isolated, language-agnostic,
  no shared-library ABI to break across Go versions.

You author Tier-2. The host launches your binary on demand for
each RPC; persistence and concurrency are not your problem.

For the precise contract — every method signature, every error code,
the full handler set — see the [Tier-2 plugin reference](../reference/plugins/index.md).
This page is the guided "build one" walkthrough.

---

## What you need

- Go 1.22 or later.
- `pg_hardstorage` v0.2 or later on `$PATH`.
- A scratch directory (`/tmp/hs-plugin-tutorial` is fine).
- One terminal.

The plugin we build is `pg-hardstorage-plugin-mem` — an in-memory
"storage" backend that stores objects in a JSON file on disk so the
test surface is observable. Useful as a teaching example, not for
real workloads.

---

## Steps

### 1. Understand the wire protocol in three sentences

The host walks `$HSPLUGIN_PATH` (default
`/usr/local/lib/pg_hardstorage/plugins:/usr/lib/pg_hardstorage/plugins`)
for executables prefixed `pg-hardstorage-plugin-`. Each candidate is
launched once with `--probe` and `PG_HARDSTORAGE_PLUGIN=1` in the
environment; it must write **exactly one JSON object** to stdout
declaring its protocol, name, kind, and schemes, then exit. For
every operation thereafter, the host re-launches the binary, writes
**one JSON-RPC request line** to stdin, reads **one response line**
from stdout, and reaps the process.

That is the entire contract.

### 2. Scaffold the plugin

```bash
mkdir -p /tmp/hs-plugin-tutorial
cd /tmp/hs-plugin-tutorial
go mod init example.com/hs-plugin-mem
```

```bash
go get github.com/cybertec-postgresql/pg_hardstorage@v0.2.0
```

Use the in-repo helpers (`external.IsPluginInvocation`,
`external.EmitProbeResponse`, `external.ServeRPC`) — they keep the
JSON shapes correct and let the host evolve the protocol without you
re-rolling boilerplate.

### 3. Write `main.go`

```go
// pg-hardstorage-plugin-mem — minimal Tier-2 storage plugin.
package main

import (
    "encoding/json"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "strings"

    "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/external"
)

const (
    name    = "mem"
    kind    = "storage"
    scheme  = "mem"
    version = "0.1.0"
    backing = "/tmp/hs-plugin-tutorial/store.json"
)

func main() {
    if !external.IsPluginInvocation() {
        fmt.Fprintln(os.Stderr,
            "this binary is a pg_hardstorage plugin; do not run directly")
        os.Exit(2)
    }
    if len(os.Args) > 1 && os.Args[1] == "--probe" {
        _ = external.EmitProbeResponse(os.Stdout, name, kind,
            []string{scheme}, version)
        return
    }
    if err := external.ServeRPC(os.Stdin, os.Stdout, handlers()); err != nil {
        fmt.Fprintf(os.Stderr, "plugin: %v\n", err)
        os.Exit(1)
    }
}

func handlers() map[string]external.Handler {
    return map[string]external.Handler{
        "Storage.Put":  put,
        "Storage.Get":  get,
        "Storage.Stat": stat,
        "Storage.List": list,
    }
}

// --- store --------------------------------------------------------

type store struct {
    Objects map[string][]byte `json:"objects"`
}

func load() (*store, error) {
    s := &store{Objects: map[string][]byte{}}
    b, err := os.ReadFile(backing)
    if err != nil {
        if os.IsNotExist(err) {
            _ = os.MkdirAll(filepath.Dir(backing), 0o755)
            return s, nil
        }
        return nil, err
    }
    return s, json.Unmarshal(b, s)
}

func (s *store) save() error {
    b, err := json.Marshal(s)
    if err != nil {
        return err
    }
    return os.WriteFile(backing, b, 0o600)
}

// --- handlers -----------------------------------------------------

type putParams struct {
    Key  string `json:"key"`
    Body []byte `json:"body"` // base64-encoded by the JSON encoder
}

func put(params json.RawMessage) (any, error) {
    var p putParams
    if err := json.Unmarshal(params, &p); err != nil {
        return nil, err
    }
    s, err := load()
    if err != nil {
        return nil, err
    }
    s.Objects[p.Key] = p.Body
    if err := s.save(); err != nil {
        return nil, err
    }
    return map[string]any{"size": len(p.Body)}, nil
}

func get(params json.RawMessage) (any, error) {
    var p struct{ Key string }
    if err := json.Unmarshal(params, &p); err != nil {
        return nil, err
    }
    s, err := load()
    if err != nil {
        return nil, err
    }
    body, ok := s.Objects[p.Key]
    if !ok {
        return nil, &external.RPCError{
            Code: "storage.not_found", Message: p.Key,
        }
    }
    return map[string]any{"body": body}, nil
}

func stat(params json.RawMessage) (any, error) {
    var p struct{ Key string }
    if err := json.Unmarshal(params, &p); err != nil {
        return nil, err
    }
    s, err := load()
    if err != nil {
        return nil, err
    }
    body, ok := s.Objects[p.Key]
    if !ok {
        return nil, &external.RPCError{
            Code: "storage.not_found", Message: p.Key,
        }
    }
    return map[string]any{"size": len(body)}, nil
}

func list(params json.RawMessage) (any, error) {
    var p struct{ Prefix string }
    _ = json.Unmarshal(params, &p)
    s, err := load()
    if err != nil {
        return nil, err
    }
    var keys []string
    for k := range s.Objects {
        if strings.HasPrefix(k, p.Prefix) {
            keys = append(keys, k)
        }
    }
    return map[string]any{"keys": keys}, nil
}

// silence unused-import warnings on tiny builds
var _ io.Writer = os.Stdout
```

### 4. Build and install

```bash
# RUNNABLE skip-in-ci="needs scaffolded Go plugin in /tmp/hs-plugin-tutorial"
cd /tmp/hs-plugin-tutorial
go build -o pg-hardstorage-plugin-mem .
```

The host walks two default directories. For development, point it at
your build directory instead:

```bash
export HSPLUGIN_PATH=/tmp/hs-plugin-tutorial
chmod +x /tmp/hs-plugin-tutorial/pg-hardstorage-plugin-mem
```

### 5. Probe the plugin manually

The probe is what the host will run; if your hand-run doesn't
produce a single JSON line and exit cleanly, neither will the host's:

```bash
PG_HARDSTORAGE_PLUGIN=1 \
    /tmp/hs-plugin-tutorial/pg-hardstorage-plugin-mem --probe
```

```console
{"protocol":"pg_hardstorage.plugin.v1","name":"mem","kind":"storage","schemes":["mem"],"version":"0.1.0"}
```

Five-second timeout on the host side: any plugin that hangs at
probe time is dropped from the registry with a logged warning.

### 6. Confirm `pg_hardstorage` discovers it

```bash
# RUNNABLE skip-in-ci="needs scaffolded Go plugin in /tmp/hs-plugin-tutorial"
HSPLUGIN_PATH=/tmp/hs-plugin-tutorial \
    pg_hardstorage plugin list
```

```console
NAME                 KIND         VERSION    PATH
mem                  storage      0.1.0      /tmp/hs-plugin-tutorial/pg-hardstorage-plugin-mem
```

The host walked `$HSPLUGIN_PATH`, probed the binary, and got a valid
handshake back — so it lists here.

### 7. Smoke-test by reading repo audit (no real backup)

A real backup against `mem://` requires the storage interface
methods this tutorial does *not* implement (`Delete`,
`RenameIfNotExists`, `SetRetention`, `Barrier`, `Capabilities`,
`Close`) — the full interface is 12 methods.

!!! warning "Not yet wired end-to-end"

    The steps below are **aspirational**. Tier-2 external plugins are
    discovered and probed (step 6 above works today), but they are not
    yet registered as storage factories, so a `--repo mem://…` URL does
    not resolve through the external plugin. `pg_hardstorage repo init`
    / `repo audit` against a plugin scheme will not run until that
    wiring lands. Treat the following as a preview of the intended
    surface, not a runnable step.

```bash
# ASPIRATIONAL — mem:// does not resolve through a Tier-2 plugin yet
HSPLUGIN_PATH=/tmp/hs-plugin-tutorial \
    pg_hardstorage repo init mem:///hs-tutorial
```

```bash
# ASPIRATIONAL — see the warning above
HSPLUGIN_PATH=/tmp/hs-plugin-tutorial \
    pg_hardstorage repo audit mem:///hs-tutorial
```

Once wired, the backing JSON would hold two objects — the `HSREPO`
magic file and the repo config — showing that the plugin moved real
bytes through your handler.

### 8. Watch the host re-spawn the plugin

Tail the plugin's stderr to watch the host spawn and probe it:

```bash
HSPLUGIN_PATH=/tmp/hs-plugin-tutorial \
    pg_hardstorage plugin list 2>>/tmp/plugin-stderr.log
```

Each call to your binary is one process: launch, read one request,
write one response, exit. There is no daemon to supervise, no
shutdown protocol to misimplement, no leaked file descriptor on
crash. The trade-off is SDK init cost on every call — fine for repo
admin, never on the hot chunk path. If you need long-lived state
(a warmed S3 client, a connection pool), that is what Tier-1 is for.

### 9. Where this ships in production

The full storage interface — the one a *production* plugin
implements — is in [`internal/plugin/storage/`](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/internal/plugin/storage).
The Tier-2 protocol mirrors it method-for-method; see the
[storage plugin contract](../reference/plugins/storage-contract.md)
for the authoritative list, the param schemas, and the error codes.

When you have a real plugin:

- Drop the binary at `/usr/local/lib/pg_hardstorage/plugins/`
  (Linux) or under `$HSPLUGIN_PATH`.
- Sign the binary with cosign — the host honours signatures when the
  config has `plugins.require_signed: true`.
- Publish to `registry.pghardstorage.org` post-v1.0 for discovery
  by other operators.

---

## What just happened

You built and registered an external plugin that the host
discovered, probed, and dispatched against. The two ideas to take
away:

- **One-shot processes are the protocol.** Every RPC is a fresh
  invocation; the only state you keep is on disk or via the host
  passing it back in the next request. This is what makes
  Tier-2 plugins crash-isolated and language-agnostic.
- **Probe is the contract.** Get the probe response right and
  registration "just works"; get it wrong (missing `protocol` field,
  wrong version, slow exit) and your plugin silently doesn't show up
  in `pg_hardstorage plugin list`.

---

## Next steps

- [Tier-2 plugin reference](../reference/plugins/index.md) — the
  full method set, param schemas, error codes.
- [Architecture tour](../explanation/architecture-tour.md) — where
  Tier-1 vs Tier-2 sits in the data plane.
- [Operator guide](../operations/operator-guide.md) — running with
  third-party plugins in production.
