package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	fog "fogkernel"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
	case "serve", "workbench":
		os.Exit(runServe(os.Args[2:]))
	case "checkpoint":
		os.Exit(runCheckpoint(os.Args[2:]))
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "export":
		os.Exit(runExport(os.Args[2:]))
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`fogctl - deterministic volume-fog kernel and diagnostic workbench

Usage:
  fogctl verify [--json]
  fogctl serve [--addr 127.0.0.1:18080]
  fogctl demo
  fogctl export --out artifacts/generated
  fogctl checkpoint --db ./fog.sqlite [--resume] [--stop-after-rays N] [--sigkill-after-blocks N]

All samples and truth are generated in-process from a fixed seed.`)
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bundle, err := fog.GenerateDefaultBundle()
	if err != nil {
		return fail(err)
	}
	report, err := fog.VerifyGeneratedBundle(bundle)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Printf("verification ok=%v rays=%d hits=%d exactMaxTError=%.3e previewMax=%.6f/%.3f diagnostic=%v terminals=%d/%d\n",
			report.OK, report.RayCount, report.HitCount, report.ExactMaxTError,
			report.PreviewMaxError, report.PreviewBound, report.DiagnosticReplays,
			report.TerminalMatches, report.RayCount)
		if report.CrossVersion != nil {
			fmt.Printf("cross-version deepEqual=%v maxTauDiffQ=%d maxTDiffQ=%d\n",
				report.CrossVersion.DeepEqual, report.CrossVersion.MaxTauDiffQ, report.CrossVersion.MaxTDiffQ)
		}
		for _, v := range report.Violations {
			fmt.Printf("VIOLATION %s %s ray=%s block=%d field=%s %s\n", v.Code, v.Message, v.RayID, v.BlockIndex, v.Field, v.Message)
		}
	}
	if !report.OK {
		return 1
	}
	return 0
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:18080", "listen address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	wb, err := fog.NewWorkbench()
	if err != nil {
		return fail(err)
	}
	server := &http.Server{Addr: *addr, Handler: wb.Handler()}
	fmt.Printf("fog diagnostic workbench: http://%s\n", *addr)
	fmt.Println("press Ctrl+C or send SIGTERM for a durable graceful checkpoint stop")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fail(err)
	}
	return 0
}

func runDemo(args []string) int {
	fmt.Println("Generating fixed-seed scene, three schema documents and analytic truth...")
	return runVerify([]string{})
}

func runExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("out", "artifacts/generated", "output directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bundle, err := fog.GenerateDefaultBundle()
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	files := map[string][]byte{
		"project-v1.json": bundle.Docs[fog.SchemaV1],
		"project-v2.json": bundle.Docs[fog.SchemaV2],
		"project-v3.json": bundle.Docs[fog.SchemaV3],
	}
	truth, err := json.MarshalIndent(bundle.Truth, "", "  ")
	if err != nil {
		return fail(err)
	}
	files["truth.json"] = truth
	audits, err := json.MarshalIndent(bundle.ConversionAudits, "", "  ")
	if err != nil {
		return fail(err)
	}
	files["conversion-audits.json"] = audits
	for name, data := range files {
		path := *out + string(os.PathSeparator) + name
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fail(err)
		}
		fmt.Println("wrote", path)
	}
	return 0
}

func runCheckpoint(args []string) int {
	fs := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	db := fs.String("db", "fog-checkpoint.sqlite", "SQLite checkpoint path")
	resume := fs.Bool("resume", false, "resume from a version-matching checkpoint")
	stopAfterRays := fs.Int("stop-after-rays", 0, "gracefully stop after N complete ray commits")
	sigkillAfterBlocks := fs.Int("sigkill-after-blocks", 0, "actually SIGKILL self after N block commits")
	crashAfterBlocks := fs.Int("crash-error-after-blocks", 0, "return an in-process crash fault after N blocks")
	blockDelayMS := fs.Int("block-delay-ms", 0, "optional delay before each block, useful for signal drills")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bundle, err := fog.GenerateDefaultBundle()
	if err != nil {
		return fail(err)
	}
	scene := bundle.Canonical
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var store *fog.CheckpointStore
	var recovery *fog.RecoveryState
	if *resume {
		var openErr error
		store, recovery, openErr = fog.OpenCheckpointStore(*db, scene.Version())
		if openErr != nil {
			biz, ok := fog.AsBizError(openErr)
			if ok && biz.Code == fog.CodeCheckpointVersion {
				fmt.Fprintf(os.Stderr, "%v\nversion mismatch: discarding partial checkpoint and recomputing all rays\n", biz)
				store, err = fog.CreateFreshCheckpoint(*db, scene.Version())
				if err != nil {
					return fail(err)
				}
				recovery = nil
			} else {
				return fail(openErr)
			}
		}
	} else {
		store, err = fog.CreateFreshCheckpoint(*db, scene.Version())
		if err != nil {
			return fail(err)
		}
	}

	opts := fog.CheckpointRunOptions{
		StopAfterCompleteRays: *stopAfterRays, CrashAfterBlockCommits: *crashAfterBlocks,
		BlockDelay: time.Duration(*blockDelayMS) * time.Millisecond,
	}
	if *sigkillAfterBlocks > 0 {
		n := *sigkillAfterBlocks
		opts.OnBlockCommitted = func(totalCommits, rayIndex, blockIndex int) error {
			if totalCommits >= n {
				fmt.Fprintf(os.Stderr, "simulating forced termination after block commit %d (ray %d block %d)\n", totalCommits, rayIndex, blockIndex)
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
			return nil
		}
	}
	out, runErr := fog.RunCheckpointed(ctx, scene, store, recovery, opts)
	_ = store.Close()
	if errors.Is(runErr, fog.ErrCrashInjected) {
		fmt.Printf(`{"status":"crash_injected","db":%q,"resume":"fogctl checkpoint --db %s --resume"}`+"\n", *db, *db)
		return 0
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || ctx.Err() != nil {
			fmt.Printf(`{"status":"%s","code":"%s","committed_rays":%d,"block_commits":%d,"resume":"fogctl checkpoint --db %s --resume"}`+"\n",
				fog.StopSignal, fog.CodeSignalShutdown, out.CompleteRayCommits, out.BlockCommits, *db)
			return 0
		}
		return fail(runErr)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"status": out.StopReason, "db": *db, "committed_rays": out.CompleteRayCommits,
		"block_commits": out.BlockCommits, "resume_hint": out.ResumeHint,
	}); err != nil {
		return fail(err)
	}
	return 0
}

func fail(err error) int {
	if biz, ok := fog.AsBizError(err); ok {
		enc := json.NewEncoder(os.Stderr)
		enc.SetIndent("", "  ")
		_ = enc.Encode(biz)
		return 1
	}
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	return 1
}
