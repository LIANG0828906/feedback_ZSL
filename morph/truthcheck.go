package morph

import (
	"fmt"
	"math"
)

type TruthPathReport struct {
	PathID            string  `json:"path_id"`
	MaxPointError     float64 `json:"max_point_error"`
	ContourAreaError  float64 `json:"contour_area_relative_error"`
	CoverageAreaError float64 `json:"coverage_area_relative_error"`
	BBoxMaxError      float64 `json:"bbox_max_error"`
	Closed            bool    `json:"closed"`
}

type TruthSampleReport struct {
	Index       int               `json:"index"`
	Time        float64           `json:"time"`
	WidthMatch  bool              `json:"width_match"`
	HeightMatch bool              `json:"height_match"`
	BBoxMatch   bool              `json:"bbox_match"`
	Paths       []TruthPathReport `json:"paths"`
}

type TruthReport struct {
	Pass           bool                `json:"pass"`
	PointTolerance float64             `json:"point_tolerance"`
	AreaTolerance  float64             `json:"area_tolerance"`
	Samples        []TruthSampleReport `json:"samples"`
}

func ValidateResultsAgainstTruth(scene *Scene, truth *Truth, results []*SampleResult, cfg Config, pointTol, areaTol float64) (*TruthReport, error) {
	if err := ValidateTruth(scene, truth); err != nil {
		return nil, err
	}
	if len(results) != len(truth.Samples) {
		return nil, withField(newError(CodeDecode, "result sample count does not match truth"), "truth.samples")
	}
	if pointTol <= 0 {
		pointTol = 1e-6
	}
	if areaTol <= 0 {
		areaTol = 0.05
	}
	report := &TruthReport{Pass: true, PointTolerance: pointTol, AreaTolerance: areaTol}
	for si, ts := range truth.Samples {
		r := results[si]
		if r == nil {
			e := withSample(withField(newError(CodeDecode, "missing rendered sample"), "results"), si, ts.Time)
			return report, e
		}
		sr := TruthSampleReport{
			Index: si, Time: ts.Time, WidthMatch: r.Width == ts.Width,
			HeightMatch: r.Height == ts.Height,
		}
		sr.BBoxMatch = bboxClose(r.Frame.GeometryBBox, ts.FrameBBox, 1e-7)
		if !sr.WidthMatch || !sr.HeightMatch || !sr.BBoxMatch {
			report.Pass = false
			e := withSample(withField(newError(CodeScale, "frame geometry does not match truth"), "frame"), si, ts.Time)
			e.Measured = fmt.Sprintf("%dx%d", r.Width, r.Height)
			e.Expected = fmt.Sprintf("%dx%d", ts.Width, ts.Height)
			report.Samples = append(report.Samples, sr)
			return report, e
		}
		for pi, pt := range ts.Paths {
			p := findScenePath(scene, pt.PathID)
			if p == nil {
				return report, withSample(withPath(withField(newError(CodePath, "truth path is missing in scene"), "path_id"), pt.PathID), si, ts.Time)
			}
			segs, _, err := InterpolatePath(p, ts.Time, cfg.Eval.Interpolation)
			if err != nil {
				return report, err
			}
			pr := TruthPathReport{PathID: pt.PathID, Closed: true}
			for _, cs := range pt.CurveSamples {
				if cs.Segment < 0 || cs.Segment >= len(segs) {
					return report, withSample(withPath(withField(newError(CodePath, "truth curve sample segment is invalid"), "curve_samples.segment"), pt.PathID), si, ts.Time)
				}
				got := EvalSegment(segs[cs.Segment], cs.T)
				d := got.Distance(cs.Point)
				if d > pr.MaxPointError {
					pr.MaxPointError = d
				}
				if d > pointTol {
					report.Pass = false
					e := withSample(withPath(withField(newError(CodeScale, "curve sample exceeds point tolerance"), "curve_samples"), pt.PathID), si, ts.Time)
					e.Metric = "curve_point"
					e.Measured = fmt.Sprintf("%.12g", d)
					e.Expected = fmt.Sprintf("<=%.12g", pointTol)
					report.Samples = append(report.Samples, sr)
					return report, e
				}
			}
			for _, cp := range pt.ClosureCheckpoints {
				if !pointOnContour(cp, segs, pointTol) {
					pr.Closed = false
					report.Pass = false
					e := withSample(withPath(withField(newError(CodePath, "closure checkpoint is not on contour"), "closure_checkpoints"), pt.PathID), si, ts.Time)
					report.Samples = append(report.Samples, sr)
					return report, e
				}
			}
			area := math.Abs(ContourArea(segs))
			den := math.Max(math.Abs(pt.ContourArea), 1e-12)
			pr.ContourAreaError = math.Abs(area-pt.ContourArea) / den
			if pr.ContourAreaError > 1e-7 {
				report.Pass = false
				e := withSample(withPath(withField(newError(CodeScale, "contour area exceeds analytic tolerance"), "contour_area"), pt.PathID), si, ts.Time)
				e.Metric = "contour_area"
				e.Measured = fmt.Sprintf("%.12g", pr.ContourAreaError)
				e.Expected = "<=1e-7"
				report.Samples = append(report.Samples, sr)
				return report, e
			}
			gb := SegmentsBBox(segs)
			if p.Stroke != nil {
				gb = gb.Expand(p.Stroke.Width * 0.5)
			}
			pr.BBoxMaxError = bboxMaxDiff(gb, pt.GeometryBBox)
			if pr.BBoxMaxError > 1e-7 {
				report.Pass = false
				e := withSample(withPath(withField(newError(CodeScale, "path bbox does not match truth"), "geometry_bbox"), pt.PathID), si, ts.Time)
				report.Samples = append(report.Samples, sr)
				return report, e
			}
			if pi < len(r.PathStats) {
				gotArea := r.PathStats[pi].CoverageArea
				cden := math.Max(pt.ReferenceCoverageArea, 1e-12)
				pr.CoverageAreaError = math.Abs(gotArea-pt.ReferenceCoverageArea) / cden
				if gotArea == 0 && pt.ReferenceCoverageArea == 0 {
					pr.CoverageAreaError = 0
				}
				if pr.CoverageAreaError > areaTol {
					report.Pass = false
					e := withSample(withPath(withField(newError(CodeScale, "coverage area exceeds declared tolerance"), "coverage_area"), pt.PathID), si, ts.Time)
					e.Metric = "coverage_area"
					e.Measured = fmt.Sprintf("%.8g", pr.CoverageAreaError)
					e.Expected = fmt.Sprintf("<=%.8g", areaTol)
					report.Samples = append(report.Samples, sr)
					return report, e
				}
			}
			sr.Paths = append(sr.Paths, pr)
		}
		report.Samples = append(report.Samples, sr)
	}
	return report, nil
}

func pointOnContour(p Vec2, segs []Segment, tol float64) bool {
	if len(segs) == 0 {
		return false
	}
	if segs[0].P0.Distance(p) <= tol || segs[len(segs)-1].End().Distance(p) <= tol {
		return true
	}
	for _, s := range segs {
		if s.P0.Distance(p) <= tol || s.End().Distance(p) <= tol {
			return true
		}
		if distancePointSegment(p, s.P0, s.End()) <= tol {
			return true
		}
	}
	return false
}

func bboxClose(a, b BBox, tol float64) bool { return bboxMaxDiff(a, b) <= tol }

func bboxMaxDiff(a, b BBox) float64 {
	if a.IsEmpty() && b.IsEmpty() {
		return 0
	}
	return math.Max(
		math.Max(math.Abs(a.Min.X-b.Min.X), math.Abs(a.Min.Y-b.Min.Y)),
		math.Max(math.Abs(a.Max.X-b.Max.X), math.Abs(a.Max.Y-b.Max.Y)),
	)
}
