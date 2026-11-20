package io.edgekv;

import java.util.List;

public final class Summary {
    private Summary() {
    }

    public static String of(EdgeKvStore store, long logicalNow) {
        List<QueryResult> keys = store.snapshotView(logicalNow);
        long live = keys.stream().filter(item -> item.status() == QueryStatus.LIVE).count();
        long expired = keys.stream().filter(item -> item.status() == QueryStatus.EXPIRED).count();
        long deleted = keys.stream().filter(item -> item.status() == QueryStatus.DELETED).count();
        RecoveryReport recovery = store.recoveryReport();
        return "{"
                + "\"now\":" + logicalNow
                + ",\"finalVersion\":" + store.version()
                + ",\"generation\":" + recovery.generation()
                + ",\"snapshotVersion\":" + recovery.snapshotVersion()
                + ",\"minLiveVersion\":" + recovery.minLiveVersion()
                + ",\"snapshotLoaded\":" + recovery.snapshotLoaded()
                + ",\"replayedRecords\":" + recovery.replayedRecords()
                + ",\"skippedTruncatedBytes\":" + recovery.skippedTruncatedBytes()
                + ",\"counts\":{\"live\":" + live + ",\"expired\":" + expired
                + ",\"deleted\":" + deleted + "}"
                + ",\"keys\":" + Json.array(keys, item -> Json.queryObject((QueryResult) item))
                + "}";
    }
}
