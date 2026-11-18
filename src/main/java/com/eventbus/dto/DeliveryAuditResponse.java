package com.eventbus.dto;

import java.time.Instant;
import java.util.List;

import com.fasterxml.jackson.annotation.JsonPropertyOrder;

@JsonPropertyOrder({"group", "eventId", "totalDeliveryCount", "redeliveryCount", "records"})
public record DeliveryAuditResponse(
        String group,
        String eventId,
        long totalDeliveryCount,
        long redeliveryCount,
        List<Record> records) {

    @JsonPropertyOrder({
            "partition", "offset", "ruleVersion", "deliveryCount",
            "confirmed", "firstDeliveredAt", "lastDeliveredAt"
    })
    public record Record(
            int partition,
            long offset,
            long ruleVersion,
            int deliveryCount,
            boolean confirmed,
            Instant firstDeliveredAt,
            Instant lastDeliveredAt) {
    }
}
