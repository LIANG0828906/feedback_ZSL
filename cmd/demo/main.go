package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"

	"multiplexgw/internal/app"
	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/engine"
	"multiplexgw/internal/model"
	"multiplexgw/internal/parallel"
	"multiplexgw/internal/sample"
)

func main() {
	if len(os.Args) == 1 {
		runAll("data", app.DefaultArtifactDir, 2, 8)
		return
	}
	switch os.Args[1] {
	case "all", "verify":
		fs := flag.NewFlagSet("all", flag.ExitOnError)
		data := fs.String("data", "data", "sample directory")
		artifacts := fs.String("artifacts", app.DefaultArtifactDir, "artifact directory")
		workers := fs.Int("workers", 2, "worker thread budget")
		buffer := fs.Int("buffer", 8, "per-stream pending frame buffer budget")
		_ = fs.Parse(os.Args[2:])
		runAll(*data, *artifacts, *workers, *buffer)
	case "generate":
		fs := flag.NewFlagSet("generate", flag.ExitOnError)
		out := fs.String("out", "data", "output directory")
		_ = fs.Parse(os.Args[2:])
		if _, err := app.Generate(*out); err != nil {
			fail(err)
		}
		fmt.Println("generated", *out)
	case "inject":
		fs := flag.NewFlagSet("inject", flag.ExitOnError)
		name := fs.String("case", "", "exception case name")
		artifacts := fs.String("artifacts", app.DefaultArtifactDir, "artifact directory for checkpoint fixtures")
		list := fs.Bool("list", false, "list case names")
		_ = fs.Parse(os.Args[2:])
		if *list {
			for _, n := range app.InjectionCaseNames() {
				fmt.Println(n)
			}
			return
		}
		r, err := app.RunNamedInjection(*name, *artifacts)
		if err != nil {
			fail(err)
		}
		fmt.Println(app.MarshalReport(r))
		if !r.Match {
			os.Exit(1)
		}
	case "crash":
		fs := flag.NewFlagSet("crash", flag.ExitOnError)
		data := fs.String("data", "data", "sample directory")
		db := fs.String("db", "artifacts/demo/checkpoint-crash.db", "SQLite checkpoint path")
		after := fs.Int("after", 2, "event index at which to inject crash")
		mode := fs.String("mode", "before_global_seq", "before_global_seq or after_commit")
		_ = fs.Parse(os.Args[2:])
		_, err := app.RunCLICheckpoint(*data, *db, *mode, *after, false)
		if err != nil {
			fail(err)
		}
		fmt.Println("checkpoint run completed without crash point")
	case "recover":
		fs := flag.NewFlagSet("recover", flag.ExitOnError)
		data := fs.String("data", "data", "sample directory")
		db := fs.String("db", "artifacts/demo/checkpoint-crash.db", "SQLite checkpoint path")
		_ = fs.Parse(os.Args[2:])
		res, err := app.RunCLICheckpoint(*data, *db, "", -1, true)
		if err != nil {
			fail(err)
		}
		match, start, err := compareRecoveredSuffix(res)
		if err != nil {
			fail(err)
		}
		out := map[string]any{
			"code":               bizerr.OK,
			"db":                 *db,
			"suffix_start_index": start,
			"recovered_traces":   len(res.Traces),
			"final_committed":    res.CommittedThrough,
			"matches_full_run":   match,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		if !match {
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runAll(data, artifacts string, workers, buffer int) {
	cfg := parallel.Config{Workers: workers, MaxPendingFrames: buffer}
	if err := cfg.Validate(); err != nil {
		be, _ := bizerr.AsError(err)
		fmt.Println(app.MarshalReport(be))
		os.Exit(2)
	}
	report, err := app.VerifyWithOptions(data, artifacts, workers, buffer)
	if report != nil {
		fmt.Printf("demo verification OK=%v report=%s/verification-report.json\n", report.OK, artifacts)
		if len(report.Exceptions) > 0 {
			fmt.Println("stable exception codes verified:")
			for _, ex := range report.Exceptions {
				fmt.Printf("  %s -> %s (match=%v)\n", ex.Case, ex.ActualCode, ex.Match)
			}
		}
	}
	if err != nil {
		fail(err)
	}
}

func compareRecoveredSuffix(res *model.RunResult) (bool, int, error) {
	bundle, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		return false, 0, err
	}
	expected, err := engine.Run(bundle.Canonical, engine.Options{MaxPendingFrames: 8})
	if err != nil {
		return false, 0, err
	}
	if len(res.Traces) == 0 {
		return res.CommittedThrough == len(expected.Traces)-1, len(expected.Traces), nil
	}
	start := res.CommittedThrough - len(res.Traces) + 1
	if start < 0 || start+len(res.Traces) > len(expected.Traces) {
		return false, start, fmt.Errorf("recovery suffix out of range")
	}
	for i, got := range res.Traces {
		want := expected.Traces[start+i]
		got.CacheStatus = ""
		got.Invalidation = nil
		want.CacheStatus = ""
		want.Invalidation = nil
		if !reflect.DeepEqual(got, want) {
			return false, start, nil
		}
	}
	return true, start, nil
}

func fail(err error) {
	code := bizerr.CodeOf(err)
	be, ok := bizerr.AsError(err)
	out := map[string]any{"code": code, "message": err.Error()}
	if ok {
		out["path"] = be.Path
		out["context"] = be.Context
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(os.Stderr, string(b))
	os.Exit(1)
}
