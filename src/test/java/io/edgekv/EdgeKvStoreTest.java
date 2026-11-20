package io.edgekv;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class EdgeKvStoreTest {
    @TempDir
    Path tempDir;

    @Test
    void versionsAreMonotonicAndExpiryUsesInjectedClock() {
        Path data = tempDir.resolve("data");
        try (EdgeKvStore store = EdgeKvStore.open(data)) {
            long stableVersion = store.put("stable", "one", 0);
            long expiringVersion = store.put("session", "token", 100);
            long deletedVersion = store.delete("stable");

            assertEquals(1, stableVersion);
            assertEquals(2, expiringVersion);
            assertEquals(3, deletedVersion);

            assertEquals(QueryStatus.LIVE, store.lookup("session", 99).status());
            assertEquals("token", store.lookup("session", 99).value());
            assertEquals(2, store.lookup("session", 99).version());
            assertEquals(QueryStatus.EXPIRED, store.lookup("session", 100).status());
            assertEquals(QueryStatus.DELETED, store.lookup("stable", 50).status());
            assertEquals(3, store.lookup("stable", 50).version());
            assertEquals(1, store.expired(100).size());
            assertEquals("session", store.expired(100).get(0).key());
            assertFalse(store.expired(99).stream().anyMatch(item -> item.key().equals("session")));
        }

        try (EdgeKvStore recovered = EdgeKvStore.open(data)) {
            assertEquals(3, recovered.version());
            assertEquals(3, recovered.recoveryReport().replayedRecords());
            assertEquals(QueryStatus.EXPIRED, recovered.lookup("session", 100).status());
            long nextVersion = recovered.put("next", "value", 0);
            assertEquals(4, nextVersion);
        }
    }

    @Test
    void fixedSampleHasLiveExpiredAndDeletedKeys() throws IOException {
        Path data = tempDir.resolve("sample-data");
        SampleData sample = SampleData.load(Path.of("samples", "fixed-sample.tsv"));
        try (EdgeKvStore store = EdgeKvStore.open(data)) {
            List<Long> versions = sample.applyTo(store);
            assertEquals(List.of(1L, 2L, 3L, 4L, 5L), versions);
            assertEquals(QueryStatus.LIVE, store.lookup("stable-node", sample.logicalNow()).status());
            assertEquals(QueryStatus.EXPIRED, store.lookup("session:old", sample.logicalNow()).status());
            assertEquals(QueryStatus.DELETED, store.lookup("doomed-node", sample.logicalNow()).status());
            assertEquals(QueryStatus.LIVE, store.lookup("session:fresh", sample.logicalNow()).status());
        }
    }

    @Test
    void safePartialTailIsSkippedAndLaterWritesRemainReadable() throws IOException {
        Path data = tempDir.resolve("partial");
        seedFiveRecords(data);
        StorePaths paths = StorePaths.of(data);
        Manifest manifest = Manifest.read(paths.manifest());
        Path log = paths.resolveDataFile(manifest.logFile());
        long originalSize = Files.size(log);
        FileUtil.truncate(log, originalSize - 7);

        try (EdgeKvStore recovered = EdgeKvStore.open(data)) {
            assertEquals(4, recovered.version());
            assertEquals(4, recovered.recoveryReport().replayedRecords());
            assertTrue(recovered.recoveryReport().skippedTruncatedBytes() > 0);
            assertTrue(recovered.recoveryReport().warnings().stream()
                    .anyMatch(warning -> warning.contains("safe-truncated")));
            recovered.put("after-recovery", "ok", 0);
        }
        try (EdgeKvStore recoveredAgain = EdgeKvStore.open(data)) {
            assertEquals(5, recoveredAgain.version());
            assertEquals(QueryStatus.LIVE, recoveredAgain.lookup("after-recovery", 0).status());
            assertEquals(0, recoveredAgain.recoveryReport().skippedTruncatedBytes());
        }
    }

    @Test
    void compactionReplacesGenerationWithoutVersionRollback() throws IOException {
        Path data = tempDir.resolve("compact");
        seedFiveRecords(data);
        try (EdgeKvStore store = EdgeKvStore.open(data)) {
            CompactionResult result = store.compact(1_700_000_100L);
            assertEquals(2, result.generation());
            assertEquals(5, result.snapshotVersion());
            assertEquals(1, result.minLiveVersion());
            assertEquals(2, result.liveEntries());
            assertEquals(0, store.recoveryReport().activeLogRecords());
        }
        try (EdgeKvStore recovered = EdgeKvStore.open(data)) {
            assertEquals(5, recovered.version());
            assertTrue(recovered.recoveryReport().snapshotLoaded());
            assertEquals(5, recovered.recoveryReport().snapshotVersion());
            assertEquals(0, recovered.recoveryReport().replayedRecords());
            assertEquals(2, recovered.snapshotView(1_700_000_100L).stream()
                    .filter(item -> item.status() == QueryStatus.LIVE).count());
        }
    }

    @Test
    void unsafeLogChecksumRequiresRepairAndPreservesOriginal() throws IOException {
        Path data = tempDir.resolve("unsafe");
        seedFiveRecords(data);
        StorePaths paths = StorePaths.of(data);
        Manifest manifest = Manifest.read(paths.manifest());
        Path log = paths.resolveDataFile(manifest.logFile());
        byte[] bytes = Files.readAllBytes(log);
        bytes[bytes.length - 5] ^= 0x5A;
        Files.write(log, bytes);

        RepairException error = assertThrows(RepairException.class, () -> EdgeKvStore.open(data));
        assertEquals("E_LOG_TRUNCATED", error.code());

        RepairOutcome outcome = new RepairManager(data).repair();
        assertEquals("E_LOG_TRUNCATED", outcome.originalCode());
        assertEquals(4, outcome.recoveryReport().finalVersion());
        Path backupRoot = data.resolve(outcome.backup()).resolve("root");
        assertTrue(Files.exists(backupRoot.resolve("manifest.ekv")));
        assertTrue(Files.exists(backupRoot.resolve(manifest.logFile())));
        assertTrue(Files.size(backupRoot.resolve(manifest.logFile())) > Files.size(log));
    }

    @Test
    void snapshotCorruptionReportsConflictAndCanRebuildFromBaseLog() throws IOException {
        Path data = tempDir.resolve("snapshot-conflict");
        seedFiveRecords(data);
        try (EdgeKvStore store = EdgeKvStore.open(data)) {
            store.compact(1_700_000_100L);
        }
        StorePaths paths = StorePaths.of(data);
        Manifest manifest = Manifest.read(paths.manifest());
        Path snapshot = paths.resolveDataFile(manifest.snapshotFile());
        FileUtil.truncate(snapshot, Files.size(snapshot) - 2);

        RepairException error = assertThrows(RepairException.class, () -> EdgeKvStore.open(data));
        assertEquals("E_SNAPSHOT_VERSION_CONFLICT", error.code());

        RepairOutcome outcome = new RepairManager(data).repair();
        assertEquals("E_SNAPSHOT_VERSION_CONFLICT", outcome.originalCode());
        assertEquals(5, outcome.recoveryReport().finalVersion());
        assertTrue(outcome.action().contains("rebuilt generation"));
        try (EdgeKvStore recovered = EdgeKvStore.open(data)) {
            assertEquals(5, recovered.version());
            assertEquals(QueryStatus.LIVE, recovered.lookup("stable-node", 1_700_000_100L).status());
            assertEquals(QueryStatus.LIVE, recovered.lookup("session:fresh", 1_700_000_100L).status());
        }
    }

    @Test
    void snapshotConflictCanFallBackToPreviousGenerationArchivedLog() throws IOException {
        Path data = tempDir.resolve("snapshot-previous-generation");
        seedFiveRecords(data);
        try (EdgeKvStore store = EdgeKvStore.open(data)) {
            store.compact(1_700_000_100L);
            store.put("after-first-compact", "still-present", 0);
            store.compact(1_700_000_100L);
        }
        StorePaths paths = StorePaths.of(data);
        Manifest manifest = Manifest.read(paths.manifest());
        Path snapshot = paths.resolveDataFile(manifest.snapshotFile());
        FileUtil.truncate(snapshot, Files.size(snapshot) - 2);

        RepairOutcome outcome = new RepairManager(data).repair();
        assertEquals("E_SNAPSHOT_VERSION_CONFLICT", outcome.originalCode());
        assertTrue(outcome.action().contains("selected snapshot generation 2"));
        try (EdgeKvStore recovered = EdgeKvStore.open(data)) {
            assertEquals(6, recovered.version());
            assertEquals("still-present", recovered.lookup("after-first-compact", 0).value());
            assertEquals(QueryStatus.LIVE, recovered.lookup("stable-node", 1_700_000_100L).status());
        }
    }

    @Test
    void logAppendFaultRecoversTheDurableRecord() {
        Path data = tempDir.resolve("fault-append");
        seedFiveRecords(data);

        try (EdgeKvStore store = EdgeKvStore.open(data,
                FaultInjector.throwing(FaultPoint.LOG_APPEND))) {
            assertThrows(SimulatedCrashException.class, () -> store.put("faulted", "durable", 0));
        }
        try (EdgeKvStore recovered = EdgeKvStore.open(data)) {
            assertEquals(6, recovered.version());
            assertEquals("durable", recovered.lookup("faulted", 0).value());
        }
    }

    @Test
    void snapshotWriteFaultLeavesOldSnapshotAndCompleteLog() {
        Path data = tempDir.resolve("fault-snapshot-write");
        seedFiveRecords(data);

        try (EdgeKvStore store = EdgeKvStore.open(data,
                FaultInjector.throwing(FaultPoint.SNAPSHOT_WRITE))) {
            assertThrows(SimulatedCrashException.class, () -> store.compact(1_700_000_100L));
        }
        try (EdgeKvStore recovered = EdgeKvStore.open(data)) {
            assertEquals(5, recovered.version());
            assertFalse(recovered.recoveryReport().snapshotLoaded());
            assertEquals(5, recovered.recoveryReport().replayedRecords());
            assertEquals(QueryStatus.LIVE, recovered.lookup("session:fresh", 1_700_000_100L).status());
        }
    }

    @Test
    void snapshotReplaceFaultExposesNewSnapshotAndEmptyLog() {
        Path data = tempDir.resolve("fault-snapshot-replace");
        seedFiveRecords(data);

        try (EdgeKvStore store = EdgeKvStore.open(data,
                FaultInjector.throwing(FaultPoint.SNAPSHOT_REPLACE))) {
            assertThrows(SimulatedCrashException.class, () -> store.compact(1_700_000_100L));
        }
        try (EdgeKvStore recovered = EdgeKvStore.open(data)) {
            assertEquals(2, recovered.recoveryReport().generation());
            assertEquals(5, recovered.version());
            assertTrue(recovered.recoveryReport().snapshotLoaded());
            assertEquals(0, recovered.recoveryReport().replayedRecords());
            assertEquals(2, recovered.snapshotView(1_700_000_100L).size());
        }
    }

    @Test
    void logTruncateFaultExposesCommittedNewGeneration() {
        Path data = tempDir.resolve("fault-log-truncate");
        seedFiveRecords(data);

        try (EdgeKvStore store = EdgeKvStore.open(data,
                FaultInjector.throwing(FaultPoint.LOG_TRUNCATE))) {
            assertThrows(SimulatedCrashException.class, () -> store.compact(1_700_000_100L));
        }
        try (EdgeKvStore recovered = EdgeKvStore.open(data)) {
            assertEquals(2, recovered.recoveryReport().generation());
            assertEquals(5, recovered.version());
            assertTrue(Files.exists(StorePaths.of(data).logs().resolve("archive").resolve("log-1.elog")));
            assertEquals(0, recovered.recoveryReport().activeLogRecords());
        }
    }

    private void seedFiveRecords(Path data) {
        try (EdgeKvStore store = EdgeKvStore.open(data)) {
            store.put("stable-node", "value-stable", 0);
            store.put("session:old", "token-old", 1_700_000_000L);
            store.put("doomed-node", "value-doomed", 0);
            store.delete("doomed-node");
            store.put("session:fresh", "token-new", 1_700_000_200L);
        }
    }
}
