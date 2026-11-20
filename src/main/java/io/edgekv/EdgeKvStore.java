package io.edgekv;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.channels.FileChannel;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;

public final class EdgeKvStore implements AutoCloseable {
    public static final String DEFAULT_CHAIN_ID = "edgekv-local-chain";

    private final StorePaths paths;
    private final FaultInjector faultInjector;
    private final Map<String, Entry> entries = new LinkedHashMap<>();
    private final List<String> warnings = new ArrayList<>();
    private Manifest manifest;
    private FileChannel appendChannel;
    private Path activeLogPath;
    private long maxVersion;
    private RecoveryReport recoveryReport;
    private boolean closed;

    private EdgeKvStore(StorePaths paths, FaultInjector faultInjector) {
        this.paths = paths;
        this.faultInjector = faultInjector;
    }

    public static EdgeKvStore open(Path root, FaultInjector faultInjector) {
        EdgeKvStore store = new EdgeKvStore(StorePaths.of(root), faultInjector);
        try {
            store.initializeAndRecover();
            return store;
        } catch (IOException e) {
            store.closeQuietly();
            throw new IllegalStateException("cannot open store at " + root + ": " + e.getMessage(), e);
        } catch (RuntimeException e) {
            store.closeQuietly();
            throw e;
        }
    }

    public static EdgeKvStore open(Path root) {
        return open(root, FaultInjector.none());
    }

    public synchronized long put(String key, String value, long expireAt) {
        validateKeyValue(key, value);
        if (expireAt < 0) {
            throw new IllegalArgumentException("expireAt must be zero (no expiry) or positive");
        }
        ensureOpen();
        LogRecord record = new LogRecord(Operation.PUT, key, value, maxVersion + 1, expireAt);
        appendAndApply(record);
        return record.version();
    }

    public synchronized long delete(String key) {
        validateKeyValue(key, "");
        ensureOpen();
        LogRecord record = new LogRecord(Operation.DELETE, key, "", maxVersion + 1, 0);
        appendAndApply(record);
        return record.version();
    }

    public synchronized Optional<Entry> get(String key, long logicalNow) {
        QueryResult result = lookup(key, logicalNow);
        if (!result.live()) {
            return Optional.empty();
        }
        Entry entry = entries.get(key);
        return Optional.ofNullable(entry);
    }

    public synchronized QueryResult lookup(String key, long logicalNow) {
        ensureOpen();
        if (logicalNow < 0) {
            throw new IllegalArgumentException("logicalNow must not be negative");
        }
        return QueryResult.from(key, entries.get(key), logicalNow);
    }

    public synchronized List<QueryResult> expired(long logicalNow) {
        ensureOpen();
        if (logicalNow < 0) {
            throw new IllegalArgumentException("logicalNow must not be negative");
        }
        return entries.values().stream()
                .filter(entry -> entry.expiredAt(logicalNow))
                .sorted(Comparator.comparing(Entry::key))
                .map(entry -> QueryResult.from(entry.key(), entry, logicalNow))
                .toList();
    }

    public synchronized List<QueryResult> snapshotView(long logicalNow) {
        ensureOpen();
        return entries.values().stream()
                .sorted(Comparator.comparing(Entry::key))
                .map(entry -> QueryResult.from(entry.key(), entry, logicalNow))
                .toList();
    }

    public synchronized CompactionResult compact(long logicalNow) {
        ensureOpen();
        if (logicalNow < 0) {
            throw new IllegalArgumentException("logicalNow must not be negative");
        }
        long newGeneration = manifest.generation() + 1;
        long snapshotVersion = maxVersion;
        List<Entry> live = entries.values().stream()
                .filter(entry -> entry.liveAt(logicalNow))
                .sorted(Comparator.comparing(Entry::key))
                .toList();
        long minLiveVersion = live.stream().mapToLong(Entry::version).min().orElse(snapshotVersion);
        SnapshotData snapshot = new SnapshotData(manifest.chainId(), newGeneration,
                snapshotVersion, minLiveVersion, live);

        String snapshotRelative = "snapshots/snapshot-" + newGeneration + ".esnap";
        String logRelative = "logs/log-" + newGeneration + ".elog";
        Path snapshotPath = paths.resolveDataFile(snapshotRelative);
        Path logPath = paths.resolveDataFile(logRelative);
        Path pendingSnapshot = snapshotPath.resolveSibling(snapshotPath.getFileName() + ".pending");
        Path oldLogPath = activeLogPath;

        try {
            Files.createDirectories(paths.snapshots());
            Files.createDirectories(paths.logs());
            SnapshotCodec.write(pendingSnapshot, snapshot);
            SnapshotData readBack;
            try {
                readBack = SnapshotCodec.read(pendingSnapshot);
            } catch (SnapshotCorruptException e) {
                throw new IOException("pending snapshot verification failed: " + e.getMessage(), e);
            }
            verifyPendingSnapshot(snapshot, readBack);
            FileUtil.moveAtomic(pendingSnapshot, snapshotPath);
            FileUtil.fsyncDirectory(paths.snapshots());
            faultInjector.hit(FaultPoint.SNAPSHOT_WRITE);

            LogCodec.create(logPath, manifest.chainId(), newGeneration);
            Manifest nextManifest = manifest.withGeneration(newGeneration, snapshotRelative,
                    snapshotVersion, minLiveVersion, logRelative);
            nextManifest.writeAtomic(paths.manifest());
            FileUtil.fsyncDirectory(paths.root());
            faultInjector.hit(FaultPoint.SNAPSHOT_REPLACE);

            closeAppendChannel();
            manifest = nextManifest;
            activeLogPath = logPath;
            entries.clear();
            for (Entry entry : live) {
                entries.put(entry.key(), entry);
            }
            maxVersion = snapshotVersion;
            appendChannel = FileChannel.open(activeLogPath,
                    StandardOpenOption.WRITE, StandardOpenOption.APPEND);

            Path archived = retireOldLog(oldLogPath, manifest.generation() - 1);
            faultInjector.hit(FaultPoint.LOG_TRUNCATE);
            refreshRecoveryReport();
            return new CompactionResult(newGeneration, snapshotVersion, minLiveVersion,
                    live.size(), archived == null ? null : paths.root().relativize(archived).toString(),
                    maxVersion);
        } catch (IOException e) {
            throw new IllegalStateException("snapshot compaction failed: " + e.getMessage(), e);
        }
    }

    public synchronized long version() {
        ensureOpen();
        return maxVersion;
    }

    public synchronized RecoveryReport recoveryReport() {
        ensureOpen();
        return recoveryReport;
    }

    public synchronized Manifest manifest() {
        ensureOpen();
        return manifest;
    }

    public synchronized Path activeLogPath() {
        ensureOpen();
        return activeLogPath;
    }

    public synchronized StorePaths paths() {
        return paths;
    }

    public synchronized int size() {
        ensureOpen();
        return entries.size();
    }

    public synchronized void exportRecoveryReport(Path target) throws IOException {
        ensureOpen();
        FileUtil.writeAtomic(target, (recoveryReport.toJson() + "\n").getBytes(java.nio.charset.StandardCharsets.UTF_8));
    }

    public synchronized void refreshRecoveryReport() {
        ensureOpen();
        long activeRecords = 0;
        try {
            LogRecovery activeRecovery = LogCodec.recover(activeLogPath);
            activeRecords = activeRecovery.records().size();
        } catch (IOException e) {
            warnings.add("cannot inspect active log: " + e.getMessage());
        }
        recoveryReport = new RecoveryReport(
                "OK",
                manifest.chainId(),
                manifest.generation(),
                manifest.snapshotFile() != null,
                manifest.snapshotVersion(),
                manifest.minLiveVersion(),
                recoveryReport == null ? 0 : recoveryReport.replayedRecords(),
                recoveryReport == null ? 0 : recoveryReport.skippedTruncatedBytes(),
                maxVersion,
                activeRecords,
                FileUtil.fileSize(activeLogPath),
                warnings
        );
    }

    @Override
    public synchronized void close() {
        if (closed) {
            return;
        }
        closeAppendChannel();
        closed = true;
    }

    private void initializeAndRecover() throws IOException {
        Files.createDirectories(paths.root());
        Files.createDirectories(paths.snapshots());
        Files.createDirectories(paths.logs());
        Files.createDirectories(paths.repair());
        Files.createDirectories(paths.reports());

        manifest = Manifest.read(paths.manifest());
        if (manifest == null) {
            manifest = Manifest.initial(DEFAULT_CHAIN_ID);
            Path initialLog = paths.resolveDataFile(manifest.logFile());
            if (!Files.exists(initialLog)) {
                LogCodec.create(initialLog, manifest.chainId(), manifest.generation());
            }
            manifest.writeAtomic(paths.manifest());
            FileUtil.fsyncDirectory(paths.root());
        }

        long snapshotVersion = 0;
        long minLiveVersion = 0;
        boolean snapshotLoaded = false;
        if (manifest.snapshotFile() != null) {
            Path snapshotPath = paths.resolveDataFile(manifest.snapshotFile());
            if (!Files.exists(snapshotPath)) {
                throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                        "manifest references a missing snapshot: " + manifest.snapshotFile());
            }
            try {
                SnapshotCodec.validate(snapshotPath, manifest);
                SnapshotData snapshot = SnapshotCodec.read(snapshotPath);
                for (Entry entry : snapshot.entries()) {
                    entries.put(entry.key(), entry);
                }
                snapshotVersion = snapshot.snapshotVersion();
                minLiveVersion = snapshot.minLiveVersion();
                snapshotLoaded = true;
            } catch (SnapshotCorruptException e) {
                throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                        "snapshot validation failed: " + e.getMessage(), e);
            }
        }

        activeLogPath = paths.resolveDataFile(manifest.logFile());
        if (!Files.exists(activeLogPath)) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                    "manifest references a missing log: " + manifest.logFile());
        }
        LogRecovery logRecovery = LogCodec.recover(activeLogPath);
        if (logRecovery.header() == null) {
            throw new RepairException("E_LOG_TRUNCATED",
                    "active log header is unreadable: " + logRecovery.error());
        }
        if (!logRecovery.header().chainId().equals(manifest.chainId())
                || logRecovery.header().generation() != manifest.generation()) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                    "active log identity does not match manifest");
        }
        if (logRecovery.unsafe()) {
            throw new RepairException("E_LOG_TRUNCATED",
                    logRecovery.error() + " (last good offset " + logRecovery.lastGoodOffset() + ")");
        }
        maxVersion = snapshotVersion;
        long lastReplayedVersion = snapshotVersion;
        int replayed = 0;
        for (LogRecord record : logRecovery.records()) {
            if (record.version() <= snapshotVersion) {
                throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                        "log record version " + record.version()
                                + " is not newer than snapshot version " + snapshotVersion);
            }
            if (record.version() <= lastReplayedVersion) {
                throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                        "log version chain is not strictly increasing at version " + record.version());
            }
            lastReplayedVersion = record.version();
            entries.put(record.key(), record.toEntry());
            maxVersion = record.version();
            replayed++;
        }
        if (logRecovery.skippedTruncatedBytes() > 0) {
            FileUtil.truncate(activeLogPath, logRecovery.lastGoodOffset());
            warnings.add("safe-truncated " + logRecovery.skippedTruncatedBytes() + " trailing bytes");
        }

        appendChannel = FileChannel.open(activeLogPath,
                StandardOpenOption.WRITE, StandardOpenOption.APPEND);
        recoveryReport = new RecoveryReport(
                "OK",
                manifest.chainId(),
                manifest.generation(),
                snapshotLoaded,
                snapshotVersion,
                minLiveVersion,
                replayed,
                logRecovery.skippedTruncatedBytes(),
                maxVersion,
                logRecovery.records().size(),
                Files.size(activeLogPath),
                warnings
        );
    }

    private void appendAndApply(LogRecord record) {
        try {
            byte[] frame = LogCodec.encodeRecord(record);
            ByteBuffer buffer = ByteBuffer.wrap(frame);
            while (buffer.hasRemaining()) {
                appendChannel.write(buffer);
            }
            appendChannel.force(true);
            entries.put(record.key(), record.toEntry());
            maxVersion = record.version();
            refreshRecoveryReport();
            faultInjector.hit(FaultPoint.LOG_APPEND);
        } catch (IOException e) {
            throw new IllegalStateException("log append failed: " + e.getMessage(), e);
        }
    }

    private static void verifyPendingSnapshot(SnapshotData expected, SnapshotData actual) {
        if (!expected.chainId().equals(actual.chainId())
                || expected.generation() != actual.generation()
                || expected.snapshotVersion() != actual.snapshotVersion()
                || expected.minLiveVersion() != actual.minLiveVersion()
                || !expected.entries().equals(actual.entries())) {
            throw new IllegalStateException("pending snapshot verification mismatch");
        }
    }

    private Path retireOldLog(Path oldLogPath, long oldGeneration) throws IOException {
        if (oldLogPath == null || !Files.exists(oldLogPath)) {
            return null;
        }
        Path archive = paths.logs().resolve("archive");
        Files.createDirectories(archive);
        if (oldLogPath.toAbsolutePath().normalize().startsWith(archive.toAbsolutePath().normalize())) {
            return oldLogPath;
        }
        Path target = archive.resolve("log-" + oldGeneration + ".elog");
        int suffix = 1;
        while (Files.exists(target)) {
            target = archive.resolve("log-" + oldGeneration + "-retired-" + suffix + ".elog");
            suffix++;
        }
        FileUtil.moveAtomic(oldLogPath, target);
        FileUtil.fsyncDirectory(paths.logs());
        FileUtil.fsyncDirectory(archive);
        return target;
    }

    private void closeAppendChannel() {
        if (appendChannel != null) {
            try {
                appendChannel.close();
            } catch (IOException ignored) {
            }
            appendChannel = null;
        }
    }

    private void closeQuietly() {
        closeAppendChannel();
        closed = true;
    }

    private void ensureOpen() {
        if (closed) {
            throw new IllegalStateException("store is closed");
        }
    }

    private static void validateKeyValue(String key, String value) {
        if (key == null || key.isEmpty()) {
            throw new IllegalArgumentException("key must not be empty");
        }
        if (key.getBytes(java.nio.charset.StandardCharsets.UTF_8).length > 16 * 1024 * 1024) {
            throw new IllegalArgumentException("key is too large");
        }
        if (value == null) {
            throw new IllegalArgumentException("value must not be null");
        }
        if (value.getBytes(java.nio.charset.StandardCharsets.UTF_8).length > 16 * 1024 * 1024) {
            throw new IllegalArgumentException("value is too large");
        }
    }
}
