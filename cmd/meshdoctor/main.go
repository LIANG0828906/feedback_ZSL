package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"meshdoctor/internal/app"
	"meshdoctor/internal/errs"
	"meshdoctor/internal/pipeline"
	"meshdoctor/internal/sample"
	"meshdoctor/internal/store"
	"meshdoctor/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "serve", "diagnostic":
		err = cmdServe(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "version":
		fmt.Println("meshdoctor", pipeline.AlgoVersion, "default-seed", sample.DefaultSeed)
	case "-h", "--help", "help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, string(errs.JSON(err)))
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `meshdoctor <generate|verify|run|serve|version>

  generate [--root .] [--seed 424242]
  verify   [--root .] [--parallel] [--out artifacts/validation/acceptance-result.json]
  run      --sample cube [--root .] [--db checkpoints/cli.db] [--stop-after 2] [--hang-before smooth] [--ready-file /tmp/ready]
  serve    [--root .] [--addr 127.0.0.1:18080]`)
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	root := fs.String("root", ".", "workspace root")
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	samples, err := sample.WriteWorkspace(*root, *seed)
	if err != nil {
		return err
	}
	fmt.Printf("generated %d deterministic samples in %s (seed=%d)\n", len(samples), *root, *seed)
	for _, s := range samples {
		fmt.Printf("- %s: input V=%d F=%d; truth V=%d F=%d chi=%d holes=%d points=%d\n",
			s.Manifest.ID, s.Manifest.InputVertices, s.Manifest.InputFaces,
			s.Manifest.Truth.Vertices, s.Manifest.Truth.Faces, s.Manifest.Truth.Euler,
			len(s.Manifest.HoleRings), len(s.Manifest.KnownPoints))
	}
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	root := fs.String("root", ".", "workspace root")
	out := fs.String("out", "artifacts/validation/acceptance-result.json", "JSON report output")
	parallel := fs.Bool("parallel", true, "run samples through bounded worker pool")
	parallelism := fs.Int("workers", 2, "max parallel sample workers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	params := pipeline.DefaultParams()
	params.MaxParallelism = *parallelism
	a, err := app.New(*root, params)
	if err != nil {
		return err
	}
	report, err := a.Verify(context.Background(), *parallel)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("verification ok=%v samples=%d boundary_checks=%d report=%s\n", report.OK, len(report.Samples), len(report.BoundaryChecks), *out)
	for _, s := range report.Samples {
		fmt.Printf("- %-5s ok=%v stages=%d cmp=%v resume_hits=%d\n", s.SampleID, s.OK, len(s.Resume.Stages), s.Comparison != nil && s.Comparison.AllIdentical, s.ResumeCache["cache_hits"])
		if !s.OK {
			fmt.Printf("  %s: %s\n", s.ErrorCode, s.Error)
		}
	}
	for _, b := range report.BoundaryChecks {
		status := "PASS"
		if !b.OK {
			status = "FAIL"
		}
		fmt.Printf("- %s %-7s %s\n", status, b.Code, b.Name)
	}
	if !report.OK {
		return errs.New("E_VERIFY", "one or more acceptance checks failed")
	}
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	root := fs.String("root", ".", "workspace root")
	addr := fs.String("addr", "127.0.0.1:18080", "loopback listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a, err := app.New(*root, pipeline.DefaultParams())
	if err != nil {
		return err
	}
	server := web.New(a)
	fmt.Printf("MeshDoctor diagnostic page: http://%s/\n", *addr)
	return server.ListenAndServe(*addr)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	root := fs.String("root", ".", "workspace root")
	sampleID := fs.String("sample", "", "sample id")
	dbPath := fs.String("db", "checkpoints/cli.db", "SQLite checkpoint path")
	stopAfter := fs.Int("stop-after", 0, "stop after this many committed stages")
	hangBefore := fs.String("hang-before", "", "hang after persisting running state for stage (SIGKILL test hook)")
	readyFile := fs.String("ready-file", "", "write PID file before hanging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sampleID == "" {
		return errors.New("--sample is required")
	}
	s, err := sample.LoadSample(*root, *sampleID)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*root, *dbPath))
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	orch := pipeline.NewOrchestrator()
	result, err := orch.RunOrResume(ctx, s.Manifest.ID, &s.Mesh, &s.Manifest, pipeline.DefaultParams(), st, pipeline.RunOptions{
		Path: "cli-resume", StopAfter: *stopAfter,
		BeforeExecute: func(stage string) {
			if *hangBefore != "" && stage == *hangBefore {
				if *readyFile != "" {
					_ = os.WriteFile(*readyFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
				}
				select {}
			}
		},
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	summary := map[string]any{
		"ok": err == nil || errors.Is(err, context.Canceled), "sample": *sampleID,
		"committed": result.CommittedSequence, "stop_reason": result.StopReason,
		"cache_hits": result.CacheHits, "cache_misses": result.CacheMisses,
		"final_hash": result.FinalHash, "error": errString(err),
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(b))
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
