package io.edgekv;

public record QueryResult(String key, QueryStatus status, String value, long version, long expireAt) {
    public static QueryResult missing(String key) {
        return new QueryResult(key, QueryStatus.MISSING, null, 0, 0);
    }

    public static QueryResult from(String key, Entry entry, long logicalNow) {
        if (entry == null) {
            return missing(key);
        }
        QueryStatus status = entry.tombstone()
                ? QueryStatus.DELETED
                : entry.expiredAt(logicalNow) ? QueryStatus.EXPIRED : QueryStatus.LIVE;
        return new QueryResult(key, status, entry.value(), entry.version(), entry.expireAt());
    }

    public boolean live() {
        return status == QueryStatus.LIVE;
    }
}
