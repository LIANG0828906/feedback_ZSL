package io.edgekv;

public class SimulatedCrashException extends RuntimeException {
    private static final long serialVersionUID = 1L;

    private final FaultPoint point;

    public SimulatedCrashException(FaultPoint point) {
        super("simulated process termination after " + point);
        this.point = point;
    }

    public FaultPoint point() {
        return point;
    }
}
