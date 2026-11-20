package io.edgekv;

public enum FaultPoint {
    LOG_APPEND,
    SNAPSHOT_WRITE,
    SNAPSHOT_REPLACE,
    LOG_TRUNCATE;

    public static FaultPoint parse(String value) {
        if (value == null || value.isBlank()) {
            return null;
        }
        String normalized = value.trim().toUpperCase().replace('-', '_');
        return switch (normalized) {
            case "LOG_APPEND", "AFTER_LOG_APPEND" -> LOG_APPEND;
            case "SNAPSHOT_WRITE", "AFTER_SNAPSHOT_WRITE" -> SNAPSHOT_WRITE;
            case "SNAPSHOT_REPLACE", "AFTER_SNAPSHOT_REPLACE" -> SNAPSHOT_REPLACE;
            case "LOG_TRUNCATE", "AFTER_LOG_TRUNCATE" -> LOG_TRUNCATE;
            default -> throw new IllegalArgumentException("unknown fault point: " + value);
        };
    }
}
