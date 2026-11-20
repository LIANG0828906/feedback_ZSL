package io.edgekv;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.EOFException;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;

public final class SnapshotCodec {
    private static final int MAGIC = 0x454B5331; // EKS1
    private static final byte FORMAT = 1;
    private static final int MAX_ENTRIES = 1_000_000;
    private static final int MAX_STRING_BYTES = 16 * 1024 * 1024;

    private SnapshotCodec() {
    }

    public static void write(Path target, SnapshotData snapshot) throws IOException {
        byte[] payload = encodePayload(snapshot.entries());
        ByteArrayOutputStream headerBuffer = new ByteArrayOutputStream();
        try (DataOutputStream header = new DataOutputStream(headerBuffer)) {
            header.writeInt(MAGIC);
            header.writeByte(FORMAT);
            header.writeUTF(snapshot.chainId());
            header.writeLong(snapshot.generation());
            header.writeLong(snapshot.snapshotVersion());
            header.writeLong(snapshot.minLiveVersion());
            header.writeInt(payload.length);
            header.write(sha256(payload));
        }
        byte[] headerBytes = headerBuffer.toByteArray();
        byte[] headerChecksum = sha256(headerBytes);
        ByteArrayOutputStream file = new ByteArrayOutputStream(headerBytes.length + 32 + payload.length);
        file.write(headerBytes);
        file.write(headerChecksum);
        file.write(payload);
        FileUtil.writeAtomic(target, file.toByteArray());
    }

    public static SnapshotData read(Path source) throws IOException, SnapshotCorruptException {
        byte[] bytes = Files.readAllBytes(source);
        try {
            ByteArrayInputStream input = new ByteArrayInputStream(bytes);
            DataInputStream in = new DataInputStream(input);
            if (in.readInt() != MAGIC) {
                throw new SnapshotCorruptException("snapshot magic mismatch");
            }
            if (in.readByte() != FORMAT) {
                throw new SnapshotCorruptException("unsupported snapshot format");
            }
            String chainId = in.readUTF();
            long generation = in.readLong();
            long snapshotVersion = in.readLong();
            long minLiveVersion = in.readLong();
            int payloadLength = in.readInt();
            byte[] payloadChecksum = in.readNBytes(32);
            if (payloadChecksum.length != 32) {
                throw new SnapshotCorruptException("snapshot payload checksum is missing");
            }
            int headerLength = input.available();
            byte[] headerBytes = Arrays.copyOfRange(bytes, 0, bytes.length - headerLength);
            byte[] headerChecksum = in.readNBytes(32);
            if (headerChecksum.length != 32 || !MessageDigest.isEqual(headerChecksum, sha256(headerBytes))) {
                throw new SnapshotCorruptException("snapshot header checksum mismatch");
            }
            if (payloadLength < 0 || payloadLength > bytes.length || in.available() != payloadLength) {
                throw new SnapshotCorruptException("snapshot payload length mismatch");
            }
            byte[] payload = in.readNBytes(payloadLength);
            if (!MessageDigest.isEqual(payloadChecksum, sha256(payload))) {
                throw new SnapshotCorruptException("snapshot payload checksum mismatch");
            }
            List<Entry> entries = decodePayload(payload);
            return new SnapshotData(chainId, generation, snapshotVersion, minLiveVersion, entries);
        } catch (EOFException e) {
            throw new SnapshotCorruptException("snapshot is truncated", e);
        } catch (IllegalArgumentException e) {
            throw new SnapshotCorruptException("snapshot record is invalid: " + e.getMessage(), e);
        }
    }

    public static void validate(Path source, Manifest expected) throws IOException, SnapshotCorruptException,
            RepairException {
        SnapshotData snapshot = read(source);
        if (!snapshot.chainId().equals(expected.chainId())) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                    "snapshot chainId does not match manifest");
        }
        if (snapshot.generation() != expected.generation()) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                    "snapshot generation does not match manifest");
        }
        if (snapshot.snapshotVersion() != expected.snapshotVersion()) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                    "snapshot version does not match manifest");
        }
        if (snapshot.minLiveVersion() != expected.minLiveVersion()) {
            throw new RepairException("E_SNAPSHOT_VERSION_CONFLICT",
                    "snapshot minLiveVersion does not match manifest");
        }
    }

    private static byte[] encodePayload(List<Entry> entries) throws IOException {
        ByteArrayOutputStream buffer = new ByteArrayOutputStream();
        try (DataOutputStream out = new DataOutputStream(buffer)) {
            out.writeInt(entries.size());
            for (Entry entry : entries) {
                out.writeUTF(entry.key());
                out.writeUTF(entry.value());
                out.writeLong(entry.version());
                out.writeLong(entry.expireAt());
            }
        }
        return buffer.toByteArray();
    }

    private static List<Entry> decodePayload(byte[] payload) throws IOException {
        try (DataInputStream in = new DataInputStream(new ByteArrayInputStream(payload))) {
            int count = in.readInt();
            if (count < 0 || count > MAX_ENTRIES) {
                throw new IllegalArgumentException("invalid snapshot entry count: " + count);
            }
            List<Entry> entries = new ArrayList<>(count);
            for (int i = 0; i < count; i++) {
                String key = in.readUTF();
                String value = in.readUTF();
                if (key.getBytes(java.nio.charset.StandardCharsets.UTF_8).length > MAX_STRING_BYTES
                        || value.getBytes(java.nio.charset.StandardCharsets.UTF_8).length > MAX_STRING_BYTES) {
                    throw new IllegalArgumentException("snapshot string is too large");
                }
                long version = in.readLong();
                long expireAt = in.readLong();
                entries.add(new Entry(key, value, version, expireAt, false));
            }
            if (in.available() != 0) {
                throw new IllegalArgumentException("trailing bytes in snapshot payload");
            }
            return entries;
        }
    }

    private static byte[] sha256(byte[] bytes) {
        try {
            return MessageDigest.getInstance("SHA-256").digest(bytes);
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException(e);
        }
    }
}
