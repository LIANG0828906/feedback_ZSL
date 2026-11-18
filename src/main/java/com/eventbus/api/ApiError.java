package com.eventbus.api;

import java.time.Instant;
import java.util.Map;

import com.fasterxml.jackson.annotation.JsonPropertyOrder;

@JsonPropertyOrder({"code", "message", "details", "timestamp"})
public record ApiError(
        String code,
        String message,
        Map<String, Object> details,
        Instant timestamp) {
}
