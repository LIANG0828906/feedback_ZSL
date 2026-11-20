package io.edgekv;

public record LogRecord(Operation operation, String key, String value, long version, long expireAt) {
    public Entry toEntry() {
        boolean tombstone = operation == Operation.DELETE;
        return new Entry(key, value, version, expireAt, tombstone);
    }
}
