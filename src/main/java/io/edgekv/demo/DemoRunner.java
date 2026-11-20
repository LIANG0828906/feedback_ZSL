package io.edgekv.demo;

import io.edgekv.Json;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CompletableFuture;

public final class DemoRunner {
    private final Path root;
    private final String classpath;
    private final Path runtimeRoot;

    private DemoRunner(Path root) {
        this.root = root.toAbsolutePath().normalize();
        this.classpath = System.getProperty("java.class.path");
        this.runtimeRoot = root.resolve(".runtime").resolve("demo");
    }

    public static void main(String[] args) {
        Path root = Path.of("");
        for (int i = 0; i < args.length - 1; i++) {
            if ("--root".equals(args[i])) {
                root = Path.of(args[i + 1]);
            }
        }
        try {
            new DemoRunner(root).run();
            System.out.flush();
        } catch (Exception e) {
            System.err.println("{\"demo\":\"FAIL\",\"message\":" + Json.quote(e.getMessage()) + "}");
            System.exit(1);
        }
    }

    private void run() throws Exception {
        baseline();
        safeLogTail();
        logAppendFault();
        snapshotWriteFault();
        snapshotReplaceFault();
        logTruncateFault();
        unsafeLogRepair();
        snapshotConflictRepair();
        System.out.println("{\"demo\":\"PASS\",\"scenarios\":8,\"faultPoints\":4}");
    }

    private void baseline() throws Exception {
        String data = ".runtime/demo/baseline";
        prepare(data);
        String report = invokeOk("report", "--data", data);
        String summary = invokeOk("summary", "--data", data, "--now", "1700000100");
        require(report, "\"replayedRecords\":5", "baseline replay count");
        require(report, "\"finalVersion\":5", "baseline final version");
        require(summary, "\"live\":2", "baseline live count");
        require(summary, "\"expired\":1", "baseline expired count");
        require(summary, "\"deleted\":1", "baseline deleted count");
        emit("baseline", report, summary);
    }

    private void safeLogTail() throws Exception {
        String data = ".runtime/demo/safe-log-tail";
        prepare(data);
        invokeOk("fixture", "--data", data, "--kind", "truncate-active-log", "--bytes", "5");
        String report = invokeOk("report", "--data", data);
        String summary = invokeOk("summary", "--data", data, "--now", "1700000100");
        require(report, "\"skippedTruncatedBytes\":", "safe truncation byte count");
        require(report, "\"finalVersion\":4", "safe truncation final version");
        require(summary, "\"stable-node\"", "safe truncation summary");
        emit("safe-log-tail", report, summary);
    }

    private void logAppendFault() throws Exception {
        String data = ".runtime/demo/fault-log-append";
        prepare(data);
        ProcessResult crash = invoke("put", "--data", data, "--key", "faulted-node",
                "--value", "durable", "--fault-at", "log_append");
        requireExit(crash, 137, "log append fault");
        String report = invokeOk("report", "--data", data);
        String summary = invokeOk("summary", "--data", data, "--now", "1700000100");
        require(report, "\"finalVersion\":6", "log append recovered version");
        require(report, "\"replayedRecords\":6", "log append recovered records");
        require(summary, "\"faulted-node\"", "log append record content");
        emit("fault-log-append", report, summary);
    }

    private void snapshotWriteFault() throws Exception {
        String data = ".runtime/demo/fault-snapshot-write";
        prepare(data);
        ProcessResult crash = invoke("compact", "--data", data, "--now", "1700000100",
                "--fault-at", "snapshot_write");
        requireExit(crash, 137, "snapshot write fault");
        String report = invokeOk("report", "--data", data);
        require(report, "\"generation\":1", "snapshot write old generation");
        require(report, "\"snapshotLoaded\":false", "snapshot write old snapshot view");
        require(report, "\"replayedRecords\":5", "snapshot write full log replay");
        require(report, "\"finalVersion\":5", "snapshot write final version");
        emit("fault-snapshot-write", report, null);
    }

    private void snapshotReplaceFault() throws Exception {
        String data = ".runtime/demo/fault-snapshot-replace";
        prepare(data);
        ProcessResult crash = invoke("compact", "--data", data, "--now", "1700000100",
                "--fault-at", "snapshot_replace");
        requireExit(crash, 137, "snapshot replace fault");
        String report = invokeOk("report", "--data", data);
        require(report, "\"generation\":2", "snapshot replace new generation");
        require(report, "\"snapshotLoaded\":true", "snapshot replace new snapshot view");
        require(report, "\"replayedRecords\":0", "snapshot replace empty log");
        require(report, "\"finalVersion\":5", "snapshot replace final version");
        emit("fault-snapshot-replace", report, null);
    }

    private void logTruncateFault() throws Exception {
        String data = ".runtime/demo/fault-log-truncate";
        prepare(data);
        ProcessResult crash = invoke("compact", "--data", data, "--now", "1700000100",
                "--fault-at", "log_truncate");
        requireExit(crash, 137, "log truncate fault");
        String report = invokeOk("report", "--data", data);
        require(report, "\"generation\":2", "log truncate new generation");
        require(report, "\"snapshotLoaded\":true", "log truncate snapshot view");
        require(report, "\"finalVersion\":5", "log truncate final version");
        if (!Files.exists(root.resolve(data).resolve("logs/archive/log-1.elog"))) {
            throw new IllegalStateException("archived old log is missing after log truncate fault");
        }
        emit("fault-log-truncate", report, null);
    }

    private void unsafeLogRepair() throws Exception {
        String data = ".runtime/demo/unsafe-log-repair";
        prepare(data);
        invokeOk("fixture", "--data", data, "--kind", "corrupt-active-log");
        ProcessResult failed = invoke("report", "--data", data);
        requireExit(failed, 3, "unsafe log detection");
        require(failed.stderr, "\"code\":\"E_LOG_TRUNCATED\"", "unsafe log error code");
        require(failed.stderr, "\"backup\":\"repair/backup-001\"", "unsafe log backup");
        ProcessResult repaired = invoke("repair", "--data", data);
        requireExit(repaired, 0, "unsafe log repair");
        require(repaired.stdout, "\"originalCode\":\"E_LOG_TRUNCATED\"", "unsafe log repair code");
        String report = invokeOk("report", "--data", data);
        require(report, "\"finalVersion\":4", "unsafe log repaired version");
        emit("repair-log-truncated", failed.stderr, report);
    }

    private void snapshotConflictRepair() throws Exception {
        String data = ".runtime/demo/snapshot-conflict-repair";
        prepare(data);
        invokeOk("compact", "--data", data, "--now", "1700000100");
        invokeOk("fixture", "--data", data, "--kind", "truncate-snapshot", "--bytes", "2");
        ProcessResult failed = invoke("report", "--data", data);
        requireExit(failed, 3, "snapshot conflict detection");
        require(failed.stderr, "\"code\":\"E_SNAPSHOT_VERSION_CONFLICT\"", "snapshot conflict error code");
        ProcessResult repaired = invoke("repair", "--data", data);
        requireExit(repaired, 0, "snapshot conflict repair");
        require(repaired.stdout, "\"originalCode\":\"E_SNAPSHOT_VERSION_CONFLICT\"",
                "snapshot conflict repair code");
        String report = invokeOk("report", "--data", data);
        require(report, "\"finalVersion\":5", "snapshot conflict repaired version");
        emit("repair-snapshot-conflict", failed.stderr, report);
    }

    private void prepare(String data) throws Exception {
        ProcessResult reset = invoke("reset", "--data", data);
        requireExit(reset, 0, "reset " + data);
        ProcessResult sample = invoke("sample", "--data", data);
        requireExit(sample, 0, "sample " + data);
    }

    private String invokeOk(String... args) throws Exception {
        ProcessResult result = invoke(args);
        requireExit(result, 0, String.join(" ", args));
        return result.stdout.strip();
    }

    private ProcessResult invoke(String... args) throws Exception {
        List<String> command = new ArrayList<>();
        command.add(javaExecutable());
        command.add("-cp");
        command.add(classpath);
        command.add("io.edgekv.Main");
        command.addAll(List.of(args));
        Process process = new ProcessBuilder(command)
                .directory(root.toFile())
                .redirectErrorStream(false)
                .start();
        CompletableFuture<String> stdout = read(process.getInputStream());
        CompletableFuture<String> stderr = read(process.getErrorStream());
        int exit = process.waitFor();
        return new ProcessResult(exit, stdout.join(), stderr.join());
    }

    private static CompletableFuture<String> read(java.io.InputStream stream) {
        return CompletableFuture.supplyAsync(() -> {
            try (stream) {
                return new String(stream.readAllBytes(), StandardCharsets.UTF_8);
            } catch (IOException e) {
                throw new IllegalStateException(e);
            }
        });
    }

    private static String javaExecutable() {
        Path java = Path.of(System.getProperty("java.home"), "bin",
                isWindows() ? "java.exe" : "java");
        return java.toString();
    }

    private static boolean isWindows() {
        return System.getProperty("os.name", "").toLowerCase().contains("win");
    }

    private static void requireExit(ProcessResult result, int expected, String label) {
        if (result.exitCode != expected) {
            throw new IllegalStateException(label + " exit code " + result.exitCode
                    + " (expected " + expected + "), stderr=" + result.stderr.strip());
        }
    }

    private static void require(String content, String needle, String label) {
        if (!content.contains(needle)) {
            throw new IllegalStateException(label + " missing " + needle + " in " + content);
        }
    }

    private static void emit(String scenario, String primary, String secondary) {
        StringBuilder out = new StringBuilder("{\"scenario\":").append(Json.quote(scenario));
        out.append(",\"primary\":").append(primary.strip());
        if (secondary != null) {
            out.append(",\"secondary\":").append(secondary.strip());
        }
        System.out.println(out.append('}'));
    }

    private record ProcessResult(int exitCode, String stdout, String stderr) {
    }
}
