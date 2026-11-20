package io.edgekv;

@FunctionalInterface
public interface FaultInjector {
    void hit(FaultPoint point) throws SimulatedCrashException;

    static FaultInjector none() {
        return point -> {
        };
    }

    static FaultInjector hardHalt(FaultPoint configured) {
        return point -> {
            if (point == configured) {
                System.err.flush();
                System.out.flush();
                Runtime.getRuntime().halt(137);
            }
        };
    }

    static FaultInjector throwing(FaultPoint configured) {
        return point -> {
            if (point == configured) {
                throw new SimulatedCrashException(point);
            }
        };
    }
}
