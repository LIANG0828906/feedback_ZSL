package iconmask

import (
	"math"
	"sort"
)

type EdgeConfig struct {
	Shape         string `json:"shape"`
	RadiusBandMin int    `json:"radiusBandMin"`
}

type SegmentCurvatureEstimate struct {
	SegmentID      string  `json:"segmentId"`
	ComponentID    string  `json:"componentId"`
	Loop           string  `json:"loop"`
	Curvature      float64 `json:"curvature"`
	TruthCurvature float64 `json:"truthCurvature"`
	Samples        int     `json:"samples"`
	Source         string  `json:"source"`
}

type EdgeResult struct {
	Band      Mask                       `json:"band"`
	Empty     bool                       `json:"empty"`
	Status    string                     `json:"status"`
	Estimates []SegmentCurvatureEstimate `json:"estimates"`
}

func LocateEdgeBand(imageID string, mask Mask, segments []BoundarySegment, cfg EdgeConfig) (*EdgeResult, *Error) {
	if mask.Empty() || !mask.Binary() {
		return nil, schemaError(imageID, "initialMask", "edge band requires a single-channel binary mask")
	}
	element, err := NewStructuringElement(imageID, cfg.Shape, cfg.RadiusBandMin)
	if err != nil {
		if err.Field == "shape" {
			err.Field = "shape"
		} else {
			err.Field = "radiusBandMin"
		}
		return nil, err
	}
	band := MorphologicalGradient(mask, element)
	result := &EdgeResult{
		Band:   band,
		Empty:  band.Count() == 0,
		Status: "ready",
	}
	if result.Empty {
		result.Status = "empty_edge_band"
		return result, nil
	}
	result.Estimates = estimateSegmentCurvatures(mask, segments)
	return result, nil
}

func estimateSegmentCurvatures(mask Mask, segments []BoundarySegment) []SegmentCurvatureEstimate {
	boundary := boundaryPixels(mask)
	if len(boundary) == 0 {
		return nil
	}
	estimates := make([]SegmentCurvatureEstimate, 0, len(segments))
	for _, segment := range segments {
		values := make([]float64, 0, len(segment.Points))
		for _, point := range segment.Points {
			anchor := nearestBoundaryPoint(boundary, point)
			if anchor < 0 {
				continue
			}
			if curvature, ok := fitLocalCurvature(boundary, anchor, 5.0); ok {
				values = append(values, curvature)
			}
		}
		curvature := 0.0
		if len(values) > 0 {
			for _, value := range values {
				curvature += value
			}
			curvature /= float64(len(values))
		}
		estimates = append(estimates, SegmentCurvatureEstimate{
			SegmentID:      segment.ID,
			ComponentID:    segment.ComponentID,
			Loop:           segment.Loop,
			Curvature:      curvature,
			TruthCurvature: segment.Curvature,
			Samples:        len(values),
			Source:         "initial_mask_boundary_circle_fit",
		})
	}
	sort.SliceStable(estimates, func(i, j int) bool { return estimates[i].SegmentID < estimates[j].SegmentID })
	return estimates
}

func boundaryPixels(mask Mask) []PointInt {
	points := make([]PointInt, 0, 256)
	for y := 0; y < mask.Height; y++ {
		for x := 0; x < mask.Width; x++ {
			if mask.At(x, y) == 0 {
				continue
			}
			if x == 0 || y == 0 || x+1 == mask.Width || y+1 == mask.Height ||
				mask.At(x-1, y) == 0 || mask.At(x+1, y) == 0 ||
				mask.At(x, y-1) == 0 || mask.At(x, y+1) == 0 {
				points = append(points, PointInt{X: x, Y: y})
			}
		}
	}
	return points
}

func nearestBoundaryPoint(points []PointInt, target Point) int {
	best := -1
	bestDistance := math.Inf(1)
	for i, point := range points {
		dx := float64(point.X) + 0.5 - target.X
		dy := float64(point.Y) + 0.5 - target.Y
		distance := dx*dx + dy*dy
		if distance < bestDistance {
			bestDistance = distance
			best = i
		}
	}
	return best
}

// fitLocalCurvature uses an algebraic circle fit over boundary pixels near an
// anchor. The result is a scale-invariant curvature magnitude; straight local
// boundaries fall back to zero when the fitted radius is very large.
func fitLocalCurvature(points []PointInt, anchorIndex int, radius float64) (float64, bool) {
	anchor := points[anchorIndex]
	near := make([]PointInt, 0, 32)
	for _, point := range points {
		dx := float64(point.X - anchor.X)
		dy := float64(point.Y - anchor.Y)
		if dx*dx+dy*dy <= radius*radius {
			near = append(near, point)
		}
	}
	if len(near) < 8 {
		return 0, false
	}
	// Kasa circle fit: x^2+y^2 + D*x + E*y + F = 0.
	var sumX, sumY, sumXX, sumYY, sumXY, sumXXX, sumYYY, sumXYY, sumXXY float64
	for _, point := range near {
		x := float64(point.X) + 0.5
		y := float64(point.Y) + 0.5
		sumX += x
		sumY += y
		sumXX += x * x
		sumYY += y * y
		sumXY += x * y
		sumXXX += x * x * x
		sumYYY += y * y * y
		sumXYY += x * y * y
		sumXXY += x * x * y
	}
	n := float64(len(near))
	// Solve a 3x3 system with Cramer's rule. The system is
	// [sumXX sumXY sumX; sumXY sumYY sumY; sumX sumY n] [D E F]^T = rhs.
	a11, a12, a13 := sumXX, sumXY, sumX
	a21, a22, a23 := sumXY, sumYY, sumY
	a31, a32, a33 := sumX, sumY, n
	b1 := -(sumXXX + sumXYY)
	b2 := -(sumXXY + sumYYY)
	b3 := -(sumXX + sumYY)
	det := determinant3(a11, a12, a13, a21, a22, a23, a31, a32, a33)
	if math.Abs(det) < 1e-9 {
		return 0, false
	}
	d := determinant3(b1, a12, a13, b2, a22, a23, b3, a32, a33) / det
	e := determinant3(a11, b1, a13, a21, b2, a23, a31, b3, a33) / det
	f := determinant3(a11, a12, b1, a21, a22, b2, a31, a32, b3) / det
	radiusSquared := (d*d+e*e)/4 - f
	if radiusSquared <= 1e-6 {
		return 0, false
	}
	fitRadius := math.Sqrt(radiusSquared)
	if fitRadius > 1e4 {
		return 0, true
	}
	return 1 / fitRadius, true
}

func determinant3(a, b, c, d, e, f, g, h, i float64) float64 {
	return a*(e*i-f*h) - b*(d*i-f*g) + c*(d*h-e*g)
}
