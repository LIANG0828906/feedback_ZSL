package imaging

import (
	"math"
	"sort"

	"iconbench/internal/model"
)

type RegionDeviation struct {
	TruthID           string  `json:"truth_id"`
	PredictedID       string  `json:"predicted_id"`
	CentroidDX        float64 `json:"centroid_dx"`
	CentroidDY        float64 `json:"centroid_dy"`
	CentroidDistance  float64 `json:"centroid_distance"`
	TruthArea         int     `json:"truth_area"`
	PredictedArea     int     `json:"predicted_area"`
	AreaRelativeError float64 `json:"area_relative_error"`
	TruthHoles        int     `json:"truth_holes"`
	PredictedHoles    int     `json:"predicted_holes"`
	HoleDifference    int     `json:"hole_difference"`
	IoU               float64 `json:"iou"`
}

type RegionReport struct {
	ImageID              string            `json:"image_id"`
	RegionCountMatch     bool              `json:"region_count_match"`
	Deviations           []RegionDeviation `json:"deviations"`
	Extra                []model.RegionRef `json:"extra"`
	Missing              []model.RegionRef `json:"missing"`
	MaxCentroidErrorPx   float64           `json:"max_centroid_error_px"`
	MaxAreaRelativeError float64           `json:"max_area_relative_error"`
	MaxHoleDifference    int               `json:"max_hole_difference"`
	WithinTolerance      bool              `json:"within_tolerance"`
	StateError           *model.Error      `json:"error,omitempty"`
}

func ValidateDegenerate(stats []model.RegionStat) error {
	for _, st := range stats {
		if st.Area == 0 || st.BBox.Width <= 0 || st.BBox.Height <= 0 {
			return &model.Error{
				Code:          model.CodeDegen,
				Message:       "connected region has zero area or degenerate bounding box",
				FirstRegionID: st.ID,
				Field:         "area/bbox",
				Actual:        map[string]any{"area": st.Area, "width": st.BBox.Width, "height": st.BBox.Height},
			}
		}
	}
	return nil
}

func MatchRegions(imageID string, predStats, truthStats []model.RegionStat, predLabels, truthLabels []int32, w, h int, tol model.Tolerances) *RegionReport {
	report := &RegionReport{ImageID: imageID, RegionCountMatch: len(predStats) == len(truthStats), WithinTolerance: true}
	if err := ValidateDegenerate(predStats); err != nil {
		report.StateError = err.(*model.Error)
		report.WithinTolerance = false
		return report
	}

	type pair struct {
		pi, ti  int
		overlap int
		iou     float64
	}
	var pairs []pair
	for pi, ps := range predStats {
		for ti, ts := range truthStats {
			var inter int
			for i := 0; i < w*h; i++ {
				if predLabels[i] == int32(pi+1) && truthLabels[i] == int32(ti+1) {
					inter++
				}
			}
			if inter == 0 {
				continue
			}
			union := ps.Area + ts.Area - inter
			pairs = append(pairs, pair{pi: pi, ti: ti, overlap: inter, iou: float64(inter) / float64(union)})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].overlap != pairs[j].overlap {
			return pairs[i].overlap > pairs[j].overlap
		}
		if pairs[i].iou != pairs[j].iou {
			return pairs[i].iou > pairs[j].iou
		}
		if pairs[i].pi != pairs[j].pi {
			return pairs[i].pi < pairs[j].pi
		}
		return pairs[i].ti < pairs[j].ti
	})

	usedP := map[int]bool{}
	usedT := map[int]bool{}
	for _, candidate := range pairs {
		if usedP[candidate.pi] || usedT[candidate.ti] {
			continue
		}
		usedP[candidate.pi] = true
		usedT[candidate.ti] = true
		ps := predStats[candidate.pi]
		ts := truthStats[candidate.ti]
		areaRel := math.Abs(float64(ps.Area-ts.Area)) / math.Max(1, float64(ts.Area))
		dx := ps.CentroidX - ts.CentroidX
		dy := ps.CentroidY - ts.CentroidY
		dist := math.Hypot(dx, dy)
		holeDiff := ps.Holes - ts.Holes
		dev := RegionDeviation{
			TruthID: ts.ID, PredictedID: ps.ID,
			CentroidDX: dx, CentroidDY: dy, CentroidDistance: dist,
			TruthArea: ts.Area, PredictedArea: ps.Area, AreaRelativeError: areaRel,
			TruthHoles: ts.Holes, PredictedHoles: ps.Holes, HoleDifference: holeDiff,
			IoU: candidate.iou,
		}
		report.Deviations = append(report.Deviations, dev)
		report.MaxCentroidErrorPx = math.Max(report.MaxCentroidErrorPx, dist)
		report.MaxAreaRelativeError = math.Max(report.MaxAreaRelativeError, areaRel)
		report.MaxHoleDifference = maxIntAbs(report.MaxHoleDifference, holeDiff)
		if dist > tol.CentroidPx || areaRel > tol.AreaRel || absInt(holeDiff) > tol.HoleCount || candidate.iou < tol.MaskIoUMin {
			report.WithinTolerance = false
		}
	}

	for pi, ps := range predStats {
		if !usedP[pi] {
			report.Extra = append(report.Extra, model.RegionRef{ID: ps.ID, Area: ps.Area})
		}
	}
	for ti, ts := range truthStats {
		if !usedT[ti] {
			report.Missing = append(report.Missing, model.RegionRef{ID: ts.ID, Area: ts.Area})
		}
	}
	if len(report.Extra) > 0 || len(report.Missing) > 0 || !report.RegionCountMatch {
		report.WithinTolerance = false
		report.StateError = &model.Error{
			Code:     model.CodeRegion,
			Message:  "predicted region set does not match truth",
			Extra:    report.Extra,
			Missing:  report.Missing,
			Expected: len(truthStats),
			Actual:   len(predStats),
		}
	}
	return report
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxIntAbs(cur, v int) int {
	a := absInt(v)
	if a > cur {
		return a
	}
	return cur
}
