package io.edgekv;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

public final class Main {
    private static final Path DEFAULT_DATA = Path.of(".edgekv-data");
    private static final Path DEFAULT_SAMPLE = Path.of("samples", "fixed-sample.tsv");

    private Main() {
    }

    public static void main(String[] args) {
        int exitCode;
        try {
            exitCode = dispatch(args);
        } catch (RepairException e) {
            Path data = dataPathFrom(args);
            RepairReport report;
            try {
                report = new RepairManager(data).preserveOriginals(e.code(), e.getMessage());
            } catch (RuntimeException backupFailure) {
                report = new RepairReport("REPAIR_REQUIRED", e.code(),
                        e.getMessage() + "; backup failed: " + backupFailure.getMessage(), null);
            }
            System.err.println(report.toJson());
            exitCode = 3;
        } catch (SimulatedCrashException e) {
            System.err.println("{\"status\":\"SIMULATED_CRASH\",\"stage\":"
                    + Json.quote(e.point().name()) + "}");
            exitCode = 137;
        } catch (IllegalArgumentException e) {
            System.err.println("{\"status\":\"USAGE_ERROR\",\"message\":" + Json.quote(e.getMessage()) + "}");
            exitCode = 2;
        } catch (Exception e) {
            System.err.println("{\"status\":\"ERROR\",\"message\":" + Json.quote(e.getMessage()) + "}");
            exitCode = 1;
        }
        if (exitCode != 0) {
            System.exit(exitCode);
        }
    }

    private static int dispatch(String[] args) throws Exception {
        if (args.length == 0 || args[0].equals("help") || args[0].equals("--help") || args[0].equals("-h")) {
            System.out.println(help());
            return 0;
        }
        String command = args[0].toLowerCase();
        Map<String, String> options = parseOptions(args);
        Path data = Path.of(options.getOrDefault("data", DEFAULT_DATA.toString()));

        return switch (command) {
            case "init", "start" -> runInit(data, options);
            case "put" -> runPut(data, options);
            case "get" -> runGet(data, options);
            case "lookup" -> runLookup(data, options);
            case "delete" -> runDelete(data, options);
            case "expired" -> runExpired(data, options);
            case "compact" -> runCompact(data, options);
            case "report" -> runReport(data, options);
            case "export-report" -> runExportReport(data, options);
            case "summary" -> runSummary(data, options);
            case "sample" -> runSample(data, options);
            case "shell" -> runShell(data, options);
            case "repair" -> runRepair(data);
            case "fixture" -> runFixture(data, options);
            case "reset" -> runReset(data);
            default -> throw new IllegalArgumentException("unknown command: " + command + System.lineSeparator() + help());
        };
    }

    private static int runInit(Path data, Map<String, String> options) {
        try (EdgeKvStore store = open(data, options)) {
            System.out.println(store.recoveryReport().toJson());
            return 0;
        }
    }

    private static int runPut(Path data, Map<String, String> options) {
        String key = required(options, "key");
        String value = required(options, "value");
        long expireAt = longOption(options, "expire-at", 0);
        try (EdgeKvStore store = open(data, options)) {
            long version = store.put(key, value, expireAt);
            System.out.println("{\"ok\":true,\"key\":" + Json.quote(key)
                    + ",\"version\":" + version
                    + ",\"expireAt\":" + expireAt
                    + ",\"finalVersion\":" + store.version() + "}");
            return 0;
        }
    }

    private static int runGet(Path data, Map<String, String> options) {
        String key = required(options, "key");
        long now = requiredLong(options, "now");
        try (EdgeKvStore store = open(data, options)) {
            QueryResult result = store.lookup(key, now);
            System.out.println(Json.queryObject(result));
            return 0;
        }
    }

    private static int runLookup(Path data, Map<String, String> options) {
        return runGet(data, options);
    }

    private static int runDelete(Path data, Map<String, String> options) {
        String key = required(options, "key");
        try (EdgeKvStore store = open(data, options)) {
            long version = store.delete(key);
            System.out.println("{\"ok\":true,\"key\":" + Json.quote(key)
                    + ",\"deleted\":true,\"version\":" + version
                    + ",\"finalVersion\":" + store.version() + "}");
            return 0;
        }
    }

    private static int runExpired(Path data, Map<String, String> options) {
        long now = requiredLong(options, "now");
        try (EdgeKvStore store = open(data, options)) {
            List<QueryResult> expired = store.expired(now);
            System.out.println(Json.array(expired, item -> Json.queryObject((QueryResult) item)));
            return 0;
        }
    }

    private static int runCompact(Path data, Map<String, String> options) {
        long now = requiredLong(options, "now");
        try (EdgeKvStore store = open(data, options)) {
            CompactionResult result = store.compact(now);
            System.out.println("{\"ok\":true,\"compaction\":" + result.toJson()
                    + ",\"recovery\":" + store.recoveryReport().toJson() + "}");
            return 0;
        }
    }

    private static int runReport(Path data, Map<String, String> options) {
        try (EdgeKvStore store = open(data, options)) {
            System.out.println(store.recoveryReport().toJson());
            return 0;
        }
    }

    private static int runExportReport(Path data, Map<String, String> options) {
        try (EdgeKvStore store = open(data, options)) {
            Path out = Path.of(options.getOrDefault("out",
                    data.resolve("reports").resolve("recovery.json").toString()));
            store.exportRecoveryReport(out);
            System.out.println("{\"ok\":true,\"report\":" + Json.quote(out.toString())
                    + ",\"recovery\":" + store.recoveryReport().toJson() + "}");
            return 0;
        } catch (IOException e) {
            throw new IllegalStateException("cannot export recovery report: " + e.getMessage(), e);
        }
    }

    private static int runSummary(Path data, Map<String, String> options) {
        long now = requiredLong(options, "now");
        try (EdgeKvStore store = open(data, options)) {
            System.out.println(Summary.of(store, now));
            return 0;
        }
    }

    private static int runSample(Path data, Map<String, String> options) {
        Path sample = Path.of(options.getOrDefault("file", DEFAULT_SAMPLE.toString()));
        try {
            SampleData sampleData = SampleData.load(sample);
            try (EdgeKvStore store = open(data, options)) {
                List<Long> versions = sampleData.applyTo(store);
                System.out.println("{\"ok\":true,\"sample\":" + Json.quote(sample.toString())
                        + ",\"appliedVersions\":" + Json.array(versions, item -> item.toString())
                        + ",\"summary\":" + Summary.of(store, sampleData.logicalNow()) + "}");
                return 0;
            }
        } catch (IOException e) {
            throw new IllegalStateException("cannot load sample: " + e.getMessage(), e);
        }
    }

    private static int runShell(Path data, Map<String, String> options) {
        long now = longOption(options, "now", 0);
        return InteractiveShell.run(data, now, faultInjector(options),
                InteractiveShell.InputStreamSource.stdin(), System.out, System.err);
    }

    private static int runRepair(Path data) {
        try {
            RepairOutcome outcome = new RepairManager(data).repair();
            System.out.println(outcome.toJson());
            return 0;
        } catch (RepairException e) {
            System.err.println("{\"status\":\"REPAIR_FAILED\",\"code\":" + Json.quote(e.code())
                    + ",\"message\":" + Json.quote(e.getMessage()) + "}");
            return 4;
        }
    }

    private static int runFixture(Path data, Map<String, String> options) throws IOException {
        String kind = required(options, "kind");
        StorePaths paths = StorePaths.of(data);
        Manifest manifest = Manifest.read(paths.manifest());
        if (manifest == null) {
            throw new IllegalArgumentException("store is not initialized: " + data);
        }
        Path logPath = paths.resolveDataFile(manifest.logFile());
        switch (kind) {
            case "truncate-active-log" -> {
                long bytes = longOption(options, "bytes", 1);
                long size = Files.size(logPath);
                if (bytes < 0 || bytes > size) {
                    throw new IllegalArgumentException("invalid truncate byte count: " + bytes);
                }
                FileUtil.truncate(logPath, size - bytes);
                System.out.println("{\"ok\":true,\"fixture\":\"truncate-active-log\""
                        + ",\"bytes\":" + bytes + ",\"size\":" + (size - bytes) + "}");
            }
            case "append-log-tail" -> {
                int bytes = (int) longOption(options, "bytes", 2);
                if (bytes < 1 || bytes > 32) {
                    throw new IllegalArgumentException("bytes must be between 1 and 32");
                }
                byte[] tail = new byte[bytes];
                for (int i = 0; i < bytes; i++) {
                    tail[i] = i == 0 ? (byte) 0x45 : i == 1 ? (byte) 0x4B : 0;
                }
                Files.write(logPath, tail, StandardOpenOption.APPEND);
                FileUtil.fsync(logPath);
                System.out.println("{\"ok\":true,\"fixture\":\"append-log-tail\",\"bytes\":" + bytes + "}");
            }
            case "corrupt-active-log" -> {
                long size = Files.size(logPath);
                if (size < 13) {
                    throw new IllegalArgumentException("log is too small to corrupt");
                }
                byte[] all = Files.readAllBytes(logPath);
                all[all.length - 5] ^= 0x5A;
                Files.write(logPath, all, StandardOpenOption.TRUNCATE_EXISTING);
                FileUtil.fsync(logPath);
                System.out.println("{\"ok\":true,\"fixture\":\"corrupt-active-log\"}");
            }
            case "truncate-snapshot" -> {
                if (manifest.snapshotFile() == null) {
                    throw new IllegalArgumentException("store has no snapshot");
                }
                Path snapshot = paths.resolveDataFile(manifest.snapshotFile());
                long bytes = longOption(options, "bytes", 1);
                long size = Files.size(snapshot);
                if (bytes < 0 || bytes > size) {
                    throw new IllegalArgumentException("invalid truncate byte count: " + bytes);
                }
                FileUtil.truncate(snapshot, size - bytes);
                System.out.println("{\"ok\":true,\"fixture\":\"truncate-snapshot\""
                        + ",\"bytes\":" + bytes + ",\"size\":" + (size - bytes) + "}");
            }
            default -> throw new IllegalArgumentException("unknown fixture: " + kind);
        }
        return 0;
    }

    private static int runReset(Path data) throws IOException {
        Path absolute = data.toAbsolutePath().normalize();
        if (absolute.getNameCount() < 2 || absolute.getParent() == null) {
            throw new IllegalArgumentException("refusing to reset unsafe path: " + absolute);
        }
        FileUtil.deleteRecursively(absolute);
        System.out.println("{\"ok\":true,\"reset\":" + Json.quote(absolute.toString()) + "}");
        return 0;
    }

    private static EdgeKvStore open(Path data, Map<String, String> options) {
        return EdgeKvStore.open(data, faultInjector(options));
    }

    private static FaultInjector faultInjector(Map<String, String> options) {
        String raw = options.get("fault-at");
        FaultPoint point = FaultPoint.parse(raw);
        return point == null ? FaultInjector.none() : FaultInjector.hardHalt(point);
    }

    private static Map<String, String> parseOptions(String[] args) {
        Map<String, String> options = new LinkedHashMap<>();
        for (int i = 1; i < args.length; i++) {
            String arg = args[i];
            if (!arg.startsWith("--")) {
                throw new IllegalArgumentException("unexpected argument: " + arg);
            }
            String name = arg.substring(2);
            if (name.isEmpty()) {
                throw new IllegalArgumentException("empty option name");
            }
            if (i + 1 >= args.length || args[i + 1].startsWith("--")) {
                throw new IllegalArgumentException("option requires a value: --" + name);
            }
            options.put(name, args[++i]);
        }
        return options;
    }

    private static String required(Map<String, String> options, String name) {
        String value = options.get(name);
        if (value == null || value.isBlank()) {
            throw new IllegalArgumentException("missing required option: --" + name);
        }
        return value;
    }

    private static long requiredLong(Map<String, String> options, String name) {
        String raw = required(options, name);
        try {
            return Long.parseLong(raw);
        } catch (NumberFormatException e) {
            throw new IllegalArgumentException("option --" + name + " must be an integer: " + raw);
        }
    }

    private static long longOption(Map<String, String> options, String name, long defaultValue) {
        String raw = options.get(name);
        if (raw == null) {
            return defaultValue;
        }
        try {
            return Long.parseLong(raw);
        } catch (NumberFormatException e) {
            throw new IllegalArgumentException("option --" + name + " must be an integer: " + raw);
        }
    }

    private static Path dataPathFrom(String[] args) {
        for (int i = 1; i < args.length - 1; i++) {
            if ("--data".equals(args[i])) {
                return Path.of(args[i + 1]);
            }
        }
        return DEFAULT_DATA;
    }

    private static String help() {
        return """
                EdgeKV Index - crash-consistent local key/value index

                Usage:
                  edgekv <command> --data <dir> [options]

                Commands:
                  init|start       initialize/recover and print the recovery report
                  put              --key <k> --value <v> [--expire-at <epoch>]
                  get|lookup       --key <k> --now <epoch>
                  delete           --key <k>
                  expired          --now <epoch>
                  compact          --now <epoch>
                  report           print startup recovery summary as JSON
                  export-report    [--out <file>]
                  summary          --now <epoch>
                  sample           [--file <fixed-sample.tsv>]
                  shell            [--now <epoch>]
                  repair           explicit repair using preserved original files
                  reset            reset a data directory (demo utility)
                  fixture          test fixture: --kind truncate-active-log|append-log-tail|corrupt-active-log|truncate-snapshot

                Fault injection:
                  --fault-at after-log-append|after-snapshot-write|after-snapshot-replace|after-log-truncate
                  The process halts with exit code 137 after the selected durable stage.

                Expiration always uses the supplied --now value. System wall-clock time is never used.
                """.strip();
    }
}
