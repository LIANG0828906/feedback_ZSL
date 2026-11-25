package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"qkernel/diag"
	"qkernel/internal/errs"
	"qkernel/internal/scenario"
	"qkernel/internal/session"
	"qkernel/internal/synth"
	"qkernel/internal/verifier"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "synth":
		err = cmdSynth(os.Args[2:])
	case "demo":
		err = cmdDemo(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "compare":
		err = cmdCompare(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		var ke *errs.Error
		if errors.As(err, &ke) {
			fmt.Fprintf(os.Stderr, "%s\n", ke.Error())
		} else {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`qkernel — deterministic game-screenshot degradation kernel

Commands:
  synth    --out sample-output [--frames 3]
  demo     --out sample-output [--frames 3] [--workers 4]
  run      --root sample-output [--frame frame_000] [--workers 4]
  verify   --root sample-output [--run run_...]
  compare  --run run_... --a rootA --b rootB [--c rootC]
  serve    --root sample-output [--addr 127.0.0.1:8787] [--run run_...]

All diagnostic assets are local and no external network resource is loaded.`)
}

func cmdSynth(args []string) error {
	fs := flag.NewFlagSet("synth", flag.ContinueOnError)
	out := fs.String("out", "sample-output", "output root")
	frames := fs.Int("frames", 3, "number of deterministic frames")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _ := scenario.Default()
	cfg.Frames = *frames
	set, err := synth.GenerateSet(cfg)
	if err != nil {
		return err
	}
	if err := scenario.SaveSet(*out, set); err != nil {
		return err
	}
	fmt.Printf("synthesized %d frame(s) in %s\n", len(set.Benchmarks), *out)
	return nil
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	out := fs.String("out", "sample-output", "demo output root")
	frames := fs.Int("frames", 3, "number of frames to synthesize")
	workers := fs.Int("workers", 4, "max parallel workers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, chain := scenario.Default()
	cfg.Frames = *frames
	set, err := synth.GenerateSet(cfg)
	if err != nil {
		return err
	}
	if err := scenario.SaveSet(*out, set); err != nil {
		return err
	}

	eng, err := session.NewEngine(*out)
	if err != nil {
		return err
	}
	var firstRes *session.RunResult
	for i, b := range set.Benchmarks {
		res, err := eng.RunFrame(session.RunOptions{
			Benchmark: b, Chain: chain, ParamVersion: 1,
			SegmentSize: scenario.SegmentSize, MetricBlockSize: scenario.MetricBlockSize,
			Workers: *workers,
		})
		if err != nil {
			_ = eng.Close()
			return err
		}
		if i == 0 {
			firstRes = res
		}
	}
	if err := eng.Close(); err != nil {
		return err
	}

	b0 := set.Benchmarks[0]
	fullRoot := filepath.Join(*out, "paths", "full")
	resumeRoot := filepath.Join(*out, "paths", "resume")
	if err := scenario.SaveSet(fullRoot, &synth.Set{Benchmarks: []*synth.Benchmark{b0}}); err != nil {
		return err
	}
	if err := scenario.SaveSet(resumeRoot, &synth.Set{Benchmarks: []*synth.Benchmark{b0}}); err != nil {
		return err
	}

	fullEng, err := session.NewEngine(fullRoot)
	if err != nil {
		return err
	}
	fullRes, err := fullEng.RunFrame(session.RunOptions{
		Benchmark: b0, Chain: chain, ParamVersion: 1,
		SegmentSize: scenario.SegmentSize, MetricBlockSize: scenario.MetricBlockSize, Workers: 1,
	})
	if err != nil {
		_ = fullEng.Close()
		return err
	}
	_ = fullEng.Close()

	resumeEng, err := session.NewEngine(resumeRoot)
	if err != nil {
		return err
	}
	_, err = resumeEng.RunFrame(session.RunOptions{
		Benchmark: b0, Chain: chain, ParamVersion: 1,
		SegmentSize: scenario.SegmentSize, MetricBlockSize: scenario.MetricBlockSize,
		Workers: *workers, InterruptAfterCommits: 7,
	})
	if !errors.Is(err, session.ErrInterrupted) {
		return fmt.Errorf("expected deterministic interruption, got %v", err)
	}
	_ = resumeEng.Close()

	resumeEng2, err := session.NewEngine(resumeRoot)
	if err != nil {
		return err
	}
	resumeRes, err := resumeEng2.RunFrame(session.RunOptions{
		Benchmark: b0, Chain: chain, ParamVersion: 1,
		SegmentSize: scenario.SegmentSize, MetricBlockSize: scenario.MetricBlockSize, Workers: 2,
	})
	if err != nil {
		_ = resumeEng2.Close()
		return err
	}
	_ = resumeEng2.Close()

	cmp, err := verifier.CompareRoots(firstRes.RunID, *out, fullRoot, resumeRoot)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*out, "verification.json"), cmp); err != nil {
		return err
	}

	fmt.Printf("demo complete\n")
	fmt.Printf("  root: %s\n", *out)
	fmt.Printf("  run: %s\n", firstRes.RunID)
	fmt.Printf("  final hash: %s\n", firstRes.Final.Hash())
	fmt.Printf("  commits: %d (full=%d resume=%d)\n", len(firstRes.Commits), len(fullRes.Commits), len(resumeRes.Commits))
	fmt.Printf("  three-path verification: ok=%v\n", cmp.OK)
	if !cmp.OK && cmp.FirstViolation != nil {
		return fmt.Errorf("verification failed: %+v", cmp.FirstViolation)
	}
	fmt.Printf("  diagnostics: go run ./cmd/qkernel serve --root %s\n", *out)
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	root := fs.String("root", "sample-output", "output root")
	frameID := fs.String("frame", synth.MustFrameID(0), "frame id")
	workers := fs.Int("workers", 4, "max parallel workers")
	version := fs.Int("version", 1, "parameter version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := scenario.LoadBenchmark(*root, *frameID)
	if err != nil {
		return err
	}
	_, chain := scenario.Default()
	eng, err := session.NewEngine(*root)
	if err != nil {
		return err
	}
	defer eng.Close()
	res, err := eng.RunFrame(session.RunOptions{
		Benchmark: b, Chain: chain, ParamVersion: *version,
		SegmentSize: scenario.SegmentSize, MetricBlockSize: scenario.MetricBlockSize, Workers: *workers,
	})
	if err != nil {
		return err
	}
	fmt.Printf("run=%s final=%s commits=%d\n", res.RunID, res.Final.Hash(), len(res.Commits))
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	root := fs.String("root", "sample-output", "persisted kernel root")
	runID := fs.String("run", "", "run id; defaults to latest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rep, err := verifier.VerifyRoot(*root, *runID)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	if !rep.OK {
		return fmt.Errorf("independent verification found %d violation(s)", len(rep.Violations))
	}
	return nil
}

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	runID := fs.String("run", "", "shared run id")
	a := fs.String("a", "", "path A")
	b := fs.String("b", "", "path B")
	c := fs.String("c", "", "path C")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *a == "" || *b == "" {
		return errs.New(errs.EPara, "--a and --b are required")
	}
	roots := []string{*a, *b}
	if *c != "" {
		roots = append(roots, *c)
	}
	rep, err := verifier.CompareRoots(*runID, roots...)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(out))
	if !rep.OK {
		os.Exit(1)
	}
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	root := fs.String("root", "sample-output", "persisted kernel root")
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	runID := fs.String("run", "", "run id; defaults to latest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv, actualAddr, err := diag.Listen(*addr, *root, *runID)
	if err != nil {
		return err
	}
	defer srv.Close()
	fmt.Printf("diagnostic UI: http://%s/\n", actualAddr.String())
	select {}
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
