package com.eventbus.repo;

import java.time.Instant;

public record ProgressRow(
        int partition,
        long readOffset,
        long confirmedOffset,
        Instant lastPullAt,
        Instant updatedAt) {
}
