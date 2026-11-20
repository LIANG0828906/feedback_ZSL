package io.edgekv;

public class SnapshotCorruptException extends Exception {
    private static final long serialVersionUID = 1L;

    public SnapshotCorruptException(String message) {
        super(message);
    }

    public SnapshotCorruptException(String message, Throwable cause) {
        super(message, cause);
    }
}
