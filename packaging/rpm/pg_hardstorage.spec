# RPM spec for pg_hardstorage.
#
# Targets Fedora and RHEL/Rocky/Alma (>= 9). Builds a static Go
# binary; CGO is off, so the resulting binary has no shared-lib
# dependencies beyond what golang's net package pulls in.
#
# Three sub-packages mirror the Debian split:
#   pg_hardstorage          -- agent and CLI
#   pg_hardstorage-common   -- noarch shared data
#   pg_hardstorage-server   -- stub for the v0.5 control plane

%global goipath  github.com/cybertec-postgresql/pg_hardstorage
%global pgbackup_uid 921
%global pgbackup_gid 921

# Disable the auto-generated -debuginfo / -debugsource packages.
# Why: the v1.1+ compat shims (pg-hardstorage-pgbackrest,
# pg-hardstorage-barman, pg-hardstorage-barman-wal-archive,
# pg-hardstorage-walg) are deployed as a multi-call binary —
# four CLI names sharing one ELF.  RPM's debug-info extraction
# walks every installed binary and refuses to ship a debug
# package when it sees the same Build-ID under multiple paths,
# erroring with
#   warning: Duplicate build-ids /.../pg-hardstorage-pgbackrest
#            and /.../pg-hardstorage-barman
#   error: Empty %files file ...debugsourcefiles.list
# even though the duplication is intentional.  No debug package
# means no extraction step, no duplicate-build-id check, and no
# empty %files error.  Operators who want symbols build from
# source — the binary is pure Go, `go build -trimpath` gives a
# reproducible output, and the spec's BuildRequires already
# pulls in the toolchain.
%global debug_package %{nil}

Name:           pg_hardstorage
Version:        0.1.1
Release:        1%{?dist}
Summary:        PostgreSQL backup, done right -- agent and CLI

License:        ASL 2.0
URL:            https://github.com/cybertec-postgresql/pg_hardstorage
Source0:        %{name}-%{version}.tar.gz

# openSUSE Leap calls the Go toolchain `go1.NN` (versioned
# packages from devel:languages:go) instead of `golang`, so we
# split the dep name by family.  systemd-rpm-macros exists on
# both Fedora/RHEL and Leap 15.4+, no split needed.
%if 0%{?suse_version}
BuildRequires:  go1.26
%else
BuildRequires:  golang >= 1.26
%endif
BuildRequires:  make
BuildRequires:  git
BuildRequires:  systemd-rpm-macros

Requires:       pg_hardstorage-common = %{version}-%{release}
# `shadow-utils` is the Fedora/RHEL/Rocky/Alma name; openSUSE
# ships the same binaries (useradd / groupadd) under the
# canonical `shadow` package.  Without the family split the
# %pre scriptlet's `groupadd pgbackup` runs against an OS that
# zypper insists doesn't provide `shadow-utils`, and the install
# aborts with
#   nothing provides 'shadow-utils' needed by pg_hardstorage
%if 0%{?suse_version}
Requires(pre):  shadow
%else
Requires(pre):  shadow-utils
%endif
Requires(post): systemd
Requires(preun): systemd
Requires(postun): systemd

Recommends:     postgresql >= 13

%description
pg_hardstorage performs incremental, content-addressed backups of
PostgreSQL clusters with WAL archiving and point-in-time recovery.
It is a single static Go binary that doubles as a long-running host
agent and as an interactive CLI.

This package ships the pg_hardstorage binary, shell completions,
manpages, and systemd units suitable for single- or multi-tenant
hosts.

# -----------------------------------------------------------------
%package common
Summary:        PostgreSQL backup, done right -- shared data
BuildArch:      noarch

%description common
Architecture-independent assets shared by every pg_hardstorage
binary package: runbooks, OpenAPI documents, and the v1 JSON
schema files that govern manifests and audit records.

# -----------------------------------------------------------------
%package server
Summary:        PostgreSQL backup, done right -- control-plane server
Requires:       %{name} = %{version}-%{release}

%description server
Stub package for the v0.5 control-plane server. Installing this
package today is a no-op other than registering the dependency on
pg_hardstorage; the server binary lands with v0.5 and unlocks the
multi-host orchestration API plus the centralised retention engine.

# -----------------------------------------------------------------
%package compat-pgbackrest
Summary:        pg_hardstorage drop-in shim for pgBackRest
Requires:       %{name} = %{version}-%{release}
Conflicts:      pgbackrest

%description compat-pgbackrest
Drop-in replacement binary that mimics the pgBackRest CLI surface
(8 verbs: stanza-create, backup, restore, archive-push,
archive-get, info, check, verify) so existing cron jobs,
archive_command lines, and monitoring scripts keep working but
produce native pg_hardstorage backups.

Installs /usr/bin/pg-hardstorage-pgbackrest. Conflicts with the
upstream pgbackrest package so the operator picks one or the
other. Symlink /usr/local/bin/pgbackrest to this binary for true
PATH-level drop-in.

# -----------------------------------------------------------------
%package compat-barman
Summary:        pg_hardstorage drop-in shim for Barman
Requires:       %{name} = %{version}-%{release}
Conflicts:      barman, barman-cli

%description compat-barman
Drop-in replacement binaries that mimic the Barman CLI surface
(7 verbs: backup, recover, list-backup, show-backup, check,
delete, plus a separate barman-wal-archive binary for
archive_command use).

Installs /usr/bin/pg-hardstorage-barman and /usr/bin/
pg-hardstorage-barman-wal-archive. Conflicts with the upstream
barman + barman-cli packages so the operator picks one or the
other. Symlink /usr/local/bin/{barman,barman-wal-archive} to
these binaries for true PATH-level drop-in.

# -----------------------------------------------------------------
%package compat-walg
Summary:        pg_hardstorage drop-in shim for WAL-G
Requires:       %{name} = %{version}-%{release}
Conflicts:      wal-g

%description compat-walg
Drop-in replacement binary that mimics the WAL-G CLI surface
(5 verbs: backup-push, backup-fetch, backup-list, wal-push,
wal-fetch).

Configuration follows WAL-G's env-var convention
(WALG_S3_PREFIX, PGHOST, ...).  Installs /usr/bin/
pg-hardstorage-walg.  Conflicts with the upstream wal-g package
so the operator picks one or the other.  Symlink
/usr/local/bin/wal-g to this binary for true PATH-level
drop-in.

# =================================================================
%prep
%autosetup -n %{name}-%{version}

%build
export CGO_ENABLED=0
export GOFLAGS="-trimpath -buildvcs=false"
%make_build VERSION=%{version} COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none) DATE=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ) build
%make_build VERSION=%{version} COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none) DATE=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ) build-testkit || true
# v1.1+ compat shims (pgBackRest + Barman drop-in binaries)
%make_build VERSION=%{version} COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none) DATE=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ) build-compat || true

%install
install -d %{buildroot}%{_bindir}
install -m 0755 bin/pg_hardstorage %{buildroot}%{_bindir}/pg_hardstorage

# v1.1+ compat shim binaries (subpackages: compat-pgbackrest,
# compat-barman, compat-walg).  The Conflicts: directives in
# %package keep operators from co-installing these alongside the
# upstream pgbackrest / barman / wal-g binaries; symlink activation
# under /usr/local/bin is documented in each subpackage's README.
install -m 0755 bin/pg-hardstorage-pgbackrest %{buildroot}%{_bindir}/pg-hardstorage-pgbackrest 2>/dev/null || true
install -m 0755 bin/pg-hardstorage-barman %{buildroot}%{_bindir}/pg-hardstorage-barman 2>/dev/null || true
install -m 0755 bin/pg-hardstorage-barman-wal-archive %{buildroot}%{_bindir}/pg-hardstorage-barman-wal-archive 2>/dev/null || true
install -m 0755 bin/pg-hardstorage-walg %{buildroot}%{_bindir}/pg-hardstorage-walg 2>/dev/null || true

# systemd units, sysusers, tmpfiles
install -d %{buildroot}%{_unitdir}
install -m 0644 deploy/systemd/pg_hardstorage.service %{buildroot}%{_unitdir}/pg_hardstorage.service
install -m 0644 deploy/systemd/pg_hardstorage@.service %{buildroot}%{_unitdir}/pg_hardstorage@.service

install -d %{buildroot}%{_sysusersdir}
install -m 0644 deploy/systemd/pg-hardstorage.sysusers.conf %{buildroot}%{_sysusersdir}/pg-hardstorage.conf

install -d %{buildroot}%{_tmpfilesdir}
install -m 0644 deploy/systemd/pg-hardstorage.tmpfiles.conf %{buildroot}%{_tmpfilesdir}/pg-hardstorage.conf

# Config + state skeleton
install -d -m 0755 %{buildroot}%{_sysconfdir}/pg_hardstorage
install -d -m 0750 %{buildroot}%{_sysconfdir}/pg_hardstorage/deployments
install -d -m 0755 %{buildroot}%{_sysconfdir}/pg_hardstorage/conf.d
install -d -m 0700 %{buildroot}%{_sysconfdir}/pg_hardstorage/keyring
install -d -m 0750 %{buildroot}%{_sharedstatedir}/pg_hardstorage
install -d -m 0750 %{buildroot}%{_sharedstatedir}/pg_hardstorage/inflight
install -d -m 0750 %{buildroot}%{_sharedstatedir}/pg_hardstorage/crashes
install -d -m 0750 %{buildroot}%{_localstatedir}/cache/pg_hardstorage
install -d -m 0750 %{buildroot}%{_localstatedir}/log/pg_hardstorage

# Shell completions
install -d %{buildroot}%{_datadir}/bash-completion/completions
install -m 0644 completions/bash/pg_hardstorage %{buildroot}%{_datadir}/bash-completion/completions/pg_hardstorage 2>/dev/null || true
install -d %{buildroot}%{_datadir}/zsh/site-functions
install -m 0644 completions/zsh/_pg_hardstorage %{buildroot}%{_datadir}/zsh/site-functions/_pg_hardstorage 2>/dev/null || true
install -d %{buildroot}%{_datadir}/fish/vendor_completions.d
install -m 0644 completions/fish/pg_hardstorage.fish %{buildroot}%{_datadir}/fish/vendor_completions.d/pg_hardstorage.fish 2>/dev/null || true

# Manpages
install -d %{buildroot}%{_mandir}/man1
install -m 0644 man/man1/*.1 %{buildroot}%{_mandir}/man1/ 2>/dev/null || true

# Common (noarch) data
# Doc paths track the v1.0 IA migration: runbooks moved
# from docs/runbooks/ → docs/reference/runbooks/, the
# OpenAPI spec is a single file at api/openapi.yaml (not a
# directory), and the design doc moved from docs/SPEC.md
# to repo-root SPEC.md.
install -d %{buildroot}%{_datadir}/pg_hardstorage/runbooks
install -d %{buildroot}%{_datadir}/pg_hardstorage/openapi
install -m 0644 docs/reference/runbooks/*.md %{buildroot}%{_datadir}/pg_hardstorage/runbooks/ 2>/dev/null || true
install -m 0644 api/openapi.yaml %{buildroot}%{_datadir}/pg_hardstorage/openapi/ 2>/dev/null || true

%check
# `make test` runs `go test -race`, which requires CGO.  Our default
# build is CGO_ENABLED=0 (pure Go), and the rpm-build images don't
# carry glibc-devel + gcc for a cgo-enabled stdlib.  Unit tests run
# in CI (which ships the cgo toolchain); the rpm package build's job
# is to produce the binary, not to gate on test results.  Skip.

# =================================================================
# Scriptlets
#
# %pre creates the pgbackup user/group ahead of file install so the
# %files attrs apply correctly. systemd-sysusers regenerates the
# account on first boot from the sysusers.d fragment, but we do it
# eagerly here for the dpkg-style "ready immediately" experience.

%pre
getent group pgbackup >/dev/null 2>&1 || groupadd -r -g %{pgbackup_gid} pgbackup
getent passwd pgbackup >/dev/null 2>&1 || \
    useradd -r -g pgbackup -u %{pgbackup_uid} \
            -d %{_sharedstatedir}/pg_hardstorage \
            -s /sbin/nologin \
            -c "pg_hardstorage" pgbackup
exit 0

%post
%systemd_post pg_hardstorage.service

%preun
%systemd_preun pg_hardstorage.service

%postun
%systemd_postun_with_restart pg_hardstorage.service

# =================================================================
%files
%license LICENSE
%doc README.md CHANGELOG.md
%{_bindir}/pg_hardstorage
%{_unitdir}/pg_hardstorage.service
%{_unitdir}/pg_hardstorage@.service
%{_sysusersdir}/pg-hardstorage.conf
%{_tmpfilesdir}/pg-hardstorage.conf
%{_datadir}/bash-completion/completions/pg_hardstorage
%{_datadir}/zsh/site-functions/_pg_hardstorage
%{_datadir}/fish/vendor_completions.d/pg_hardstorage.fish
%{_mandir}/man1/*.1*
%dir %attr(0755, root, root)     %{_sysconfdir}/pg_hardstorage
%dir %attr(0750, root, pgbackup) %{_sysconfdir}/pg_hardstorage/deployments
%dir %attr(0755, root, root)     %{_sysconfdir}/pg_hardstorage/conf.d
%dir %attr(0700, pgbackup, pgbackup) %{_sysconfdir}/pg_hardstorage/keyring
%dir %attr(0750, pgbackup, pgbackup) %{_sharedstatedir}/pg_hardstorage
%dir %attr(0750, pgbackup, pgbackup) %{_sharedstatedir}/pg_hardstorage/inflight
%dir %attr(0750, pgbackup, pgbackup) %{_sharedstatedir}/pg_hardstorage/crashes
%dir %attr(0750, pgbackup, pgbackup) %{_localstatedir}/cache/pg_hardstorage
%dir %attr(0750, pgbackup, pgbackup) %{_localstatedir}/log/pg_hardstorage

%files common
%license LICENSE
%dir %{_datadir}/pg_hardstorage
%{_datadir}/pg_hardstorage/runbooks
%{_datadir}/pg_hardstorage/openapi

%files server
%license LICENSE
%doc README.md

%files compat-pgbackrest
%license LICENSE
%{_bindir}/pg-hardstorage-pgbackrest

%files compat-barman
%license LICENSE
%{_bindir}/pg-hardstorage-barman
%{_bindir}/pg-hardstorage-barman-wal-archive

%files compat-walg
%license LICENSE
%{_bindir}/pg-hardstorage-walg

# =================================================================
%changelog
* Wed Apr 29 2026 Hans-Jürgen Schönig <hs@cybertec.at> - 0.1.1-1
- Initial RPM packaging for the v0.1.1 cheap-closure release.
- Three sub-packages: pg_hardstorage (agent + CLI),
  pg_hardstorage-common (shared data), pg_hardstorage-server
  (stub for the v0.5 control plane).
