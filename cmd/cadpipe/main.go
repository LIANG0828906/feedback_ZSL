package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"cadpipe/internal/acceptance"
	"cadpipe/internal/pipeline"
	"cadpipe/internal/sample"
	"cadpipe/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "gen":
		genCmd(os.Args[2:])
	case "verify":
		verifyCmd(os.Args[2:])
	case "crash":
		crashCmd(os.Args[2:])
	case "recover":
		recoverCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("cadpipe commands: gen, verify, crash, recover")
}

func genCmd(args []string) {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	out := fs.String("out", "samples", "sample output directory")
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic seed")
	_ = fs.Parse(args)
	if _, err := sample.GenerateFiles(*out, *seed); err != nil {
		fatal(err)
	}
	fmt.Printf("generated samples in %s\n", *out)
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	samplesDir := fs.String("samples", "samples", "sample directory")
	reportPath := fs.String("report", "artifacts/verification-report.json", "JSON report path")
	workDir := fs.String("work", "artifacts/acceptance", "acceptance SQLite work directory")
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic seed")
	workers := fs.Int("workers", 4, "worker parallelism")
	_ = fs.Parse(args)

	if _, err := sample.GenerateFiles(*samplesDir, *seed); err != nil {
		fatal(err)
	}
	report := acceptance.Run(acceptance.Options{Seed: *seed, WorkDir: *workDir, Workers: *workers})
	if err := os.MkdirAll(filepath.Dir(*reportPath), 0o755); err != nil {
		fatal(err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*reportPath, append(data, '\n'), 0o644); err != nil {
		fatal(err)
	}
	passed := 0
	for _, check := range report.Checks {
		status := "PASS"
		if !check.OK {
			status = "FAIL"
		} else {
			passed++
		}
		fmt.Printf("[%s] %s", status, check.Name)
		if check.Error != "" {
			fmt.Printf(" -- %s", check.Error)
		}
		fmt.Println()
	}
	fmt.Printf("report=%s checks=%d/%d ok=%v\n", *reportPath, passed, len(report.Checks), report.OK)
	if !report.OK {
		os.Exit(1)
	}
}

func crashCmd(args []string) {
	fs := flag.NewFlagSet("crash", flag.ExitOnError)
	db := fs.String("db", "artifacts/crash.sqlite", "checkpoint database")
	after := fs.Int("after", 4, "committed tiles after which SIGKILL is sent")
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic seed")
	_ = fs.Parse(args)

	bundle, err := sample.Generate(*seed)
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(*db)
	if err != nil {
		fatal(err)
	}
	p, err := pipeline.New(bundle.Scene, bundle.Camera, bundle.Truth.ClipProbes, pipeline.Options{
		Workers: 4, BufferCount: 4, Store: st,
	})
	if err != nil {
		fatal(err)
	}
	p.CommitHook = func(_ int, committed int) {
		if committed == *after {
			fmt.Printf("committed %d tiles; sending SIGKILL to self\n", committed)
			forceKillSelf()
		}
	}
	fmt.Println("starting crash simulation; no exit report will be generated after SIGKILL")
	if _, err := p.RunFull(context.Background()); err != nil {
		fatal(err)
	}
}

func recoverCmd(args []string) {
	fs := flag.NewFlagSet("recover", flag.ExitOnError)
	db := fs.String("db", "artifacts/crash.sqlite", "checkpoint database")
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic seed")
	_ = fs.Parse(args)
	bundle, err := sample.Generate(*seed)
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(*db)
	if err != nil {
		fatal(err)
	}
	defer st.Close()
	p, err := pipeline.New(bundle.Scene, bundle.Camera, bundle.Truth.ClipProbes, pipeline.Options{
		Workers: 2, BufferCount: 2, Store: st,
	})
	if err != nil {
		fatal(err)
	}
	n, err := p.Recover(context.Background())
	if err != nil {
		fatal(err)
	}
	report, err := p.ResumeMissing(context.Background())
	if err != nil {
		fatal(err)
	}
	want, err := pipeline.New(bundle.Scene, bundle.Camera, bundle.Truth.ClipProbes, pipeline.Options{
		Workers: 1, BufferCount: 1,
	})
	if err != nil {
		fatal(err)
	}
	if _, err := want.RunFull(context.Background()); err != nil {
		fatal(err)
	}
	if err := p.CompareWith(want); err != nil {
		fatal(err)
	}
	count, _ := st.Count(context.Background())
	fmt.Printf("recovered=%d resumed=%d checkpoint_rows=%d pixel_match=true\n", n, len(report.Recomputed), count)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
