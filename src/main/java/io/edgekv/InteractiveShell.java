package io.edgekv;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.List;

public final class InteractiveShell {
    private final Path dataDir;
    private final long initialNow;
    private final FaultInjector faultInjector;
    private final BufferedReader input;
    private final PrintStream out;
    private final PrintStream err;
    private long now;

    public InteractiveShell(Path dataDir, long initialNow, FaultInjector faultInjector,
                            BufferedReader input, PrintStream out, PrintStream err) {
        this.dataDir = dataDir;
        this.initialNow = initialNow;
        this.faultInjector = faultInjector;
        this.input = input;
        this.out = out;
        this.err = err;
        this.now = initialNow;
    }

    public int run() {
        try (EdgeKvStore store = EdgeKvStore.open(dataDir, faultInjector)) {
            out.println("{\"status\":\"READY\",\"data\":" + Json.quote(dataDir.toString())
                    + ",\"now\":" + now + ",\"recovery\":" + store.recoveryReport().toJson() + "}");
            String line;
            while ((line = input.readLine()) != null) {
                line = line.strip();
                if (line.isEmpty()) {
                    continue;
                }
                if (!execute(store, line)) {
                    break;
                }
            }
            return 0;
        } catch (RepairException e) {
            RepairReport report = new RepairManager(dataDir).preserveOriginals(e.code(), e.getMessage());
            err.println(report.toJson());
            return 3;
        } catch (IOException e) {
            err.println("{\"status\":\"ERROR\",\"message\":" + Json.quote(e.getMessage()) + "}");
            return 2;
        }
    }

    public static int run(Path dataDir, long initialNow, FaultInjector faultInjector,
                          InputStreamSource source, PrintStream out, PrintStream err) {
        return new InteractiveShell(dataDir, initialNow, faultInjector,
                source.reader(), out, err).run();
    }

    private boolean execute(EdgeKvStore store, String line) {
        String[] parts = line.split("\\s+");
        String command = parts[0].toLowerCase();
        try {
            switch (command) {
                case "help" -> out.println(helpText());
                case "put" -> {
                    requireArgs(parts, 3, "put <key> <value> [expireAt]");
                    long expireAt = parts.length >= 4 ? Long.parseLong(parts[3]) : 0;
                    long version = store.put(parts[1], parts[2], expireAt);
                    out.println("{\"ok\":true,\"key\":" + Json.quote(parts[1])
                            + ",\"version\":" + version + "}");
                }
                case "get" -> {
                    requireArgs(parts, 2, "get <key>");
                    out.println(Json.queryObject(store.lookup(parts[1], now)));
                }
                case "delete" -> {
                    requireArgs(parts, 2, "delete <key>");
                    long version = store.delete(parts[1]);
                    out.println("{\"ok\":true,\"key\":" + Json.quote(parts[1])
                            + ",\"deleted\":true,\"version\":" + version + "}");
                }
                case "expired" -> out.println(Json.array(store.expired(now),
                        item -> Json.queryObject((QueryResult) item)));
                case "compact" -> out.println(store.compact(now).toJson());
                case "report" -> out.println(store.recoveryReport().toJson());
                case "summary" -> out.println(Summary.of(store, now));
                case "now" -> {
                    requireArgs(parts, 2, "now <epoch>");
                    long parsed = Long.parseLong(parts[1]);
                    if (parsed < 0) {
                        throw new IllegalArgumentException("now must not be negative");
                    }
                    now = parsed;
                    out.println("{\"ok\":true,\"now\":" + now + "}");
                }
                case "version" -> out.println("{\"version\":" + store.version() + "}");
                case "quit", "exit" -> {
                    out.println("{\"status\":\"CLOSED\"}");
                    return false;
                }
                default -> out.println("{\"status\":\"ERROR\",\"message\":"
                        + Json.quote("unknown command: " + command) + "}");
            }
        } catch (RuntimeException e) {
            out.println("{\"status\":\"ERROR\",\"message\":" + Json.quote(e.getMessage()) + "}");
        }
        return true;
    }

    private static void requireArgs(String[] parts, int count, String usage) {
        if (parts.length < count) {
            throw new IllegalArgumentException("usage: " + usage);
        }
    }

    private static String helpText() {
        return """
                commands:
                  put <key> <value> [expireAt]
                  get <key>
                  delete <key>
                  expired
                  compact
                  report
                  summary
                  now <epoch>
                  version
                  help
                  quit
                """.strip();
    }

    @FunctionalInterface
    public interface InputStreamSource {
        BufferedReader reader();

        static InputStreamSource stdin() {
            return () -> new BufferedReader(new InputStreamReader(System.in, StandardCharsets.UTF_8));
        }
    }
}
