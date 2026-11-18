package com.eventbus.repo;

import java.time.Instant;

public record GroupRow(
        String group,
        long currentRuleVersion,
        Instant createdAt,
        Instant updatedAt) {
}
