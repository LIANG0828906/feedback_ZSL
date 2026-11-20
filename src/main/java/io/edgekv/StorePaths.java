package io.edgekv;

import java.nio.file.Path;

public record StorePaths(Path root, Path manifest, Path snapshots, Path logs, Path repair, Path reports) {
    public static StorePaths of(Path root) {
        Path absolute = root.toAbsolutePath().normalize();
        return new StorePaths(
                absolute,
                absolute.resolve("manifest.ekv"),
                absolute.resolve("snapshots"),
                absolute.resolve("logs"),
                absolute.resolve("repair"),
                absolute.resolve("reports")
        );
    }

    public Path resolveDataFile(String relative) {
        if (relative == null || relative.isBlank()) {
            return null;
        }
        Path resolved = root.resolve(relative).normalize();
        if (!resolved.startsWith(root)) {
            throw new IllegalArgumentException("data file escapes store root: " + relative);
        }
        return resolved;
    }
}
