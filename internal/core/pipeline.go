package core

import (
	"math"
	"time"
)

type StageResult struct {
	Stage       string  `json:"stage"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	Channels    int     `json:"channels"`
	Coverage    float64 `json:"coverage"`
	Fingerprint string  `json:"fingerprint"`
	DurationMS  float64 `json:"durationMs"`
	Pix         []byte  `json:"-"`
}

func (s StageResult) Image() Image {
	return Image{Width: s.Width, Height: s.Height, Channels: s.Channels, Pix: s.Pix}
}

func ComputeStages(src Image, params Params) ([]StageResult, error) {
	if err := src.Validate(); err != nil {
		return nil, err
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	stages := make([]StageResult, 0, len(StageOrder))
	current := src.Clone()

	start := time.Now()
	magnitude, err := GradientMagnitude(current, params.Operator)
	if err != nil {
		return nil, err
	}
	gradient, err := DualThresholdHysteresis(magnitude, params.LowThreshold, params.HighThreshold)
	if err != nil {
		return nil, err
	}
	stages = append(stages, makeStageResult(StageGradient, gradient, time.Since(start)))
	current = gradient

	type morphStage struct {
		name string
		fn   func(Image, int) (Image, error)
		r    int
	}
	for _, st := range []morphStage{
		{name: StageOpen, fn: MorphOpen, r: params.OpenRadius},
		{name: StageClose, fn: MorphClose, r: params.CloseRadius},
	} {
		start = time.Now()
		next, err := st.fn(current, st.r)
		if err != nil {
			return nil, err
		}
		if err := assertSameShape(current, next, st.name); err != nil {
			return nil, err
		}
		stages = append(stages, makeStageResult(st.name, next, time.Since(start)))
		current = next
	}

	start = time.Now()
	thinned, err := Thin(current, params.ThinIterations)
	if err != nil {
		return nil, err
	}
	if err := assertSameShape(current, thinned, StageThin); err != nil {
		return nil, err
	}
	stages = append(stages, makeStageResult(StageThin, thinned, time.Since(start)))
	current = thinned

	start = time.Now()
	pruned, err := PruneShortComponents(current, params.PruneLength)
	if err != nil {
		return nil, err
	}
	if err := assertSameShape(current, pruned, StagePrune); err != nil {
		return nil, err
	}
	stages = append(stages, makeStageResult(StagePrune, pruned, time.Since(start)))
	return stages, nil
}

func makeStageResult(stage string, im Image, elapsed time.Duration) StageResult {
	return StageResult{
		Stage:       stage,
		Width:       im.Width,
		Height:      im.Height,
		Channels:    im.Channels,
		Coverage:    coverage(im.Pix, im.PixelCount()),
		Fingerprint: FingerprintImage(im),
		DurationMS:  float64(elapsed.Microseconds()) / 1000.0,
		Pix:         im.Pix,
	}
}

func assertSameShape(a, b Image, stage string) error {
	if a.Width != b.Width || a.Height != b.Height || a.Channels != b.Channels {
		return &AppError{
			Code:    CodeState,
			Field:   "stageShape",
			Message: stage + " changed the image dimensions or channel count",
			Details: map[string]any{
				"input":  map[string]int{"width": a.Width, "height": a.Height, "channels": a.Channels},
				"output": map[string]int{"width": b.Width, "height": b.Height, "channels": b.Channels},
			},
		}
	}
	return nil
}

type PreviewPolicy struct {
	DownsampleFactor  int  `json:"downsampleFactor"`
	ScaledStructure   bool `json:"scaledStructure"`
	ScaledThinning    bool `json:"scaledThinning"`
	ScaledPruneLength bool `json:"scaledPruneLength"`
	Simplified        bool `json:"simplified"`
}

func ChoosePreviewPolicy(width, height int, params Params) PreviewPolicy {
	targetPixels := 420000
	factor := 1
	for (width/factor)*(height/factor) > targetPixels {
		factor++
	}
	policy := PreviewPolicy{DownsampleFactor: factor}
	effective := ApplyPreviewPolicy(params, policy)
	policy.ScaledStructure = effective.OpenRadius != params.OpenRadius || effective.CloseRadius != params.CloseRadius
	policy.ScaledThinning = effective.ThinIterations != params.ThinIterations
	policy.ScaledPruneLength = effective.PruneLength != params.PruneLength
	policy.Simplified = factor > 1 || policy.ScaledStructure || policy.ScaledThinning || policy.ScaledPruneLength
	return policy
}

func ApplyPreviewPolicy(params Params, policy PreviewPolicy) Params {
	out := params
	factor := max(1, policy.DownsampleFactor)
	// Area-averaging thickens thin binary edges at lower resolution. Scaling the
	// square structure element by the sampling factor keeps the coarse preview
	// from retaining edge fragments that the full-resolution opening erases.
	if out.OpenRadius > 0 {
		out.OpenRadius = min(16, out.OpenRadius*factor)
	}
	if out.CloseRadius > 0 {
		out.CloseRadius = min(16, out.CloseRadius*factor)
	}
	if out.PruneLength > 0 {
		area := factor * factor
		out.PruneLength = max(1, (out.PruneLength+area-1)/area)
	}
	return out
}

type PreviewOutcome struct {
	Mask         Image
	DisplayMask  Image
	Policy       PreviewPolicy
	Params       Params
	ParamVersion string
	Stages       []StageResult
	Stats        Geometry
	ElapsedMS    float64
	TotalPixels  int
}

func ComputePreview(src Image, params Params, truth Truth) (PreviewOutcome, error) {
	if err := params.Validate(); err != nil {
		return PreviewOutcome{}, err
	}
	start := time.Now()
	policy := ChoosePreviewPolicy(src.Width, src.Height, params)
	small := Downsample(src, policy.DownsampleFactor)
	previewParams := ApplyPreviewPolicy(params, policy)
	stages, err := ComputeStages(small, previewParams)
	if err != nil {
		return PreviewOutcome{}, err
	}
	display := stages[len(stages)-1].Image()
	full := UpsampleNearest(display, src.Width, src.Height)
	stats := AnalyzeGeometry(full, truth)
	return PreviewOutcome{
		Mask:         full,
		DisplayMask:  display,
		Policy:       policy,
		Params:       params,
		ParamVersion: params.Version(),
		Stages:       stages,
		Stats:        stats,
		ElapsedMS:    float64(time.Since(start).Microseconds()) / 1000.0,
		TotalPixels:  src.Width * src.Height * src.Channels,
	}, nil
}

type Tolerance struct {
	CoverageDeviation float64 `json:"coverageDeviation"`
	GeometryDeviation float64 `json:"geometryDeviation"`
	PixelFraction     float64 `json:"pixelFraction"`
}

func DefaultTolerance() Tolerance {
	return Tolerance{
		CoverageDeviation: 0.05,
		GeometryDeviation: 12.0,
		PixelFraction:     0.20,
	}
}

type PixelCoord struct {
	X            int  `json:"x"`
	Y            int  `json:"y"`
	Channel      int  `json:"channel"`
	PreviewValue byte `json:"previewValue"`
	ExactValue   byte `json:"exactValue"`
}

type Divergence struct {
	CoverageDeviation float64     `json:"coverageDeviation"`
	GeometryDeviation float64     `json:"geometryDeviation"`
	PixelDiffFraction float64     `json:"pixelDiffFraction"`
	Tolerance         Tolerance   `json:"tolerance"`
	Pass              bool        `json:"pass"`
	FirstMismatch     *PixelCoord `json:"firstMismatch,omitempty"`
}

func ComparePreviewExact(preview, exact Image, previewStats, exactStats Geometry, tol Tolerance) Divergence {
	d := Divergence{Tolerance: tol}
	d.CoverageDeviation = math.Abs(previewStats.Coverage - exactStats.Coverage)
	d.GeometryDeviation = GeometryDeviation(previewStats, exactStats)
	if preview.Width == exact.Width && preview.Height == exact.Height && preview.Channels == exact.Channels {
		mismatch := 0
		total := preview.PixelCount()
		for i := 0; i < total; i++ {
			if preview.Pix[i] != exact.Pix[i] {
				mismatch++
				if d.FirstMismatch == nil {
					pixel := i / preview.Channels
					c := i % preview.Channels
					coord := PixelCoord{
						X:            pixel % preview.Width,
						Y:            pixel / preview.Width,
						Channel:      c,
						PreviewValue: preview.Pix[i],
						ExactValue:   exact.Pix[i],
					}
					d.FirstMismatch = &coord
				}
			}
		}
		d.PixelDiffFraction = float64(mismatch) / float64(total)
	} else {
		d.PixelDiffFraction = 1
	}
	d.Pass = d.CoverageDeviation <= tol.CoverageDeviation &&
		d.GeometryDeviation <= tol.GeometryDeviation &&
		d.PixelDiffFraction <= tol.PixelFraction
	return d
}
