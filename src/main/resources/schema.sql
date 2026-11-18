CREATE SEQUENCE IF NOT EXISTS subscription_rule_version_seq START WITH 1;

CREATE TABLE IF NOT EXISTS subscription_group (
    group_name VARCHAR(128) PRIMARY KEY,
    current_rule_version BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS subscription_rule (
    group_name VARCHAR(128) NOT NULL REFERENCES subscription_group(group_name) ON DELETE CASCADE,
    version BIGINT NOT NULL,
    definition JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_name, version)
);

CREATE TABLE IF NOT EXISTS rule_switch (
    group_name VARCHAR(128) NOT NULL REFERENCES subscription_group(group_name) ON DELETE CASCADE,
    partition_no INTEGER NOT NULL,
    old_version BIGINT NOT NULL,
    new_version BIGINT NOT NULL,
    boundary_offset BIGINT NOT NULL,
    switched_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_name, new_version, partition_no)
);

CREATE INDEX IF NOT EXISTS idx_rule_switch_lookup
    ON rule_switch(group_name, partition_no, boundary_offset DESC, new_version DESC);

CREATE TABLE IF NOT EXISTS partition_progress (
    group_name VARCHAR(128) NOT NULL REFERENCES subscription_group(group_name) ON DELETE CASCADE,
    partition_no INTEGER NOT NULL,
    read_offset BIGINT NOT NULL DEFAULT -1,
    confirmed_offset BIGINT NOT NULL DEFAULT -1,
    last_pull_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_name, partition_no)
);

CREATE TABLE IF NOT EXISTS event_delivery (
    group_name VARCHAR(128) NOT NULL,
    partition_no INTEGER NOT NULL,
    event_offset BIGINT NOT NULL,
    event_id VARCHAR(192) NOT NULL,
    event_type VARCHAR(128) NOT NULL,
    rule_version BIGINT NOT NULL,
    event_json JSONB NOT NULL,
    first_delivered_at TIMESTAMPTZ NOT NULL,
    last_delivered_at TIMESTAMPTZ NOT NULL,
    delivery_count INTEGER NOT NULL DEFAULT 1,
    confirmed_at TIMESTAMPTZ,
    PRIMARY KEY (group_name, partition_no, event_offset)
);

CREATE INDEX IF NOT EXISTS idx_event_delivery_pending
    ON event_delivery(group_name, partition_no, confirmed_at, event_offset);
CREATE INDEX IF NOT EXISTS idx_event_delivery_event_id
    ON event_delivery(group_name, event_id);

CREATE TABLE IF NOT EXISTS event_decision (
    group_name VARCHAR(128) NOT NULL,
    partition_no INTEGER NOT NULL,
    event_offset BIGINT NOT NULL,
    event_id VARCHAR(192),
    event_type VARCHAR(128),
    rule_version BIGINT NOT NULL,
    matched BOOLEAN NOT NULL,
    reason VARCHAR(64) NOT NULL,
    decided_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_name, partition_no, event_offset)
);

CREATE TABLE IF NOT EXISTS delivery_metric (
    group_name VARCHAR(128) NOT NULL,
    partition_no INTEGER NOT NULL,
    pull_count BIGINT NOT NULL DEFAULT 0,
    hit_count BIGINT NOT NULL DEFAULT 0,
    excluded_count BIGINT NOT NULL DEFAULT 0,
    redelivery_count BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_name, partition_no)
);
