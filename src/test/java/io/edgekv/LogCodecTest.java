package io.edgekv;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class LogCodecTest {
    @TempDir
    Path tempDir;

    @Test
    void recoversCompleteFramesBeforePartialTail() throws IOException {
        Path log = tempDir.resolve("log.elog");
        LogCodec.create(log, "chain", 1);
        LogRecord first = new LogRecord(Operation.PUT, "a", "one", 1, 0);
        LogRecord second = new LogRecord(Operation.PUT, "b", "two", 2, 123);
        Files.write(log, LogCodec.encodeRecord(first), StandardOpenOption.APPEND);
        Files.write(log, LogCodec.encodeRecord(second), StandardOpenOption.APPEND);
        long fullSize = Files.size(log);
        Files.write(log, new byte[]{0x45, 0x4B, 0x52}, StandardOpenOption.APPEND);

        LogRecovery recovery = LogCodec.recover(log);
        assertFalse(recovery.unsafe());
        assertEquals(List.of(first, second), recovery.records());
        assertEquals(3, recovery.skippedTruncatedBytes());
        assertEquals(fullSize, recovery.lastGoodOffset());
    }

    @Test
    void completeRecordWithBadChecksumIsUnsafe() throws IOException {
        Path log = tempDir.resolve("bad.elog");
        LogCodec.create(log, "chain", 1);
        Files.write(log, LogCodec.encodeRecord(
                new LogRecord(Operation.PUT, "a", "one", 1, 0)), StandardOpenOption.APPEND);
        byte[] bytes = Files.readAllBytes(log);
        bytes[bytes.length - 5] ^= 0x33;
        Files.write(log, bytes, StandardOpenOption.TRUNCATE_EXISTING);

        LogRecovery recovery = LogCodec.recover(log);
        assertTrue(recovery.unsafe());
        assertTrue(recovery.error().contains("checksum"));
        assertEquals(0, recovery.lastGoodOffset() - recovery.header().headerLength());
    }
}
