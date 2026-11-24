package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/engine"
	"gameintegrity/internal/frameio"
	"gameintegrity/internal/model"
	"gameintegrity/internal/sample"
	"gameintegrity/internal/scale"
)

type runSummary struct {
	Mode           string                  `json:"mode"`
	Path           string                  `json:"path"`
	DurationMS     int64                   `json:"duration_ms"`
	ExactFrames    int                     `json:"exact_frames"`
	ScreenedFrames int                     `json:"screened_frames"`
	Replacement    model.ReplacementResult `json:"replacement"`
	Counts         model.Counts            `json:"counts"`
	TruthMatch     bool                    `json:"truth_match"`
}

type scaleCheck struct {
	SampleID         string              `json:"id"`
	Decision         model.ScaleDecision `json:"mode_decision"`
	Auto             runSummary          `json:"auto"`
	Fine             runSummary          `json:"fine"`
	Candidate        runSummary          `json:"candidate"`
	ScaleError       string              `json:"scale_error,omitempty"`
	ScaleErrorDetail *apperr.Error       `json:"scale_error_detail,omitempty"`
	TruthMatch       bool                `json:"truth_match"`
	Pass             bool                `json:"pass"`
}

type pathCheck struct {
	SampleID                 string `json:"sample_id"`
	FullDurationMS           int64  `json:"full_duration_ms"`
	RealtimeDurationMS       int64  `json:"realtime_duration_ms"`
	ResumeDurationMS         int64  `json:"resume_duration_ms"`
	PartialCommittedSegments int    `json:"partial_committed_segments"`
	FullVsRealtime           string `json:"full_vs_realtime"`
	FullVsResume             string `json:"full_vs_resume"`
	RealtimeVsResume         string `json:"realtime_vs_resume"`
	Pass                     bool   `json:"pass"`
}

type anomalyCheck struct {
	SampleID string `json:"sample_id"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Frame    int    `json:"frame"`
	Block    string `json:"block"`
	Message  string `json:"message,omitempty"`
	Pass     bool   `json:"pass"`
}

type invalidationCheck struct {
	Triggered bool              `json:"triggered"`
	Fields    []string          `json:"fields"`
	Reasons   []string          `json:"reasons"`
	Expected  map[string]string `json:"expected"`
	Actual    map[string]string `json:"actual"`
	Pass      bool              `json:"pass"`
}

type demoResult struct {
	Seed         int64             `json:"seed"`
	DataRoot     string            `json:"data_root"`
	Generated    bool              `json:"generated"`
	ScaleChecks  []scaleCheck      `json:"scale_checks"`
	ThreePaths   pathCheck         `json:"three_paths"`
	Invalidation invalidationCheck `json:"checkpoint_invalidation"`
	Anomalies    []anomalyCheck    `json:"anomalies"`
	AllPass      bool              `json:"all_pass"`
	StartedAt    string            `json:"started_at"`
	DurationMS   int64             `json:"duration_ms"`
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	data := fs.String("data", "data/samples", "sample root")
	out := fs.String("out", "artifacts/demo-results.json", "demo result JSON")
	seed := fs.Int64("seed", 20261124, "generator seed when auto-generating")
	if err := fs.Parse(args); err != nil {
		return err
	}
	started := time.Now()
	generated := false
	ids, _ := frameio.ListSamples(*data)
	if len(ids) == 0 {
		if _, err := sample.Generate(*data, *seed); err != nil {
			return err
		}
		generated = true
	}
	result := demoResult{Seed: *seed, DataRoot: *data, Generated: generated, StartedAt: started.Format(time.RFC3339Nano)}
	ctx := context.Background()
	autoReports := map[string]*model.RunReport{}
	for _, spec := range sample.Specs() {
		if spec.Injection != "" {
			continue
		}
		ds, err := frameio.Open(*data, spec.ID)
		if err != nil {
			return fmt.Errorf("open %s: %w", spec.ID, err)
		}
		cfg := demoConfig(ds)
		auto, err := engine.Verify(ctx, ds, cfg, engine.Options{Path: "full", Mode: "auto"})
		if err != nil {
			return fmt.Errorf("auto verify %s: %w", spec.ID, err)
		}
		fine, err := engine.Verify(ctx, ds, cfg, engine.Options{Path: "full", Mode: "fine"})
		if err != nil {
			return fmt.Errorf("fine verify %s: %w", spec.ID, err)
		}
		candidate, err := engine.Verify(ctx, ds, cfg, engine.Options{Path: "full", Mode: "candidate"})
		if err != nil {
			return fmt.Errorf("candidate verify %s: %w", spec.ID, err)
		}
		autoReports[spec.ID] = auto
		check := scaleCheck{
			SampleID: spec.ID, Decision: auto.Scale,
			Auto: summarize(auto), Fine: summarize(fine), Candidate: summarize(candidate),
			TruthMatch: auto.Replacement.WithinTolerance && fine.Replacement.WithinTolerance && candidate.Replacement.WithinTolerance,
		}
		if err := scale.CheckIntervalAgreement(fine.Replacement.Detected, candidate.Replacement.Detected, spec.Tolerance); err != nil {
			check.ScaleError = err.Error()
			check.ScaleErrorDetail = apperr.As(err)
		}
		check.Pass = check.TruthMatch && check.ScaleError == ""
		result.ScaleChecks = append(result.ScaleChecks, check)
	}

	medium, err := frameio.Open(*data, "medium")
	if err != nil {
		return err
	}
	cfg := demoConfig(medium)
	full := autoReports["medium"]
	if full == nil {
		return fmt.Errorf("medium auto report missing")
	}
	realtimeWork := filepath.Join("artifacts", "demo-work", "paths-realtime-medium")
	_ = os.RemoveAll(realtimeWork)
	realtime, err := engine.Verify(ctx, medium, cfg, engine.Options{Path: "realtime", Mode: "auto", WorkDir: realtimeWork})
	if err != nil {
		return fmt.Errorf("realtime: %w", err)
	}
	resumeWork := filepath.Join("artifacts", "demo-work", "paths-resume-medium")
	_ = os.RemoveAll(resumeWork)
	partial, err := engine.Verify(ctx, medium, cfg, engine.Options{
		Path: "realtime", Mode: "auto", WorkDir: resumeWork, StopAfterCommitted: 3,
	})
	if err != nil {
		return fmt.Errorf("resume setup: %w", err)
	}
	resumed, err := engine.Verify(ctx, medium, cfg, engine.Options{Path: "resume", Mode: "auto", WorkDir: resumeWork})
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	pathResult := pathCheck{
		SampleID:       "medium",
		FullDurationMS: full.DurationMS, RealtimeDurationMS: realtime.DurationMS,
		ResumeDurationMS: resumed.DurationMS, PartialCommittedSegments: partial.CommittedSegments,
	}
	pathResult.FullVsRealtime = equalOrError(engine.Compare(full, realtime))
	pathResult.FullVsResume = equalOrError(engine.Compare(full, resumed))
	pathResult.RealtimeVsResume = equalOrError(engine.Compare(realtime, resumed))
	pathResult.Pass = pathResult.FullVsRealtime == "equal" && pathResult.FullVsResume == "equal" && pathResult.RealtimeVsResume == "equal"
	result.ThreePaths = pathResult

	mismatchCfg := cfg
	mismatchCfg.ResidualThreshold += 0.001
	invalidated, err := engine.Verify(ctx, medium, mismatchCfg, engine.Options{Path: "resume", Mode: "auto", WorkDir: resumeWork})
	if err != nil {
		return fmt.Errorf("invalidation check: %w", err)
	}
	inv := invalidationCheck{Triggered: invalidated.Invalidation != nil}
	if invalidated.Invalidation != nil {
		inv.Fields = invalidated.Invalidation.Fields
		inv.Reasons = invalidated.Invalidation.Reasons
		inv.Expected = invalidated.Invalidation.Expected
		inv.Actual = invalidated.Invalidation.Actual
		for _, field := range inv.Fields {
			if field == "param_version" {
				inv.Pass = true
			}
		}
	}
	result.Invalidation = inv

	expected := map[string]string{
		"small-e-order": apperr.Order,
		"small-e-mark":  apperr.Mark,
		"small-e-frame": apperr.Frame,
		"small-e-size":  apperr.Meta,
	}
	for _, spec := range sample.Specs() {
		if spec.Injection == "" {
			continue
		}
		_, openErr := frameio.Open(*data, spec.ID)
		check := anomalyCheck{SampleID: spec.ID, Expected: expected[spec.ID], Frame: -1}
		if openErr == nil {
			check.Actual = "nil"
		} else if e := apperr.As(openErr); e != nil {
			check.Actual = e.Code
			check.Frame = e.Frame
			check.Block = fmt.Sprintf("col=%d,row=%d,x=%d,y=%d", e.Block.Col, e.Block.Row, e.Block.X, e.Block.Y)
			check.Message = e.Message
		} else {
			check.Actual = "unknown"
			check.Message = openErr.Error()
		}
		check.Pass = check.Actual == check.Expected
		result.Anomalies = append(result.Anomalies, check)
	}
	result.AllPass = pathResult.Pass && inv.Pass && len(result.Anomalies) == 4
	for _, c := range result.ScaleChecks {
		result.AllPass = result.AllPass && c.Pass
	}
	for _, c := range result.Anomalies {
		result.AllPass = result.AllPass && c.Pass
	}
	result.DurationMS = time.Since(started).Milliseconds()
	if err := writeJSONFile(*out, result); err != nil {
		return err
	}
	printDemo(result)
	if !result.AllPass {
		e := apperr.New(apperr.Scale, "demo acceptance did not pass")
		e.Details = map[string]any{"result": *out, "all_pass": false}
		return e
	}
	return nil
}

func demoConfig(ds *frameio.Dataset) model.Config {
	cfg := model.DefaultConfig(ds.Manifest.Width, ds.Manifest.Height, ds.Manifest.Channels, ds.Manifest.FrameCount, ds.Manifest.BlockSize, ds.Manifest.SamplingBudget)
	cfg.SegmentSeconds = ds.Manifest.SegmentSeconds
	return cfg
}

func summarize(r *model.RunReport) runSummary {
	return runSummary{
		Mode: r.Mode, Path: r.Path, DurationMS: r.DurationMS,
		ExactFrames:    len(r.Scale.RefinedFrames),
		ScreenedFrames: max(0, len(r.Frames)-len(r.Scale.RefinedFrames)),
		Replacement:    r.Replacement, Counts: r.Counts,
		TruthMatch: r.Replacement.WithinTolerance,
	}
}

func equalOrError(err error) string {
	if err == nil {
		return "equal"
	}
	if e := apperr.As(err); e != nil {
		return e.Code + ": " + e.Error()
	}
	return err.Error()
}

func printDemo(r demoResult) {
	fmt.Printf("demo all_pass=%v duration_ms=%d generated=%v\n", r.AllPass, r.DurationMS, r.Generated)
	for _, c := range r.ScaleChecks {
		fmt.Printf("  %-7s auto=%-18s stride=%-2d ds=%-2d truth=%v fine=%d..%d candidate=%d..%d pass=%v\n",
			c.SampleID, c.Decision.Mode, c.Decision.Stride, c.Decision.BlockDownsample,
			c.TruthMatch, c.Fine.Replacement.Detected.Start, c.Fine.Replacement.Detected.End,
			c.Candidate.Replacement.Detected.Start, c.Candidate.Replacement.Detected.End, c.Pass)
	}
	fmt.Printf("  three paths full/realtime/resume: %s | %s | %s\n",
		r.ThreePaths.FullVsRealtime, r.ThreePaths.FullVsResume, r.ThreePaths.RealtimeVsResume)
	fmt.Printf("  checkpoint invalidation fields=%v pass=%v\n", r.Invalidation.Fields, r.Invalidation.Pass)
	for _, a := range r.Anomalies {
		fmt.Printf("  %-15s expected=%-8s actual=%-8s frame=%d block=%s pass=%v\n",
			a.SampleID, a.Expected, a.Actual, a.Frame, a.Block, a.Pass)
	}
}
