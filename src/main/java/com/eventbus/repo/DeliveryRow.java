package com.eventbus.repo;

import java.time.Instant;

import com.fasterxml.jackson.databind.JsonNode;

public record DeliveryRow(
        int partition,
        long offset,
        String eventId,
        String eventType,
        long ruleVersion,
        JsonNode json,
        Instant firstDeliveredAt,
        Instant lastDeliveredAt,
        int deliveryCount,
        Instant confirmedAt) {
}
