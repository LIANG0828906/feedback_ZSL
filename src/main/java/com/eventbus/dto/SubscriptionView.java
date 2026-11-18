package com.eventbus.dto;

import java.time.Instant;

import com.eventbus.domain.RuleDefinition;
import com.fasterxml.jackson.annotation.JsonPropertyOrder;

@JsonPropertyOrder({"group", "currentRuleVersion", "rule", "createdAt", "updatedAt"})
public record SubscriptionView(
        String group,
        long currentRuleVersion,
        RuleDefinition rule,
        Instant createdAt,
        Instant updatedAt) {
}
