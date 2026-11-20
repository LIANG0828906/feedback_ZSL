package io.edgekv;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.Map;

public record Manifest(
        int formatVersion,
        String chainId,
        long generation,
        String snapshotFile,
        long snapshotVersion,
        long minLiveVersion,
        String logFile
) {
    public static final int CURRENT_FORMAT = 1;

    public Manifest {
        if (formatVersion != CURRENT_FORMAT) {
            throw new IllegalArgumentException("unsupported manifest format: " + formatVersion);
        }
        if (chainId == null || chainId.isBlank()) {
            throw new IllegalArgumentException("chainId is required");
        }
        if (generation <= 0) {
            throw new IllegalArgumentException("generation must be positive");
        }
        if (logFile == null || logFile.isBlank()) {
            throw new IllegalArgumentException("logFile is required");
        }
        if (snapshotVersion < 0 || minLiveVersion < 0) {
            throw new IllegalArgumentException("versions must not be negative");
        }
    }

    public static Manifest initial(String chainId) {
        return new Manifest(CURRENT_FORMAT, chainId, 1, null, 0, 0, "logs/log-1.elog");
    }

    public static Manifest read(Path path) throws IOException, RepairException {
        if (!Files.exists(path)) {
            return null;
        }
        String text = Files.readString(path, StandardCharsets.UTF_8);
        String[] lines = text.split("\\R", -1);
        if (lines.length < 2 || !"EDGEKV_MANIFEST_V1".equals(lines[0])) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT", "manifest magic is invalid");
        }
        Map<String, String> values = new LinkedHashMap<>();
        String checksum = null;
        for (int i = 1; i < lines.length; i++) {
            String line = lines[i];
            if (line.isEmpty()) {
                continue;
            }
            if (line.startsWith("checksum=")) {
                checksum = line.substring("checksum=".length());
                continue;
            }
            int equals = line.indexOf('=');
            if (equals < 0) {
                throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT", "manifest line is invalid: " + line);
            }
            values.put(line.substring(0, equals), line.substring(equals + 1));
        }
        String expected = manifestChecksum(values);
        if (checksum == null || !checksum.equals(expected)) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT", "manifest checksum mismatch");
        }
        try {
            return new Manifest(
                    Integer.parseInt(required(values, "formatVersion")),
                    required(values, "chainId"),
                    Long.parseLong(required(values, "generation")),
                    blankToNull(values.get("snapshotFile")),
                    Long.parseLong(values.getOrDefault("snapshotVersion", "0")),
                    Long.parseLong(values.getOrDefault("minLiveVersion", "0")),
                    required(values, "logFile")
            );
        } catch (RuntimeException e) {
            if (e instanceof RepairException repairException) {
                throw repairException;
            }
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT", "manifest fields are invalid", e);
        }
    }

    public void writeAtomic(Path path) throws IOException {
        Map<String, String> values = new LinkedHashMap<>();
        values.put("formatVersion", Integer.toString(formatVersion));
        values.put("chainId", chainId);
        values.put("generation", Long.toString(generation));
        values.put("snapshotFile", snapshotFile == null ? "" : snapshotFile);
        values.put("snapshotVersion", Long.toString(snapshotVersion));
        values.put("minLiveVersion", Long.toString(minLiveVersion));
        values.put("logFile", logFile);
        StringBuilder out = new StringBuilder("EDGEKV_MANIFEST_V1\n");
        for (Map.Entry<String, String> entry : values.entrySet()) {
            out.append(entry.getKey()).append('=').append(entry.getValue()).append('\n');
        }
        out.append("checksum=").append(manifestChecksum(values)).append('\n');
        FileUtil.writeAtomic(path, out.toString().getBytes(StandardCharsets.UTF_8));
    }

    public Manifest withGeneration(long newGeneration, String newSnapshotFile, long newSnapshotVersion,
                                   long newMinLiveVersion, String newLogFile) {
        return new Manifest(CURRENT_FORMAT, chainId, newGeneration, newSnapshotFile,
                newSnapshotVersion, newMinLiveVersion, newLogFile);
    }

    private static String required(Map<String, String> values, String key) {
        String value = values.get(key);
        if (value == null || value.isBlank()) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT", "manifest field missing: " + key);
        }
        return value;
    }

    private static String blankToNull(String value) {
        return value == null || value.isBlank() ? null : value;
    }

    private static String manifestChecksum(Map<String, String> values) {
        try {
            MessageDigest digest = MessageDigest.getInstance("SHA-256");
            for (Map.Entry<String, String> entry : values.entrySet()) {
                digest.update(entry.getKey().getBytes(StandardCharsets.UTF_8));
                digest.update((byte) '=');
                digest.update(entry.getValue().getBytes(StandardCharsets.UTF_8));
                digest.update((byte) '\n');
            }
            return HexFormat.of().formatHex(digest.digest());
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException(e);
        }
    }
}
