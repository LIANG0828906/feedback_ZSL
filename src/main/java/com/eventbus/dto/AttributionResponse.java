package com.eventbus.dto;

import java.util.List;

import com.fasterxml.jackson.annotation.JsonPropertyOrder;

@JsonPropertyOrder({"group", "currentRuleVersion", "partitions", "totals"})
public record AttributionResponse(
        String group,
        long currentRuleVersion,
        List<PartitionAttribution> partitions,
        AttributionTotals totals) {

    @JsonPropertyOrder({
            "partition", "committedOffset", "latestOffset", "gap",
            "ruleExcluded", "unconfirmed", "redelivery", "sum", "balanced"
    })
    public record PartitionAttribution(
            int partition,
            long committedOffset,
            long latestOffset,
            long gap,
            long ruleExcluded,
            long unconfirmed,
            long redelivery,
            long sum,
            boolean balanced) {
    }

    @JsonPropertyOrder({
            "gap", "ruleExcluded", "unconfirmed", "redelivery", "sum", "balanced"
    })
    public record AttributionTotals(
            long gap,
            long ruleExcluded,
            long unconfirmed,
            long redelivery,
            long sum,
            boolean balanced) {
    }
}
