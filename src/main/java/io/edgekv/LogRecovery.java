package io.edgekv;

import java.util.List;

public record LogRecovery(
        LogHeader header,
        List<LogRecord> records,
        long lastGoodOffset,
        long skippedTruncatedBytes,
        boolean unsafe,
        String error
) {
    public LogRecovery {
        records = List.copyOf(records);
    }
}
