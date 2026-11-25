package seg

import (
	"math"

	"iconseg/internal/core"
)

type RegionDeviation struct {
	TruthID         int     `json:"truth_id"`
	MaskID          int     `json:"mask_id"`
	CentroidDX      float64 `json:"centroid_dx"`
	CentroidDY      float64 `json:"centroid_dy"`
	CentroidDelta   float64 `json:"centroid_delta"`
	AreaRelError    float64 `json:"area_rel_error"`
	HoleDelta       int     `json:"hole_delta"`
	BBoxDeltaPixels int     `json:"bbox_delta_pixels"`
}

type MatchReport struct {
	ReferenceMatch     bool              `json:"reference_match"`
	RegionCount        int               `json:"region_count"`
	MaskMismatchPixels int               `json:"mask_mismatch_pixels"`
	MaskMismatchRatio  float64           `json:"mask_mismatch_ratio"`
	MaxCentroidDelta   float64           `json:"max_centroid_delta"`
	MaxAreaRelError    float64           `json:"max_area_rel_error"`
	MaxHoleDelta       int               `json:"max_hole_delta"`
	Regions            []RegionDeviation `json:"regions"`
}

func MatchTruth(mask []byte, components []Component, manifest core.TruthManifest) (MatchReport, error) {
	report := MatchReport{RegionCount: len(components)}
	for i := range mask {
		expected := manifest.ReferenceMask[i] != 0
		actual := mask[i] != 0
		if expected != actual {
			report.MaskMismatchPixels++
		}
	}
	report.MaskMismatchRatio = float64(report.MaskMismatchPixels) / float64(len(mask))
	if len(components) != len(manifest.Regions) {
		var briefs []core.RegionBrief
		for i := len(manifest.Regions); i < len(components); i++ {
			briefs = append(briefs, core.RegionBrief{ID: components[i].ID, Area: components[i].Area, Reason: "extra"})
		}
		for i := len(components); i < len(manifest.Regions); i++ {
			briefs = append(briefs, core.RegionBrief{ID: manifest.Regions[i].ID, Area: manifest.Regions[i].Area, Reason: "missing"})
		}
		return report, core.RegionError("foreground region count differs from truth", briefs)
	}

	n := len(components)
	cost := make([][]float64, n)
	for i := range cost {
		cost[i] = make([]float64, n)
		for j := 0; j < n; j++ {
			truth := manifest.Regions[i]
			actual := components[j]
			dx := actual.CentroidX - truth.CentroidX
			dy := actual.CentroidY - truth.CentroidY
			dist := math.Hypot(dx, dy)
			areaRel := math.Abs(float64(actual.Area-truth.Area)) / math.Max(1, float64(truth.Area))
			holeDelta := float64(abs(actual.HoleCount - truth.HoleCount))
			bboxDX := math.Abs(float64(actual.BBox.X - truth.BBox.X))
			bboxDY := math.Abs(float64(actual.BBox.Y - truth.BBox.Y))
			cost[i][j] = dist/float64(manifest.Width+manifest.Height) + areaRel + 0.5*holeDelta +
				(bboxDX+bboxDY)/float64(manifest.Width+manifest.Height)
		}
	}
	assignment := hungarian(cost)
	for i := 0; i < n; i++ {
		truth := manifest.Regions[i]
		j := assignment[i]
		actual := components[j]
		dx := actual.CentroidX - truth.CentroidX
		dy := actual.CentroidY - truth.CentroidY
		delta := math.Hypot(dx, dy)
		areaRel := math.Abs(float64(actual.Area-truth.Area)) / math.Max(1, float64(truth.Area))
		holeDelta := abs(actual.HoleCount - truth.HoleCount)
		bboxDelta := abs(actual.BBox.X-truth.BBox.X) + abs(actual.BBox.Y-truth.BBox.Y) +
			abs(actual.BBox.W-truth.BBox.W) + abs(actual.BBox.H-truth.BBox.H)
		report.Regions = append(report.Regions, RegionDeviation{
			TruthID: truth.ID, MaskID: actual.ID, CentroidDX: dx, CentroidDY: dy,
			CentroidDelta: delta, AreaRelError: areaRel, HoleDelta: holeDelta, BBoxDeltaPixels: bboxDelta,
		})
		if delta > report.MaxCentroidDelta {
			report.MaxCentroidDelta = delta
		}
		if areaRel > report.MaxAreaRelError {
			report.MaxAreaRelError = areaRel
		}
		if holeDelta > report.MaxHoleDelta {
			report.MaxHoleDelta = holeDelta
		}
	}
	report.ReferenceMatch = report.MaskMismatchRatio <= manifest.MaskMismatchTolerance &&
		report.MaxHoleDelta <= manifest.HoleCountTolerance
	if !report.ReferenceMatch {
		briefs := make([]core.RegionBrief, 0, len(report.Regions))
		for _, r := range report.Regions {
			if r.HoleDelta > manifest.HoleCountTolerance {
				briefs = append(briefs, core.RegionBrief{ID: r.MaskID, Area: actualArea(components, r.MaskID), Reason: "hole_delta"})
			}
		}
		return report, core.RegionError("foreground mask exceeds reference tolerance", briefs)
	}
	return report, nil
}

func actualArea(components []Component, id int) int {
	for _, c := range components {
		if c.ID == id {
			return c.Area
		}
	}
	return 0
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func AssignMinimum(cost [][]float64) []int {
	return hungarian(cost)
}

// hungarian solves the square minimum-cost assignment problem.
func hungarian(cost [][]float64) []int {
	n := len(cost)
	if n == 0 {
		return nil
	}
	u := make([]float64, n+1)
	v := make([]float64, n+1)
	p := make([]int, n+1)
	way := make([]int, n+1)
	const inf = math.MaxFloat64 / 4
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]float64, n+1)
		used := make([]bool, n+1)
		for j := range minv {
			minv[j] = inf
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta := inf
			j1 := 0
			for j := 1; j <= n; j++ {
				if used[j] {
					continue
				}
				cur := cost[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= n; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
			if j0 == 0 {
				break
			}
		}
	}
	assignment := make([]int, n)
	for j := 1; j <= n; j++ {
		if p[j] != 0 {
			assignment[p[j]-1] = j - 1
		}
	}
	return assignment
}
