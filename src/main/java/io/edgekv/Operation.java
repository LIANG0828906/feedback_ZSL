package io.edgekv;

public enum Operation {
    PUT(1),
    DELETE(2);

    private final int code;

    Operation(int code) {
        this.code = code;
    }

    public int code() {
        return code;
    }

    public static Operation fromCode(int code) {
        for (Operation operation : values()) {
            if (operation.code == code) {
                return operation;
            }
        }
        throw new IllegalArgumentException("unknown operation code: " + code);
    }
}
