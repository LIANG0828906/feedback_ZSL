package com.eventbus.dto;

import java.time.Instant;

import com.fasterxml.jackson.annotation.JsonPropertyOrder;

@JsonPropertyOrder({"group", "partition", "offset", "readOffset", "confirmedOffset", "confirmedAt"})
public record ConfirmResponse(
        String group,
        int partition,
        long offset,
        long readOffset,
        long confirmedOffset,
        Instant confirmedAt) {
}
