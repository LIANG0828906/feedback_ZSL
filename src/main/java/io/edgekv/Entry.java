package io.edgekv;

import java.util.Objects;

public record Entry(String key, String value, long version, long expireAt, boolean tombstone) {
    public Entry {
        Objects.requireNonNull(key, "key");
        Objects.requireNonNull(value, "value");
        if (version <= 0) {
            throw new IllegalArgumentException("version must be positive");
        }
    }

    public boolean hasExpiry() {
        return expireAt > 0;
    }

    public boolean expiredAt(long logicalNow) {
        return !tombstone && hasExpiry() && logicalNow >= expireAt;
    }

    public boolean liveAt(long logicalNow) {
        return !tombstone && !expiredAt(logicalNow);
    }
}
