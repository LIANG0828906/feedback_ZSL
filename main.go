package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	dc "dialoguecal/core"
)

type jsonReport struct {
	Seed               int64                `json:"seed"`
	SchemaHashes       map[string]string    `json:"schema_hashes"`
	FrameCount         int                  `json:"frame_count"`
	TruthFailures      int                  `json:"truth_failures"`
	ParallelMatches    bool                 `json:"parallel_matches"`
	IncrementalMatches bool                 `json:"incremental_matches"`
	CheckpointMatches  bool                 `json:"checkpoint_matches"`
	Recovered          bool                 `json:"recovered"`
	Injections         []dc.InjectionResult `json:"injections"`
	InjectionsAllOK    bool                 `json:"injections_all_ok"`
	Frames             []dc.FrameResult     `json:"frames"`
	Comparisons        []dc.FrameComparison `json:"comparisons"`
	Incremental        *dc.RecomputeReport  `json:"incremental,omitempty"`
	Critical           *dc.CriticalSuite    `json:"critical,omitempty"`
}

func main() {
	seed := flag.Int64("seed", 42, "deterministic generator seed")
	limit := flag.Int("limit", 0, "limit text frame rows; 0 prints every frame")
	checkpointDir := flag.String("checkpoint", filepath.Join("artifacts", "demo-checkpoint"), "checkpoint directory")
	useSQLite := flag.Bool("sqlite", true, "use pure-Go SQLite checkpoint store")
	asJSON := flag.Bool("json", false, "emit comprehensive JSON instead of text")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fixture, err := dc.GenerateFixture(*seed)
	if err != nil {
		fatal("generate fixture", err)
	}

	loaded := map[string]*dc.Project{}
	hashes := map[string]string{}
	for name, raw := range map[string][]byte{"v1": fixture.V1JSON, "v2": fixture.V2JSON, "v3": fixture.V3JSON} {
		result, loadErr := dc.LoadProjectJSON(raw)
		if loadErr != nil {
			fatal("load "+name, loadErr)
		}
		loaded[name] = result.Project
		hashes[name] = dc.ContentHash(result.Project)
	}
	if hashes["v1"] != hashes["v2"] || hashes["v1"] != hashes["v3"] {
		fatal("cross-schema hash", fmt.Errorf("hashes differ: %v", hashes))
	}

	engine, err := dc.NewEngine(fixture.Project)
	if err != nil {
		fatal("engine", err)
	}
	full, err := engine.ComputeAll()
	if err != nil {
		fatal("full compute", err)
	}
	comparisons := dc.CompareResults(full.Results, fixture.Truth)
	truthFailures := 0
	for _, cmp := range comparisons {
		if !cmp.AllWithin {
			truthFailures++
		}
	}

	parallel, err := dc.ParallelComputeAll(ctx, fixture.Project, dc.DefaultBudget())
	if err != nil {
		fatal("parallel compute", err)
	}
	parallelMatches := dc.ResultsEquivalent(parallel.Results, full.Results)

	edit := dc.Edit{Kind: dc.EditGain, SegmentID: "seg-1", GainPermille: 950, Reason: "demo: lower seg-1 gain"}
	incremental, err := engine.RecomputeWithEdits(edit)
	if err != nil {
		fatal("incremental recompute", err)
	}
	editedProject, err := dc.ApplyEdits(fixture.Project, edit)
	if err != nil {
		fatal("apply edit", err)
	}
	editedEngine, err := dc.NewEngine(editedProject)
	if err != nil {
		fatal("edited engine", err)
	}
	fullEdited, err := editedEngine.ComputeAll()
	if err != nil {
		fatal("edited full compute", err)
	}
	incrementalMatches := dc.ResultsEquivalent(incremental.Results, fullEdited.Results)

	_ = os.RemoveAll(*checkpointDir)
	if err := os.MkdirAll(*checkpointDir, 0o755); err != nil {
		fatal("checkpoint dir", err)
	}
	version := dc.ContentHash(fixture.Project)
	store, err := openStore(*checkpointDir, *useSQLite)
	if err != nil {
		fatal("open checkpoint", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	interrupted, interruptErr := dc.RunCheckpointed(runCtx, fixture.Project, store, version, func(seq int) {
		if seq == 24 {
			cancel()
		}
	})
	if interruptErr != nil && interruptErr != context.Canceled {
		fatal("interrupted checkpoint run", interruptErr)
	}
	committedBeforeRestart := 0
	if interrupted != nil {
		committedBeforeRestart = len(interrupted.Results)
	}
	_ = store.Close()

	store, err = openStore(*checkpointDir, *useSQLite)
	if err != nil {
		fatal("reopen checkpoint", err)
	}
	recoveredReport, recovered, err := dc.RecoverAndComplete(ctx, fixture.Project, store, version, nil)
	if err != nil {
		fatal("recover checkpoint", err)
	}
	_ = store.Close()
	checkpointMatches := dc.ResultsEquivalent(recoveredReport.Results, full.Results)

	injections := dc.RunInjectionSuite(fixture, *checkpointDir)
	injectionsOK := true
	for _, result := range injections {
		if !result.OK || !result.SourceRemainedSame {
			injectionsOK = false
		}
	}

	report := jsonReport{
		Seed: *seed, SchemaHashes: hashes, FrameCount: len(full.Results), TruthFailures: truthFailures,
		ParallelMatches: parallelMatches, IncrementalMatches: incrementalMatches,
		CheckpointMatches: checkpointMatches, Recovered: recovered,
		Injections: injections, InjectionsAllOK: injectionsOK,
		Frames: full.Results, Comparisons: comparisons, Incremental: incremental, Critical: fixture.Critical,
	}

	if *asJSON {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fatal("json encode", err)
		}
		fmt.Println(string(encoded))
	} else {
		printText(report, *limit, committedBeforeRestart, *useSQLite)
	}

	if truthFailures != 0 || !parallelMatches || !incrementalMatches || !checkpointMatches || !injectionsOK {
		os.Exit(1)
	}
}

func openStore(dir string, useSQLite bool) (dc.CheckpointStore, error) {
	if !useSQLite {
		return dc.NewFileCheckpointStore(filepath.Join(dir, "file"))
	}
	return dc.NewSQLiteCheckpointStore(filepath.Join(dir, "checkpoint.db"))
}

func fatal(action string, err error) {
	if be, ok := dc.AsBusinessError(err); ok {
		fmt.Fprintf(os.Stderr, "%s failed: %s path=%s context=%v\n", action, be.Code, be.Path, be.Context)
	} else {
		fmt.Fprintf(os.Stderr, "%s failed: %v\n", action, err)
	}
	os.Exit(1)
}

func printText(report jsonReport, limit int, committed int, sqlite bool) {
	fmt.Println("DialogueCal deterministic verification demo")
	fmt.Printf("seed=%d frames=%d schema_hash=%s\n", report.Seed, report.FrameCount, report.SchemaHashes["v1"])
	fmt.Printf("cross_schema=OK parallel=%v incremental=%v truth_failures=%d\n", report.ParallelMatches, report.IncrementalMatches, report.TruthFailures)
	storeName := "atomic file"
	if sqlite {
		storeName = "pure-Go SQLite"
	}
	fmt.Printf("checkpoint=%s interrupted_after_committed_frames=%d recovered=%v matches_full=%v\n\n", storeName, committed, report.Recovered, report.CheckpointMatches)

	fmt.Println("FRAME FEATURES / CALIBRATION")
	fmt.Println("seq seg loc  rms   zcr f0Hz  state           corrDB  calDB   why")
	for i, frame := range report.Frames {
		if limit > 0 && i >= limit {
			break
		}
		f0 := "-"
		if len(frame.Features.F0Candidates) > 0 {
			f0 = fmt.Sprintf("%.1f", float64(frame.Features.F0Candidates[0].HzQ8)/256.0)
		}
		why := frame.Provenance.CacheState
		if len(frame.Provenance.Sources) > 0 {
			why = frame.Provenance.Sources[0]
		}
		fmt.Printf("%3d %5s %3d %5d %4d %5s %-15s %7.2f %7.2f %s\n",
			frame.Frame.GlobalIndex, frame.Frame.SegmentID, frame.Frame.IndexInSegment,
			frame.Features.RMSQ15, frame.Features.ZCRCount, f0,
			frame.Calibration.State, float64(frame.Calibration.CorrectionDBQ8)/256.0,
			float64(frame.Calibration.CalibratedDBQ8)/256.0, why)
	}
	if limit > 0 && report.FrameCount > limit {
		fmt.Printf("... %d more frames; rerun with --limit=0 for all frames or --json for complete provenance\n", report.FrameCount-limit)
	}

	if report.Incremental != nil {
		fmt.Println("\nINVALIDATION CLOSURES")
		for _, closure := range report.Incremental.Closures {
			fmt.Printf("%s source=%s direct=%v closure=%v\n", closure.ID, closure.Source, closure.DirectRanges, closure.ClosureRanges)
		}
		fmt.Printf("recomputed=%d reused=%d incremental_matches_full=%v\n", len(report.Incremental.RecomputedSeqs), len(report.Incremental.ReusedSeqs), report.IncrementalMatches)
	}

	if report.Critical != nil {
		fmt.Println("\nCRITICAL FIXED-POINT CASES")
		for _, caseReport := range report.Critical.Cases {
			bad := 0
			for _, cmp := range caseReport.Comparisons {
				if !cmp.AllWithin {
					bad++
				}
			}
			fmt.Printf("%s frames=%d out_of_tolerance=%d all_within=%v\n", caseReport.ID, len(caseReport.Results), bad, caseReport.AllWithin)
			for i, frame := range caseReport.Results {
				if i >= 12 {
					break
				}
				cmp := caseReport.Comparisons[i]
				fmt.Printf("  f=%02d energy=%d dE=%d within=%v corrDB=%.3f dC=%d within=%v calDB=%.3f loudWithin=%v stateMatch=%v\n",
					frame.Frame.GlobalIndex, frame.Calibration.EnergyQ15, cmp.EnergyDeltaQ15, cmp.EnergyWithin,
					float64(frame.Calibration.CorrectionDBQ8)/256.0, cmp.CorrectionDeltaDBQ8, cmp.CorrectionWithin,
					float64(frame.Calibration.CalibratedDBQ8)/256.0, cmp.LoudnessWithin, cmp.StateMatch)
			}
		}
		for _, check := range report.Critical.OverflowChecks {
			fmt.Printf("overflow %s code=%s saturated=%v ok=%v\n", check.Name, check.GotCode, check.Saturated, check.OK)
		}
	}

	fmt.Println("\nEXCEPTION CODES")
	for _, injection := range report.Injections {
		status := "OK"
		if !injection.OK || !injection.SourceRemainedSame {
			status = "FAIL"
		}
		fmt.Printf("[%s] %-42s code=%-28s source_unchanged=%v path=%s\n", status, injection.Name, injection.Code, injection.SourceRemainedSame, injection.Path)
	}
}
