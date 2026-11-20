package io.edgekv;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;

public record SampleData(long logicalNow, List<LogRecord> operations) {
    public SampleData {
        operations = List.copyOf(operations);
    }

    public static SampleData load(Path path) throws IOException {
        List<String> lines = Files.readAllLines(path, StandardCharsets.UTF_8);
        long now = -1;
        List<LogRecord> operations = new ArrayList<>();
        for (int i = 0; i < lines.size(); i++) {
            String line = lines.get(i).strip();
            if (line.isEmpty() || line.startsWith("#")) {
                continue;
            }
            if (line.startsWith("now=")) {
                now = Long.parseLong(line.substring("now=".length()).strip());
                continue;
            }
            String[] fields = line.split("\\t", -1);
            if (fields.length < 3) {
                throw new IllegalArgumentException("sample line " + (i + 1) + " is invalid");
            }
            Operation operation;
            try {
                operation = Operation.valueOf(fields[0].strip().toUpperCase());
            } catch (IllegalArgumentException e) {
                throw new IllegalArgumentException("sample line " + (i + 1)
                        + " has unknown operation: " + fields[0]);
            }
            String key = fields[1];
            String value = fields[2];
            long expireAt = 0;
            if (fields.length >= 4 && !fields[3].isBlank()) {
                expireAt = Long.parseLong(fields[3].strip());
            }
            if (operation == Operation.DELETE) {
                value = "";
                expireAt = 0;
            }
            operations.add(new LogRecord(operation, key, value, 0, expireAt));
        }
        if (now < 0) {
            throw new IllegalArgumentException("sample must declare now=<epoch>");
        }
        if (operations.isEmpty()) {
            throw new IllegalArgumentException("sample has no operations");
        }
        return new SampleData(now, operations);
    }

    public List<Long> applyTo(EdgeKvStore store) {
        List<Long> versions = new ArrayList<>(operations.size());
        for (LogRecord operation : operations) {
            if (operation.operation() == Operation.DELETE) {
                versions.add(store.delete(operation.key()));
            } else {
                versions.add(store.put(operation.key(), operation.value(), operation.expireAt()));
            }
        }
        return versions;
    }
}
