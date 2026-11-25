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
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"terrainbake/bake"
	"terrainbake/checkpoint"
	"terrainbake/diagnostic"
	"terrainbake/generator"
	"terrainbake/model"
	"terrainbake/project"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "validate":
		err = runValidate(os.Args[2:])
	case "demo":
		err = runDemo(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	default:
		usage()
		err = model.NewError(model.CodeValue, "unknown command %q", os.Args[1])
	}
	if err != nil {
		var coded *model.CodedError
		if errors.As(err, &coded) {
			payload, _ := json.MarshalIndent(coded, "", "  ")
			fmt.Fprintln(os.Stderr, string(payload))
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `terrainbake commands:
  generate  -seed N [-out artifacts/sample-N.json]
  validate  -sample artifacts/sample-424242.json
  demo      [-seed N] [-db artifacts/demo.sqlite] [-sample artifacts/sample-N.json] [-json]
  serve     [-seed N] [-db artifacts/diagnostic.sqlite] [-addr 127.0.0.1:8090]`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", 424242, "deterministic seed")
	out := fs.String("out", "", "output bundle path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join("artifacts", fmt.Sprintf("sample-%d.json", *seed))
	}
	bundle, err := generator.Generate(*seed)
	if err != nil {
		return err
	}
	data, err := generator.MarshalBundle(bundle)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return model.NewError(model.CodeIO, "create output directory: %v", err).WithField("out")
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return model.NewError(model.CodeIO, "write sample: %v", err).WithField("out")
	}
	fmt.Printf("generated seed=%d chunks=%d truth_points=%d file=%s\n",
		*seed, len(bundle.Project.Chunks), len(bundle.Truth.Points), *out)
	return nil
}

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	sample := fs.String("sample", filepath.Join("artifacts", "sample-424242.json"), "sample bundle path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := os.ReadFile(*sample)
	if err != nil {
		return model.NewError(model.CodeIO, "read sample: %v", err).WithField("sample")
	}
	bundle, err := project.LoadBundle(data)
	if err != nil {
		return err
	}
	report := map[string]any{
		"seed":             bundle.Project.Seed,
		"chunks":           len(bundle.Project.Chunks),
		"sky_samples":      len(bundle.Project.Sky),
		"sun_samples":      len(bundle.Project.Sun),
		"truth_points":     len(bundle.Truth.Points),
		"parameter_ver":    bundle.Project.Params.ParameterVersion,
		"algorithm_ver":    bundle.Project.Params.AlgorithmVersion,
		"fixed_scale":      bundle.Project.Params.FixedScale,
		"project_identity": model.ProjectIdentity(bundle.Project),
	}
	data, _ = json.MarshalIndent(report, "", "  ")
	fmt.Println(string(data))
	return nil
}

// DemoReport is the machine-readable acceptance summary.
type DemoReport struct {
	Seed                 int64               `json:"seed"`
	SamplePath           string              `json:"sample_path"`
	Database             string              `json:"database"`
	Chunks               int                 `json:"chunks"`
	TruthPoints          int                 `json:"truth_points"`
	TruthReport          *bake.TruthReport   `json:"truth_report"`
	TruthOK              bool                `json:"truth_ok"`
	AnyOrderEqual        bool                `json:"any_order_equal"`
	ResumeLoaded         int                 `json:"resume_loaded"`
	ResumeReused         int                 `json:"resume_reused"`
	ResumeEqual          bool                `json:"resume_equal"`
	Invalidations        []bake.Invalidation `json:"invalidations"`
	IncrementalEqual     bool                `json:"incremental_equal"`
	IncrementalRebaked   int                 `json:"incremental_rebaked"`
	SeamEdges            int                 `json:"seam_edges"`
	SeamTransfers        int                 `json:"seam_transfers"`
	QueueEvents          int                 `json:"queue_events"`
	BufferBatches        int                 `json:"buffer_batches"`
	CommitSequenceStable bool                `json:"commit_sequence_stable"`
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 424242, "deterministic seed")
	dbPath := fs.String("db", filepath.Join("artifacts", "demo.sqlite"), "SQLite checkpoint path")
	samplePath := fs.String("sample", "", "generated sample path")
	jsonOut := fs.Bool("json", false, "emit only JSON report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *samplePath == "" {
		*samplePath = filepath.Join("artifacts", fmt.Sprintf("sample-%d.json", *seed))
	}
	ctx := context.Background()
	bundle, err := generator.Generate(*seed)
	if err != nil {
		return err
	}
	data, err := generator.MarshalBundle(bundle)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*samplePath), 0o755); err != nil {
		return model.NewError(model.CodeIO, "create sample directory: %v", err)
	}
	if err := os.WriteFile(*samplePath, data, 0o644); err != nil {
		return model.NewError(model.CodeIO, "write sample: %v", err)
	}
	loaded, err := project.LoadBundle(data)
	if err != nil {
		return err
	}
	opts := bake.DefaultScheduleOptions(loaded.Project.Params)

	wholeEngine, err := bake.NewEngine(&loaded.Project, nil)
	if err != nil {
		return err
	}
	whole, err := wholeEngine.BakeFull(ctx, opts)
	if err != nil {
		return err
	}
	truthReport, truthErr := bake.CompareTruth(whole, loaded.Truth, loaded.Project.Params.IrradianceTolerance)
	truthOK := truthErr == nil
	if truthErr != nil {
		return truthErr
	}

	reverse := make([]string, 0, len(loaded.Project.Chunks))
	for i := len(loaded.Project.Chunks) - 1; i >= 0; i-- {
		reverse = append(reverse, loaded.Project.Chunks[i].ID)
	}
	orderEngine, _ := bake.NewEngine(&loaded.Project, nil)
	ordered, err := orderEngine.BakeFullOrder(ctx, reverse, opts)
	if err != nil {
		return err
	}
	if _, err := bake.CompareResults(whole, ordered); err != nil {
		return err
	}
	if _, err := bake.AnalyzeSeams(whole, wholeEngine.Terrain()); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		return model.NewError(model.CodeIO, "create db directory: %v", err)
	}
	for _, path := range []string{*dbPath, *dbPath + "-wal", *dbPath + "-shm"} {
		_ = os.Remove(path)
	}
	store, engine, err := openStoredEngine(ctx, *dbPath, &loaded.Project)
	if err != nil {
		return err
	}
	if _, err := engine.LoadPersisted(ctx); err != nil {
		store.Close()
		return err
	}
	persisted, err := engine.BakeIncremental(ctx, opts)
	if err != nil {
		store.Close()
		return err
	}
	if _, err := bake.CompareResults(whole, persisted); err != nil {
		store.Close()
		return err
	}
	if err := store.Close(); err != nil {
		return err
	}

	store2, engine2, err := openStoredEngine(ctx, *dbPath, &loaded.Project)
	if err != nil {
		return err
	}
	defer store2.Close()
	resumeLoaded, err := engine2.LoadPersisted(ctx)
	if err != nil {
		return err
	}
	resumed, err := engine2.BakeIncremental(ctx, opts)
	if err != nil {
		return err
	}
	if _, err := bake.CompareResults(whole, resumed); err != nil {
		return err
	}

	invalidations, err := engine2.RaiseHeight(ctx, "chunk-00-00", model.VertexPoint{X: 1, Z: 1}, 3.5)
	if err != nil {
		return err
	}
	incremental, err := engine2.BakeIncremental(ctx, opts)
	if err != nil {
		return err
	}
	freshEngine, _ := bake.NewEngine(engine2.Project(), nil)
	fresh, err := freshEngine.BakeFull(ctx, opts)
	if err != nil {
		return err
	}
	if _, err := bake.CompareResults(incremental, fresh); err != nil {
		return err
	}
	editedSeams, err := bake.AnalyzeSeams(incremental, engine2.Terrain())
	if err != nil {
		return err
	}

	report := DemoReport{
		Seed: *seed, SamplePath: *samplePath, Database: *dbPath,
		Chunks: len(loaded.Project.Chunks), TruthPoints: len(loaded.Truth.Points),
		TruthReport: truthReport, TruthOK: truthOK, AnyOrderEqual: true,
		ResumeLoaded: resumeLoaded, ResumeReused: len(resumed.Reused), ResumeEqual: true,
		Invalidations: invalidations, IncrementalEqual: true, IncrementalRebaked: len(incremental.Rebaked),
		SeamEdges: len(editedSeams.Edges), SeamTransfers: len(editedSeams.Transfers),
		QueueEvents: len(incremental.Stats.QueueEvents), BufferBatches: incremental.Stats.BufferBatches,
		CommitSequenceStable: equalRefs(incremental.CommitSequence, fresh.CommitSequence),
	}
	if len(report.Invalidations) > 0 {
		sort.Slice(report.Invalidations, func(i, j int) bool {
			if report.Invalidations[i].Time != report.Invalidations[j].Time {
				return report.Invalidations[i].Time < report.Invalidations[j].Time
			}
			return report.Invalidations[i].ChunkID < report.Invalidations[j].ChunkID
		})
	}
	if *jsonOut {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("seed=%d chunks=%d truth_points=%d\n", report.Seed, report.Chunks, report.TruthPoints)
	fmt.Printf("truth: shadow_mismatch=%d max_sky=%.9g max_irr=%.9g tolerance=%.9g\n",
		report.TruthReport.ShadowMismatches, report.TruthReport.MaxSkyError,
		report.TruthReport.MaxIrradianceError, report.TruthReport.Tolerance)
	fmt.Printf("paths: any_order=%t resume_equal=%t incremental_equal=%t commit_stable=%t\n",
		report.AnyOrderEqual, report.ResumeEqual, report.IncrementalEqual, report.CommitSequenceStable)
	fmt.Printf("resume: loaded=%d reused=%d\n", report.ResumeLoaded, report.ResumeReused)
	fmt.Printf("edit: invalidations=%d incremental_rebaked=%d\n", len(report.Invalidations), report.IncrementalRebaked)
	for _, inv := range report.Invalidations {
		fmt.Printf("  invalidate %s@%d reason=%s dependency=%s\n", inv.ChunkID, inv.Time, inv.Reason, inv.Dependency)
	}
	fmt.Printf("seams: edges=%d transfers=%d queue_events=%d buffer_batches=%d\n",
		report.SeamEdges, report.SeamTransfers, report.QueueEvents, report.BufferBatches)
	fmt.Printf("sample=%s db=%s\n", report.SamplePath, report.Database)
	return nil
}

func openStoredEngine(ctx context.Context, path string, p *model.Project) (*checkpoint.Store, *bake.Engine, error) {
	store, err := checkpoint.Open(ctx, path, model.ProjectIdentity(*p), p.Params)
	if err != nil {
		return nil, nil, err
	}
	engine, err := bake.NewEngine(p, store)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return store, engine, nil
}

func equalRefs(a, b []bake.UnitRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	seed := fs.Int64("seed", 424242, "deterministic seed")
	dbPath := fs.String("db", filepath.Join("artifacts", "diagnostic.sqlite"), "SQLite checkpoint path")
	addr := fs.String("addr", "127.0.0.1:8090", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	controller, err := diagnostic.New(*seed, *dbPath)
	if err != nil {
		return err
	}
	defer controller.Close()
	server := &http.Server{
		Addr:              *addr,
		Handler:           diagnostic.NewHTTPHandler(controller),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Printf("terrain bake diagnostics: http://%s (seed=%d db=%s)\n", server.Addr, *seed, *dbPath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return model.NewError(model.CodeIO, "serve diagnostics: %v", err)
	}
	return nil
}
