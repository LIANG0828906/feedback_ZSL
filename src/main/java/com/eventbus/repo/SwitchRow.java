package com.eventbus.repo;

import java.time.Instant;

public record SwitchRow(
        int partition,
        long oldVersion,
        long newVersion,
        long boundaryOffset,
        Instant switchedAt) {
}
