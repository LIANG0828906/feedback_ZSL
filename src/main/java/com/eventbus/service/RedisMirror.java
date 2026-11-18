package com.eventbus.service;

import java.time.Instant;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicLong;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.data.redis.core.StringRedisTemplate;
import org.springframework.stereotype.Component;

import com.eventbus.domain.RuleDefinition;
import com.eventbus.repo.GroupRow;
import com.eventbus.repo.ProgressRow;
import com.fasterxml.jackson.core.JsonProcessingException;
import com.fasterxml.jackson.databind.ObjectMapper;

@Component
public class RedisMirror {

    private static final Logger log = LoggerFactory.getLogger(RedisMirror.class);
    private static final long WARN_INTERVAL_MILLIS = 60_000L;

    private final StringRedisTemplate redis;
    private final ObjectMapper objectMapper;
    private final AtomicLong lastWarning = new AtomicLong(0);
    private final AtomicLong unavailableUntil = new AtomicLong(0);

    public RedisMirror(StringRedisTemplate redis, ObjectMapper objectMapper) {
        this.redis = redis;
        this.objectMapper = objectMapper;
    }

    public void mirrorSubscription(GroupRow group, RuleDefinition rule, List<ProgressRow> progress) {
        if (System.currentTimeMillis() < unavailableUntil.get()) {
            return;
        }
        try {
            String metaKey = metaKey(group.group());
            redis.opsForHash().putAll(metaKey, Map.of(
                    "currentRuleVersion", Long.toString(group.currentRuleVersion()),
                    "updatedAt", group.updatedAt().toString(),
                    "rule", toJson(rule),
                    "readOffsetUnit", "inclusive"));
            for (ProgressRow row : progress) {
                putProgress(group.group(), row.partition(), row.readOffset(), row.confirmedOffset());
            }
        } catch (RuntimeException ex) {
            warnOnce("Redis metadata mirror failed; PostgreSQL remains authoritative", ex);
        }
    }

    public void updateProgress(String group, int partition, long readOffset, long confirmedOffset) {
        if (System.currentTimeMillis() < unavailableUntil.get()) {
            return;
        }
        try {
            putProgress(group, partition, readOffset, confirmedOffset);
        } catch (RuntimeException ex) {
            warnOnce("Redis progress mirror failed; PostgreSQL remains authoritative", ex);
        }
    }

    private void putProgress(String group, int partition, long readOffset, long confirmedOffset) {
        String progressKey = progressKey(group);
        String prefix = "p" + partition + ":";
        Map<String, String> values = new LinkedHashMap<>();
        values.put(prefix + "read", Long.toString(readOffset));
        values.put(prefix + "confirmed", Long.toString(confirmedOffset));
        values.put("updatedAt", Instant.now().toString());
        redis.opsForHash().putAll(progressKey, values);
    }

    private String toJson(Object value) {
        try {
            return objectMapper.writeValueAsString(value);
        } catch (JsonProcessingException ex) {
            throw new IllegalStateException("cannot serialize data for Redis", ex);
        }
    }

    private void warnOnce(String message, RuntimeException ex) {
        long now = System.currentTimeMillis();
        long previous = lastWarning.get();
        if (now - previous >= WARN_INTERVAL_MILLIS && lastWarning.compareAndSet(previous, now)) {
            log.warn("{}: {}", message, ex.getMessage());
        }
        unavailableUntil.set(now + WARN_INTERVAL_MILLIS);
    }

    private static String metaKey(String group) {
        return "eventbus:subscription:" + group + ":meta";
    }

    private static String progressKey(String group) {
        return "eventbus:subscription:" + group + ":progress";
    }
}
