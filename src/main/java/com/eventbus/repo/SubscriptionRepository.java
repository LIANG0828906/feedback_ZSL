package com.eventbus.repo;

import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Timestamp;
import java.time.Instant;
import java.util.Collection;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;

import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.jdbc.core.RowMapper;
import org.springframework.stereotype.Repository;

import com.eventbus.domain.RuleDefinition;
import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;

@Repository
public class SubscriptionRepository {

    private final JdbcTemplate jdbc;
    private final ObjectMapper objectMapper;

    public SubscriptionRepository(JdbcTemplate jdbc, ObjectMapper objectMapper) {
        this.jdbc = jdbc;
        this.objectMapper = objectMapper;
    }

    public long nextRuleVersion() {
        Long version = jdbc.queryForObject("SELECT nextval('subscription_rule_version_seq')", Long.class);
        if (version == null) {
            throw new IllegalStateException("rule version sequence returned null");
        }
        return version;
    }

    public Optional<GroupRow> findGroup(String name) {
        return jdbc.query("""
                SELECT group_name, current_rule_version, created_at, updated_at
                FROM subscription_group
                WHERE group_name = ?
                """, groupMapper(), name).stream().findFirst();
    }

    public List<GroupRow> findAllGroups() {
        return jdbc.query("""
                SELECT group_name, current_rule_version, created_at, updated_at
                FROM subscription_group
                ORDER BY group_name
                """, groupMapper());
    }

    public Optional<GroupRow> lockGroup(String name) {
        return jdbc.query("""
                SELECT group_name, current_rule_version, created_at, updated_at
                FROM subscription_group
                WHERE group_name = ?
                FOR UPDATE
                """, groupMapper(), name).stream().findFirst();
    }

    public void insertGroup(String name, long version) {
        jdbc.update("""
                INSERT INTO subscription_group(group_name, current_rule_version)
                VALUES (?, ?)
                """, name, version);
    }

    public void updateGroupVersion(String name, long version) {
        jdbc.update("""
                UPDATE subscription_group
                SET current_rule_version = ?, updated_at = NOW()
                WHERE group_name = ?
                """, version, name);
    }

    public void insertRule(String group, long version, RuleDefinition rule) {
        jdbc.update("""
                INSERT INTO subscription_rule(group_name, version, definition)
                VALUES (?, ?, ?::jsonb)
                """, group, version, toJson(rule));
    }

    public List<RuleRow> findRules(String group) {
        return jdbc.query("""
                SELECT version, definition::text AS definition_json, created_at
                FROM subscription_rule
                WHERE group_name = ?
                ORDER BY version
                """, ruleMapper(), group);
    }

    public Optional<RuleRow> findRule(String group, long version) {
        return jdbc.query("""
                SELECT version, definition::text AS definition_json, created_at
                FROM subscription_rule
                WHERE group_name = ? AND version = ?
                """, ruleMapper(), group, version).stream().findFirst();
    }

    public void insertSwitch(String group, int partition, long oldVersion,
                             long newVersion, long boundaryOffset) {
        jdbc.update("""
                INSERT INTO rule_switch(
                    group_name, partition_no, old_version, new_version, boundary_offset
                ) VALUES (?, ?, ?, ?, ?)
                """, group, partition, oldVersion, newVersion, boundaryOffset);
    }

    public List<SwitchRow> findSwitches(String group) {
        return jdbc.query("""
                SELECT partition_no, old_version, new_version, boundary_offset, switched_at
                FROM rule_switch
                WHERE group_name = ?
                ORDER BY partition_no, new_version
                """, switchMapper(), group);
    }

    public void insertProgress(String group, int partition) {
        jdbc.update("""
                INSERT INTO partition_progress(group_name, partition_no)
                VALUES (?, ?)
                ON CONFLICT (group_name, partition_no) DO NOTHING
                """, group, partition);
    }

    public Optional<ProgressRow> findProgress(String group, int partition) {
        return jdbc.query("""
                SELECT partition_no, read_offset, confirmed_offset, last_pull_at, updated_at
                FROM partition_progress
                WHERE group_name = ? AND partition_no = ?
                """, progressMapper(), group, partition).stream().findFirst();
    }

    public List<ProgressRow> findProgress(String group) {
        return jdbc.query("""
                SELECT partition_no, read_offset, confirmed_offset, last_pull_at, updated_at
                FROM partition_progress
                WHERE group_name = ?
                ORDER BY partition_no
                """, progressMapper(), group);
    }

    public void updateAfterPull(String group, int partition, long readOffset, Instant pulledAt) {
        jdbc.update("""
                INSERT INTO partition_progress(
                    group_name, partition_no, read_offset, last_pull_at, updated_at
                ) VALUES (?, ?, ?, ?, NOW())
                ON CONFLICT (group_name, partition_no) DO UPDATE SET
                    read_offset = GREATEST(partition_progress.read_offset, EXCLUDED.read_offset),
                    last_pull_at = EXCLUDED.last_pull_at,
                    updated_at = NOW()
                """, group, partition, readOffset, Timestamp.from(pulledAt));
    }

    public void advanceConfirmed(String group, int partition, long offset) {
        jdbc.update("""
                UPDATE partition_progress
                SET confirmed_offset = GREATEST(confirmed_offset, ?), updated_at = NOW()
                WHERE group_name = ? AND partition_no = ?
                """, offset, group, partition);
    }

    public Optional<DeliveryRow> findFirstUnconfirmed(String group, int partition) {
        return jdbc.query("""
                SELECT partition_no, event_offset, event_id, event_type, rule_version,
                       event_json::text AS event_json, first_delivered_at, last_delivered_at,
                       delivery_count, confirmed_at
                FROM event_delivery
                WHERE group_name = ? AND partition_no = ? AND confirmed_at IS NULL
                ORDER BY event_offset
                LIMIT 1
                """, deliveryMapper(), group, partition).stream().findFirst();
    }

    public Optional<DeliveryRow> findDelivery(String group, int partition, long offset) {
        return jdbc.query("""
                SELECT partition_no, event_offset, event_id, event_type, rule_version,
                       event_json::text AS event_json, first_delivered_at, last_delivered_at,
                       delivery_count, confirmed_at
                FROM event_delivery
                WHERE group_name = ? AND partition_no = ? AND event_offset = ?
                """, deliveryMapper(), group, partition, offset).stream().findFirst();
    }

    public List<DeliveryRow> findDeliveriesByEventId(String group, String eventId) {
        return jdbc.query("""
                SELECT partition_no, event_offset, event_id, event_type, rule_version,
                       event_json::text AS event_json, first_delivered_at, last_delivered_at,
                       delivery_count, confirmed_at
                FROM event_delivery
                WHERE group_name = ? AND event_id = ?
                ORDER BY partition_no, event_offset
                """, deliveryMapper(), group, eventId);
    }

    public Map<Long, DeliveryRow> findDeliveries(String group, int partition,
                                                 long fromOffset, long toOffsetInclusive) {
        Map<Long, DeliveryRow> result = new HashMap<>();
        jdbc.query("""
                SELECT partition_no, event_offset, event_id, event_type, rule_version,
                       event_json::text AS event_json, first_delivered_at, last_delivered_at,
                       delivery_count, confirmed_at
                FROM event_delivery
                WHERE group_name = ? AND partition_no = ?
                  AND event_offset BETWEEN ? AND ?
                """, rs -> {
                    DeliveryRow row = deliveryMapper().mapRow(rs, 0);
                    result.put(row.offset(), row);
                }, group, partition, fromOffset, toOffsetInclusive);
        return result;
    }

    public void insertDelivery(String group, DeliveryRow row) {
        jdbc.update("""
                INSERT INTO event_delivery(
                    group_name, partition_no, event_offset, event_id, event_type,
                    rule_version, event_json, first_delivered_at, last_delivered_at,
                    delivery_count, confirmed_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?, ?, NULL)
                ON CONFLICT (group_name, partition_no, event_offset) DO NOTHING
                """, group, row.partition(), row.offset(), row.eventId(), row.eventType(),
                row.ruleVersion(), toJson(row.json()), Timestamp.from(row.firstDeliveredAt()),
                Timestamp.from(row.lastDeliveredAt()), row.deliveryCount());
    }

    public int markRedelivered(String group, int partition, long offset, Instant deliveredAt) {
        return jdbc.update("""
                UPDATE event_delivery
                SET delivery_count = delivery_count + 1, last_delivered_at = ?
                WHERE group_name = ? AND partition_no = ? AND event_offset = ?
                  AND confirmed_at IS NULL
                """, Timestamp.from(deliveredAt), group, partition, offset);
    }

    public int markConfirmed(String group, int partition, long offset, Instant confirmedAt) {
        return jdbc.update("""
                UPDATE event_delivery
                SET confirmed_at = ?
                WHERE group_name = ? AND partition_no = ? AND event_offset = ?
                  AND confirmed_at IS NULL
                """, Timestamp.from(confirmedAt), group, partition, offset);
    }

    public long duplicateDeliveryCount(String group) {
        Long value = jdbc.queryForObject("""
                SELECT COALESCE(SUM(GREATEST(delivery_count - 1, 0)), 0)
                FROM event_delivery
                WHERE group_name = ?
                """, Long.class, group);
        return value == null ? 0 : value;
    }

    public long duplicateDeliveryCount(String group, int partition) {
        Long value = jdbc.queryForObject("""
                SELECT COALESCE(SUM(GREATEST(delivery_count - 1, 0)), 0)
                FROM event_delivery
                WHERE group_name = ? AND partition_no = ?
                """, Long.class, group, partition);
        return value == null ? 0 : value;
    }

    public void upsertDecisions(String group, Collection<DecisionRow> decisions) {
        if (decisions.isEmpty()) {
            return;
        }
        jdbc.batchUpdate("""
                INSERT INTO event_decision(
                    group_name, partition_no, event_offset, event_id, event_type,
                    rule_version, matched, reason
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT (group_name, partition_no, event_offset) DO UPDATE SET
                    event_id = EXCLUDED.event_id,
                    event_type = EXCLUDED.event_type,
                    rule_version = EXCLUDED.rule_version,
                    matched = EXCLUDED.matched,
                    reason = EXCLUDED.reason
                """, decisions, 200, (ps, decision) -> {
            ps.setString(1, group);
            ps.setInt(2, decision.partition());
            ps.setLong(3, decision.offset());
            ps.setString(4, decision.eventId());
            ps.setString(5, decision.eventType());
            ps.setLong(6, decision.ruleVersion());
            ps.setBoolean(7, decision.matched());
            ps.setString(8, decision.reason());
        });
    }

    public Optional<DecisionRow> findDecision(String group, int partition, long offset) {
        return jdbc.query("""
                SELECT partition_no, event_offset, event_id, event_type,
                       rule_version, matched, reason, decided_at
                FROM event_decision
                WHERE group_name = ? AND partition_no = ? AND event_offset = ?
                """, (rs, rowNum) -> new DecisionRow(
                rs.getInt("partition_no"),
                rs.getLong("event_offset"),
                rs.getString("event_id"),
                rs.getString("event_type"),
                rs.getLong("rule_version"),
                rs.getBoolean("matched"),
                rs.getString("reason"),
                instant(rs, "decided_at")), group, partition, offset).stream().findFirst();
    }

    public void bumpMetrics(String group, int partition, long pulls, long hits,
                            long excluded, long redeliveries) {
        jdbc.update("""
                INSERT INTO delivery_metric(
                    group_name, partition_no, pull_count, hit_count,
                    excluded_count, redelivery_count, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, NOW())
                ON CONFLICT (group_name, partition_no) DO UPDATE SET
                    pull_count = delivery_metric.pull_count + EXCLUDED.pull_count,
                    hit_count = delivery_metric.hit_count + EXCLUDED.hit_count,
                    excluded_count = delivery_metric.excluded_count + EXCLUDED.excluded_count,
                    redelivery_count = delivery_metric.redelivery_count + EXCLUDED.redelivery_count,
                    updated_at = NOW()
                """, group, partition, pulls, hits, excluded, redeliveries);
    }

    public List<MetricRow> findMetrics(String group) {
        return jdbc.query("""
                SELECT group_name, partition_no, pull_count, hit_count,
                       excluded_count, redelivery_count
                FROM delivery_metric
                WHERE group_name = ?
                ORDER BY partition_no
                """, metricMapper(), group);
    }

    public List<MetricRow> findAllMetrics() {
        return jdbc.query("""
                SELECT group_name, partition_no, pull_count, hit_count,
                       excluded_count, redelivery_count
                FROM delivery_metric
                ORDER BY group_name, partition_no
                """, metricMapper());
    }

    private String toJson(Object value) {
        try {
            return objectMapper.writeValueAsString(value);
        } catch (JsonProcessingException ex) {
            throw new IllegalArgumentException("cannot serialize JSON", ex);
        }
    }

    private RuleDefinition fromJson(String json) {
        try {
            return objectMapper.readValue(json, RuleDefinition.class);
        } catch (JsonProcessingException ex) {
            throw new IllegalStateException("stored rule JSON is invalid", ex);
        }
    }

    private JsonNode readJson(String json) {
        try {
            return objectMapper.readTree(json);
        } catch (JsonProcessingException ex) {
            throw new IllegalStateException("stored event JSON is invalid", ex);
        }
    }

    private RowMapper<GroupRow> groupMapper() {
        return (rs, rowNum) -> new GroupRow(
                rs.getString("group_name"),
                rs.getLong("current_rule_version"),
                instant(rs, "created_at"),
                instant(rs, "updated_at"));
    }

    private RowMapper<RuleRow> ruleMapper() {
        return (rs, rowNum) -> new RuleRow(
                rs.getLong("version"),
                fromJson(rs.getString("definition_json")),
                instant(rs, "created_at"));
    }

    private RowMapper<SwitchRow> switchMapper() {
        return (rs, rowNum) -> new SwitchRow(
                rs.getInt("partition_no"),
                rs.getLong("old_version"),
                rs.getLong("new_version"),
                rs.getLong("boundary_offset"),
                instant(rs, "switched_at"));
    }

    private RowMapper<ProgressRow> progressMapper() {
        return (rs, rowNum) -> new ProgressRow(
                rs.getInt("partition_no"),
                rs.getLong("read_offset"),
                rs.getLong("confirmed_offset"),
                nullableInstant(rs, "last_pull_at"),
                instant(rs, "updated_at"));
    }

    private RowMapper<DeliveryRow> deliveryMapper() {
        return (rs, rowNum) -> new DeliveryRow(
                rs.getInt("partition_no"),
                rs.getLong("event_offset"),
                rs.getString("event_id"),
                rs.getString("event_type"),
                rs.getLong("rule_version"),
                readJson(rs.getString("event_json")),
                instant(rs, "first_delivered_at"),
                instant(rs, "last_delivered_at"),
                rs.getInt("delivery_count"),
                nullableInstant(rs, "confirmed_at"));
    }

    private RowMapper<MetricRow> metricMapper() {
        return (rs, rowNum) -> new MetricRow(
                rs.getString("group_name"),
                rs.getInt("partition_no"),
                rs.getLong("pull_count"),
                rs.getLong("hit_count"),
                rs.getLong("excluded_count"),
                rs.getLong("redelivery_count"));
    }

    private static Instant instant(ResultSet rs, String column) throws SQLException {
        Timestamp value = rs.getTimestamp(column);
        return value == null ? null : value.toInstant();
    }

    private static Instant nullableInstant(ResultSet rs, String column) throws SQLException {
        return instant(rs, column);
    }
}
