package com.eventbus.repo;

public record MetricRow(
        String group,
        int partition,
        long pullCount,
        long hitCount,
        long excludedCount,
        long redeliveryCount) {
}
