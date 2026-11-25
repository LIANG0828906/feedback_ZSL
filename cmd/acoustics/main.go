package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"roomacoustics/pkg/pipeline"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "verify"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	root := fs.String("root", ".", "workspace root for samples and artifacts")
	addr := fs.String("addr", "127.0.0.1:8088", "local HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()

	switch cmd {
	case "generate":
		spec, levelPath, truthPath, err := pipeline.GenerateSample(*root)
		if err != nil {
			return err
		}
		fmt.Printf("generated seed=%d rooms=%d listeners=%d\nlevel=%s\ntruth=%s\n", spec.Level.Seed, len(spec.Level.Rooms), len(spec.Level.Listeners), levelPath, truthPath)
		return nil
	case "verify":
		report, err := pipeline.RunAcceptance(ctx, *root)
		if err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(*root, "artifacts", "acceptance-report.json"), report); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(map[string]any{
			"ok": report.OK, "message": report.Message, "truth": report.TruthVerification,
			"preview": report.Preview, "recovery": report.Recovery, "full": report.Full, "diff": report.Diff,
		}, "", "  ")
		fmt.Println(string(b))
		if !report.OK {
			return fmt.Errorf("acceptance verification failed: %s", report.Message)
		}
		return nil
	case "serve":
		return runServer(ctx, *root, *addr)
	default:
		return fmt.Errorf("unknown command %q (use generate, verify, or serve)", cmd)
	}
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
