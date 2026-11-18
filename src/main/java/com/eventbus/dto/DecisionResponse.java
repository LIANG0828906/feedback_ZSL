package com.eventbus.dto;

import com.fasterxml.jackson.annotation.JsonPropertyOrder;

@JsonPropertyOrder({
        "group", "partition", "offset", "eventId", "eventType",
        "ruleVersion", "matched", "reason", "deliveryCount", "confirmed"
})
public record DecisionResponse(
        String group,
        int partition,
        long offset,
        String eventId,
        String eventType,
        long ruleVersion,
        boolean matched,
        String reason,
        int deliveryCount,
        boolean confirmed) {
}
