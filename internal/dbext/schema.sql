-- pg_hardstorage extension v1.0
--
-- Creates the in-database surface for backup-state introspection
-- from inside the PostgreSQL cluster pg_hardstorage backs up.
-- The schema is populated by `pg_hardstorage db refresh
-- --pg-connection ...`, run by the agent on a schedule (or
-- manually by an operator).
--
-- Three operator-facing views land in this version:
--
--   pg_hardstorage.backups   — one row per committed backup
--   pg_hardstorage.health    — one row per deployment with
--                              the most-recent doctor verdict
--   pg_hardstorage.rpo       — one row per deployment with
--                              the SLO target + the live RPO/RTO
--
-- The underlying tables are write-restricted to the
-- pg_hardstorage_writer role; readers can SELECT freely.
--
-- We deliberately avoid extension-specific types (e.g. a
-- backup_id domain) — every column uses standard PG types so
-- consumers can `CREATE INDEX`, `CREATE MATERIALIZED VIEW`,
-- and `CREATE VIEW` against the data without depending on
-- extension-defined type machinery.

\echo Use "CREATE EXTENSION pg_hardstorage" to load this file. \quit

-- Roles -------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pg_hardstorage_writer') THEN
        CREATE ROLE pg_hardstorage_writer NOLOGIN;
    END IF;
END
$$;

-- Backup catalog ---------------------------------------------------

CREATE TABLE pg_hardstorage.backups_state (
    deployment             text        NOT NULL,
    backup_id              text        NOT NULL,
    type                   text        NOT NULL,
    parent_backup_id       text,
    pg_version             integer,
    started_at             timestamptz NOT NULL,
    stopped_at             timestamptz,
    physical_bytes         bigint,
    logical_bytes          bigint,
    dedup_ratio            numeric(8,3),
    verified               boolean     NOT NULL DEFAULT false,
    encrypted              boolean     NOT NULL DEFAULT false,
    timeline               integer,
    start_lsn              text,
    stop_lsn               text,
    last_refreshed_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (deployment, backup_id)
);
COMMENT ON TABLE pg_hardstorage.backups_state IS
    'Authoritative table populated by the agent via `pg_hardstorage db refresh`.';

CREATE INDEX backups_state_started_at_idx
    ON pg_hardstorage.backups_state (deployment, started_at DESC);

-- Health snapshot --------------------------------------------------

CREATE TABLE pg_hardstorage.health_state (
    deployment             text        PRIMARY KEY,
    healthy                boolean     NOT NULL,
    issues                 jsonb       NOT NULL DEFAULT '[]'::jsonb,
    pg_version             text,
    role                   text,
    last_backup_id         text,
    last_backup_at         timestamptz,
    wal_mode               text,
    wal_lag_seconds        bigint,
    wal_lag_bytes          bigint,
    last_refreshed_at      timestamptz NOT NULL DEFAULT now()
);

-- RPO / RTO snapshot -----------------------------------------------

CREATE TABLE pg_hardstorage.rpo_state (
    deployment             text        PRIMARY KEY,
    rpo_target_seconds     bigint,
    rpo_actual_seconds     bigint      NOT NULL,
    rto_estimate_seconds   bigint,
    classification         text,
    legal_hold             boolean     NOT NULL DEFAULT false,
    last_refreshed_at      timestamptz NOT NULL DEFAULT now()
);

-- Operator-facing views --------------------------------------------

CREATE OR REPLACE VIEW pg_hardstorage.backups AS
    SELECT
        deployment,
        backup_id,
        type,
        parent_backup_id,
        pg_version,
        started_at,
        stopped_at,
        physical_bytes,
        logical_bytes,
        dedup_ratio,
        verified,
        encrypted,
        timeline,
        start_lsn,
        stop_lsn,
        last_refreshed_at
    FROM pg_hardstorage.backups_state;
COMMENT ON VIEW pg_hardstorage.backups IS
    'One row per committed backup.  Refresh via `pg_hardstorage db refresh`.';

CREATE OR REPLACE VIEW pg_hardstorage.health AS
    SELECT
        deployment,
        healthy,
        issues,
        pg_version,
        role,
        last_backup_id,
        last_backup_at,
        wal_mode,
        wal_lag_seconds,
        wal_lag_bytes,
        last_refreshed_at
    FROM pg_hardstorage.health_state;
COMMENT ON VIEW pg_hardstorage.health IS
    'Per-deployment doctor verdict (healthy + issues + WAL lag).';

CREATE OR REPLACE VIEW pg_hardstorage.rpo AS
    SELECT
        deployment,
        rpo_target_seconds,
        rpo_actual_seconds,
        rto_estimate_seconds,
        classification,
        legal_hold,
        CASE
            WHEN rpo_target_seconds IS NULL THEN NULL
            WHEN rpo_actual_seconds <= rpo_target_seconds THEN 'meeting'
            ELSE 'breaching'
        END AS slo_status,
        last_refreshed_at
    FROM pg_hardstorage.rpo_state;
COMMENT ON VIEW pg_hardstorage.rpo IS
    'Per-deployment RPO target vs actual + RTO estimate + classification.';

-- Refresh helper ---------------------------------------------------

CREATE OR REPLACE FUNCTION pg_hardstorage.upsert_backup(
    p_deployment        text,
    p_backup_id         text,
    p_type              text,
    p_parent_backup_id  text,
    p_pg_version        integer,
    p_started_at        timestamptz,
    p_stopped_at        timestamptz,
    p_physical_bytes    bigint,
    p_logical_bytes     bigint,
    p_dedup_ratio       numeric,
    p_verified          boolean,
    p_encrypted         boolean,
    p_timeline          integer,
    p_start_lsn         text,
    p_stop_lsn          text
)
RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_hardstorage, pg_catalog
AS $$
    INSERT INTO pg_hardstorage.backups_state(
        deployment, backup_id, type, parent_backup_id, pg_version,
        started_at, stopped_at, physical_bytes, logical_bytes, dedup_ratio,
        verified, encrypted, timeline, start_lsn, stop_lsn, last_refreshed_at
    )
    VALUES (
        p_deployment, p_backup_id, p_type, p_parent_backup_id, p_pg_version,
        p_started_at, p_stopped_at, p_physical_bytes, p_logical_bytes, p_dedup_ratio,
        coalesce(p_verified, false), coalesce(p_encrypted, false),
        p_timeline, p_start_lsn, p_stop_lsn, now()
    )
    ON CONFLICT (deployment, backup_id) DO UPDATE SET
        type             = excluded.type,
        parent_backup_id = excluded.parent_backup_id,
        pg_version       = excluded.pg_version,
        started_at       = excluded.started_at,
        stopped_at       = excluded.stopped_at,
        physical_bytes   = excluded.physical_bytes,
        logical_bytes    = excluded.logical_bytes,
        dedup_ratio      = excluded.dedup_ratio,
        verified         = excluded.verified,
        encrypted        = excluded.encrypted,
        timeline         = excluded.timeline,
        start_lsn        = excluded.start_lsn,
        stop_lsn         = excluded.stop_lsn,
        last_refreshed_at = now();
$$;

CREATE OR REPLACE FUNCTION pg_hardstorage.upsert_health(
    p_deployment       text,
    p_healthy          boolean,
    p_issues           jsonb,
    p_pg_version       text,
    p_role             text,
    p_last_backup_id   text,
    p_last_backup_at   timestamptz,
    p_wal_mode         text,
    p_wal_lag_seconds  bigint,
    p_wal_lag_bytes    bigint
)
RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_hardstorage, pg_catalog
AS $$
    INSERT INTO pg_hardstorage.health_state(
        deployment, healthy, issues, pg_version, role,
        last_backup_id, last_backup_at, wal_mode, wal_lag_seconds, wal_lag_bytes,
        last_refreshed_at
    )
    VALUES (
        p_deployment, p_healthy, coalesce(p_issues, '[]'::jsonb), p_pg_version, p_role,
        p_last_backup_id, p_last_backup_at, p_wal_mode, p_wal_lag_seconds, p_wal_lag_bytes,
        now()
    )
    ON CONFLICT (deployment) DO UPDATE SET
        healthy           = excluded.healthy,
        issues            = excluded.issues,
        pg_version        = excluded.pg_version,
        role              = excluded.role,
        last_backup_id    = excluded.last_backup_id,
        last_backup_at    = excluded.last_backup_at,
        wal_mode          = excluded.wal_mode,
        wal_lag_seconds   = excluded.wal_lag_seconds,
        wal_lag_bytes     = excluded.wal_lag_bytes,
        last_refreshed_at = now();
$$;

CREATE OR REPLACE FUNCTION pg_hardstorage.upsert_rpo(
    p_deployment           text,
    p_rpo_target_seconds   bigint,
    p_rpo_actual_seconds   bigint,
    p_rto_estimate_seconds bigint,
    p_classification       text,
    p_legal_hold           boolean
)
RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_hardstorage, pg_catalog
AS $$
    INSERT INTO pg_hardstorage.rpo_state(
        deployment, rpo_target_seconds, rpo_actual_seconds,
        rto_estimate_seconds, classification, legal_hold, last_refreshed_at
    )
    VALUES (
        p_deployment, p_rpo_target_seconds, p_rpo_actual_seconds,
        p_rto_estimate_seconds, p_classification, coalesce(p_legal_hold, false), now()
    )
    ON CONFLICT (deployment) DO UPDATE SET
        rpo_target_seconds   = excluded.rpo_target_seconds,
        rpo_actual_seconds   = excluded.rpo_actual_seconds,
        rto_estimate_seconds = excluded.rto_estimate_seconds,
        classification       = excluded.classification,
        legal_hold           = excluded.legal_hold,
        last_refreshed_at    = now();
$$;

-- Permissions ------------------------------------------------------

REVOKE ALL ON ALL TABLES    IN SCHEMA pg_hardstorage FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA pg_hardstorage FROM PUBLIC;

GRANT USAGE ON SCHEMA pg_hardstorage TO PUBLIC;
GRANT SELECT ON pg_hardstorage.backups TO PUBLIC;
GRANT SELECT ON pg_hardstorage.health  TO PUBLIC;
GRANT SELECT ON pg_hardstorage.rpo     TO PUBLIC;

GRANT INSERT, UPDATE ON pg_hardstorage.backups_state TO pg_hardstorage_writer;
GRANT INSERT, UPDATE ON pg_hardstorage.health_state  TO pg_hardstorage_writer;
GRANT INSERT, UPDATE ON pg_hardstorage.rpo_state     TO pg_hardstorage_writer;

GRANT EXECUTE ON FUNCTION pg_hardstorage.upsert_backup(
    text, text, text, text, integer, timestamptz, timestamptz,
    bigint, bigint, numeric, boolean, boolean, integer, text, text
) TO pg_hardstorage_writer;
GRANT EXECUTE ON FUNCTION pg_hardstorage.upsert_health(
    text, boolean, jsonb, text, text, text, timestamptz, text, bigint, bigint
) TO pg_hardstorage_writer;
GRANT EXECUTE ON FUNCTION pg_hardstorage.upsert_rpo(
    text, bigint, bigint, bigint, text, boolean
) TO pg_hardstorage_writer;
