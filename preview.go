package iconmask

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"time"
)

type ParameterSnapshot struct {
	Version string       `json:"version"`
	Config  RepairConfig `json:"config"`
}

func NewParameterSnapshot(cfg RepairConfig) ParameterSnapshot {
	data, _ := json.Marshal(cfg)
	sum := sha256.Sum256(append([]byte(AlgorithmVersion+"\x00"), data...))
	return ParameterSnapshot{
		Version: hex.EncodeToString(sum[:12]),
		Config:  cfg,
	}
}

func validateParameterSnapshot(imageID string, snapshot ParameterSnapshot) *Error {
	if snapshot.Version == "" {
		return &Error{
			Code:    CodeState,
			ImageID: imageID,
			Field:   "parameterVersion",
			Message: "parameter snapshot version is required",
		}
	}
	if expected := NewParameterSnapshot(snapshot.Config); expected.Version != snapshot.Version {
		return &Error{
			Code:    CodeState,
			ImageID: imageID,
			Field:   "parameterVersion",
			Message: "parameter snapshot version does not match its configuration",
			Details: map[string]any{
				"version":  snapshot.Version,
				"expected": expected.Version,
			},
		}
	}
	return nil
}

type PreviewConfig struct {
	Downsample            int           `json:"downsample"`
	Rounds                int           `json:"rounds"`
	LatencyBudget         time.Duration `json:"latencyBudget"`
	CentroidTolerance     float64       `json:"centroidTolerance"`
	AreaRelativeTolerance float64       `json:"areaRelativeTolerance"`
}

func DefaultPreviewConfig() PreviewConfig {
	return PreviewConfig{
		Downsample:            2,
		Rounds:                2,
		LatencyBudget:         800 * time.Millisecond,
		CentroidTolerance:     3.0,
		AreaRelativeTolerance: 0.22,
	}
}

type PreviewResult struct {
	Mask             Mask          `json:"mask"`
	Stats            RegionStats   `json:"stats"`
	StatsPlaceholder bool          `json:"statsPlaceholder"`
	ActualDownsample int           `json:"actualDownsample"`
	RoundBudget      int           `json:"roundBudget"`
	RoundsUsed       int           `json:"roundsUsed"`
	Elapsed          time.Duration `json:"elapsedNanos"`
	LatencyBudget    time.Duration `json:"latencyBudgetNanos"`
	WithinLatency    bool          `json:"withinLatency"`
	ParameterVersion string        `json:"parameterVersion"`
	BudgetExhausted  bool          `json:"budgetExhausted"`
	Rationale        string        `json:"rationale"`
	RadiusTable      *RadiusTable  `json:"radiusTable,omitempty"`
}

type RegionDivergence struct {
	Index             int     `json:"index"`
	CentroidDelta     float64 `json:"centroidDelta"`
	AreaRelativeError float64 `json:"areaRelativeError"`
}

type PathComparison struct {
	PreviewRegionCount int                `json:"previewRegionCount"`
	ExactRegionCount   int                `json:"exactRegionCount"`
	MaxCentroidDelta   float64            `json:"maxCentroidDelta"`
	MaxAreaRelativeErr float64            `json:"maxAreaRelativeError"`
	Regions            []RegionDivergence `json:"regions"`
	FirstDiff          *PointInt          `json:"firstDiff,omitempty"`
	Passed             bool               `json:"passed"`
	ParameterVersion   string             `json:"parameterVersion"`
}

type DualPathResult struct {
	ParameterVersion string         `json:"parameterVersion"`
	Preview          *PreviewResult `json:"preview"`
	Exact            *RepairResult  `json:"exact"`
	Comparison       PathComparison `json:"comparison"`
}

func RunPreview(sample ImageSample, snapshot ParameterSnapshot, previewConfig PreviewConfig) (*PreviewResult, *Error) {
	if err := validateParameterSnapshot(sample.ID, snapshot); err != nil {
		return nil, err
	}
	if previewConfig.LatencyBudget == 0 {
		previewConfig.LatencyBudget = DefaultPreviewConfig().LatencyBudget
	}
	if previewConfig.LatencyBudget < 0 {
		return nil, parameterError(sample.ID, "latencyBudget", "preview latency budget must not be negative", "non-negative duration")
	}
	if previewConfig.Downsample <= 0 {
		return nil, parameterError(sample.ID, "downsample", "downsample factor must be a positive integer", "positive integer")
	}
	if previewConfig.Rounds <= 0 {
		return nil, parameterError(sample.ID, "previewRounds", "preview rounds must be a positive integer", "positive integer")
	}
	if previewConfig.Downsample >= minInt(sample.Width, sample.Height) {
		return nil, parameterError(sample.ID, "downsample", "downsample factor must be smaller than the image", "1 <= downsample < min(width,height)")
	}
	start := time.Now()
	down := previewConfig.Downsample
	smallInitial := downsampleMask(sample.InitialMask, down)
	smallReference := downsampleMask(sample.Truth.ReferenceMask, down)
	scaledSegments := scaleSegments(sample.Truth.Segments, down)
	scaledConfig := snapshot.Config
	scaledConfig.Radius.Min = maxInt(1, scaledConfig.Radius.Min/down)
	scaledConfig.Radius.Max = maxInt(scaledConfig.Radius.Min, scaledConfig.Radius.Max/down)
	scaledConfig.MaxRounds = previewConfig.Rounds
	scaledConfig.BlockSize = maxInt(1, scaledConfig.BlockSize/down)
	previewSmall, previewErr := Refine(sample.ID, smallInitial, smallReference, scaledSegments, scaledConfig)
	budgetExhausted := false
	if previewErr != nil {
		if IsCode(previewErr, CodeIteration) && previewSmall != nil {
			budgetExhausted = true
		} else {
			return nil, previewErr
		}
	}
	upsampled := upsampleMask(previewSmall.Mask, sample.Width, sample.Height)
	elapsed := time.Since(start)
	result := &PreviewResult{
		Mask:             upsampled,
		Stats:            AnalyzeMask(upsampled),
		StatsPlaceholder: true,
		ActualDownsample: down,
		RoundBudget:      previewConfig.Rounds,
		RoundsUsed:       len(previewSmall.Rounds),
		Elapsed:          elapsed,
		LatencyBudget:    previewConfig.LatencyBudget,
		WithinLatency:    elapsed <= previewConfig.LatencyBudget,
		ParameterVersion: snapshot.Version,
		BudgetExhausted:  budgetExhausted,
		RadiusTable:      previewSmall.RadiusTable,
		Rationale:        "nearest-neighbor downsample, scaled radius band, reduced round budget, shared parameter snapshot",
	}
	return result, nil
}

func ComparePaths(
	sample ImageSample,
	snapshot ParameterSnapshot,
	preview *PreviewResult,
	exact *RepairResult,
	previewConfig PreviewConfig,
) PathComparison {
	comparison := PathComparison{
		PreviewRegionCount: preview.Stats.Count,
		ExactRegionCount:   exact.Stats.Count,
		ParameterVersion:   snapshot.Version,
	}
	comparison.FirstDiff = preview.Mask.FirstDiff(exact.Mask)
	count := minInt(len(preview.Stats.Regions), len(exact.Stats.Regions))
	for i := 0; i < count; i++ {
		p := preview.Stats.Regions[i]
		e := exact.Stats.Regions[i]
		delta := math.Hypot(p.Centroid.X-e.Centroid.X, p.Centroid.Y-e.Centroid.Y)
		denominator := float64(maxInt(1, e.Area))
		areaError := math.Abs(float64(p.Area)-float64(e.Area)) / denominator
		comparison.Regions = append(comparison.Regions, RegionDivergence{
			Index:             i,
			CentroidDelta:     delta,
			AreaRelativeError: areaError,
		})
		if delta > comparison.MaxCentroidDelta {
			comparison.MaxCentroidDelta = delta
		}
		if areaError > comparison.MaxAreaRelativeErr {
			comparison.MaxAreaRelativeErr = areaError
		}
	}
	comparison.Passed = comparison.PreviewRegionCount == comparison.ExactRegionCount &&
		comparison.MaxCentroidDelta <= previewConfig.CentroidTolerance &&
		comparison.MaxAreaRelativeErr <= previewConfig.AreaRelativeTolerance
	return comparison
}

func RunDualPath(sample ImageSample, snapshot ParameterSnapshot, previewConfig PreviewConfig) (*DualPathResult, *Error) {
	if err := validateParameterSnapshot(sample.ID, snapshot); err != nil {
		return nil, err
	}
	exact, err := RefineSample(sample, snapshot.Config)
	if err != nil {
		return nil, err
	}
	preview, err := RunPreview(sample, snapshot, previewConfig)
	if err != nil {
		return nil, err
	}
	if preview.ParameterVersion != snapshot.Version || exact.ImageID != sample.ID {
		return nil, &Error{
			Code:    CodeState,
			ImageID: sample.ID,
			Field:   "parameterVersion",
			Message: "preview and exact paths did not use the same parameter snapshot",
		}
	}
	comparison := ComparePaths(sample, snapshot, preview, exact, previewConfig)
	result := &DualPathResult{
		ParameterVersion: snapshot.Version,
		Preview:          preview,
		Exact:            exact,
		Comparison:       comparison,
	}
	if !comparison.Passed {
		return result, &Error{
			Code:        CodeDivergence,
			ImageID:     sample.ID,
			Field:       "previewVsExact",
			Message:     "preview and exact paths exceed declared region/centroid/area tolerances",
			FirstDiffXY: comparison.FirstDiff,
			Details: map[string]any{
				"previewRegionCount":    comparison.PreviewRegionCount,
				"exactRegionCount":      comparison.ExactRegionCount,
				"maxCentroidDelta":      comparison.MaxCentroidDelta,
				"maxAreaRelativeError":  comparison.MaxAreaRelativeErr,
				"centroidTolerance":     previewConfig.CentroidTolerance,
				"areaRelativeTolerance": previewConfig.AreaRelativeTolerance,
				"parameterVersion":      snapshot.Version,
			},
		}
	}
	return result, nil
}

func downsampleMask(mask Mask, factor int) Mask {
	width := maxInt(1, mask.Width/factor)
	height := maxInt(1, mask.Height/factor)
	out := NewMask(width, height)
	cell := factor * factor
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			foreground := 0
			for dy := 0; dy < factor; dy++ {
				for dx := 0; dx < factor; dx++ {
					if mask.At(x*factor+dx, y*factor+dy) != 0 {
						foreground++
					}
				}
			}
			if foreground*2 >= cell {
				out.Set(x, y, 1)
			}
		}
	}
	return out
}

func upsampleMask(mask Mask, width, height int) Mask {
	out := NewMask(width, height)
	for y := 0; y < height; y++ {
		sy := minInt(mask.Height-1, y*mask.Height/height)
		for x := 0; x < width; x++ {
			sx := minInt(mask.Width-1, x*mask.Width/width)
			out.Set(x, y, mask.At(sx, sy))
		}
	}
	return out
}

func scaleSegments(segments []BoundarySegment, factor int) []BoundarySegment {
	scaled := make([]BoundarySegment, len(segments))
	for i, segment := range segments {
		scaled[i] = segment
		scaled[i].Curvature = segment.Curvature * float64(factor)
		scaled[i].Points = make([]Point, len(segment.Points))
		for j, point := range segment.Points {
			scaled[i].Points[j] = Point{X: point.X / float64(factor), Y: point.Y / float64(factor)}
		}
	}
	return scaled
}
