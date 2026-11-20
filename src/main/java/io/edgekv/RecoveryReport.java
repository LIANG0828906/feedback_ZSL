package io.edgekv;

import java.util.List;

public record RecoveryReport(
        String status,
        String chainId,
        long generation,
        boolean snapshotLoaded,
        long snapshotVersion,
        long minLiveVersion,
        int replayedRecords,
        long skippedTruncatedBytes,
        long finalVersion,
        long activeLogRecords,
        long activeLogBytes,
        List<String> warnings
) {
    public RecoveryReport {
        warnings = List.copyOf(warnings);
    }

    public String toJson() {
        return "{"
                + "\"status\":" + Json.quote(status)
                + ",\"chainId\":" + Json.quote(chainId)
                + ",\"generation\":" + generation
                + ",\"snapshotLoaded\":" + snapshotLoaded
                + ",\"snapshotVersion\":" + snapshotVersion
                + ",\"minLiveVersion\":" + minLiveVersion
                + ",\"replayedRecords\":" + replayedRecords
                + ",\"skippedTruncatedBytes\":" + skippedTruncatedBytes
                + ",\"finalVersion\":" + finalVersion
                + ",\"activeLogRecords\":" + activeLogRecords
                + ",\"activeLogBytes\":" + activeLogBytes
                + ",\"warnings\":" + Json.array(warnings, item -> Json.quote((String) item))
                + "}";
    }
}
