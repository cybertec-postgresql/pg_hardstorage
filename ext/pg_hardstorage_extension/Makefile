# pg_hardstorage extension Makefile.
#
# Builds + installs the in-database SQL extension via pgxs.
# Usage:
#
#   make
#   sudo make install
#   psql -c 'CREATE EXTENSION pg_hardstorage'
#
# pgxs lives under `pg_config --pgxs`; this Makefile only sets
# the metadata it expects.  The extension is pure-SQL, so no
# C build step is involved — `make` is a no-op, `make install`
# copies the .control + .sql + README into PG's extension
# directory.

EXTENSION       = pg_hardstorage
EXTVERSION      = 1.0
DATA            = pg_hardstorage--$(EXTVERSION).sql
DOCS            = README.md

PG_CONFIG       ?= pg_config
PGXS            := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
