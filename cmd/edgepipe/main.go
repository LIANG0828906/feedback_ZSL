package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"edgerefine/internal/diagnostic"
	"edgerefine/internal/errs"
	"edgerefine/internal/runner"
	"edgerefine/internal/sample"
	"edgerefine/internal/store"
	"edgerefine/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "generate":
		generateCmd(os.Args[2:])
	case "run":
		runCmd(os.Args[2:])
	case "three-path":
		threePathCmd(os.Args[2:])
	case "serve":
		serveCmd(os.Args[2:])
	case "verify":
		fmt.Fprintln(os.Stderr, "use cmd/verify or: go run ./cmd/verify")
		os.Exit(2)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("edgepipe <generate|run|three-path|serve> [flags]")
}

func generateCmd(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	dir := fs.String("dir", "samples", "sample directory")
	_ = fs.Parse(args)
	ms, err := sample.GenerateAll(*dir)
	must(err)
	for _, m := range ms {
		fmt.Printf("generated %s (%dx%d)\n", m.SampleID, m.Width, m.Height)
	}
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	samples := fs.String("samples", "samples", "samples root")
	id := fs.String("sample", "step128", "sample id")
	dbPath := fs.String("db", "checkpoints/edge.db", "sqlite path")
	workers := fs.Int("workers", 4, "parallel tile workers")
	stopAfter := fs.Int("stop-after", 0, "test hook: stop after N checkpoint writes")
	radius := fs.Int("radius", -1, "override structure radius")
	budget := fs.Int("budget", -1, "override iteration budget")
	threshold := fs.Float64("threshold", -1, "manual threshold")
	_ = fs.Parse(args)

	dir := filepath.Join(*samples, *id)
	b, err := sample.LoadBundle(dir)
	must(err)
	p := b.Manifest.Params
	if *radius >= 0 {
		p.Radius = *radius
		p.ParamVersion++
	}
	if *budget > 0 {
		p.IterBudget = *budget
		p.ParamVersion++
	}
	var manual *float64
	if *threshold >= 0 {
		manual = threshold
		p.ParamVersion++
	}
	fp := sample.Fingerprint(b.Manifest)
	st, err := store.Open(*dbPath)
	must(err)
	defer st.Close()

	rep, err := runner.Run(b, runner.Options{
		Workers: *workers, Params: p, Store: st, TruthFingerprint: fp,
		Resume: true, Persist: true, StrictAccuracy: true,
		StopAfterCommits: *stopAfter, ManualThreshold: manual, Ctx: context.Background(),
	})
	if rep != nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"sample_id": rep.SampleID, "mask_hash": rep.MaskHash, "deviation_hash": rep.DeviationHash,
			"committed_stages": rep.CommittedStages, "total_stages": rep.TotalStages,
			"converged_tile_ratio": rep.ConvergedTileRatio, "commit_sequence_len": len(rep.CommitSequence),
			"accuracy": rep.Accuracy, "reference": rep.Reference, "progress": rep.Progress,
		})
	}
	if err != nil {
		if *stopAfter > 0 && err == runner.ErrStopAfterCommit {
			fmt.Println("stopped at declared commit boundary")
			return
		}
		if ee, ok := errs.As(err); ok {
			fmt.Fprintf(os.Stderr, "%s: %s\n", ee.Code, ee.Error())
			os.Exit(1)
		}
		must(err)
	}
}

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	samples := fs.String("samples", "samples", "samples root")
	dbPath := fs.String("db", "checkpoints/edge.db", "sqlite path")
	_ = fs.Parse(args)
	srv, err := web.New(*samples, *dbPath)
	must(err)
	defer srv.Close()
	display := *addr
	if len(display) > 0 && display[0] == ':' {
		display = "localhost" + display
	}
	fmt.Printf("edge refinement dashboard: http://%s\n", display)
	must(srv.ListenAndServe(*addr))
}

func threePathCmd(args []string) {
	fs := flag.NewFlagSet("three-path", flag.ExitOnError)
	samples := fs.String("samples", "samples", "samples root")
	id := fs.String("sample", "step128", "sample id")
	work := fs.String("checkpoint-dir", "checkpoints", "checkpoint working directory")
	_ = fs.Parse(args)
	res, err := diagnostic.RunThreePaths(*samples, *id, *work)
	must(err)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	must(enc.Encode(res))
	if !res.Comparison.AllPixelEqual || !res.Comparison.CommitSequencesEqual || !res.Verification.OK {
		os.Exit(1)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
