from __future__ import annotations

import psycopg

SCHEMA_SQL = r"""
CREATE TABLE IF NOT EXISTS sync_instance (
    id smallint PRIMARY KEY CHECK (id = 1),
    site text NOT NULL UNIQUE,
    priority integer NOT NULL
);

CREATE TABLE IF NOT EXISTS members (
    member_id bigint PRIMARY KEY,
    points bigint NOT NULL DEFAULT 0,
    row_version bigint NOT NULL DEFAULT 1 CHECK (row_version > 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS orders (
    order_id bigint PRIMARY KEY,
    status text NOT NULL DEFAULT 'pending_shipment',
    row_version bigint NOT NULL DEFAULT 1 CHECK (row_version > 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS sync_change (
    change_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    origin_site text NOT NULL,
    table_name text NOT NULL CHECK (table_name IN ('members', 'orders')),
    row_id bigint NOT NULL,
    row_version bigint NOT NULL,
    op text NOT NULL CHECK (op IN ('insert', 'update')),
    old_row jsonb,
    new_row jsonb NOT NULL,
    points_delta bigint,
    old_points bigint,
    new_points bigint,
    old_status text,
    new_status text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS sync_change_stream_idx
    ON sync_change (origin_site, change_id);
CREATE INDEX IF NOT EXISTS sync_change_row_idx
    ON sync_change (table_name, row_id, change_id DESC);

CREATE TABLE IF NOT EXISTS sync_row_state (
    table_name text NOT NULL,
    row_id bigint NOT NULL,
    row_version bigint NOT NULL,
    points_version bigint,
    points_site text,
    last_points_change_key text,
    status_version bigint,
    status_site text,
    last_status_change_key text,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (table_name, row_id)
);

CREATE TABLE IF NOT EXISTS sync_applied (
    change_key text PRIMARY KEY,
    origin_site text NOT NULL,
    source_change_id bigint NOT NULL,
    target_site text NOT NULL,
    table_name text NOT NULL,
    row_id bigint NOT NULL,
    row_version bigint NOT NULL,
    claimed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS sync_apply_events (
    event_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    change_key text NOT NULL UNIQUE,
    origin_site text NOT NULL,
    source_change_id bigint NOT NULL,
    target_site text NOT NULL,
    table_name text NOT NULL,
    row_id bigint NOT NULL,
    row_version bigint NOT NULL,
    outcome text NOT NULL,
    stale_fields jsonb NOT NULL DEFAULT '[]'::jsonb,
    field_decisions jsonb NOT NULL DEFAULT '{}'::jsonb,
    old_row jsonb,
    new_row jsonb,
    points_delta bigint,
    points_merged boolean NOT NULL DEFAULT false,
    status_overridden boolean NOT NULL DEFAULT false,
    applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS sync_apply_events_row_idx
    ON sync_apply_events (table_name, row_id, event_id DESC);

CREATE OR REPLACE FUNCTION sync_capture_change()
RETURNS trigger
LANGUAGE plpgsql
AS $fn$
DECLARE
    v_site text;
    v_change_id bigint;
    v_change_key text;
    v_old_points bigint;
    v_new_points bigint;
    v_old_status text;
    v_new_status text;
    v_points_changed boolean := false;
    v_status_changed boolean := false;
    v_op text;
BEGIN
    IF current_setting('app.sync_apply', true) = 'on' THEN
        RETURN NEW;
    END IF;

    SELECT site INTO v_site FROM sync_instance WHERE id = 1;
    IF v_site IS NULL THEN
        RAISE EXCEPTION 'sync_instance is not configured';
    END IF;

    IF TG_TABLE_NAME = 'members' THEN
        v_new_points := NEW.points;
        IF TG_OP = 'INSERT' THEN
            NEW.row_version := GREATEST(COALESCE(NEW.row_version, 1), 1);
            NEW.updated_at := clock_timestamp();
            v_op := 'insert';
            v_points_changed := true;
        ELSE
            IF NEW.row_version IS NULL OR NEW.row_version <= OLD.row_version THEN
                NEW.row_version := OLD.row_version + 1;
            END IF;
            NEW.updated_at := clock_timestamp();
            v_op := 'update';
            v_old_points := OLD.points;
            v_points_changed := true;
        END IF;
    ELSIF TG_TABLE_NAME = 'orders' THEN
        v_new_status := NEW.status;
        IF TG_OP = 'INSERT' THEN
            NEW.row_version := GREATEST(COALESCE(NEW.row_version, 1), 1);
            NEW.updated_at := clock_timestamp();
            v_op := 'insert';
            v_status_changed := true;
        ELSE
            IF NEW.row_version IS NULL OR NEW.row_version <= OLD.row_version THEN
                NEW.row_version := OLD.row_version + 1;
            END IF;
            NEW.updated_at := clock_timestamp();
            v_op := 'update';
            v_old_status := OLD.status;
            v_status_changed := true;
        END IF;
    ELSE
        RAISE EXCEPTION 'unsupported table %', TG_TABLE_NAME;
    END IF;

    INSERT INTO sync_change (
        origin_site,
        table_name,
        row_id,
        row_version,
        op,
        old_row,
        new_row,
        points_delta,
        old_points,
        new_points,
        old_status,
        new_status
    )
    VALUES (
        v_site,
        TG_TABLE_NAME,
        COALESCE(
            (to_jsonb(NEW) ->> 'member_id')::bigint,
            (to_jsonb(NEW) ->> 'order_id')::bigint
        ),
        NEW.row_version,
        v_op,
        CASE WHEN TG_OP = 'UPDATE' THEN to_jsonb(OLD) ELSE NULL END,
        to_jsonb(NEW),
        CASE WHEN TG_TABLE_NAME = 'members' THEN v_new_points - COALESCE(v_old_points, 0) ELSE NULL END,
        v_old_points,
        v_new_points,
        v_old_status,
        v_new_status
    )
    RETURNING change_id INTO v_change_id;

    v_change_key := v_site || ':' || v_change_id::text;

    INSERT INTO sync_row_state (
        table_name,
        row_id,
        row_version,
        points_version,
        points_site,
        last_points_change_key,
        status_version,
        status_site,
        last_status_change_key
    )
    VALUES (
        TG_TABLE_NAME,
        COALESCE(
            (to_jsonb(NEW) ->> 'member_id')::bigint,
            (to_jsonb(NEW) ->> 'order_id')::bigint
        ),
        NEW.row_version,
        CASE WHEN TG_TABLE_NAME = 'members' THEN NEW.row_version ELSE NULL END,
        CASE WHEN TG_TABLE_NAME = 'members' THEN v_site ELSE NULL END,
        CASE WHEN TG_TABLE_NAME = 'members' THEN v_change_key ELSE NULL END,
        CASE WHEN TG_TABLE_NAME = 'orders' THEN NEW.row_version ELSE NULL END,
        CASE WHEN TG_TABLE_NAME = 'orders' THEN v_site ELSE NULL END,
        CASE WHEN TG_TABLE_NAME = 'orders' THEN v_change_key ELSE NULL END
    )
    ON CONFLICT (table_name, row_id) DO UPDATE
    SET row_version = GREATEST(sync_row_state.row_version, EXCLUDED.row_version),
        points_version = CASE
            WHEN v_points_changed THEN EXCLUDED.points_version
            ELSE sync_row_state.points_version
        END,
        points_site = CASE
            WHEN v_points_changed THEN EXCLUDED.points_site
            ELSE sync_row_state.points_site
        END,
        last_points_change_key = CASE
            WHEN v_points_changed THEN EXCLUDED.last_points_change_key
            ELSE sync_row_state.last_points_change_key
        END,
        status_version = CASE
            WHEN v_status_changed THEN EXCLUDED.status_version
            ELSE sync_row_state.status_version
        END,
        status_site = CASE
            WHEN v_status_changed THEN EXCLUDED.status_site
            ELSE sync_row_state.status_site
        END,
        last_status_change_key = CASE
            WHEN v_status_changed THEN EXCLUDED.last_status_change_key
            ELSE sync_row_state.last_status_change_key
        END,
        updated_at = clock_timestamp();

    RETURN NEW;
END;
$fn$;

DROP TRIGGER IF EXISTS members_capture_change ON members;
CREATE TRIGGER members_capture_change
    BEFORE INSERT OR UPDATE ON members
    FOR EACH ROW EXECUTE FUNCTION sync_capture_change();

DROP TRIGGER IF EXISTS orders_capture_change ON orders;
CREATE TRIGGER orders_capture_change
    BEFORE INSERT OR UPDATE ON orders
    FOR EACH ROW EXECUTE FUNCTION sync_capture_change();
"""


def ensure_schema(conn: psycopg.Connection, site: str, priority: int) -> None:
    with conn.transaction():
        conn.execute(SCHEMA_SQL)
        conn.execute(
            """
            INSERT INTO sync_instance (id, site, priority)
            VALUES (1, %s, %s)
            ON CONFLICT (id) DO UPDATE
            SET site = EXCLUDED.site, priority = EXCLUDED.priority
            """,
            (site, priority),
        )


def seed_rows(conn: psycopg.Connection, member_count: int, order_count: int) -> None:
    with conn.transaction():
        conn.execute("SELECT set_config('app.sync_apply', 'on', true)")
        conn.execute(
            """
            INSERT INTO members (member_id, points, row_version, updated_at)
            SELECT value,
                   ((value * 7919) %% 100000)::bigint,
                   1,
                   '2026-01-01 00:00:00+00'::timestamptz
            FROM generate_series(1, %s) AS value
            ON CONFLICT (member_id) DO NOTHING
            """,
            (member_count,),
        )
        conn.execute(
            """
            INSERT INTO orders (order_id, status, row_version, updated_at)
            SELECT value,
                   CASE WHEN value %% 2 = 0 THEN 'completed' ELSE 'pending_shipment' END,
                   1,
                   '2026-01-01 00:00:00+00'::timestamptz
            FROM generate_series(1, %s) AS value
            ON CONFLICT (order_id) DO NOTHING
            """,
            (order_count,),
        )
        conn.execute("DELETE FROM sync_row_state")
        conn.execute(
            """
            INSERT INTO sync_row_state (
                table_name, row_id, row_version,
                points_version, points_site, last_points_change_key,
                status_version, status_site, last_status_change_key
            )
            SELECT 'members', member_id, row_version,
                   row_version, 'east', NULL, NULL, NULL, NULL
            FROM members
            WHERE NOT EXISTS (
                SELECT 1 FROM sync_row_state s
                WHERE s.table_name = 'members' AND s.row_id = members.member_id
            )
            """
        )
        conn.execute(
            """
            INSERT INTO sync_row_state (
                table_name, row_id, row_version,
                points_version, points_site, last_points_change_key,
                status_version, status_site, last_status_change_key
            )
            SELECT 'orders', order_id, row_version,
                   NULL, NULL, NULL, row_version, 'east', NULL
            FROM orders
            WHERE NOT EXISTS (
                SELECT 1 FROM sync_row_state s
                WHERE s.table_name = 'orders' AND s.row_id = orders.order_id
            )
            """
        )
        conn.execute(
            """
            UPDATE sync_row_state s
            SET points_version = m.row_version,
                points_site = (SELECT site FROM sync_instance WHERE id = 1)
            FROM members m
            WHERE s.table_name = 'members'
              AND s.row_id = m.member_id
              AND s.last_points_change_key IS NULL
            """
        )
        conn.execute(
            """
            UPDATE sync_row_state s
            SET status_version = o.row_version,
                status_site = (SELECT site FROM sync_instance WHERE id = 1)
            FROM orders o
            WHERE s.table_name = 'orders'
              AND s.row_id = o.order_id
              AND s.last_status_change_key IS NULL
            """
        )


def reset_data(conn: psycopg.Connection) -> None:
    with conn.transaction():
        conn.execute("SELECT set_config('app.sync_apply', 'on', true)")
        conn.execute("TRUNCATE members, orders, sync_change, sync_row_state, sync_applied, sync_apply_events RESTART IDENTITY")
