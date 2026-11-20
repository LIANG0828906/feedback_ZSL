package io.edgekv;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.channels.FileChannel;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.util.Comparator;
import java.util.List;
import java.util.stream.Stream;

public final class RepairManager {
    private final StorePaths paths;

    public RepairManager(Path root) {
        this.paths = StorePaths.of(root);
    }

    public RepairReport preserveOriginals(String code, String message) {
        try {
            Files.createDirectories(paths.repair());
            Path backup = nextBackupDirectory();
            FileUtil.copyTree(paths.root(), backup.resolve("root"));
            RepairReport report = new RepairReport("REPAIR_REQUIRED", code, message,
                    paths.root().relativize(backup).toString().replace('\\', '/'));
            FileUtil.writeAtomic(backup.resolve("report.json"),
                    (report.toJson() + "\n").getBytes(StandardCharsets.UTF_8));
            FileUtil.fsyncDirectory(paths.repair());
            return report;
        } catch (IOException e) {
            throw new IllegalStateException("cannot preserve repair originals: " + e.getMessage(), e);
        }
    }

    public RepairOutcome repair() {
        String detectedCode;
        String detectedMessage;
        try (EdgeKvStore ignored = EdgeKvStore.open(paths.root())) {
            return new RepairOutcome("NO_REPAIR_NEEDED", null, null, "store already consistent",
                    ignored.recoveryReport());
        } catch (RepairException e) {
            detectedCode = e.code();
            detectedMessage = e.getMessage();
        }

        RepairReport preserved = preserveOriginals(detectedCode, detectedMessage);
        String action;
        try {
            if ("E_LOG_TRUNCATED".equals(detectedCode)) {
                action = repairLogTail();
            } else {
                action = repairSnapshotConflict();
            }
            try (EdgeKvStore repaired = EdgeKvStore.open(paths.root())) {
                return new RepairOutcome("REPAIRED", detectedCode, preserved.backup(), action,
                        repaired.recoveryReport());
            }
        } catch (RepairException e) {
            throw new RepairException(e.code(), e.getMessage() + "; original backup: " + preserved.backup(), e);
        } catch (IOException e) {
            throw new IllegalStateException("repair I/O failed after backup " + preserved.backup()
                    + ": " + e.getMessage(), e);
        }
    }

    private String repairLogTail() throws IOException {
        Manifest manifest = readManifestRequired();
        Path logPath = paths.resolveDataFile(manifest.logFile());
        if (!Files.exists(logPath)) {
            return repairSnapshotConflict();
        }
        LogRecovery recovery = LogCodec.recover(logPath);
        if (recovery.header() == null) {
            return repairSnapshotConflict();
        }
        if (!recovery.header().chainId().equals(manifest.chainId())
                || recovery.header().generation() != manifest.generation()) {
            return repairSnapshotConflict();
        }
        if (recovery.unsafe()) {
            FileUtil.truncate(logPath, recovery.lastGoodOffset());
            FileUtil.fsyncDirectory(paths.logs());
            return "truncated unreadable log tail at offset " + recovery.lastGoodOffset();
        }
        if (recovery.skippedTruncatedBytes() > 0) {
            FileUtil.truncate(logPath, recovery.lastGoodOffset());
            return "truncated safe partial log tail at offset " + recovery.lastGoodOffset();
        }
        return repairSnapshotConflict();
    }

    private String repairSnapshotConflict() throws IOException {
        Manifest manifest = readManifestRequired();
        Candidate best = findBestCandidate(manifest.chainId());
        if (best != null) {
            Manifest repaired = new Manifest(Manifest.CURRENT_FORMAT, manifest.chainId(),
                    best.snapshot.generation(),
                    pathRelative(best.snapshotPath),
                    best.snapshot.snapshotVersion(),
                    best.snapshot.minLiveVersion(),
                    pathRelative(best.logPath));
            repaired.writeAtomic(paths.manifest());
            return "selected snapshot generation " + best.snapshot.generation()
                    + " at version " + best.snapshot.snapshotVersion();
        }
        BaseLogCandidate baseLog = findBaseLogCandidate(manifest.chainId());
        if (baseLog == null) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                    "no valid snapshot/log pair or complete base log remains; originals are preserved");
        }
        long newGeneration = Math.max(manifest.generation(), Math.max(
                highestGeneration("snapshots", ".esnap"),
                Math.max(highestGeneration("logs", ".elog"),
                        highestGeneration("logs/archive", ".elog")))) + 1;
        String newLogRelative = "logs/log-" + newGeneration + ".elog";
        Path newLog = paths.resolveDataFile(newLogRelative);
        LogCodec.create(newLog, manifest.chainId(), newGeneration);
        try (FileChannel channel = FileChannel.open(newLog,
                StandardOpenOption.WRITE, StandardOpenOption.APPEND)) {
            for (LogRecord record : baseLog.recovery.records()) {
                ByteBuffer frame = ByteBuffer.wrap(LogCodec.encodeRecord(record));
                while (frame.hasRemaining()) {
                    channel.write(frame);
                }
            }
            channel.force(true);
        }
        LogRecovery verification = LogCodec.recover(newLog);
        if (verification.unsafe() || !verification.records().equals(baseLog.recovery.records())) {
            throw new RepairException("E_LOG_TRUNCATED",
                    "rebuilt log did not verify; originals are preserved");
        }
        Manifest repaired = new Manifest(Manifest.CURRENT_FORMAT, manifest.chainId(), newGeneration,
                null, 0, 0, newLogRelative);
        repaired.writeAtomic(paths.manifest());
        return "rebuilt generation " + newGeneration + " from complete base log "
                + pathRelative(baseLog.path) + " with " + baseLog.recovery.records().size() + " records";
    }

    private Candidate findBestCandidate(String chainId) throws IOException {
        if (!Files.isDirectory(paths.snapshots())) {
            return null;
        }
        List<Path> snapshotFiles;
        try (Stream<Path> stream = Files.list(paths.snapshots())) {
            snapshotFiles = stream
                    .filter(path -> path.getFileName().toString().endsWith(".esnap"))
                    .sorted(Comparator.comparing(Path::toString).reversed())
                    .toList();
        }
        Candidate best = null;
        for (Path snapshotPath : snapshotFiles) {
            SnapshotData snapshot;
            try {
                snapshot = SnapshotCodec.read(snapshotPath);
            } catch (SnapshotCorruptException | IOException e) {
                continue;
            }
            if (!snapshot.chainId().equals(chainId)) {
                continue;
            }
            Path logPath = findLogForGeneration(snapshot.generation());
            if (logPath == null) {
                continue;
            }
            LogRecovery recovery;
            try {
                recovery = LogCodec.recover(logPath);
            } catch (IOException e) {
                continue;
            }
            if (recovery.header() == null
                    || !recovery.header().chainId().equals(chainId)
                    || recovery.header().generation() != snapshot.generation()
                    || recovery.unsafe()) {
                continue;
            }
            boolean stale = recovery.records().stream()
                    .anyMatch(record -> record.version() <= snapshot.snapshotVersion());
            if (stale) {
                continue;
            }
            Candidate candidate = new Candidate(snapshotPath, snapshot, logPath);
            if (best == null
                    || candidate.snapshot.snapshotVersion() > best.snapshot.snapshotVersion()
                    || (candidate.snapshot.snapshotVersion() == best.snapshot.snapshotVersion()
                    && candidate.snapshot.generation() > best.snapshot.generation())) {
                best = candidate;
            }
        }
        return best;
    }

    private Path findLogForGeneration(long generation) {
        String fileName = "log-" + generation + ".elog";
        Path active = paths.logs().resolve(fileName);
        if (Files.exists(active)) {
            return active;
        }
        Path archived = paths.logs().resolve("archive").resolve(fileName);
        return Files.exists(archived) ? archived : null;
    }

    private BaseLogCandidate findBaseLogCandidate(String chainId) throws IOException {
        List<Path> logFiles;
        try (Stream<Path> stream = Files.walk(paths.logs())) {
            logFiles = stream
                    .filter(Files::isRegularFile)
                    .filter(path -> path.getFileName().toString().endsWith(".elog"))
                    .sorted(Comparator.comparing(Path::toString))
                    .toList();
        }
        BaseLogCandidate best = null;
        for (Path logPath : logFiles) {
            LogRecovery recovery;
            try {
                recovery = LogCodec.recover(logPath);
            } catch (IOException e) {
                continue;
            }
            if (recovery.header() == null
                    || !recovery.header().chainId().equals(chainId)
                    || recovery.unsafe()
                    || recovery.records().isEmpty()
                    || recovery.records().get(0).version() != 1) {
                continue;
            }
            boolean contiguous = true;
            for (int i = 1; i < recovery.records().size(); i++) {
                if (recovery.records().get(i).version() != recovery.records().get(i - 1).version() + 1) {
                    contiguous = false;
                    break;
                }
            }
            if (!contiguous) {
                continue;
            }
            if (best == null || recovery.records().size() > best.recovery.records().size()) {
                best = new BaseLogCandidate(logPath, recovery);
            }
        }
        return best;
    }

    private long highestGeneration(String relativeDirectory, String suffix) throws IOException {
        Path directory = paths.root().resolve(relativeDirectory);
        if (!Files.isDirectory(directory)) {
            return 0;
        }
        long highest = 0;
        try (Stream<Path> stream = Files.list(directory)) {
            for (Path path : stream.toList()) {
                String name = path.getFileName().toString();
                if (!name.endsWith(suffix)) {
                    continue;
                }
                int dash = name.lastIndexOf('-');
                int dot = name.lastIndexOf('.');
                if (dash < 0 || dot <= dash) {
                    continue;
                }
                try {
                    highest = Math.max(highest, Long.parseLong(name.substring(dash + 1, dot)));
                } catch (NumberFormatException ignored) {
                }
            }
        }
        return highest;
    }

    private Manifest readManifestRequired() {
        try {
            Manifest manifest = Manifest.read(paths.manifest());
            if (manifest == null) {
                throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT", "manifest is missing");
            }
            return manifest;
        } catch (IOException e) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                    "manifest cannot be read: " + e.getMessage(), e);
        }
    }

    private Path nextBackupDirectory() throws IOException {
        int sequence = 1;
        while (true) {
            Path candidate = paths.repair().resolve(String.format("backup-%03d", sequence));
            if (!Files.exists(candidate)) {
                Files.createDirectories(candidate);
                return candidate;
            }
            sequence++;
        }
    }

    private String pathRelative(Path path) {
        return paths.root().relativize(path.toAbsolutePath().normalize()).toString().replace('\\', '/');
    }

    private record Candidate(Path snapshotPath, SnapshotData snapshot, Path logPath) {
    }

    private record BaseLogCandidate(Path path, LogRecovery recovery) {
    }
}
