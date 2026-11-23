package morph

import (
	"fmt"
	"math"
)

type SampleConsistency struct {
	Index     int     `json:"index"`
	Time      float64 `json:"time"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	PixelHash string  `json:"pixel_hash"`
	Identical bool    `json:"identical"`
}

type ThreePathReport struct {
	Pass          bool                `json:"pass"`
	Labels        []string            `json:"labels"`
	Samples       []SampleConsistency `json:"samples"`
	FirstMismatch *Divergence         `json:"first_mismatch,omitempty"`
}

type Divergence struct {
	Sample   int    `json:"sample"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	PathID   string `json:"path_id"`
	Expected uint8  `json:"expected"`
	Actual   uint8  `json:"actual"`
	Message  string `json:"message"`
}

func CompareResultFrames(scene *Scene, labels []string, runs [][]*SampleResult) (*ThreePathReport, error) {
	report := &ThreePathReport{Pass: true, Labels: append([]string(nil), labels...)}
	if len(runs) < 2 {
		return report, withField(newError(CodeDiverge, "at least two execution paths are required"), "compare")
	}
	n := len(runs[0])
	for _, r := range runs[1:] {
		if len(r) != n {
			return report, withField(newError(CodeDiverge, "execution paths have different sample counts"), "compare")
		}
	}
	for i := 0; i < n; i++ {
		base := runs[0][i]
		if base == nil {
			return report, withSample(withField(newError(CodeDiverge, "execution path has missing sample"), "compare"), i, 0)
		}
		for ri := 1; ri < len(runs); ri++ {
			other := runs[ri][i]
			if other == nil {
				return report, withSample(withField(newError(CodeDiverge, "execution path has missing sample"), "compare"), i, base.Time)
			}
			if base.Width != other.Width || base.Height != other.Height {
				d := &Divergence{Sample: i, X: -1, Y: -1, Message: fmt.Sprintf("%s=%dx%d %s=%dx%d", labels[0], base.Width, base.Height, labels[ri], other.Width, other.Height)}
				report.Pass = false
				report.FirstMismatch = d
				return report, divergeError(d)
			}
			for y := 0; y < base.Height; y++ {
				for x := 0; x < base.Width; x++ {
					p := y*base.Width + x
					if p >= len(base.Gray) || p >= len(other.Gray) {
						d := &Divergence{Sample: i, X: x, Y: y, Message: "bitmap length mismatch"}
						report.Pass = false
						report.FirstMismatch = d
						return report, divergeError(d)
					}
					if base.Gray[p] != other.Gray[p] {
						pathID := ownerAtPixel(scene, base, x, y)
						d := &Divergence{Sample: i, X: x, Y: y, PathID: pathID, Expected: base.Gray[p], Actual: other.Gray[p],
							Message: fmt.Sprintf("%s=%d %s=%d", labels[0], base.Gray[p], labels[ri], other.Gray[p])}
						report.Pass = false
						report.FirstMismatch = d
						return report, divergeError(d)
					}
				}
			}
		}
		report.Samples = append(report.Samples, SampleConsistency{
			Index: i, Time: base.Time, Width: base.Width, Height: base.Height,
			PixelHash: base.PixelHash, Identical: true,
		})
	}
	return report, nil
}

func divergeError(d *Divergence) error {
	e := withField(newError(CodeDiverge, "execution paths diverged: "+d.Message), "pixels")
	e.Sample = d.Sample
	e.PathID = d.PathID
	if d.X >= 0 && d.Y >= 0 {
		e.HasPixel = true
		e.PixelX, e.PixelY = d.X, d.Y
		e.Pixel = []int{d.X, d.Y}
	}
	return e
}

func ownerAtPixel(scene *Scene, res *SampleResult, x, y int) string {
	world := res.Frame.PixelToWorld(float64(x)+0.5, float64(y)+0.5)
	for i, flat := range res.Flattened {
		p := findScenePath(scene, flat.PathID)
		if p == nil {
			continue
		}
		if p.Fill != nil && pointInPolyline(world, flat.Points) {
			return flat.PathID
		}
		if p.Stroke != nil && distanceToPolyline(world, flat.Points) <= p.Stroke.Width*0.5 {
			return flat.PathID
		}
		if i < len(res.PathStats) && containsPixel(res.PathStats[i].CoverageBBox, x, y) {
			return flat.PathID
		}
	}
	// Anti-aliased edge pixels can sit a fraction outside the analytic point
	// test. Prefer the nearest coverage bbox, then the first path, so divergence
	// diagnostics always carry an owning path identifier.
	bestID, bestDist := "", math.Inf(1)
	for i, flat := range res.Flattened {
		if i >= len(res.PathStats) {
			continue
		}
		r := res.PathStats[i].CoverageBBox
		if r.Empty() {
			continue
		}
		dx := 0
		if x < r.X0 {
			dx = r.X0 - x
		} else if x >= r.X1 {
			dx = x - r.X1 + 1
		}
		dy := 0
		if y < r.Y0 {
			dy = r.Y0 - y
		} else if y >= r.Y1 {
			dy = y - r.Y1 + 1
		}
		d := float64(dx*dx + dy*dy)
		if d < bestDist {
			bestDist, bestID = d, flat.PathID
		}
	}
	if bestID != "" {
		return bestID
	}
	if len(res.Flattened) > 0 {
		return res.Flattened[0].PathID
	}
	return ""
}

func containsPixel(r PixelRect, x, y int) bool {
	return !r.Empty() && x >= r.X0 && x < r.X1 && y >= r.Y0 && y < r.Y1
}
