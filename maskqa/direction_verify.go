package maskqa

import (
	"math"
)

const DirectionAngleToleranceDeg = 18.0

type DirectionResult struct {
	SegmentID        string  `json:"segment_id"`
	DomainID         int     `json:"domain_id"`
	StrokeIndex      int     `json:"stroke_index"`
	Probe            Point   `json:"probe"`
	ExpectedCategory string  `json:"expected_category"`
	ActualCategory   string  `json:"actual_category"`
	ExpectedAngle    float64 `json:"expected_angle_deg"`
	MeasuredAngle    float64 `json:"measured_angle_deg"`
	RawTangentAngle  float64 `json:"raw_tangent_angle_deg"`
	NormalAngle      float64 `json:"normal_angle_deg"`
	NormalPerpError  float64 `json:"normal_perp_error_deg"`
	AngleError       float64 `json:"angle_error_deg"`
	OK               bool    `json:"ok"`
}

type DirectionReport struct {
	OK         bool              `json:"ok"`
	Segments   []DirectionResult `json:"segments"`
	Mismatches []SegmentIssue    `json:"mismatches,omitempty"`
}

// VerifyDirections estimates boundary tangent/normal angles and compares category against truth.
func VerifyDirections(m *Mask, truth *TruthManifest) (*DirectionReport, error) {
	rep := &DirectionReport{OK: true}
	for _, seg := range truth.Segments {
		points := collectBoundaryWindow(m, seg.Probe, 5)
		result := DirectionResult{
			SegmentID: seg.SegmentID, DomainID: seg.DomainID, StrokeIndex: seg.StrokeIndex,
			Probe:            seg.Probe,
			ExpectedCategory: seg.ExpectedCategory, ExpectedAngle: seg.ExpectedAngleDeg,
			OK: false,
		}
		if len(points) < 6 {
			result.ActualCategory = "unknown"
			rep.Segments = append(rep.Segments, result)
			rep.OK = false
			rep.Mismatches = append(rep.Mismatches, SegmentIssue{SegmentID: seg.SegmentID, DomainID: seg.DomainID, Expected: seg.ExpectedCategory, Actual: "unknown", ExpectedAngle: seg.ExpectedAngleDeg, MeasuredAngle: 0})
			continue
		}
		theta := principalTangentAngle(points)
		measured := OrientAngleToExpected(theta*180/math.Pi, seg.ExpectedAngleDeg)
		normal := sobelNormalAngle(m, nearestPoint(points, seg.Probe))
		normalErr := math.Min(math.Abs(AngleDistanceDeg(normal, seg.ExpectedAngleDeg+90)), math.Abs(AngleDistanceDeg(normal, seg.ExpectedAngleDeg-90)))
		angleErr := math.Abs(AngleDistanceDeg(measured, seg.ExpectedAngleDeg))
		actualCat := CategoryFromAngle(measured)
		result.RawTangentAngle = theta * 180 / math.Pi
		result.MeasuredAngle = measured
		result.NormalAngle = normal
		result.NormalPerpError = normalErr
		result.AngleError = angleErr
		result.ActualCategory = actualCat
		result.OK = actualCat == seg.ExpectedCategory && angleErr <= DirectionAngleToleranceDeg && normalErr <= 35
		rep.Segments = append(rep.Segments, result)
		if !result.OK {
			rep.OK = false
			rep.Mismatches = append(rep.Mismatches, SegmentIssue{
				SegmentID: seg.SegmentID, DomainID: seg.DomainID,
				Expected: seg.ExpectedCategory, Actual: actualCat,
				ExpectedAngle: seg.ExpectedAngleDeg, MeasuredAngle: measured, NormalAngle: normal,
			})
		}
	}
	if len(rep.Mismatches) > 0 {
		return rep, &Error{Code: CodeDIR, Message: "direction verification failed", Segments: rep.Mismatches}
	}
	return rep, nil
}

func collectBoundaryWindow(m *Mask, center Point, radius int) []Point {
	var pts []Point
	for y := center.Y - radius; y <= center.Y+radius; y++ {
		for x := center.X - radius; x <= center.X+radius; x++ {
			if m.Get(x, y) == 0 {
				continue
			}
			if m.Get(x-1, y) == 0 || m.Get(x+1, y) == 0 || m.Get(x, y-1) == 0 || m.Get(x, y+1) == 0 {
				pts = append(pts, Point{X: x, Y: y})
			}
		}
	}
	return pts
}

func principalTangentAngle(points []Point) float64 {
	var sx, sy float64
	for _, p := range points {
		sx += float64(p.X)
		sy += float64(p.Y)
	}
	n := float64(len(points))
	mx, my := sx/n, sy/n
	var sxx, syy, sxy float64
	for _, p := range points {
		dx := float64(p.X) - mx
		dy := float64(p.Y) - my
		sxx += dx * dx
		syy += dy * dy
		sxy += dx * dy
	}
	return 0.5 * math.Atan2(2*sxy, sxx-syy)
}

func nearestPoint(points []Point, target Point) Point {
	best := points[0]
	bestD := (best.X-target.X)*(best.X-target.X) + (best.Y-target.Y)*(best.Y-target.Y)
	for _, p := range points[1:] {
		d := (p.X-target.X)*(p.X-target.X) + (p.Y-target.Y)*(p.Y-target.Y)
		if d < bestD {
			bestD = d
			best = p
		}
	}
	return best
}

func sobelNormalAngle(m *Mask, p Point) float64 {
	var gx, gy int
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			v := int(m.Get(p.X+dx, p.Y+dy))
			wx := sobelX[dy+1][dx+1]
			wy := sobelY[dy+1][dx+1]
			gx += v * wx
			gy += v * wy
		}
	}
	if gx == 0 && gy == 0 {
		return 0
	}
	return AngleFromVector(float64(gx), float64(gy))
}

var sobelX = [3][3]int{
	{-1, 0, 1},
	{-2, 0, 2},
	{-1, 0, 1},
}

var sobelY = [3][3]int{
	{-1, -2, -1},
	{0, 0, 0},
	{1, 2, 1},
}
