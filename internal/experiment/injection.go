package experiment

import (
	"context"
	"path/filepath"

	"screenverify/internal/dataset"
	"screenverify/internal/model"
	"screenverify/internal/rverr"
	"screenverify/internal/store"
	"screenverify/internal/verify"
)

type InjectionCase struct {
	Name    string
	Field   string
	Initial verify.Params
	Next    verify.Params
}

type InjectionResult struct {
	Case                 InjectionCase        `json:"case"`
	Change               *verify.ChangeResult `json:"change"`
	IncrementalStats     verify.Stats         `json:"incremental_stats"`
	FullStats            verify.Stats         `json:"full_stats"`
	IncrementalIntervals []verify.Interval    `json:"incremental_intervals"`
	FullIntervals        []verify.Interval    `json:"full_intervals"`
	Consistent           bool                 `json:"consistent"`
	Diff                 *verify.Difference   `json:"diff,omitempty"`
}

func DefaultInjectionCases(base verify.Params) []InjectionCase {
	block := base
	block.BlockSize = 8

	threshold := base
	threshold.TamperThreshold = 0.8

	wrongRegion := base
	wrongRegion.WatermarkRegion = &model.Rect{X: 60, Y: 4, Width: 32, Height: 16}
	correctRegion := base
	correctRegion.WatermarkRegion = &model.Rect{X: 62, Y: 4, Width: 32, Height: 16}

	window := base
	window.ResidualWindow = 2

	return []InjectionCase{
		{Name: "block_size_noise_only", Field: "block_size", Initial: base, Next: block},
		{Name: "decision_threshold_downstream_only", Field: "tamper_threshold", Initial: base, Next: threshold},
		{Name: "watermark_region_precise", Field: "watermark_region", Initial: wrongRegion, Next: correctRegion},
		{Name: "residual_window", Field: "residual_window", Initial: base, Next: window},
	}
}

func RunInjection(ctx context.Context, clipDir, workDB string, rt verify.RuntimeConfig, c InjectionCase) (*InjectionResult, error) {
	out := &InjectionResult{Case: c, Consistent: true}

	incSet, err := dataset.Load(clipDir)
	if err != nil {
		return nil, err
	}
	incDB := filepath.Join(workDB, "incremental-"+c.Name+".db")
	st, err := store.Open(incDB)
	if err != nil {
		return nil, err
	}
	eng, err := verify.NewEngine(incSet, c.Initial, rt, st)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	if _, err := eng.EnsureIdentity(ctx, "injection-"+c.Name); err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := runAll(ctx, eng); err != nil {
		_ = st.Close()
		return nil, err
	}
	_ = st.Close()
	st, err = store.Open(incDB)
	if err != nil {
		return nil, err
	}
	eng, err = verify.NewEngine(incSet, c.Initial, rt, st)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	if _, err := eng.Recover(ctx, "injection-"+c.Name); err != nil {
		_ = st.Close()
		return nil, err
	}
	change, err := eng.ApplyParameterChange(ctx, 0, c.Name, c.Field, c.Next)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	incReport := eng.BuildReport(nil)
	out.Change = change
	out.IncrementalStats = incReport.Stats
	out.IncrementalIntervals = incReport.Intervals
	_ = st.Close()

	fullSet, err := dataset.Load(clipDir)
	if err != nil {
		return nil, err
	}
	fullDB := filepath.Join(workDB, "full-"+c.Name+".db")
	fst, err := store.Open(fullDB)
	if err != nil {
		return nil, err
	}
	feng, err := verify.NewEngine(fullSet, c.Next, rt, fst)
	if err != nil {
		_ = fst.Close()
		return nil, err
	}
	if _, err := feng.EnsureIdentity(ctx, "full-"+c.Name); err != nil {
		_ = fst.Close()
		return nil, err
	}
	if err := runAll(ctx, feng); err != nil {
		_ = fst.Close()
		return nil, err
	}
	fullReport := feng.BuildReport(nil)
	out.FullStats = fullReport.Stats
	out.FullIntervals = fullReport.Intervals
	_ = fst.Close()

	if d := verify.CompareSnapshots(incReport.Snapshot(), fullReport.Snapshot()); d != nil {
		out.Consistent = false
		out.Diff = d
		return out, rverr.New(rverr.EDiff, d.Message)
	}
	return out, nil
}
