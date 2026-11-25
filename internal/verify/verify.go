package verify

import (
	"context"
	"fmt"
	"math"
	"sort"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/core"
	"qualitybench/internal/model"
	"qualitybench/internal/quality"
)

type Report struct {
	RunID             string    `json:"run_id"`
	TruthFingerprint  string    `json:"truth_fingerprint"`
	AlgorithmVersion  string    `json:"algorithm_version"`
	Images            int       `json:"images"`
	Blocks            int       `json:"blocks"`
	MetricsPassed     int       `json:"metrics_passed"`
	AttributionPassed int       `json:"attribution_passed"`
	SamplesPassed     int       `json:"samples_passed"`
	Failures          []Failure `json:"failures"`
	Verified          bool      `json:"verified"`
}

type Failure struct {
	Image  string         `json:"image"`
	Step   int            `json:"step"`
	Block  *core.BlockRef `json:"block,omitempty"`
	Field  string         `json:"field"`
	Detail string         `json:"detail"`
}

func Run(ctx context.Context, store checkpoint.Store, runID string, truth *model.Truth,
	truthFP string) (*Report, error) {
	if truth == nil {
		return nil, core.Schema("truth", "truth is missing")
	}
	report := &Report{RunID: runID, TruthFingerprint: truthFP, AlgorithmVersion: truth.AlgorithmVersion}
	for _, imageTruth := range truth.Images {
		if err := ctx.Err(); err != nil {
			return report, core.CanceledError("verification canceled")
		}
		meta, ok, err := store.GetRunMeta(ctx, runID, imageTruth.ID)
		if err != nil {
			return report, err
		}
		if !ok {
			report.Failures = append(report.Failures, Failure{
				Image: imageTruth.ID, Field: "run.presence", Detail: "run is missing",
			})
			continue
		}
		report.Images++
		if meta.TruthFingerprint != truthFP {
			report.Failures = append(report.Failures, Failure{
				Image: imageTruth.ID, Field: "truth_fingerprint",
				Detail: fmt.Sprintf("%s != %s", meta.TruthFingerprint, truthFP),
			})
			continue
		}
		if meta.AlgorithmVersion != truth.AlgorithmVersion {
			report.Failures = append(report.Failures, Failure{
				Image: imageTruth.ID, Field: "algorithm_version",
				Detail: "stored algorithm version differs from truth",
			})
			continue
		}
		if meta.ProjectFingerprint != truth.ProjectFingerprint {
			report.Failures = append(report.Failures, Failure{
				Image: imageTruth.ID, Field: "project_fingerprint",
				Detail: "stored project fingerprint differs from truth",
			})
			continue
		}
		if meta.StepCount != imageTruth.StepCount {
			report.Failures = append(report.Failures, Failure{
				Image: imageTruth.ID, Field: "step_count",
				Detail: fmt.Sprintf("run has %d steps, truth has %d", meta.StepCount, imageTruth.StepCount),
			})
			continue
		}
		artifacts, err := store.LoadArtifacts(ctx, runID, imageTruth.ID)
		if err != nil {
			return report, err
		}
		frames, byStep, err := rebuildFrames(meta, artifacts)
		if err != nil {
			report.Failures = append(report.Failures, Failure{
				Image: imageTruth.ID, Field: "artifacts", Detail: err.Error(),
			})
			continue
		}
		if len(frames) != imageTruth.StepCount+1 {
			report.Failures = append(report.Failures, Failure{
				Image: imageTruth.ID, Field: "step_count",
				Detail: fmt.Sprintf("persisted %d steps, truth declares %d", len(frames)-1, imageTruth.StepCount),
			})
			continue
		}
		blocksByCoord := make(map[string]model.BlockTruth, len(imageTruth.Blocks))
		for _, bt := range imageTruth.Blocks {
			blocksByCoord[fmt.Sprintf("%d,%d", bt.X, bt.Y)] = bt
		}
		for _, a := range byStep[len(byStep)-1] {
			bt, ok := blocksByCoord[fmt.Sprintf("%d,%d", a.Block.X, a.Block.Y)]
			if !ok {
				report.Failures = append(report.Failures, Failure{
					Image: imageTruth.ID, Step: len(frames) - 1,
					Block: blockRef(a.Block), Field: "block_truth.presence",
					Detail: "no truth entry for block",
				})
				continue
			}
			report.Blocks++
			eval := quality.EvaluateBlock(frames[0], frames[1:], a.Block, truth.AttributionShare, model.Chain{
				Steps: imageTruth.Steps,
			})
			if quality.MetricsWithin(eval.Metrics, bt.Metrics) {
				report.MetricsPassed++
			} else {
				field, detail := metricMismatch(eval.Metrics, bt.Metrics)
				report.Failures = append(report.Failures, Failure{
					Image: imageTruth.ID, Step: len(frames) - 1, Block: blockRef(a.Block),
					Field: field, Detail: detail,
				})
			}
			if quality.AttributionMatches(eval.Attribution, bt.Attribution) {
				report.AttributionPassed++
			} else {
				report.Failures = append(report.Failures, Failure{
					Image: imageTruth.ID, Step: eval.Attribution.Step, Block: blockRef(a.Block),
					Field: "attribution.step",
					Detail: fmt.Sprintf("got step=%d category=%s share=%.6f, want step=%d category=%s share=%.6f",
						eval.Attribution.Step, eval.Attribution.Category, eval.Attribution.Share,
						bt.Attribution.Step, bt.Attribution.Category, bt.Attribution.Share),
				})
			}
		}
		final := frames[len(frames)-1]
		for i, sample := range imageTruth.Samples {
			x := int(math.Floor(sample.X))
			y := int(math.Floor(sample.Y))
			got := final.At(x, y)
			ok := true
			for c := 0; c < 3; c++ {
				if absInt(int(got[c])-int(sample.Expected[c])) > sample.Tolerance {
					ok = false
				}
			}
			if ok {
				report.SamplesPassed++
			} else {
				report.Failures = append(report.Failures, Failure{
					Image: imageTruth.ID, Step: len(frames) - 1,
					Block: &core.BlockRef{Index: (y/meta.BlockSize)*(meta.Width/meta.BlockSize) + x/meta.BlockSize,
						X: x / meta.BlockSize * meta.BlockSize, Y: y / meta.BlockSize * meta.BlockSize,
						W: meta.BlockSize, H: meta.BlockSize},
					Field:  fmt.Sprintf("samples[%d].pixel", i),
					Detail: fmt.Sprintf("got %v want %v tolerance %d", got, sample.Expected, sample.Tolerance),
				})
			}
		}
	}
	report.Verified = len(report.Failures) == 0
	if !report.Verified {
		f := report.Failures[0]
		e := core.DiffError(f.Step, f.Block, f.Field, f.Detail)
		e.Details = map[string]any{"image": f.Image, "failures": len(report.Failures)}
		return report, e
	}
	return report, nil
}

func rebuildFrames(meta checkpoint.RunMeta, artifacts []checkpoint.Artifact) ([]*core.Frame, map[int][]checkpoint.Artifact, error) {
	byStep := make(map[int][]checkpoint.Artifact)
	for _, a := range artifacts {
		if a.State != core.StateCommitted {
			return nil, nil, fmt.Errorf("artifact step %d block %d is not committed", a.Step, a.Block.X)
		}
		if a.AlgorithmVersion != meta.AlgorithmVersion {
			return nil, nil, fmt.Errorf("artifact algorithm version mismatch")
		}
		if a.OutputHash != checkpoint.PixelHash(a.Pixels) {
			return nil, nil, fmt.Errorf("artifact content hash mismatch at step %d", a.Step)
		}
		if err := a.Validate(); err != nil {
			return nil, nil, err
		}
		byStep[a.Step] = append(byStep[a.Step], a)
	}
	steps := make([]int, 0, len(byStep))
	for s := range byStep {
		steps = append(steps, s)
	}
	sort.Ints(steps)
	for s := 0; s <= meta.StepCount; s++ {
		if len(byStep[s]) != (meta.Width/meta.BlockSize)*(meta.Height/meta.BlockSize) {
			return nil, nil, fmt.Errorf("step %d is incomplete", s)
		}
	}
	frames := make([]*core.Frame, meta.StepCount+1)
	for s := 0; s <= meta.StepCount; s++ {
		frame, err := core.NewFrame(meta.Width, meta.Height, meta.Channels)
		if err != nil {
			return nil, nil, err
		}
		for _, a := range byStep[s] {
			if a.Block.Empty() || !a.Block.InFrame(meta.Width, meta.Height) {
				return nil, nil, fmt.Errorf("invalid block geometry at step %d", s)
			}
			b, err := core.NewFrame(a.Block.W, a.Block.H, meta.Channels)
			if err != nil {
				return nil, nil, err
			}
			if len(a.Pixels) != len(b.Pix) {
				return nil, nil, fmt.Errorf("block byte size mismatch at step %d", s)
			}
			copy(b.Pix, a.Pixels)
			if err := frame.SetBlock(a.Block, b); err != nil {
				return nil, nil, err
			}
		}
		frames[s] = frame
	}
	return frames, byStep, nil
}

func metricMismatch(got quality.Metrics, want model.MetricInterval) (string, string) {
	if !model.IntervalContains(want.NoiseVariance, got.NoiseVariance) {
		return "metrics.noise_variance", fmt.Sprintf("got %.9f want [%.9f,%.9f]",
			got.NoiseVariance, want.NoiseVariance[0], want.NoiseVariance[1])
	}
	if !model.IntervalContains(want.EdgeSharpness, got.EdgeSharpness) {
		return "metrics.edge_sharpness", fmt.Sprintf("got %.9f want [%.9f,%.9f]",
			got.EdgeSharpness, want.EdgeSharpness[0], want.EdgeSharpness[1])
	}
	return "metrics.chroma_shift", fmt.Sprintf("got %.9f want [%.9f,%.9f]",
		got.ChromaShift, want.ChromaShift[0], want.ChromaShift[1])
}

func blockRef(r core.Rect) *core.BlockRef {
	return &core.BlockRef{X: r.X, Y: r.Y, W: r.W, H: r.H}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
