# pg_hardstorage Makefile
#
# Build / test / lint targets. CGO is disabled by default — every
# production dependency is pure Go. Set CGO_ENABLED=1 explicitly if
# you want to build the FIPS variant against BoringCrypto (v0.5+).

BINARY  := pg_hardstorage
TESTKIT := pg_hardstorage_testkit
BIN_DIR := bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
  -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Version=$(VERSION) \
  -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Commit=$(COMMIT) \
  -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Date=$(DATE)

GOFLAGS := -trimpath
CGO_ENABLED ?= 0

# Test timeout for the integration suite. PG container startup +
# real BASE_BACKUP + WAL streaming can take a couple of minutes on
# slow runners; 10m is comfortable.
INTEGRATION_TIMEOUT ?= 10m

# Pin TMPDIR off /tmp.  Why: /tmp is a tmpfs with a fixed inode
# ceiling (1 M on most distros).  testcontainers' minio sinks bind-
# mount root-owned scratch dirs there, and a few concurrent test
# campaigns blow through the inode cap — at which point even small
# scratch writes start failing with ENOSPC and you can't
# clean up without sudo.  Pointing TMPDIR at an ext4 path under the
# repo (test-runs/tmp/, already gitignored) makes test runs survive
# arbitrarily many iterations on the same checkout.
#
# Override via the HS_TMPDIR env var when the repo-local path is the
# wrong filesystem for your host — e.g. a checkout on a small disk
# with a separate large mount nearby:
#
#     HS_TMPDIR=/data/tmp make test-all
#
# The resolved path is announced once per make invocation (see the
# $(info) line below), so it's never invisible.
#
# `export` propagates to all child processes (go test, the testkit
# binary, run_*.sh, etc.) so the entire test toolchain inherits the
# override without each callsite needing to set it.
HS_TMPDIR ?= $(CURDIR)/test-runs/tmp
export TMPDIR := $(HS_TMPDIR)
$(info pg_hardstorage: TMPDIR=$(TMPDIR) (override: HS_TMPDIR=<path> make ...))

# Disable testcontainers-go's Ryuk reaper container by default.
# Why: testcontainers/ryuk:0.13.0 starts and exits with code 1
# ~2 s later on Docker 29.x + cgroup-v2 + overlay2 (reproduced on
# Fedora 42 host).  Once it dies, every subsequent integration
# test that asks for a reaper either races on the half-removed
# container ("unexpected container status 'removing'") or hits a
# name conflict with the dead one, and the entire test-integration
# / test-wal-stream-suite / test-release-gate go-test wave fails
# at PG-container creation before any pg_hardstorage code runs.
#
# Disabling Ryuk skips the reaper entirely; testcontainers-go
# honours TESTCONTAINERS_RYUK_DISABLED=true as a first-class no-op.
# The reaper's only job — reap leaked testcontainers if the test
# process dies mid-run — overlaps the per-test t.Cleanup() blocks
# that our integration suites already wire, so its absence is a
# no-op on a clean exit.
#
# `?=` so a CI host where Ryuk works can flip it back via
# TESTCONTAINERS_RYUK_DISABLED=false make test-integration.
export TESTCONTAINERS_RYUK_DISABLED ?= true

.PHONY: all help build build-testkit build-fips build-pkcs11 build-firecracker \
	test test-integration test-all \
	test-mutations \
	check cover vet lint fmt tidy clean install release-snapshot \
	sync-llm-docs \
	docs-build docs-serve docs-regen docs-cli docs-man docs-doctest \
	docs-completions docs-deps docs-clean

# Default target shows help — discoverable from a fresh clone.
all: help

help:
	@echo "pg_hardstorage Makefile — common targets:"
	@echo ""
	@echo "  make build              build bin/$(BINARY)"
	@echo "  make build-testkit      build bin/$(TESTKIT)"
	@echo "  make all-binaries       build both"
	@echo "  make build-fips         BoringCrypto FIPS variant (CGO + Linux/amd64)"
	@echo "  make build-pkcs11       HSM variant (-tags pkcs11; needs miekg/pkcs11)"
	@echo "  make build-firecracker  microVM verify-sandbox variant (-tags firecracker)"
	@echo ""
	@echo "  make test               go test -race -count=1 ./..."
	@echo "  make test-integration   go test -tags=integration ... (Docker required)"
	@echo "  make test-all           default + integration suites"
	@echo "  make test-mutations     run the testkit mutation harness (asserts the"
	@echo "                          test suite catches deliberately-broken variants"
	@echo "                          guarded by mutation_<tag> build tags; ~30s)"
	@echo "  make test-wal-stream-suite"
	@echo "                          full WAL-streamer scenario sweep (8 variants;"
	@echo "                          ~30 minutes serial; needs Docker).  Pass"
	@echo "                          TESTKIT_SCENARIO=<name> to run one variant."
	@echo "  make test-wal-stream-lint"
	@echo "                          lint-only sweep over the WAL-streamer scenarios"
	@echo "                          (no Docker; catches YAML schema drift)"
	@echo "  make cover              coverage report at coverage.out"
	@echo ""
	@echo "  make check              vet + test (the pre-PR sanity gate)"
	@echo "  make vet                go vet ./..."
	@echo "  make lint               golangci-lint run (install separately)"
	@echo "  make fmt                gofmt -s -w ."
	@echo "  make tidy               go mod tidy"
	@echo ""
	@echo "  make install            install bin/$(BINARY) to /usr/local/bin"
	@echo "  make clean              remove $(BIN_DIR)/"
	@echo ""
	@echo "  make release-snapshot   build a stamped binary with the current"
	@echo "                          VERSION ($(VERSION))"
	@echo ""
	@echo "Documentation:"
	@echo "  make docs-build         build the MkDocs site to ./site/"
	@echo "  make docs-serve         live-preview at http://localhost:8000"
	@echo "  make docs-regen         regenerate every auto-generated page"
	@echo "                          (CLI, manpages); CI fails on drift"
	@echo "  make docs-deps          install MkDocs Material + plugins"
	@echo "  make docs-clean         remove ./site/"
	@echo "  make docs-doctest       run \`# RUNNABLE\` code blocks against a real binary"

# Build both binaries — the production CLI and the testkit harness.
all-binaries: build build-testkit build-simple

build: | $(HS_TMPDIR)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)

build-testkit: | $(HS_TMPDIR)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(TESTKIT) ./cmd/$(TESTKIT)

# Build the interactive companion binary.  No flags, just a numbered
# menu — covers the six most common operations against a real
# pg_hardstorage repo.  Same Go toolchain + ldflags as the main
# binary so version reporting stays consistent.
build-simple: | $(HS_TMPDIR)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/pg_hardstorage_simple ./cmd/pg_hardstorage_simple

# Drop-in CLI shims for legacy backup tools.  Operators
# symlink one of these onto PATH as `pgbackrest` (or
# `barman`, `wal-g`, `barman-wal-archive`) so existing cron
# jobs and archive_command lines keep working but produce
# native pg_hardstorage backups.  See compat/README.md.
#
# As of this commit the four shims SHARE A SINGLE BINARY
# via the BusyBox / coreutils multi-call pattern: one
# pg-hardstorage-compat binary that dispatches on its
# argv[0].  The four shim names install as symlinks to the
# multi-call binary.  Disk footprint goes from
# 4 × 62 MiB to 1 × 62 MiB plus four symlinks; the Linux
# page cache already shared text segments across same-binary
# invocations, so the change is pure size win — no runtime
# cost.
#
# `make build-compat` builds the multi-call binary and
# creates the four symlinks under ./bin.  The legacy
# per-shim build targets (build-compat-pgbackrest,
# build-compat-barman, build-compat-walg) still work for
# operators who want a standalone binary per shim — they
# build the same compat-package code under separate
# cmd/pg-hardstorage-<name>/main.go entry points.
.PHONY: build-compat build-compat-multicall build-compat-pgbackrest build-compat-barman build-compat-walg
build-compat-multicall: | $(HS_TMPDIR)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/pg-hardstorage-compat ./cmd/pg-hardstorage-compat
	@# Replace any prior real binaries (from a pre-multi-call
	@# build) with symlinks to the new compat dispatcher.
	@# `ln -sfn` handles both the "no prior file" and "prior
	@# file is itself a symlink" cases atomically.
	@for n in pg-hardstorage-pgbackrest \
	          pg-hardstorage-barman \
	          pg-hardstorage-barman-wal-archive \
	          pg-hardstorage-walg \
	          pg-hardstorage-barman-cloud-backup \
	          pg-hardstorage-barman-cloud-restore \
	          pg-hardstorage-barman-cloud-wal-archive \
	          pg-hardstorage-barman-cloud-wal-restore; do \
	    rm -f $(BIN_DIR)/$$n; \
	    ln -sf pg-hardstorage-compat $(BIN_DIR)/$$n; \
	done

# Default `build-compat` is the multi-call build.  Operators
# (and CI) should not need to opt in to it.
build-compat: build-compat-multicall

# Legacy single-shim builds — kept so distros that prefer one
# binary per shim (e.g. for stricter dpkg / rpm separation)
# can still produce them.  Each one is the same ~62 MiB; the
# multi-call build above is what we ship by default.
build-compat-pgbackrest: | $(HS_TMPDIR)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/pg-hardstorage-pgbackrest ./cmd/pg-hardstorage-pgbackrest

build-compat-barman: | $(HS_TMPDIR)
	@mkdir -p $(BIN_DIR)
	@if [ -f cmd/pg-hardstorage-barman/main.go ]; then \
		CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' \
			-o $(BIN_DIR)/pg-hardstorage-barman ./cmd/pg-hardstorage-barman; \
	else \
		echo "skipping pg-hardstorage-barman: cmd/pg-hardstorage-barman/main.go not present yet"; \
	fi
	@if [ -f cmd/pg-hardstorage-barman-wal-archive/main.go ]; then \
		CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' \
			-o $(BIN_DIR)/pg-hardstorage-barman-wal-archive ./cmd/pg-hardstorage-barman-wal-archive; \
	else \
		echo "skipping pg-hardstorage-barman-wal-archive: cmd/pg-hardstorage-barman-wal-archive/main.go not present yet"; \
	fi

build-compat-walg: | $(HS_TMPDIR)
	@mkdir -p $(BIN_DIR)
	@if [ -f cmd/pg-hardstorage-walg/main.go ]; then \
		CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' \
			-o $(BIN_DIR)/pg-hardstorage-walg ./cmd/pg-hardstorage-walg; \
	else \
		echo "skipping pg-hardstorage-walg: cmd/pg-hardstorage-walg/main.go not present yet"; \
	fi

# FIPS variant.  Builds against Go's BoringCrypto experiment so
# every `crypto/tls`, `crypto/aes`, `crypto/sha256` etc. routes
# through a FIPS 140-2 validated module (the one shipped with
# Google's BoringSSL).
#
# Requirements:
#
#   - Go 1.19+ on linux/amd64 (other GOOS/GOARCH combinations
#     don't have a BoringSSL build available — Go's
#     GOEXPERIMENT=boringcrypto refuses with a clear error
#     anywhere else).
#   - CGO enabled (BoringSSL is C; the wrapper links it in).
#
# We embed the build tag `fips` so the runtime knows which
# variant it is (internal/fips.Enabled returns true on a
# fips-built binary; that flag is what `pg_hardstorage doctor`
# surfaces to operators).
#
# Usage:
#
#   make build-fips        # writes bin/$(BINARY)-fips
#   ./bin/$(BINARY)-fips doctor   # the doctor section reports FIPS=true
build-fips: | $(HS_TMPDIR)
	@mkdir -p $(BIN_DIR)
	GOEXPERIMENT=boringcrypto CGO_ENABLED=1 go build -tags fips $(GOFLAGS) \
		-ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/$(BINARY)-fips ./cmd/$(BINARY)
	@echo
	@echo "FIPS variant built at $(BIN_DIR)/$(BINARY)-fips"
	@echo "Verify the BoringCrypto symbols are present:"
	@echo "  go tool nm $(BIN_DIR)/$(BINARY)-fips | grep -i goboringcrypto | head -5"

# PKCS#11 / HSM variant.  Activates the cgo-backed PKCS#11
# KMS provider over `github.com/miekg/pkcs11`.  CGO is
# required because the binding wraps libpkcs11 / opensc /
# libsofthsm2 etc. — the binary links against the operator-
# selected PKCS#11 module at runtime via dlopen, but the
# binding itself is C.
#
# The SDK dep isn't in the default-build go.mod (the file is
# gated behind //go:build pkcs11).  Operators wanting HSM
# vendor the dep into their fork's go.mod once and then
# `make build-pkcs11` repeatedly:
#
#   go get github.com/miekg/pkcs11@v1.1.1   # one-time
#   make build-pkcs11                       # every build
#
# CI's tag-build smoke job runs `go get` on a throwaway
# checkout to validate the file compiles; production
# packagers (pg-hardstorage-fips artifact) carry the dep in
# their fork.
build-pkcs11: | $(HS_TMPDIR)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 go build -tags pkcs11 $(GOFLAGS) \
		-ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/$(BINARY)-pkcs11 ./cmd/$(BINARY)
	@echo
	@echo "PKCS#11 variant built at $(BIN_DIR)/$(BINARY)-pkcs11"

# Firecracker microVM verifier-sandbox variant.  Activates
# the firecracker-go-sdk-backed sandbox backend.  Pure Go
# (no CGO required); only the firecracker process itself
# (which the agent execs as a subprocess) is C.
#
# Operators wanting microVM isolation pick this variant
# instead of (or alongside) the default Docker-backed
# sandbox.  Linux + KVM only.
#
# Same go.mod posture as build-pkcs11: operators vendor the
# SDK once into their fork's go.mod, then `make build-
# firecracker` repeatedly.
#
#   go get github.com/firecracker-microvm/firecracker-go-sdk
#   make build-firecracker
build-firecracker: | $(HS_TMPDIR)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build -tags firecracker $(GOFLAGS) \
		-ldflags '$(LDFLAGS)' \
		-o $(BIN_DIR)/$(BINARY)-firecracker ./cmd/$(BINARY)
	@echo
	@echo "Firecracker variant built at $(BIN_DIR)/$(BINARY)-firecracker"

# Default test suite — pure-Go, no external services.
#
# `GO_PKGS` is the explicit list of package roots we test.  We
# intentionally do NOT use `./...` here: a soak run with
# --keep-on-failure leaves test-runs/<run>/soak/repo-data/{audit,
# chunks,manifests} owned by root:root mode 750 (created by the
# in-container agent), and `go list ./...` walks the tree recursively
# to discover packages — the moment it hits a root-owned subdir
# without read perm it aborts with `pattern ./...: open …: permission
# denied` and the entire test target fails as "FAIL ./... [setup
# failed]" with zero packages tested.  Listing the four real top-
# level package roots keeps go test out of test-runs/, bin/, .git/
# entirely.  Add a new top-level dir here if and when one ships.
GO_PKGS ?= ./cmd/... ./compat/... ./dockerfiles/... ./internal/...

test: | $(HS_TMPDIR)
	go test -race -count=1 $(GO_PKGS)

# Materialise the off-/tmp scratch dir.  Order-only prerequisite
# (`|`) so a re-stat doesn't trip mtime-based rebuilds.
$(HS_TMPDIR):
	@mkdir -p "$(HS_TMPDIR)"

# Tests that spin up real PostgreSQL containers via testcontainers-go.
# Requires a running Docker daemon; tests skip cleanly when Docker
# is unreachable so this target is safe to run in environments
# without Docker (it'll simply produce no real coverage there).
test-integration: | $(HS_TMPDIR)
	go test -tags=integration -race -count=1 -timeout=$(INTEGRATION_TIMEOUT) $(GO_PKGS)

# Mutation harness. Loops over internal/testkit/mutation/Registry and
# runs `go test -tags=<mutation-tag>` against each affected package,
# asserting the suite catches the deliberate regression. A failure
# here is a coverage gap (an existing mutation no longer breaks any
# test). Adds ~30s wallclock; not part of the default `test` target.
test-mutations: | $(HS_TMPDIR)
	go test -tags=mutation_runner -count=1 -timeout=300s ./internal/testkit/mutation/...

# WAL-stream scenario suite. Continuous WAL streaming is the
# headline feature of pg_hardstorage; this target gates every
# variant of the streamer life-cycle on every test run, not
# just the per-PR fast L1-L7 stack.
#
# Each scenario brings up its own local-docker (or s3-minio)
# topology and runs end-to-end through the testkit binary.
# Failures preserve the artefact dir under test-runs/; on
# success the dirs are torn down (cleanup.on_success).
#
# Total wall-clock budget: ~30 minutes serial.  Run a subset
# with TESTKIT_SCENARIO=<name> if you only want one variant:
#
#     make test-wal-stream-suite TESTKIT_SCENARIO=L3_wal_stream_continuous
#
# Requires Docker (testcontainers-go); no-ops cleanly when
# the daemon is unreachable.
.PHONY: test-wal-stream-suite
WAL_STREAM_SCENARIOS := \
	test/scenarios/L3_wal_stream_continuous.scenario.yaml \
	test/scenarios/L3_wal_stream_restart.scenario.yaml \
	test/scenarios/L3_wal_stream_ddl_storm.scenario.yaml \
	test/scenarios/L3_wal_stream_long_backup_window.scenario.yaml \
	test/scenarios/L3_wal_stream_double_backup.scenario.yaml \
	test/scenarios/L3_wal_stream_pg_restart.scenario.yaml \
	test/scenarios/L3_wal_stream_storage_outage.scenario.yaml \
	test/scenarios/L3_wal_stream_s3.scenario.yaml \
	test/scenarios/L4_wal_stream_patroni_single_failover.scenario.yaml \
	test/scenarios/L4_wal_stream_pitr_through_failover.scenario.yaml \
	test/scenarios/L4_wal_stream_full_lifecycle.scenario.yaml \
	test/scenarios/L4_wal_stream_patroni_slot_recreate.scenario.yaml
.PHONY: build-multipg-image
# Build the multi-PG (16+17 side-by-side) testbed image consumed by
# the L4_pg_upgrade_cross_major scenario.  This image is single-purpose
# and intentionally outside the testkit's `image build` catalog (which
# walks the family Dockerfiles instead).  Without it, the scenario
# fails instantly with "pull access denied for pg-hardstorage-l4-multipg".
build-multipg-image:
	docker build -f dockerfiles/testbed/Dockerfile.multi-pg-l4 \
		-t pg-hardstorage-l4-multipg:latest .

test-wal-stream-suite: build-testkit | $(HS_TMPDIR)
	@if [ -n "$(TESTKIT_SCENARIO)" ]; then \
		echo "→ running single scenario: $(TESTKIT_SCENARIO)"; \
		$(BIN_DIR)/$(TESTKIT) scenario run \
			test/scenarios/$(TESTKIT_SCENARIO).scenario.yaml; \
	else \
		set -e; \
		for s in $(WAL_STREAM_SCENARIOS); do \
			echo "→ $$s"; \
			$(BIN_DIR)/$(TESTKIT) scenario run "$$s"; \
		done; \
		echo "all wal-stream scenarios passed"; \
	fi

# Lint-only sweep over the wal-stream suite — fast, no
# Docker.  Catches schema drift in any of the YAML files
# without spending the wall-clock budget of a real run.
.PHONY: test-wal-stream-lint
test-wal-stream-lint: build-testkit
	@set -e; \
	for s in $(WAL_STREAM_SCENARIOS); do \
		$(BIN_DIR)/$(TESTKIT) scenario lint "$$s" >/dev/null; \
		echo "lint ok: $$s"; \
	done

# Release-gate roundtrip: spin a fresh PG via testcontainers,
# take a backup, verify it, restore into a fresh datadir, sniff
# PG_VERSION + canonical files.  Six-step "is the headline path
# functional?" check that should run before every tagged release.
#
# Skipped automatically (not failed) when Docker is unreachable —
# matches test-integration semantics.  Build tag `release_gate`
# keeps it out of the default `go test ./...` because the PG
# container boot dominates the wall-clock and this isn't a fast
# PR-gate signal.
.PHONY: test-release-gate
test-release-gate: build | $(HS_TMPDIR)
	go test -tags release_gate -count=1 -timeout 180s -v ./internal/regression/...

# Everything: default + integration + release-gate. The pre-release gate.
test-all: test test-integration test-release-gate

# Coverage report. Open coverage.html in a browser for the heatmap.
#
# Use $(GO_PKGS) — not `./...` — for the same reason as the `test`
# target: a soak run leaves root-owned scratch dirs under test-runs/
# and stale go-build caches under test-artefacts-*/, and `go list ./...`
# aborts the moment it walks into one of them.
cover:
	go test -race -count=1 -coverprofile=coverage.out -covermode=atomic $(GO_PKGS)
	@go tool cover -func=coverage.out | tail -1
	@go tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.out + coverage.html"

# Pre-PR sanity gate: vet first (cheap, catches type errors), then
# the full default test suite (race detector). Skip integration —
# operators run that explicitly.
check: vet test

# vet uses $(GO_PKGS) for the same reason `test` does — see the
# comment above $(GO_PKGS) for the full story.  `go vet ./...`
# walks the entire repo and aborts on root-owned test-runs/repo-data
# or stray test-artefacts-*/ go-build caches.
vet:
	go vet $(GO_PKGS)

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

# golangci-lint config lives at .golangci.yml when we add it.
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { echo "install golangci-lint: https://golangci-lint.run"; exit 1; }
	golangci-lint run

# govulncheck — hard gate in CI from v0.4+. Reports only on reachable
# vulnerable code paths via the call-graph walk. Run locally before
# every release to catch CVEs in transitive deps that we actually
# touch.
#
# $(GO_PKGS) for the same reason as `test` / `vet` — see above.
govulncheck:
	@command -v govulncheck >/dev/null 2>&1 || { echo "install: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	govulncheck -show=verbose $(GO_PKGS)
	govulncheck -show=verbose -tags=integration $(GO_PKGS)

clean:
	rm -rf $(BIN_DIR)/ coverage.out coverage.html

install: build
	install -m 0755 $(BIN_DIR)/$(BINARY) /usr/local/bin/$(BINARY)

# Build a stamped binary using the current VERSION/COMMIT/DATE. This
# is what goreleaser will invoke when we wire up release artefacts in
# v0.5; for now it's a smoke-test that the LDFLAGS injection works.
release-snapshot: build
	@$(BIN_DIR)/$(BINARY) version

# Sync the LLM helper's bundled docs corpus from the repository
# sources.  go:embed can only reach files inside the package
# directory, so we copy the canonical docs into
# internal/llm/docs/{root,runbooks}/ as the embed source.
#
# Run this whenever CHANGELOG.md / README.md / docs/runbooks/*.md
# change; CI checks that the bundled copies match.
sync-llm-docs:
	@mkdir -p internal/llm/docs/runbooks internal/llm/docs/root
	cp docs/reference/runbooks/*.md internal/llm/docs/runbooks/
	cp CHANGELOG.md internal/llm/docs/root/
	cp README.md internal/llm/docs/root/
	@echo "synced LLM docs corpus"

# ----- Documentation site -----------------------------------
#
# pg_hardstorage's user-facing documentation site lives in
# ./docs/ and builds with MkDocs Material.  See
# docs/DOC_PLAN.md for the design + IA + tooling decisions.
#
# The site builds without a public domain — set site_url
# in mkdocs.yml when one lands.

# docs-deps installs the Python toolchain into a project-
# local venv.  Idempotent.  CI installs the same set via
# pip with the pinned versions in requirements-docs.txt.
docs-deps:
	@command -v python3 >/dev/null 2>&1 || \
		(echo "python3 not found; install Python 3.10+ first"; exit 1)
	@if [ ! -d .venv ]; then python3 -m venv .venv; fi
	@. .venv/bin/activate && pip install --quiet --upgrade pip
	@. .venv/bin/activate && pip install --quiet \
		'mkdocs-material>=9.5,<10' \
		'mkdocs-material-extensions>=1.3' \
		'pymdown-extensions>=10.7' \
		'mkdocs-static-i18n>=1.2'
	@echo "docs deps installed in ./.venv (activate: source .venv/bin/activate)"

# docs-build runs mkdocs build with --strict so a broken
# cross-link, missing nav target, or unrecognised extension
# fails the build.  The output lands in ./site/.
docs-build: docs-deps
	. .venv/bin/activate && mkdocs build --strict

# docs-serve runs mkdocs serve with live reload.  Operators
# previewing changes locally use this; the URL is
# http://localhost:8000.
docs-serve: docs-deps
	. .venv/bin/activate && mkdocs serve

# docs-regen rebuilds every auto-generated page.  CI fails
# if a `git diff` after this is non-empty — drift between
# the source of truth (Cobra command tree, OpenAPI spec,
# .proto files) and the committed reference pages is
# treated as a bug.
docs-regen: docs-cli docs-man docs-completions
	@echo "docs regenerated; commit any diffs"

# docs-completions emits bash / zsh / fish completion
# scripts under completions/<shell>/.  Both the Debian and
# RPM packaging install from these paths; keeping them in
# the regen target means a CLI flag rename is reflected in
# completion files at the same time as the docs.
docs-completions: | $(HS_TMPDIR)
	@mkdir -p completions/bash completions/zsh completions/fish
	go run ./cmd/docsgen -target=completions -completions-dir=completions

# docs-cli emits one Markdown page per Cobra subcommand
# into docs/reference/cli/, plus an index.md table of
# contents.  Built from the live Cobra command tree so the
# pages can never disagree with `pg_hardstorage --help`.
docs-cli: | $(HS_TMPDIR)
	@mkdir -p docs/reference/cli
	go run ./cmd/docsgen -target=cli -cli-dir=docs/reference/cli

# docs-man emits manpages from the Cobra tree.  The
# debian/pg-hardstorage.manpages packaging file installs
# them under /usr/share/man/man1/.
docs-man: | $(HS_TMPDIR)
	@mkdir -p man/man1
	go run ./cmd/docsgen -target=man -man-dir=man/man1

# docs-clean removes the rendered site.  Useful before a
# release-prep build to ensure no stale pages slip in.
docs-clean:
	rm -rf site/

# docs-doctest runs the markdown-test-runner over every
# tutorial + how-to page that has `# RUNNABLE` code blocks
# (per docs/CONTRIBUTING-DOCS.md).  Catches tutorial
# bit-rot — a CLI flag rename that the docs missed fails
# here.  Requires:
#
#   - a running pg_hardstorage binary (`make build` first)
#   - a reachable PG to talk to (set
#     PG_HARDSTORAGE_DOCTEST_PG=postgres://… or pass
#     --pg-connection)
#
# In CI: set PG_HARDSTORAGE_DOCTEST_CI=1 so blocks marked
# `skip-in-ci="..."` are skipped (e.g. tutorials that need
# a Patroni cluster, K8s, or AWS KMS).  Locally: omit the
# var to run everything.
#
# Use `make docs-doctest LIST=1` to list runnable blocks
# without executing them.
docs-doctest: build
	@if [ -n "$(LIST)" ]; then \
		go run ./cmd/doctest -root docs/tutorials -list; \
	else \
		go run ./cmd/doctest -root docs/tutorials; \
	fi
