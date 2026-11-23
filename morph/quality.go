package morph

import (
	"fmt"
	"math"
	"strings"
)

type QualityPathMetric struct {
	PathID              string    `json:"path_id"`
	AreaFine            float64   `json:"area_fine"`
	AreaApprox          float64   `json:"area_approx"`
	RelativeAreaError   float64   `json:"relative_area_error"`
	BBoxIoU             float64   `json:"bbox_iou"`
	MeanCoverageDiff    float64   `json:"mean_coverage_diff"`
	SegmentCountFine    int       `json:"segment_count_fine"`
	SegmentCountApprox  int       `json:"segment_count_approx"`
	InterpolationFine   int       `json:"interpolation_count_fine"`
	InterpolationApprox int       `json:"interpolation_count_approx"`
	FineCoverageBBox    PixelRect `json:"fine_coverage_bbox"`
	ApproxCoverageBBox  PixelRect `json:"approx_coverage_bbox"`
}

type PixelErrorStats struct {
	MAE     float64 `json:"mae"`
	RMSE    float64 `json:"rmse"`
	MaxAbs  float64 `json:"max_abs"`
	MeanCov float64 `json:"mean_coverage_difference"`
	Changed int     `json:"changed_pixels"`
}

type QualitySampleReport struct {
	Index          int                 `json:"index"`
	Time           float64             `json:"time"`
	Midpoint       bool                `json:"midpoint_between_keyframes"`
	Width          int                 `json:"width"`
	Height         int                 `json:"height"`
	Paths          []QualityPathMetric `json:"paths"`
	PixelError     PixelErrorStats     `json:"pixel_error"`
	DimensionMatch bool                `json:"dimension_match"`
}

type QualityReport struct {
	Pass                 bool                  `json:"pass"`
	Tolerance            ConsistencyTolerance  `json:"tolerance"`
	FineMode             Quality               `json:"fine_mode"`
	ApproxMode           Quality               `json:"approx_mode"`
	FineSegments         int                   `json:"fine_segments"`
	ApproxSegments       int                   `json:"approx_segments"`
	FineInterpolations   int                   `json:"fine_interpolations"`
	ApproxInterpolations int                   `json:"approx_interpolations"`
	Samples              []QualitySampleReport `json:"samples"`
	MidpointCovered      bool                  `json:"midpoint_sample_covered"`
	FirstFailure         *QualityFailure       `json:"first_failure,omitempty"`
}

type QualityFailure struct {
	Sample   int     `json:"sample"`
	PathID   string  `json:"path_id"`
	Metric   string  `json:"metric"`
	Measured float64 `json:"measured_approx"`
	Expected float64 `json:"expected_fine"`
	Limit    float64 `json:"limit"`
}

func CompareQuality(scene *Scene, truth *Truth, tol ConsistencyTolerance, fine, approx []*SampleResult) (*QualityReport, error) {
	report := &QualityReport{Pass: true, Tolerance: tol}
	if len(fine) == 0 || len(approx) == 0 {
		return report, withField(newError(CodeScale, "both fine and approximate results are required"), "quality")
	}
	if len(fine) != len(approx) {
		return report, withField(newError(CodeScale, "fine and approximate sample counts differ"), "quality.samples")
	}
	report.FineMode = fine[0].Quality
	report.ApproxMode = approx[0].Quality
	for i := range fine {
		f, a := fine[i], approx[i]
		if f == nil || a == nil {
			return report, withSample(withField(newError(CodeScale, "missing frame in quality comparison"), "quality.frame"), i, 0)
		}
		sr := QualitySampleReport{Index: i, Time: f.Time, Width: f.Width, Height: f.Height, DimensionMatch: f.Width == a.Width && f.Height == a.Height}
		if truth != nil && i < len(truth.Samples) {
			sr.Midpoint = truth.Samples[i].MidpointOfKeyframes
		}
		if sr.Midpoint {
			report.MidpointCovered = true
		}
		if !sr.DimensionMatch {
			fail := &QualityFailure{Sample: i, Metric: "frame_dimensions"}
			report.FirstFailure = fail
			report.Pass = false
			e := scaleError(fail, "frame dimensions differ")
			report.Samples = append(report.Samples, sr)
			return report, e
		}
		if f.Width*f.Height != len(f.Gray) || a.Width*a.Height != len(a.Gray) {
			return report, withSample(withField(newError(CodeScale, "frame byte length mismatch"), "quality.frame"), i, f.Time)
		}
		pix := pixelError(f.Gray, a.Gray)
		sr.PixelError = pix
		report.FineSegments += sumSegments(f)
		report.ApproxSegments += sumSegments(a)
		report.FineInterpolations += sumInterpolations(f)
		report.ApproxInterpolations += sumInterpolations(a)
		for pi := range f.Flattened {
			if pi >= len(a.Flattened) || f.Flattened[pi].PathID != a.Flattened[pi].PathID {
				return report, withSample(withField(newError(CodeScale, "path order differs between modes"), "quality.paths"), i, f.Time)
			}
			fs := findStats(f.PathStats, f.Flattened[pi].PathID)
			as := findStats(a.PathStats, a.Flattened[pi].PathID)
			metric := QualityPathMetric{
				PathID: fs.PathID, AreaFine: fs.CoverageArea, AreaApprox: as.CoverageArea,
				BBoxIoU:          fs.CoverageBBox.IoU(as.CoverageBBox),
				SegmentCountFine: fs.SegmentCount, SegmentCountApprox: as.SegmentCount,
				InterpolationFine: fs.InterpolationCount, InterpolationApprox: as.InterpolationCount,
				FineCoverageBBox: fs.CoverageBBox, ApproxCoverageBBox: as.CoverageBBox,
			}
			den := math.Max(math.Abs(fs.CoverageArea), 1e-12)
			metric.RelativeAreaError = math.Abs(as.CoverageArea-fs.CoverageArea) / den
			if fs.CoverageArea == 0 && as.CoverageArea == 0 {
				metric.RelativeAreaError = 0
			}
			if p := findScenePath(scene, fs.PathID); p != nil {
				fm, err := RasterizePath(p, f.Flattened[pi], f.Frame, f.Quality.Grid)
				if err != nil {
					return report, err
				}
				am, err := RasterizePath(p, a.Flattened[pi], a.Frame, a.Quality.Grid)
				if err != nil {
					return report, err
				}
				metric.MeanCoverageDiff = maskMeanDiff(fm, am)
			}
			sr.Paths = append(sr.Paths, metric)
			if report.FirstFailure == nil {
				if metric.RelativeAreaError > tol.RelativeArea {
					report.Pass = false
					report.FirstFailure = &QualityFailure{Sample: i, PathID: metric.PathID, Metric: "relative_area", Measured: metric.RelativeAreaError, Expected: 0, Limit: tol.RelativeArea}
				} else if metric.BBoxIoU < tol.BBoxIoU {
					report.Pass = false
					report.FirstFailure = &QualityFailure{Sample: i, PathID: metric.PathID, Metric: "bbox_iou", Measured: metric.BBoxIoU, Expected: 1, Limit: tol.BBoxIoU}
				} else if metric.MeanCoverageDiff > tol.MeanCoverage {
					report.Pass = false
					report.FirstFailure = &QualityFailure{Sample: i, PathID: metric.PathID, Metric: "mean_coverage", Measured: metric.MeanCoverageDiff, Expected: 0, Limit: tol.MeanCoverage}
				}
			}
		}
		report.Samples = append(report.Samples, sr)
	}
	if truth != nil && !report.MidpointCovered {
		report.Pass = false
		report.FirstFailure = &QualityFailure{Metric: "midpoint_coverage", Limit: 1}
		return report, withField(newError(CodeScale, "quality check did not cover a keyframe midpoint"), "midpoint")
	}
	if report.FirstFailure != nil {
		return report, scaleError(report.FirstFailure, "quality tolerance exceeded")
	}
	return report, nil
}

func scaleError(f *QualityFailure, msg string) error {
	e := withField(newError(CodeScale, msg), "quality")
	e.Sample = f.Sample
	if f.PathID != "" {
		e.PathID = f.PathID
	}
	e.Metric = f.Metric
	if f.Metric == "bbox_iou" {
		e.Measured = fmt.Sprintf("approx_iou=%.8g", f.Measured)
		e.Expected = fmt.Sprintf("fine_iou=1 limit=%.8g", f.Limit)
	} else {
		e.Measured = fmt.Sprintf("approx=%.8g", f.Measured)
		e.Expected = fmt.Sprintf("fine=%.8g limit=%.8g", f.Expected, f.Limit)
	}
	return e
}

func pixelError(a, b []uint8) PixelErrorStats {
	if len(a) != len(b) {
		return PixelErrorStats{MaxAbs: 1}
	}
	var sumAbs, sumSq float64
	maxAbs := 0.0
	changed := 0
	for i := range a {
		d := math.Abs(float64(a[i])-float64(b[i])) / 255.0
		sumAbs += d
		sumSq += d * d
		if d > maxAbs {
			maxAbs = d
		}
		if a[i] != b[i] {
			changed++
		}
	}
	n := math.Max(1, float64(len(a)))
	return PixelErrorStats{MAE: sumAbs / n, RMSE: math.Sqrt(sumSq / n), MaxAbs: maxAbs, MeanCov: sumAbs / n, Changed: changed}
}

func sumSegments(r *SampleResult) int {
	n := 0
	for _, s := range r.PathStats {
		n += s.SegmentCount
	}
	return n
}

func sumInterpolations(r *SampleResult) int {
	n := 0
	for _, s := range r.PathStats {
		n += s.InterpolationCount
	}
	return n
}

func findScenePath(scene *Scene, id string) *Path {
	if scene == nil {
		return nil
	}
	for i := range scene.Paths {
		if scene.Paths[i].ID == id {
			return &scene.Paths[i]
		}
	}
	return nil
}

func maskMeanDiff(a, b *PathRaster) float64 {
	if a == nil || b == nil {
		return 1
	}
	rect := a.Rect.Union(b.Rect)
	if rect.Empty() {
		return 0
	}
	sum := 0.0
	for y := rect.Y0; y < rect.Y1; y++ {
		for x := rect.X0; x < rect.X1; x++ {
			sum += math.Abs(float64(maskAt(a, x, y)) - float64(maskAt(b, x, y)))
		}
	}
	if rect.Area() == 0 {
		return 0
	}
	return sum / float64(rect.Area())
}

func maskAt(r *PathRaster, x, y int) float32 {
	if r == nil || x < r.Rect.X0 || y < r.Rect.Y0 || x >= r.Rect.X1 || y >= r.Rect.Y1 {
		return 0
	}
	return r.Coverage[(y-r.Rect.Y0)*r.Rect.Width()+x-r.Rect.X0]
}

func SourceSummary(q Quality) string {
	return fmt.Sprintf("%s grid=%d mid_tol=%.6g chunk=%d budget=%d reasons=%s",
		q.Mode, q.Grid, q.MidTolerance, q.ChunkSize, q.MemoryBudget, strings.Join(q.Reasons, "; "))
}
