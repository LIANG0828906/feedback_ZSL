package com.eventbus.domain;

import com.fasterxml.jackson.databind.JsonNode;

public record StoredEvent(
        int partition,
        long offset,
        String eventId,
        String eventType,
        String sourceService,
        String orderId,
        JsonNode json,
        boolean valid,
        String parseError) {

    public String safeEventId() {
        return eventId == null || eventId.isBlank() ? partition + ":" + offset : eventId;
    }
}
