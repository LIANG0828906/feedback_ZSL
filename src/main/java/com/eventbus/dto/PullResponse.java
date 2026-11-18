package com.eventbus.dto;

import java.time.Instant;
import java.util.List;

import com.fasterxml.jackson.annotation.JsonPropertyOrder;

@JsonPropertyOrder({"group", "currentRuleVersion", "pulledAt", "partitions"})
public record PullResponse(
        String group,
        long currentRuleVersion,
        Instant pulledAt,
        List<PartitionPull> partitions) {

    @JsonPropertyOrder({
            "partition", "readOffset", "confirmedOffset", "scanned",
            "excluded", "hasMore", "events"
    })
    public record PartitionPull(
            int partition,
            long readOffset,
            long confirmedOffset,
            int scanned,
            int excluded,
            boolean hasMore,
            List<DeliveredEvent> events) {
    }

    @JsonPropertyOrder({
            "offset", "eventId", "eventType", "sourceService", "orderId",
            "payload", "ruleVersion", "deliveryCount", "deliveredAt"
    })
    public record DeliveredEvent(
            long offset,
            String eventId,
            String eventType,
            String sourceService,
            String orderId,
            com.fasterxml.jackson.databind.JsonNode payload,
            long ruleVersion,
            int deliveryCount,
            Instant deliveredAt) {
    }
}
