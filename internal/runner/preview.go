package runner

import (
	"context"
	"math"
	"time"

	"edgerefine/internal/model"
	"edgerefine/internal/pipeline"
	"edgerefine/internal/sample"
)

type PreviewResult struct {
	Report    *model.RunReport
	Snapshots map[[2]int]model.ThresholdSnapshot
}

func BuildSnapshots(b *sample.Bundle, p model.Params, manual *float64) (map[[2]int]model.ThresholdSnapshot, error) {
	tiles := tileList(b.Manifest)
	out := make(map[[2]int]model.ThresholdSnapshot, len(tiles))
	for _, t := range tiles {
		r := pipeline.TileRect(p, t)
		snap, err := pipeline.EstimateThreshold(b.Manifest.SampleID, b.Image, t, r, p, manual)
		if err != nil {
			return nil, err
		}
		out[t.Array()] = snap
	}
	return out, nil
}

func PreviewRadius(radius, step int) int {
	if step <= 1 {
		return radius
	}
	return int(math.Round(float64(radius)/float64(step))) * step
}

func RunPreview(b *sample.Bundle, base model.Params, manual *float64, workers int, gate *Gate, ctx context.Context) (*PreviewResult, error) {
	started := time.Now()
	snaps, err := BuildSnapshots(b, base, manual)
	if err != nil {
		return nil, err
	}
	step := 2
	previewParams := base
	previewParams.IterBudget = int(math.Max(1, float64(base.IterBudget/2)))
	previewParams.Radius = PreviewRadius(base.Radius, step)
	rep, err := Run(b, Options{
		Workers: workers, Params: previewParams, Snapshots: snaps, ManualThreshold: manual,
		Gate: gate, Ctx: ctx, ElementStep: step, StrictAccuracy: false, Persist: false,
	})
	delay := time.Since(started).Milliseconds()
	if err != nil && rep == nil {
		return nil, err
	}
	median := 999.0
	ratio := 0.0
	if rep != nil {
		if rep.Accuracy != nil {
			median = rep.Accuracy.Median
		}
		ratio = rep.ConvergedTileRatio
		rep.Preview = &model.PreviewReport{
			ActualBudget: previewParams.IterBudget, ElementStep: step,
			DelayMS: delay, DelayLimitMS: base.PreviewDelayMS,
			WithinLimit: delay <= int64(base.PreviewDelayMS),
			MedianBias:  median, ConvergedRatio: ratio, MaskHash: rep.MaskHash,
			DecisionBasis: "drag/coarse path uses shared threshold snapshot, halved iteration budget, radius snapped with step 2",
		}
	}
	return &PreviewResult{Report: rep, Snapshots: snaps}, err
}
