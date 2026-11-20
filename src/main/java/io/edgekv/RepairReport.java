package io.edgekv;

public record RepairReport(
        String status,
        String code,
        String message,
        String backup
) {
    public String toJson() {
        return "{"
                + "\"status\":" + Json.quote(status)
                + ",\"code\":" + Json.quote(code)
                + ",\"message\":" + Json.quote(message)
                + ",\"backup\":" + Json.quote(backup)
                + "}";
    }
}
