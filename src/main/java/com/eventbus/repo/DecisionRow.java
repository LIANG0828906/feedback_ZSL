package com.eventbus.repo;

import java.time.Instant;

public record DecisionRow(
        int partition,
        long offset,
        String eventId,
        String eventType,
        long ruleVersion,
        boolean matched,
        String reason,
        Instant decidedAt) {
}
