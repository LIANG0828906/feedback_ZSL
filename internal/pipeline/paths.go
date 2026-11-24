package pipeline

import (
	"context"

	"audiocheck/internal/model"
)

func RunFull(ctx context.Context, cfg Config) (*model.Report, error) {
	cfg.Mode = "full_recompute"
	cfg.Force = true
	return Run(ctx, cfg)
}

func RunRealtime(ctx context.Context, cfg Config) (*model.Report, error) {
	cfg.Mode = "realtime_stream"
	cfg.Force = false
	return Run(ctx, cfg)
}

func Resume(ctx context.Context, cfg Config) (*model.Report, error) {
	cfg.Mode = "checkpoint_resume"
	cfg.Force = false
	return Run(ctx, cfg)
}

type ParameterChange struct {
	QualityChanged bool
	OldParams      model.EvalParams
	NewParams      model.EvalParams
}

func AffectedBlocks(m model.Manifest, budget model.Budget, change ParameterChange) []int {
	chunkFrames := budget.ChunkFrames
	if chunkFrames <= 0 {
		chunkFrames = DefaultBudget().ChunkFrames
	}
	chunkCount := (m.FrameCount + chunkFrames - 1) / chunkFrames
	oldP := change.OldParams.WithDefaults(m)
	newP := change.NewParams.WithDefaults(m)

	metricChanged := change.QualityChanged ||
		oldP.WindowSizeSamples != newP.WindowSizeSamples ||
		oldP.StepSamples != newP.StepSamples ||
		oldP.SSIMWindowSamples != newP.SSIMWindowSamples ||
		oldP.SSIMStepSamples != newP.SSIMStepSamples
	if !metricChanged {
		return []int{}
	}

	out := make([]int, 0, chunkCount)
	for i := 0; i < chunkCount; i++ {
		f0 := i * chunkFrames
		if f0 >= m.FrameCount {
			break
		}
		out = append(out, i)
	}
	return out
}

func AffectedBlocksForSampleRange(m model.Manifest, budget model.Budget, startSample, endSample int, p model.EvalParams) []int {
	chunkFrames := budget.ChunkFrames
	if chunkFrames <= 0 {
		chunkFrames = DefaultBudget().ChunkFrames
	}
	pp := p.WithDefaults(m)
	halo := pp.WindowSizeSamples
	start := startSample - halo
	end := endSample + halo
	var out []int
	for f := 0; f < m.FrameCount; f++ {
		fs := f * m.HopSize
		fe := fs + m.FrameSize + pp.WindowSizeSamples
		if fs <= end && fe >= start {
			idx := f / chunkFrames
			if len(out) == 0 || out[len(out)-1] != idx {
				out = append(out, idx)
			}
		}
	}
	return out
}
