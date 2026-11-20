package io.edgekv;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.EOFException;
import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.zip.CRC32;

public final class LogCodec {
    private static final int LOG_MAGIC = 0x454B4C31; // EKL1
    private static final int RECORD_MAGIC = 0x454B5231; // EKR1
    private static final byte FORMAT = 1;
    private static final int MAX_RECORD_PAYLOAD = 64 * 1024 * 1024;
    private static final int MAX_STRING_BYTES = 16 * 1024 * 1024;

    private LogCodec() {
    }

    public static LogHeader create(Path path, String chainId, long generation) throws IOException {
        byte[] headerBytes = encodeHeaderPrefix(chainId, generation);
        CRC32 crc = new CRC32();
        crc.update(headerBytes);
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        output.write(headerBytes);
        writeInt(output, (int) crc.getValue());
        FileUtil.writeAtomic(path, output.toByteArray());
        return new LogHeader(chainId, generation, output.size());
    }

    public static byte[] encodeRecord(LogRecord record) throws IOException {
        ByteArrayOutputStream payloadBuffer = new ByteArrayOutputStream();
        try (DataOutputStream payload = new DataOutputStream(payloadBuffer)) {
            payload.writeByte(record.operation().code());
            payload.writeLong(record.version());
            payload.writeLong(record.expireAt());
            payload.writeUTF(record.key());
            payload.writeUTF(record.value());
        }
        byte[] payload = payloadBuffer.toByteArray();
        CRC32 crc = new CRC32();
        crc.update(payload);
        ByteArrayOutputStream frame = new ByteArrayOutputStream(payload.length + 12);
        writeInt(frame, RECORD_MAGIC);
        writeInt(frame, payload.length);
        frame.write(payload);
        writeInt(frame, (int) crc.getValue());
        return frame.toByteArray();
    }

    public static LogRecovery recover(Path path) throws IOException {
        byte[] bytes = Files.readAllBytes(path);
        HeaderResult headerResult = readHeader(bytes);
        if (headerResult.error != null) {
            return new LogRecovery(headerResult.header, List.of(), 0, bytes.length, true, headerResult.error);
        }
        int offset = headerResult.header.headerLength();
        long lastGood = offset;
        List<LogRecord> records = new ArrayList<>();
        while (offset < bytes.length) {
            int remaining = bytes.length - offset;
            if (remaining < Integer.BYTES) {
                return safeTail(headerResult.header, records, lastGood, remaining);
            }
            int magic = readInt(bytes, offset);
            if (magic != RECORD_MAGIC) {
                return unsafe(headerResult.header, records, lastGood, remaining,
                        "record magic mismatch at offset " + offset);
            }
            if (remaining < Integer.BYTES * 2) {
                return safeTail(headerResult.header, records, lastGood, remaining);
            }
            int payloadLength = readInt(bytes, offset + 4);
            if (payloadLength < 0 || payloadLength > MAX_RECORD_PAYLOAD) {
                return unsafe(headerResult.header, records, lastGood, remaining,
                        "invalid record length at offset " + offset);
            }
            int frameLength = 8 + payloadLength + 4;
            if (remaining < frameLength) {
                return safeTail(headerResult.header, records, lastGood, remaining);
            }
            int payloadOffset = offset + 8;
            int expectedCrc = readInt(bytes, payloadOffset + payloadLength);
            CRC32 crc = new CRC32();
            crc.update(bytes, payloadOffset, payloadLength);
            if ((int) crc.getValue() != expectedCrc) {
                return unsafe(headerResult.header, records, lastGood, remaining,
                        "record checksum mismatch at offset " + offset);
            }
            try {
                records.add(decodePayload(bytes, payloadOffset, payloadLength));
            } catch (IOException | RuntimeException e) {
                return unsafe(headerResult.header, records, lastGood, remaining,
                        "record payload is invalid at offset " + offset + ": " + e.getMessage());
            }
            offset += frameLength;
            lastGood = offset;
        }
        return new LogRecovery(headerResult.header, records, lastGood, 0, false, null);
    }

    private static LogRecovery safeTail(LogHeader header, List<LogRecord> records, long lastGood, long skipped) {
        return new LogRecovery(header, records, lastGood, skipped, false, null);
    }

    private static LogRecovery unsafe(LogHeader header, List<LogRecord> records, long lastGood,
                                      long remaining, String error) {
        return new LogRecovery(header, records, lastGood, remaining, true, error);
    }

    private static LogRecord decodePayload(byte[] bytes, int offset, int length) throws IOException {
        try (DataInputStream in = new DataInputStream(new ByteArrayInputStream(bytes, offset, length))) {
            Operation operation = Operation.fromCode(in.readUnsignedByte());
            long version = in.readLong();
            long expireAt = in.readLong();
            String key = in.readUTF();
            String value = in.readUTF();
            if (version <= 0) {
                throw new IllegalArgumentException("record version must be positive");
            }
            if (key.getBytes(StandardCharsets.UTF_8).length > MAX_STRING_BYTES
                    || value.getBytes(StandardCharsets.UTF_8).length > MAX_STRING_BYTES) {
                throw new IllegalArgumentException("record string is too large");
            }
            if (in.available() != 0) {
                throw new IllegalArgumentException("trailing bytes in record payload");
            }
            return new LogRecord(operation, key, value, version, expireAt);
        } catch (EOFException e) {
            throw new IOException("truncated record payload", e);
        }
    }

    private static HeaderResult readHeader(byte[] bytes) {
        try (DataInputStream in = new DataInputStream(new ByteArrayInputStream(bytes))) {
            if (in.readInt() != LOG_MAGIC) {
                return new HeaderResult(null, "log magic mismatch");
            }
            if (in.readByte() != FORMAT) {
                return new HeaderResult(null, "unsupported log format");
            }
            String chainId = in.readUTF();
            long generation = in.readLong();
            int crcOffset = bytes.length - in.available();
            int expectedCrc = in.readInt();
            CRC32 crc = new CRC32();
            crc.update(bytes, 0, crcOffset);
            if ((int) crc.getValue() != expectedCrc) {
                return new HeaderResult(null, "log header checksum mismatch");
            }
            int headerLength = bytes.length - in.available();
            return new HeaderResult(new LogHeader(chainId, generation, headerLength), null);
        } catch (EOFException e) {
            return new HeaderResult(null, "log header is truncated");
        } catch (IOException e) {
            return new HeaderResult(null, "log header is invalid: " + e.getMessage());
        }
    }

    private static byte[] encodeHeaderPrefix(String chainId, long generation) throws IOException {
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        try (DataOutputStream out = new DataOutputStream(output)) {
            out.writeInt(LOG_MAGIC);
            out.writeByte(FORMAT);
            out.writeUTF(chainId);
            out.writeLong(generation);
        }
        return output.toByteArray();
    }

    private static int readInt(byte[] bytes, int offset) {
        return ByteBuffer.wrap(bytes, offset, Integer.BYTES).getInt();
    }

    private static void writeInt(ByteArrayOutputStream output, int value) {
        output.write((value >>> 24) & 0xFF);
        output.write((value >>> 16) & 0xFF);
        output.write((value >>> 8) & 0xFF);
        output.write(value & 0xFF);
    }

    private record HeaderResult(LogHeader header, String error) {
    }
}
