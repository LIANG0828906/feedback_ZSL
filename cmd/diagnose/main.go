package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"integritykernel/internal/checkpoint"
	"integritykernel/internal/core"
	"integritykernel/internal/engine"
	"integritykernel/internal/loader"
	"integritykernel/internal/pipeline"
	"integritykernel/internal/review"
	"integritykernel/internal/samplegen"
	"integritykernel/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = runGen(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "crosscheck":
		err = runCrossCheck(os.Args[2:])
	case "review":
		err = runReview(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %s", os.Args[1])
	}
	if err != nil {
		var kernelErr *core.Error
		if errors.As(err, &kernelErr) {
			b, _ := json.MarshalIndent(kernelErr, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`integrity-diagnose

Commands:
  gen         Generate deterministic screenshot frames and truth.json
  verify      Run one checkpoint-backed verification and write JSON report
  crosscheck  Run incremental, recovery and full paths plus independent review
  review      Independently review persisted evidence and truth fingerprint
  serve       Start local dependency-free diagnostic web UI

Examples:
  integrity-diagnose gen -sample samples/default
  integrity-diagnose crosscheck -sample samples/default -artifacts artifacts
  integrity-diagnose serve -sample samples/default -artifacts artifacts -addr 127.0.0.1:8088`)
}

func runGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	sample := fs.String("sample", "samples/default", "sample output directory")
	frames := fs.Int("frames", 24, "deterministic frame count")
	seed := fs.Int64("seed", 0x42A8691E, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifest, err := samplegen.Generate(samplegen.Options{OutputDir: *sample, Frames: *frames, Seed: *seed, Force: true})
	if err != nil {
		return err
	}
	fmt.Printf("generated %d frames at %s; truth=%s\n", manifest.FrameCount, *sample, filepath.Join(*sample, "truth.json"))
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	sample := fs.String("sample", "samples/default", "sample directory")
	db := fs.String("db", "artifacts/verify-checkpoint.db", "SQLite checkpoint path")
	reportPath := fs.String("report", "artifacts/verify-report.json", "JSON report output")
	blockSize := fs.Int("block-size", core.DefaultParams().BlockSize, "noise block size")
	threshold := fs.Float64("verdict-threshold", core.DefaultParams().VerdictThreshold, "verdict threshold")
	parallel := fs.Int("parallel", core.DefaultParams().MaxParallel, "maximum evidence workers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := samplegen.Generate(samplegen.DefaultOptions(*sample)); err != nil {
		return err
	}
	seq, err := loader.Load(*sample)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*db), 0o755); err != nil {
		return err
	}
	store, err := checkpoint.Open(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	params := core.DefaultParams()
	params.BlockSize = *blockSize
	params.VerdictThreshold = *threshold
	params.MaxParallel = *parallel
	eng, err := engine.New(seq, engine.Options{Params: params, Store: store})
	if err != nil {
		return err
	}
	if _, txnErr, err := eng.LoadCheckpoint(); err != nil {
		return err
	} else if txnErr != nil {
		fmt.Fprintln(os.Stderr, "recovering unfinished batch:", txnErr.Error())
	}
	if err := eng.Run(); err != nil {
		return err
	}
	report := eng.BuildReport()
	if err := writeJSONFile(*reportPath, report); err != nil {
		return err
	}
	fmt.Printf("verified %d frames; intervals=%s; report=%s\n", len(report.Frames), core.MustJSON(report.Intervals), *reportPath)
	return nil
}

func runCrossCheck(args []string) error {
	fs := flag.NewFlagSet("crosscheck", flag.ContinueOnError)
	sample := fs.String("sample", "samples/default", "sample directory")
	artifactsDir := fs.String("artifacts", "artifacts", "artifact and checkpoint directory")
	jsonPath := fs.String("json", "artifacts/crosscheck.json", "JSON result output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := pipeline.RunCrossCheck(*sample, *artifactsDir)
	if err != nil {
		return err
	}
	if err := writeJSONFile(*jsonPath, result); err != nil {
		return err
	}
	for _, state := range result.States {
		fmt.Printf("state=%s invalidations=%d inc_diff=%v recovery_diff=%v\n",
			state.Name, len(state.Invalidations), state.IncrementalVsFullError != nil, state.RecoveryVsFullError != nil)
	}
	fmt.Printf("crosscheck ok=%v transaction=%v review=%v log_records=%d result=%s\n",
		result.OK, result.TransactionCheckOK, result.Review != nil && result.Review.OK,
		len(result.InitialRecoveryLog), *jsonPath)
	if !result.OK {
		if result.FirstMismatch != nil {
			b, _ := json.MarshalIndent(result.FirstMismatch, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		}
		return core.NewError(core.CodeDiff, "three-path cross-check failed")
	}
	return nil
}

func runReview(args []string) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	db := fs.String("db", "artifacts/recovery-checkpoint.db", "SQLite evidence database")
	fingerprint := fs.String("fingerprint", "samples/default/truth.fingerprint.json", "truth fingerprint JSON")
	jsonPath := fs.String("json", "artifacts/review.json", "review result output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := review.Run(*db, *fingerprint)
	if err != nil {
		return err
	}
	if err := writeJSONFile(*jsonPath, result); err != nil {
		return err
	}
	fmt.Printf("review ok=%v checked=%d intervals=%s result=%s\n", result.OK, result.CheckedFrames, core.MustJSON(result.RecomputedIntervals), *jsonPath)
	if !result.OK {
		b, _ := json.MarshalIndent(result.FirstViolation, "", "  ")
		fmt.Fprintln(os.Stderr, string(b))
		return core.NewError(core.CodeDiff, "independent review found inconsistency")
	}
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	sample := fs.String("sample", "samples/default", "sample directory")
	artifactsDir := fs.String("artifacts", "artifacts", "artifact directory")
	addr := fs.String("addr", "127.0.0.1:8088", "listen address; use :0 for an ephemeral port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	server, err := web.NewServer(*sample, *artifactsDir)
	if err != nil {
		return err
	}
	defer server.Close()

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	actualAddr := listener.Addr().String()
	fmt.Printf("integrity diagnostic UI: http://%s\n", actualAddr)
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func writeJSONFile(path string, value any) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
