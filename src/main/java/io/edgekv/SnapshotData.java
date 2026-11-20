package io.edgekv;

import java.util.List;

public record SnapshotData(
        String chainId,
        long generation,
        long snapshotVersion,
        long minLiveVersion,
        List<Entry> entries
) {
    public SnapshotData {
        entries = List.copyOf(entries);
    }
}
