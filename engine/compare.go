package engine

import (
	"fmt"
	"math"
)

type DiffReport struct {
	Consistent     bool      `json:"consistent"`
	Frame          int       `json:"frame"`
	Time           int64     `json:"timeUs"`
	Pixel          *Pixel    `json:"pixel,omitempty"`
	Field          string    `json:"field,omitempty"`
	Left           string    `json:"left,omitempty"`
	Right          string    `json:"right,omitempty"`
	CommitSequence []int     `json:"commitSequence"`
	FrameHashes    []string  `json:"frameHashes"`
	CheckedRuns    []RunMode `json:"checkedRuns"`
}

// CompareRuns compares preview, resume and full reports deterministically.
func CompareRuns(runs ...*RunReport) (*DiffReport, error) {
	report := &DiffReport{Consistent: true}
	if len(runs) == 0 {
		return report, nil
	}
	for _, r := range runs {
		if r == nil {
			report.Consistent = false
			report.Field = "run.nil"
			return report, diffError(report)
		}
		report.CheckedRuns = append(report.CheckedRuns, r.Mode)
	}
	base := runs[0]
	report.CommitSequence = append([]int(nil), base.CommitSequence...)
	for _, f := range base.Frames {
		report.FrameHashes = append(report.FrameHashes, f.ContentHash)
	}
	for ri := 1; ri < len(runs); ri++ {
		other := runs[ri]
		if len(base.CommitSequence) != len(other.CommitSequence) {
			report.Consistent = false
			report.Frame = minInt(len(base.CommitSequence), len(other.CommitSequence))
			report.Field = "commitSequence.length"
			report.Left = fmt.Sprint(len(base.CommitSequence))
			report.Right = fmt.Sprint(len(other.CommitSequence))
			return report, diffError(report)
		}
		for i := range base.CommitSequence {
			if base.CommitSequence[i] != other.CommitSequence[i] {
				report.Consistent = false
				report.Frame = i + 1
				if i < len(base.Frames) {
					report.Time = base.Frames[i].Time
				}
				report.Field = "commitSequence"
				report.Left = fmt.Sprint(base.CommitSequence[i])
				report.Right = fmt.Sprint(other.CommitSequence[i])
				return report, diffError(report)
			}
		}
		if len(base.Frames) != len(other.Frames) {
			report.Consistent = false
			report.Frame = minInt(len(base.Frames), len(other.Frames)) + 1
			report.Field = "frames.length"
			report.Left = fmt.Sprint(len(base.Frames))
			report.Right = fmt.Sprint(len(other.Frames))
			return report, diffError(report)
		}
		for i := range base.Frames {
			if err := compareFrame(base.Frames[i], other.Frames[i], report); err != nil {
				report.Frame = i + 1
				report.Time = base.Frames[i].Time
				return report, err
			}
		}
	}
	return report, nil
}

func compareFrame(a, b *RenderedFrame, report *DiffReport) error {
	if a.Time != b.Time {
		report.Consistent = false
		report.Field = "timeUs"
		report.Left = fmt.Sprint(a.Time)
		report.Right = fmt.Sprint(b.Time)
		return diffError(report)
	}
	if a.Width != b.Width || a.Height != b.Height {
		report.Consistent = false
		report.Field = "bitmap.size"
		report.Left = fmt.Sprintf("%dx%d", a.Width, a.Height)
		report.Right = fmt.Sprintf("%dx%d", b.Width, b.Height)
		return diffError(report)
	}
	if a.LineCount != b.LineCount {
		report.Consistent = false
		report.Field = "lineCount"
		report.Left = fmt.Sprint(a.LineCount)
		report.Right = fmt.Sprint(b.LineCount)
		return diffError(report)
	}
	if a.ContentHash != b.ContentHash {
		report.Consistent = false
		report.Field = "contentHash"
		report.Left = a.ContentHash
		report.Right = b.ContentHash
		for i := 0; i < minInt(len(a.RGBA), len(b.RGBA)); i += 4 {
			if a.RGBA[i] != b.RGBA[i] || a.RGBA[i+1] != b.RGBA[i+1] || a.RGBA[i+2] != b.RGBA[i+2] || a.RGBA[i+3] != b.RGBA[i+3] {
				pix := i / 4
				report.Pixel = &Pixel{X: pix % a.Width, Y: pix / a.Width}
				report.Field = "bitmap.rgba"
				break
			}
		}
		return diffError(report)
	}
	return nil
}

func diffError(report *DiffReport) error {
	e := &Error{Code: CodeDiff, Frame: report.Frame, Field: report.Field, Path: report.Field, Left: report.Left, Right: report.Right}
	e.Pixel = report.Pixel
	if report.Time != 0 {
		e.Message = fmt.Sprintf("first inconsistency at time %d", report.Time)
	}
	return e
}

type TruthTolerance struct {
	CurveAxisUnits   int64   `json:"curveAxisQ10"`
	AreaRelative     float64 `json:"areaRelative"`
	BoundsIoU        float64 `json:"boundsIoU"`
	CoverageAbsolute float64 `json:"coverageAbsolute"`
}

func DefaultTruthTolerance() TruthTolerance {
	return TruthTolerance{CurveAxisUnits: 96, AreaRelative: 0.01, BoundsIoU: 0.99, CoverageAbsolute: 0.02}
}

// VerifyTruth checks curve samples, area, bounds and coverage against the
// generator-produced truth document.
func VerifyTruth(scene *Scene, truth *TruthDocument, frames []*RenderedFrame, tol TruthTolerance) error {
	if truth == nil {
		return &Error{Code: CodeSchema, Path: "truth", Message: "no truth document"}
	}
	if tol.CurveAxisUnits <= 0 {
		tol = DefaultTruthTolerance()
	}
	frameByTime := map[int64]*RenderedFrame{}
	for _, f := range frames {
		frameByTime[f.Time] = f
	}
	for fi, tf := range truth.Samples {
		rf := frameByTime[tf.Time]
		if rf == nil {
			return &Error{Code: CodeDiff, Frame: fi + 1, Field: "frame.missing", Message: fmt.Sprintf("no rendered frame at %d", tf.Time)}
		}
		if rf.LineCount <= 0 {
			return &Error{Code: CodeDiff, Frame: fi + 1, Field: "lines", Message: "empty flattened frame"}
		}
		geometry := rf.GeometryBounds
		if iou := rectIoU(geometry, tf.Bounds); iou < tol.BoundsIoU {
			return &Error{Code: CodeDiff, Frame: fi + 1, Field: "bounds", Metric: "iou", Actual: iou, Limit: tol.BoundsIoU}
		}
		for pi, tp := range tf.Paths {
			if pi >= len(rf.Paths) {
				return &Error{Code: CodeDiff, Frame: fi + 1, Field: "paths", Message: "missing truth path"}
			}
			path := findPath(scene, tp.ID)
			if path == nil {
				return &Error{Code: CodeSchema, Path: "truth.paths." + tp.ID, Message: "unknown truth path"}
			}
			segments, err := EvalPathAt(path, tf.Time, scene.Interpolation)
			if err != nil {
				return err
			}
			for _, cs := range tp.Samples {
				if cs.Segment < 0 || cs.Segment >= len(segments) {
					return &Error{Code: CodeDiff, Frame: fi + 1, Field: "curveSample.segment", Message: "segment index out of range"}
				}
				got := EvalCurvePoint(segments[cs.Segment], cs.T)
				dx, dy := got.X-cs.Point.X, got.Y-cs.Point.Y
				if dx < 0 {
					dx = -dx
				}
				if dy < 0 {
					dy = -dy
				}
				if dx > tol.CurveAxisUnits || dy > tol.CurveAxisUnits {
					return &Error{
						Code: CodeDiff, Frame: fi + 1, Field: fmt.Sprintf("paths.%s.curveSamples[%d]", tp.ID, cs.Segment),
						Left: fmt.Sprintf("%+v", got), Right: fmt.Sprintf("%+v", cs.Point),
						Metric: "axisDelta", Actual: math.Max(float64(dx), float64(dy)), Limit: float64(tol.CurveAxisUnits),
					}
				}
			}
			flat, err := FlattenContour(segments, max64(1, tol.CurveAxisUnits/2))
			if err != nil {
				return err
			}
			area := PolylineArea(flat)
			areaRel := math.Abs(float64(area-tp.Area)) / maxFloat(math.Abs(float64(tp.Area)), 1)
			if areaRel > tol.AreaRelative {
				return &Error{
					Code: CodeDiff, Frame: fi + 1, Field: fmt.Sprintf("paths.%s.area", tp.ID),
					Metric: "areaRelativeError", Actual: areaRel, Limit: tol.AreaRelative,
				}
			}
			got := rf.Paths[pi]
			if math.Abs(got.CoverageRatio-tp.CoverageRatio) > tol.CoverageAbsolute {
				return &Error{
					Code: CodeDiff, Frame: fi + 1, Field: fmt.Sprintf("paths.%s.coverageRatio", tp.ID),
					Metric: "coverageCanvas", Actual: got.CoverageRatio, Limit: tp.CoverageRatio + tol.CoverageAbsolute,
				}
			}
			if math.Abs(got.CoverageBBoxRatio-tp.CoverageBBoxRatio) > tol.CoverageAbsolute {
				return &Error{
					Code: CodeDiff, Frame: fi + 1, Field: fmt.Sprintf("paths.%s.coverage", tp.ID),
					Metric: "coverageBBox", Actual: got.CoverageBBoxRatio, Limit: tp.CoverageBBoxRatio + tol.CoverageAbsolute,
				}
			}
		}
	}
	return nil
}

// VerifyFlattenAgainstTruth checks that every generator curve sample is within
// the configured flatten tolerance of the evaluated polyline.
func VerifyFlattenAgainstTruth(scene *Scene, truth *TruthDocument, frames []*RenderedFrame) error {
	frameMap := map[int64]*RenderedFrame{}
	for _, f := range frames {
		frameMap[f.Time] = f
	}
	for fi, tf := range truth.Samples {
		frame := frameMap[tf.Time]
		if frame == nil {
			return &Error{Code: CodeDiff, Frame: fi + 1, Field: "flatten.missingFrame"}
		}
		for pi, tp := range tf.Paths {
			if pi >= len(frame.Lines) {
				return &Error{Code: CodeDiff, Frame: fi + 1, Field: "flatten.lines"}
			}
			line := frame.Lines[pi]
			for _, cs := range tp.Samples {
				dist := distanceToPolyline(cs.Point, line.Points)
				if dist > float64(scene.Tolerance) {
					return &Error{
						Code: CodeDiff, Frame: fi + 1, Field: fmt.Sprintf("paths.%s.flatten", tp.ID),
						Metric: "curvePointDistanceQ10", Actual: dist, Limit: float64(scene.Tolerance),
					}
				}
			}
		}
	}
	return nil
}

func distanceToPolyline(p Vec2, poly []Vec2) float64 {
	if len(poly) == 1 {
		return math.Hypot(float64(p.X-poly[0].X), float64(p.Y-poly[0].Y))
	}
	best := math.Inf(1)
	for i := range poly {
		a, b := poly[i], poly[(i+1)%len(poly)]
		best = math.Min(best, pointSegmentDistance(p, a, b))
	}
	return best
}

func pointSegmentDistance(p, a, b Vec2) float64 {
	dx := float64(b.X - a.X)
	dy := float64(b.Y - a.Y)
	if dx == 0 && dy == 0 {
		return math.Hypot(float64(p.X-a.X), float64(p.Y-a.Y))
	}
	t := (float64(p.X-a.X)*dx + float64(p.Y-a.Y)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	x := float64(a.X) + t*dx
	y := float64(a.Y) + t*dy
	return math.Hypot(float64(p.X)-x, float64(p.Y)-y)
}

func findPath(scene *Scene, id string) *Path {
	for i := range scene.Paths {
		if scene.Paths[i].ID == id {
			return &scene.Paths[i]
		}
	}
	return nil
}
