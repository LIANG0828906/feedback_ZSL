package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"acousticfield"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = runDemo(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `acousticfield - deterministic chunked early-reflection field

Usage:
  acoustic demo   [-seed N] [-workers N] [-abort-after N] [-dir PATH] [-json]
  acoustic serve  [-seed N] [-addr :8080] [-db PATH] [-workers N]
  acoustic verify [-seed N] [-db PATH] [-workers N] [-json]

The demo uses a fixed deterministic edit sequence and compares one-shot full,
incremental cached, and abort/recover rebuilds.
`)
}

func optionsFromFlags(workers int) acousticfield.Options {
	opts := acousticfield.DefaultOptions()
	if workers > 0 {
		opts.MaxParallel = workers
		opts.QueueBudget = max(4, workers*2)
	}
	return opts
}

type demoOutput struct {
	Seed                   int64                            `json:"seed"`
	Chunks                 int                              `json:"chunks"`
	Bands                  int                              `json:"bands"`
	Options                acousticfield.Options            `json:"options"`
	Full                   acousticfield.RunReport          `json:"full"`
	Incremental            acousticfield.RunReport          `json:"incremental"`
	Resume                 acousticfield.RunReport          `json:"resume"`
	IncrementalMatchesFull bool                             `json:"incremental_matches_full"`
	ResumeMatchesFull      bool                             `json:"resume_matches_full"`
	Verification           acousticfield.VerifyReport       `json:"verification"`
	Invalidations          []acousticfield.InvalidatedChunk `json:"invalidations"`
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 20261126, "deterministic sample seed")
	workers := fs.Int("workers", 4, "maximum worker count")
	abortAfter := fs.Int("abort-after", 25, "commit count before simulated abort")
	dir := fs.String("dir", "", "working directory; defaults to a temporary directory")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	sample, err := acousticfield.Generate(*seed)
	if err != nil {
		return err
	}
	if *dir == "" {
		d, err := os.MkdirTemp("", "acousticfield-demo-")
		if err != nil {
			return err
		}
		*dir = d
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	opts := optionsFromFlags(*workers)

	full, err := buildFullDemo(ctx, sample, opts, filepath.Join(*dir, "full.db"))
	if err != nil {
		return err
	}
	incremental, invalidations, incEngine, incStore, err := buildIncrementalDemo(ctx, sample, opts, filepath.Join(*dir, "incremental.db"))
	if err != nil {
		return err
	}
	defer incStore.Close()
	resumed, err := buildResumeDemo(ctx, sample, opts, filepath.Join(*dir, "resume.db"), *abortAfter)
	if err != nil {
		return err
	}
	incDiff := acousticfield.CompareRunReports(full, incremental)
	resumeDiff := acousticfield.CompareRunReports(full, resumed)
	verify, err := acousticfield.VerifySnapshot(ctx, incStore, incEngine.SnapshotID(), incEngine.Truth(), opts)
	if err != nil {
		return err
	}
	out := demoOutput{
		Seed: *seed, Chunks: len(sample.Level.Chunks), Bands: len(sample.Level.Bands), Options: opts,
		Full: *full, Incremental: *incremental, Resume: *resumed,
		IncrementalMatchesFull: incDiff == nil, ResumeMatchesFull: resumeDiff == nil,
		Verification: *verify, Invalidations: invalidations,
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("sample: seed=%d chunks=%d bands=%d options=%s\n", out.Seed, out.Chunks, out.Bands, opts)
	fmt.Printf("workdir: %s\n", *dir)
	printRun("full", full)
	printRun("incremental", incremental)
	printRun("resume", resumed)
	fmt.Printf("invalidation list: %d chunks\n", len(invalidations))
	for _, item := range invalidations {
		fmt.Printf("  chunk %d distance=%d reasons=%v\n", item.ChunkID, item.Distance, item.Reasons)
	}
	if incDiff != nil {
		return incDiff
	}
	if resumeDiff != nil {
		return resumeDiff
	}
	fmt.Printf("three-path compare: MATCH\n")
	fmt.Printf("independent verify: PASS field=%s sequence=%s\n", verify.FieldFingerprint[:16], verify.CommitSequenceHash[:16])
	return nil
}

func printRun(name string, r *acousticfield.RunReport) {
	fmt.Printf("%-11s round=%d committed=%d reused=%d recomputed=%d max_residual=%d sequence=%s\n",
		name, r.RoundID, r.CommittedChunks, r.CacheHits, r.Recomputed,
		r.Conservation.MaxResidual, sequenceDigest(r.Submissions))
}

func sequenceDigest(s []acousticfield.Submission) string {
	if len(s) == 0 {
		return "-"
	}
	b, _ := json.Marshal(s)
	sum := fmt.Sprintf("%x", b)
	if len(sum) > 16 {
		sum = sum[:16]
	}
	return sum
}

func buildFullDemo(ctx context.Context, sample *acousticfield.Sample, opts acousticfield.Options, path string) (*acousticfield.RunReport, error) {
	store, err := acousticfield.OpenStore(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	engine, err := acousticfield.NewEngine(sample.Level, &sample.Truth, opts, store)
	if err != nil {
		return nil, err
	}
	if _, err := engine.Initialize(ctx); err != nil {
		return nil, err
	}
	if err := applyDemoEdits(engine); err != nil {
		return nil, err
	}
	return engine.Rebuild(ctx, acousticfield.BuildOptions{ForceFull: true})
}

func buildIncrementalDemo(ctx context.Context, sample *acousticfield.Sample, opts acousticfield.Options, path string) (*acousticfield.RunReport, []acousticfield.InvalidatedChunk, *acousticfield.Engine, *acousticfield.Store, error) {
	store, err := acousticfield.OpenStore(path)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	engine, err := acousticfield.NewEngine(sample.Level, &sample.Truth, opts, store)
	if err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	if _, err := engine.Initialize(ctx); err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	if _, err := engine.Rebuild(ctx, acousticfield.BuildOptions{}); err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	_, err = applyDemoEditsReturn(engine)
	if err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	report, err := engine.Rebuild(ctx, acousticfield.BuildOptions{})
	if err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	return report, report.Invalidations, engine, store, nil
}

func buildResumeDemo(ctx context.Context, sample *acousticfield.Sample, opts acousticfield.Options, path string, abortAfter int) (*acousticfield.RunReport, error) {
	store, err := acousticfield.OpenStore(path)
	if err != nil {
		return nil, err
	}
	engine, err := acousticfield.NewEngine(sample.Level, &sample.Truth, opts, store)
	if err != nil {
		store.Close()
		return nil, err
	}
	if _, err := engine.Initialize(ctx); err != nil {
		store.Close()
		return nil, err
	}
	_, err = engine.Rebuild(ctx, acousticfield.BuildOptions{AbortAfterCommits: abortAfter})
	if !errors.Is(err, acousticfield.ErrAborted) {
		store.Close()
		return nil, fmt.Errorf("expected abort: %w", err)
	}
	if err := store.Close(); err != nil {
		return nil, err
	}
	// Reopen as a fresh process would.
	store2, err := acousticfield.OpenStore(path)
	if err != nil {
		return nil, err
	}
	defer store2.Close()
	recovered, err := acousticfield.NewEngine(sample.Level, &sample.Truth, opts, store2)
	if err != nil {
		return nil, err
	}
	restore, err := recovered.Initialize(ctx)
	if err != nil {
		return nil, err
	}
	if len(restore.Loaded) != abortAfter {
		return nil, fmt.Errorf("restored %d chunks, expected %d", len(restore.Loaded), abortAfter)
	}
	if err := applyDemoEdits(recovered); err != nil {
		return nil, err
	}
	return recovered.Rebuild(ctx, acousticfield.BuildOptions{})
}

func applyDemoEdits(engine *acousticfield.Engine) error {
	_, err := applyDemoEditsReturn(engine)
	return err
}

func applyDemoEditsReturn(engine *acousticfield.Engine) ([]acousticfield.InvalidatedChunk, error) {
	steps := []acousticfield.Edit{
		{Kind: acousticfield.EditAbsorption, ChunkID: 7, BandID: "125Hz", Coefficient: 27_000},
		{Kind: acousticfield.EditReflector, ChunkID: 8, Reflector: &acousticfield.Reflector{
			ID: "c008-left", A: acousticfield.Point{X: 4_100_000, Y: 2_100_000},
			B: acousticfield.Point{X: 4_100_000, Y: 3_100_000}, Normal: acousticfield.Vec{X: acousticfield.Q16}, Material: "edited-wall",
		}},
		{Kind: acousticfield.EditSourceEmission, SourceID: "src-009", Emission: []acousticfield.BandEnergy{
			{BandID: "125Hz", Energy: 3_300_000},
			{BandID: "2000Hz", Energy: 4_300_000},
			{BandID: "500Hz", Energy: 4_100_000},
		}},
	}
	var all []acousticfield.InvalidatedChunk
	for _, step := range steps {
		inv, err := engine.ApplyEdit(step)
		if err != nil {
			return nil, err
		}
		all = append(all, inv...)
	}
	return all, nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	seed := fs.Int64("seed", 20261126, "deterministic sample seed")
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	db := fs.String("db", filepath.Join(os.TempDir(), "acousticfield-dashboard.db"), "SQLite checkpoint path")
	workers := fs.Int("workers", 4, "maximum worker count")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	sample, err := acousticfield.Generate(*seed)
	if err != nil {
		return err
	}
	opts := optionsFromFlags(*workers)
	store, err := acousticfield.OpenStore(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	engine, err := acousticfield.NewEngine(sample.Level, &sample.Truth, opts, store)
	if err != nil {
		return err
	}
	restore, err := engine.Initialize(ctx)
	if err != nil {
		return err
	}
	run, err := engine.Rebuild(ctx, acousticfield.BuildOptions{})
	if err != nil {
		return err
	}
	diag := acousticfield.NewDiagnosticServer(engine)
	fullDir, err := os.MkdirTemp("", "acousticfield-full-compare-")
	if err != nil {
		return err
	}
	fullStore, err := acousticfield.OpenStore(filepath.Join(fullDir, "full.db"))
	if err != nil {
		return err
	}
	defer fullStore.Close()
	level := engine.Level()
	levelTruth := engine.Truth()
	fullEngine, err := acousticfield.NewEngine(level, &levelTruth, opts, fullStore)
	if err != nil {
		return err
	}
	if _, err := fullEngine.Initialize(ctx); err != nil {
		return err
	}
	fullRun, err := fullEngine.Rebuild(ctx, acousticfield.BuildOptions{ForceFull: true})
	if err != nil {
		return err
	}
	diag.SetComparison("incremental/full", acousticfield.CompareRunReports(fullRun, run))
	server := &http.Server{
		Addr:              *addr,
		Handler:           diag,
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("dashboard: http://%s\n", *addr)
	fmt.Printf("snapshot=%s restored=%d chunks cache_hits=%d conservation=%v\n",
		engine.SnapshotID(), len(restore.Loaded), run.CacheHits, run.Conservation.OK)
	return server.ListenAndServe()
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := fs.Int64("seed", 20261126, "deterministic sample seed")
	db := fs.String("db", filepath.Join(os.TempDir(), "acousticfield-dashboard.db"), "SQLite checkpoint path")
	workers := fs.Int("workers", 4, "maximum worker count")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sample, err := acousticfield.Generate(*seed)
	if err != nil {
		return err
	}
	opts := optionsFromFlags(*workers)
	store, err := acousticfield.OpenStore(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	report, err := acousticfield.VerifySnapshot(context.Background(), store, acousticfield.SnapshotID(sample.Level, opts), sample.Truth, opts)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	fmt.Printf("verify: %s chunks=%d max_residual=%d field=%s sequence=%s\n",
		map[bool]string{true: "PASS", false: "FAIL"}[report.OK],
		report.Chunks, report.Conservation.MaxResidual, report.FieldFingerprint, report.CommitSequenceHash)
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
