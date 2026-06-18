# paths/

FHS-aware path resolution: where config, state, cache, runtime, and logs land on
every supported OS, with one precedence chain.

## What lives here

A single resolver that turns the abstract paths `Config`, `State`, `Cache`,
`Runtime`, `Logs`, and `Shared` into concrete filesystem locations based on
whether `pg_hardstorage` is running in XDG mode (user, e.g.
`~/.config/pg_hardstorage`) or system mode (`/etc/pg_hardstorage`,
`/var/lib/pg_hardstorage`, `/run/pg_hardstorage`). All path discovery in the
codebase routes through here — no `os.Getenv("HOME")` scattered around.

## Precedence chain

1. Explicit flag (`--config-dir`, `--state-dir`, ...)
2. Environment override (`PG_HARDSTORAGE_CONFIG_DIR`,
   `PG_HARDSTORAGE_STATE_DIR`, ...)
3. XDG variable (`XDG_CONFIG_HOME`, `XDG_STATE_HOME`, ...) when in user mode
4. FHS default (`/etc`, `/var/lib`, `/var/cache`, `/run`, `/var/log`) when in
   system mode

## Key files

- `paths.go` — `Resolver`, `Mode` (auto/user/system), all `*Dir()` accessors
- `paths_test.go` — table-driven coverage across XDG, FHS, override, and macOS
  quirks
- `winresolution_test.go` — Windows path resolution (Known Folders,
  ProgramData)

## Read next

- `../config/README.md` — first consumer; loader looks in `ConfigDir()`
- `debian/pg-hardstorage.dirs` — Debian's view of the FHS layout
- `docs/reference/file-layout.md` — user-facing layout reference

## Don't put X here

- File I/O — this package only resolves; reading and writing happens at call
  sites.
- Tilde expansion or shell glob handling — the resolver returns absolute,
  canonical paths.
- Plugin-specific path schemes — plugins ask the resolver, they don't extend
  it.
