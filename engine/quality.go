package engine

import (
	"fmt"
)

const (
	FineTolerance          int64 = 64
	FineSubsamples               = 4
	FinePrecisionBits            = 12
	ApproxAreaTolerance          = 0.03
	ApproxBBoxIoUTolerance       = 0.95
)

type QualityDecision struct {
	Mode          string   `json:"mode"`
	Tolerance     int64    `json:"tolerance"`
	Subsamples    int      `json:"subsamples"`
	PrecisionBits int      `json:"precisionBits"`
	Downsample    int      `json:"downsample"`
	MaxWorkers    int      `json:"maxWorkers"`
	BufferBytes   int64    `json:"bufferBytesPerWorker"`
	EstimatedCost int64    `json:"estimatedCost"`
	BudgetBytes   int64    `json:"budgetBytes"`
	Reasons       []string `json:"reasons"`
}

type SceneStats struct {
	PathCount     int
	KeyframeCount int
	SampleCount   int
	EstPixels     int64
}

func StatsOf(scene *Scene) SceneStats {
	stats := SceneStats{SampleCount: len(scene.Samples), PathCount: len(scene.Paths)}
	for i := range scene.Paths {
		stats.KeyframeCount += len(scene.Paths[i].Keyframes)
	}
	b := scene.Bounds()
	w := PixelLength(b.Width(), max64(scene.Scale, CoordScale))
	h := PixelLength(b.Height(), max64(scene.Scale, CoordScale))
	w = max64(w, 1)
	h = max64(h, 1)
	stats.EstPixels = w * h
	return stats
}

// SelectQuality chooses fine or approximate settings from deterministic cost estimates.
func SelectQuality(scene *Scene, budgetBytes int64, maxWorkers int) (QualityDecision, error) {
	if scene == nil {
		return QualityDecision{}, &Error{Code: CodeSchema, Path: "$", Message: "nil scene"}
	}
	if err := ValidateParameters(scene); err != nil {
		return QualityDecision{}, err
	}
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	if maxWorkers > 8 {
		maxWorkers = 8
	}
	stats := StatsOf(scene)
	grid := scene.Subsamples
	frameBuffer := stats.EstPixels*4*int64(stats.PathCount+1) + int64(grid*grid*4)
	estimate := frameBuffer*int64(max64(int64(stats.SampleCount), 1)) +
		int64(stats.KeyframeCount)*256 +
		int64(stats.PathCount)*512
	if budgetBytes <= 0 {
		budgetBytes = 128 * 1024 * 1024
	}
	fine := QualityDecision{
		Mode:          "fine",
		Tolerance:     scene.Tolerance,
		Subsamples:    scene.Subsamples,
		PrecisionBits: scene.Interpolation.PrecisionBits,
		Downsample:    1,
		MaxWorkers:    maxWorkers,
		BufferBytes:   frameBuffer,
		EstimatedCost: estimate,
		BudgetBytes:   budgetBytes,
		Reasons:       []string{fmt.Sprintf("estimated %d bytes <= budget %d bytes", estimate, budgetBytes)},
	}
	if estimate <= budgetBytes {
		return fine, nil
	}
	downsample := 2
	if estimate > budgetBytes*2 {
		downsample = 4
	}
	grid = maxInt(1, scene.Subsamples/downsample)
	if downsample > 1 && grid < 2 && scene.Subsamples >= 2 {
		grid = 2
	}
	tol := scene.Tolerance * int64(downsample)
	if tol < 1 {
		tol = 1
	}
	bits := scene.Interpolation.PrecisionBits
	if downsample >= 2 {
		bits = maxInt(4, bits-2)
	}
	if downsample >= 4 {
		bits = maxInt(4, bits-2)
	}
	approxFrameBuffer := stats.EstPixels*4*int64(stats.PathCount+1) + int64(grid*grid*4)
	approx := QualityDecision{
		Mode:          "approx",
		Tolerance:     tol,
		Subsamples:    grid,
		PrecisionBits: bits,
		Downsample:    downsample,
		MaxWorkers:    maxWorkers,
		BufferBytes:   approxFrameBuffer,
		EstimatedCost: estimate,
		BudgetBytes:   budgetBytes,
		Reasons: []string{
			fmt.Sprintf("estimated %d bytes > budget %d bytes", estimate, budgetBytes),
			fmt.Sprintf("downsample=%dx, tolerance=%d, grid=%d, interpBits=%d", downsample, tol, grid, bits),
			"bitmap dimensions remain derived from bounds and scale; reduced grid affects AA sample density",
		},
	}
	return approx, nil
}

type ScaleMetric struct {
	Frame             int     `json:"frame"`
	AreaRelativeError float64 `json:"areaRelativeError"`
	BBoxIoU           float64 `json:"bboxIoU"`
}

// ApplyQuality returns a scene whose numerical parameters match the decision.
func ApplyQuality(scene *Scene, q QualityDecision) *Scene {
	out := scene.Clone()
	if q.Tolerance > 0 {
		out.Tolerance = q.Tolerance
	}
	if q.Subsamples > 0 {
		out.Subsamples = q.Subsamples
	}
	if q.PrecisionBits > 0 {
		out.Interpolation.PrecisionBits = q.PrecisionBits
	}
	return out
}

func CompareQualityMetrics(fine, approx *RenderedFrame) ScaleMetric {
	m := ScaleMetric{Frame: fine.Index}
	var fa, aa float64
	for _, p := range fine.Paths {
		fa += p.CoverageRatio
	}
	for _, p := range approx.Paths {
		aa += p.CoverageRatio
	}
	den := maxFloat(fa, aa)
	if den > 1e-12 {
		m.AreaRelativeError = absFloat(fa-aa) / den
	}
	m.BBoxIoU = rectIoU(fine.GeometryBounds, approx.GeometryBounds)
	return m
}

func CheckScaleMetric(m ScaleMetric) error {
	if m.AreaRelativeError > ApproxAreaTolerance || m.BBoxIoU < ApproxBBoxIoUTolerance {
		return &Error{
			Code:    CodeScale,
			Path:    "quality",
			Frame:   m.Frame + 1,
			Metric:  "areaRelativeError/bboxIoU",
			Actual:  m.AreaRelativeError,
			Limit:   ApproxAreaTolerance,
			Message: fmt.Sprintf("areaRelativeError=%.6f bboxIoU=%.6f (bbox limit %.4f)", m.AreaRelativeError, m.BBoxIoU, ApproxBBoxIoUTolerance),
		}
	}
	return nil
}

func rectIoU(a, b Rect) float64 {
	if a.Empty() || b.Empty() {
		return 0
	}
	ix0, iy0 := max64(a.MinX, b.MinX), max64(a.MinY, b.MinY)
	ix1, iy1 := min64(a.MaxX, b.MaxX), min64(a.MaxY, b.MaxY)
	if ix1 <= ix0 || iy1 <= iy0 {
		return 0
	}
	inter := float64((ix1 - ix0) * (iy1 - iy0))
	ua := float64(a.Width() * a.Height())
	ub := float64(b.Width() * b.Height())
	return inter / (ua + ub - inter)
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
