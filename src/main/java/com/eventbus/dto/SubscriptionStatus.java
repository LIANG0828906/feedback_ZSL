package com.eventbus.dto;

import java.time.Instant;
import java.util.List;

import com.eventbus.domain.RuleDefinition;
import com.fasterxml.jackson.annotation.JsonPropertyOrder;

@JsonPropertyOrder({
        "group", "currentRuleVersion", "rule", "updatedAt",
        "duplicateDeliveryCount", "partitions"
})
public record SubscriptionStatus(
        String group,
        long currentRuleVersion,
        RuleDefinition rule,
        Instant updatedAt,
        long duplicateDeliveryCount,
        List<PartitionStatus> partitions) {

    @JsonPropertyOrder({
            "partition", "readOffset", "confirmedOffset", "latestOffset",
            "backlogEvents", "duplicateDeliveryCount", "ruleSwitchRecords"
    })
    public record PartitionStatus(
            int partition,
            long readOffset,
            long confirmedOffset,
            Long latestOffset,
            Long backlogEvents,
            long duplicateDeliveryCount,
            List<RuleSwitchView> ruleSwitchRecords) {
    }

    @JsonPropertyOrder({"oldVersion", "newVersion", "boundaryOffset", "switchedAt"})
    public record RuleSwitchView(
            long oldVersion,
            long newVersion,
            long boundaryOffset,
            Instant switchedAt) {
    }
}
