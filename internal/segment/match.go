package segment

import (
	"fmt"
	"math"
	"sort"

	"maskpreview/internal/core"
)

type RegionDelta struct {
	RegionID          int           `json:"region_id"`
	TruthID           int           `json:"truth_id"`
	PredictedArea     int           `json:"predicted_area"`
	TruthArea         int           `json:"truth_area"`
	AreaRelative      float64       `json:"area_relative"`
	CentroidDeviation float64       `json:"centroid_deviation"`
	PredictedBBox     core.BBox     `json:"predicted_bbox"`
	TruthBBox         core.BBox     `json:"truth_bbox"`
	BBoxDeviation     BBoxDeviation `json:"bbox_deviation"`
	BBoxIoU           float64       `json:"bbox_iou"`
}

type BBoxDeviation struct {
	DX0 int `json:"dx0"`
	DY0 int `json:"dy0"`
	DX1 int `json:"dx1"`
	DY1 int `json:"dy1"`
}

type MatchReport struct {
	Matches              []RegionDelta `json:"matches"`
	MaxCentroidDeviation float64       `json:"max_centroid_deviation"`
	MaxAreaRelative      float64       `json:"max_area_relative"`
	WithinTolerance      bool          `json:"within_tolerance"`
}

type RegionBrief struct {
	ID   int `json:"id"`
	Area int `json:"area"`
}

func CompareToTruth(predicted []RegionStat, truth []core.TruthRegion, tol core.PreviewTolerance) (*MatchReport, error) {
	if len(predicted) != len(truth) {
		return nil, regionCountError(predicted, truth)
	}
	costs := make([][]float64, len(predicted))
	for i, p := range predicted {
		costs[i] = make([]float64, len(truth))
		for j, t := range truth {
			centroid := math.Hypot(p.Centroid.X-t.Centroid.X, p.Centroid.Y-t.Centroid.Y)
			area := relativeError(p.Area, t.Area)
			costs[i][j] = centroid + 50*area
		}
	}
	assign := hungarian(costs)
	report := &MatchReport{Matches: make([]RegionDelta, 0, len(predicted)), WithinTolerance: true}
	for i, j := range assign {
		p, t := predicted[i], truth[j]
		cd := math.Hypot(p.Centroid.X-t.Centroid.X, p.Centroid.Y-t.Centroid.Y)
		ar := relativeError(p.Area, t.Area)
		d := RegionDelta{
			RegionID:          p.ID,
			TruthID:           t.ID,
			PredictedArea:     p.Area,
			TruthArea:         t.Area,
			AreaRelative:      ar,
			CentroidDeviation: cd,
			PredictedBBox:     p.BBox,
			TruthBBox:         t.BBox,
			BBoxDeviation:     bboxDeviation(p.BBox, t.BBox),
			BBoxIoU:           bboxIoU(p.BBox, t.BBox),
		}
		report.Matches = append(report.Matches, d)
		if cd > report.MaxCentroidDeviation {
			report.MaxCentroidDeviation = cd
		}
		if ar > report.MaxAreaRelative {
			report.MaxAreaRelative = ar
		}
	}
	sort.Slice(report.Matches, func(i, j int) bool {
		return report.Matches[i].RegionID < report.Matches[j].RegionID
	})
	report.WithinTolerance = report.MaxCentroidDeviation <= tol.CentroidPx && report.MaxAreaRelative <= tol.AreaRelative
	return report, nil
}

func MatchRegions(predicted, reference []RegionStat) []RegionDelta {
	if len(predicted) == 0 || len(reference) == 0 {
		return nil
	}
	n := max(len(predicted), len(reference))
	const dummyCost = 1e9
	costs := make([][]float64, n)
	for i := 0; i < n; i++ {
		costs[i] = make([]float64, n)
		for j := 0; j < n; j++ {
			if i >= len(predicted) || j >= len(reference) {
				costs[i][j] = dummyCost
				continue
			}
			p, r := predicted[i], reference[j]
			c := math.Hypot(p.Centroid.X-r.Centroid.X, p.Centroid.Y-r.Centroid.Y)
			c += 50 * relativeError(p.Area, r.Area)
			costs[i][j] = c
		}
	}
	assign := hungarian(costs)
	out := make([]RegionDelta, 0, len(predicted))
	for i, j := range assign {
		if i >= len(predicted) || j >= len(reference) {
			continue
		}
		p, r := predicted[i], reference[j]
		out = append(out, RegionDelta{
			RegionID:          p.ID,
			TruthID:           r.ID,
			PredictedArea:     p.Area,
			TruthArea:         r.Area,
			AreaRelative:      relativeError(p.Area, r.Area),
			CentroidDeviation: math.Hypot(p.Centroid.X-r.Centroid.X, p.Centroid.Y-r.Centroid.Y),
			PredictedBBox:     p.BBox,
			TruthBBox:         r.BBox,
			BBoxDeviation:     bboxDeviation(p.BBox, r.BBox),
			BBoxIoU:           bboxIoU(p.BBox, r.BBox),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RegionID < out[j].RegionID })
	return out
}

func bboxDeviation(a, b core.BBox) BBoxDeviation {
	return BBoxDeviation{
		DX0: a.X0 - b.X0,
		DY0: a.Y0 - b.Y0,
		DX1: a.X1 - b.X1,
		DY1: a.Y1 - b.Y1,
	}
}

func regionCountError(predicted []RegionStat, truth []core.TruthRegion) error {
	predByID := make(map[int]int, len(predicted))
	for _, p := range predicted {
		predByID[p.ID] = p.Area
	}
	truthByID := make(map[int]int, len(truth))
	for _, t := range truth {
		truthByID[t.ID] = t.Area
	}
	extra := make([]RegionBrief, 0)
	missing := make([]RegionBrief, 0)
	if len(predicted) > len(truth) {
		matched := make(map[int]bool)
		for _, d := range MatchRegions(predicted, truthToStats(truth)) {
			matched[d.RegionID] = true
		}
		for _, p := range predicted {
			if !matched[p.ID] {
				extra = append(extra, RegionBrief{ID: p.ID, Area: p.Area})
			}
		}
	} else if len(truth) > len(predicted) {
		matched := make(map[int]bool)
		for _, d := range MatchRegions(predicted, truthToStats(truth)) {
			matched[d.TruthID] = true
		}
		for _, t := range truth {
			if !matched[t.ID] {
				missing = append(missing, RegionBrief{ID: t.ID, Area: t.Area})
			}
		}
	}
	return &core.Error{
		Code:    core.CodeRegion,
		Op:      "segment.compare_truth",
		Field:   "region_count",
		Got:     len(predicted),
		Want:    len(truth),
		Message: fmt.Sprintf("extra=%v missing=%v", extra, missing),
		Data: map[string]any{
			"extra_regions":   extra,
			"missing_regions": missing,
			"predicted":       len(predicted),
			"truth":           len(truth),
		},
	}
}

func truthToStats(truth []core.TruthRegion) []RegionStat {
	out := make([]RegionStat, 0, len(truth))
	for _, t := range truth {
		out = append(out, RegionStat{ID: t.ID, Area: t.Area, Centroid: t.Centroid, BBox: t.BBox})
	}
	return out
}

func relativeError(a, b int) float64 {
	if b == 0 {
		if a == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return math.Abs(float64(a-b)) / float64(b)
}

func bboxIoU(a, b core.BBox) float64 {
	x0 := max(a.X0, b.X0)
	y0 := max(a.Y0, b.Y0)
	x1 := min(a.X1, b.X1)
	y1 := min(a.Y1, b.Y1)
	if x1 < x0 || y1 < y0 {
		return 0
	}
	inter := float64((x1 - x0 + 1) * (y1 - y0 + 1))
	areaA := float64(a.Width() * a.Height())
	areaB := float64(b.Width() * b.Height())
	return inter / (areaA + areaB - inter)
}

// hungarian computes a minimum-cost assignment for a square cost matrix.
func hungarian(cost [][]float64) []int {
	n := len(cost)
	if n == 0 {
		return nil
	}
	u := make([]float64, n+1)
	v := make([]float64, n+1)
	p := make([]int, n+1)
	way := make([]int, n+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]float64, n+1)
		used := make([]bool, n+1)
		for j := range minv {
			minv[j] = math.Inf(1)
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta := math.Inf(1)
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
	assign := make([]int, n)
	for j := 1; j <= n; j++ {
		if p[j] != 0 {
			assign[p[j]-1] = j - 1
		}
	}
	return assign
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
