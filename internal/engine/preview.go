package engine

import (
	"context"
	"time"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/sample"
)

func (e *Engine) Preview(ctx context.Context, smp *sample.Sample) (*PreviewResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if smp == nil || smp.Image == nil {
		return nil, apperr.New(apperr.CodeMeta, "preview", "sample image is required")
	}
	if smp.Manifest.Width != e.Versions.Width() || smp.Manifest.Height != e.Versions.Height() {
		return nil, apperr.New(apperr.CodeMeta, "preview", "sample dimensions differ from versioned pipeline dimensions").
			WithField("width")
	}
	snap := e.Versions.Latest()
	if snap.AlgorithmVersion != smp.Manifest.AlgorithmVersion {
		return nil, apperr.New(apperr.CodeVersion, "preview", "sample algorithm version differs from pipeline version").
			WithDetail("left_version", snap.AlgorithmVersion).
			WithDetail("right_version", smp.Manifest.AlgorithmVersion)
	}
	start := time.Now()
	opts := e.sampleToSegmentOptions(smp, snap.Params, e.Opts.PreviewIterationBudget)
	res, err := runSegment(smp.Image, opts)
	if err != nil {
		return nil, err
	}
	initialIterations := append([]int(nil), res.Iterations...)
	fallback := false
	if res.Truncated {
		fallback = true
		opts.budget = 0
		full, err := runSegment(smp.Image, opts)
		if err != nil {
			return nil, err
		}
		res = full
	}
	latency := time.Since(start)
	latencyMS := float64(latency.Nanoseconds()) / 1e6
	within := latency <= time.Duration(smp.Manifest.PreviewDeadlineMS)*time.Millisecond
	basis := "full-resolution feature threshold with reduced BFS iteration budget; same snapshot and cleanup chain"
	if fallback {
		basis = "reduced BFS budget was insufficient; preview completed with the same full-resolution snapshot to prevent silent drift"
	}
	out := &PreviewResult{
		Labels:    append([]byte(nil), res.Labels...),
		Stats:     res.Stats,
		MaskHash:  res.MaskHash,
		StatsHash: res.StatsHash,
		Meta: PreviewMeta{
			ParamVersion:      snap.Version,
			ParamsHash:        snap.ParamsHash,
			AlgorithmVersion:  snap.AlgorithmVersion,
			DownsampleFactor:  1,
			IterationBudget:   e.Opts.PreviewIterationBudget,
			InitialIterations: initialIterations,
			UsedIterations:    append([]int(nil), res.Iterations...),
			FallbackFull:      fallback,
			Basis:             basis,
			LatencyMS:         latencyMS,
			DeadlineMS:        smp.Manifest.PreviewDeadlineMS,
			WithinDeadline:    within,
		},
	}
	if !within {
		return out, apperr.New(apperr.CodeLatency, "preview", "preview exceeded the manifest latency ceiling").
			WithDetail("actual_ms", latencyMS).
			WithDetail("deadline_ms", smp.Manifest.PreviewDeadlineMS).
			WithDetail("param_version", snap.Version)
	}
	return out, nil
}
