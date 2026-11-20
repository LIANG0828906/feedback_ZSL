package io.edgekv;

public record RepairOutcome(
        String status,
        String originalCode,
        String backup,
        String action,
        RecoveryReport recoveryReport
) {
    public String toJson() {
        return "{"
                + "\"status\":" + Json.quote(status)
                + ",\"originalCode\":" + Json.quote(originalCode)
                + ",\"backup\":" + Json.quote(backup)
                + ",\"action\":" + Json.quote(action)
                + ",\"recovery\":" + recoveryReport.toJson()
                + "}";
    }
}
