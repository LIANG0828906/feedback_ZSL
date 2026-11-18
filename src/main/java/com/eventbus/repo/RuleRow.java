package com.eventbus.repo;

import java.time.Instant;

import com.eventbus.domain.RuleDefinition;

public record RuleRow(
        long version,
        RuleDefinition definition,
        Instant createdAt) {
}
