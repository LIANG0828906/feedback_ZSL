package io.edgekv;

public record CompactionResult(
        long generation,
        long snapshotVersion,
        long minLiveVersion,
        int liveEntries,
        String retiredLog,
        long finalVersion
) {
    public String toJson() {
        return "{"
                + "\"generation\":" + generation
                + ",\"snapshotVersion\":" + snapshotVersion
                + ",\"minLiveVersion\":" + minLiveVersion
                + ",\"liveEntries\":" + liveEntries
                + ",\"retiredLog\":" + Json.quote(retiredLog)
                + ",\"finalVersion\":" + finalVersion
                + "}";
    }
}
