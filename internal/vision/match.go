package vision

import (
	"math"

	"maskpipe/internal/model"
)

type Match struct {
	PredictedID int     `json:"predicted_id"`
	TruthID     int     `json:"truth_id"`
	Centroid    float64 `json:"centroid_error_px"`
	AreaRel     float64 `json:"area_relative_error"`
	BBox        float64 `json:"bbox_error_px"`
	Cost        float64 `json:"cost"`
}

type RegionReport struct {
	PredictedCount int            `json:"predicted_count"`
	TruthCount     int            `json:"truth_count"`
	Matches        []Match        `json:"matches"`
	Extra          []model.Region `json:"extra_regions,omitempty"`
	Missing        []model.Region `json:"missing_regions,omitempty"`
	QualityErrors  []model.Error  `json:"quality_errors,omitempty"`
	OK             bool           `json:"ok"`
}

func CentroidDistance(a, b model.Point) float64 {
	return math.Hypot(a.X-b.X, a.Y-b.Y)
}

func AreaRelativeError(pred, truth int) float64 {
	if truth == 0 {
		if pred == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return math.Abs(float64(pred-truth)) / float64(truth)
}

func BBoxError(a, b model.BBox) float64 {
	dx := math.Max(math.Abs(float64(a.MinX-b.MinX)), math.Abs(float64(a.MaxX-b.MaxX)))
	dy := math.Max(math.Abs(float64(a.MinY-b.MinY)), math.Abs(float64(a.MaxY-b.MaxY)))
	return math.Max(dx, dy)
}

func matchingCost(pred, truth model.Region) float64 {
	c := CentroidDistance(pred.Centroid, truth.Centroid)
	a := AreaRelativeError(pred.Area, truth.Area) * 20
	b := BBoxError(pred.BBox, truth.BBox) * 0.5
	return c + a + b
}

// OptimalMatches solves the rectangular assignment problem with the Hungarian
// algorithm. It returns predicted/truth indexes, not stable API IDs.
func OptimalMatches(pred, truth []model.Region) [][2]int {
	if len(pred) == 0 || len(truth) == 0 {
		return nil
	}
	transposed := false
	rows, cols := pred, truth
	if len(rows) > len(cols) {
		rows, cols = cols, pred
		transposed = true
	}
	n, m := len(rows), len(cols)
	a := make([][]float64, n)
	for i := range rows {
		a[i] = make([]float64, m)
		for j := range cols {
			if transposed {
				a[i][j] = matchingCost(cols[j], rows[i])
			} else {
				a[i][j] = matchingCost(rows[i], cols[j])
			}
		}
	}

	u := make([]float64, n+1)
	v := make([]float64, m+1)
	p := make([]int, m+1)
	way := make([]int, m+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]float64, m+1)
		used := make([]bool, m+1)
		for j := range minv {
			minv[j] = math.Inf(1)
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta := math.Inf(1)
			j1 := 0
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				cur := a[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= m; j++ {
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

	var pairs [][2]int
	for j := 1; j <= m; j++ {
		if p[j] == 0 {
			continue
		}
		rowIdx := p[j] - 1
		colIdx := j - 1
		if transposed {
			pairs = append(pairs, [2]int{colIdx, rowIdx})
		} else {
			pairs = append(pairs, [2]int{rowIdx, colIdx})
		}
	}
	return pairs
}

func EvaluateRegions(pred, truth []model.Region, tol model.QualityTolerance) *RegionReport {
	report := &RegionReport{PredictedCount: len(pred), TruthCount: len(truth), OK: true}
	if len(pred) != len(truth) {
		matchedPred := map[int]bool{}
		matchedTruth := map[int]bool{}
		for _, pair := range OptimalMatches(pred, truth) {
			matchedPred[pair[0]] = true
			matchedTruth[pair[1]] = true
		}
		for i, r := range pred {
			if !matchedPred[i] {
				r.Extra = true
				report.Extra = append(report.Extra, r)
			}
		}
		for i, r := range truth {
			if !matchedTruth[i] {
				r.Missing = true
				report.Missing = append(report.Missing, r)
			}
		}
		report.OK = false
		details := map[string]any{
			"predicted_count": len(pred),
			"truth_count":     len(truth),
			"extra":           report.Extra,
			"missing":         report.Missing,
		}
		report.QualityErrors = append(report.QualityErrors, *model.WithDetails(
			model.NewError(model.CodeRegion, "regions", "region count mismatch"), details))
		return report
	}

	for _, pair := range OptimalMatches(pred, truth) {
		pr, tr := pred[pair[0]], truth[pair[1]]
		m := Match{
			PredictedID: pr.ID,
			TruthID:     tr.ID,
			Centroid:    CentroidDistance(pr.Centroid, tr.Centroid),
			AreaRel:     AreaRelativeError(pr.Area, tr.Area),
			BBox:        BBoxError(pr.BBox, tr.BBox),
		}
		m.Cost = m.Centroid + m.AreaRel*20 + m.BBox*0.5
		report.Matches = append(report.Matches, m)
		if m.Centroid > tol.CentroidPixels || m.AreaRel > tol.AreaRelative || m.BBox > tol.BBoxPixels {
			report.OK = false
			report.QualityErrors = append(report.QualityErrors, *model.WithDetails(
				model.NewError(model.CodeQual, "region", "region %d exceeds quality tolerance", pr.ID),
				map[string]any{
					"region_id":           pr.ID,
					"truth_id":            tr.ID,
					"centroid_error_px":   m.Centroid,
					"area_relative_error": m.AreaRel,
					"bbox_error_px":       m.BBox,
				}))
		}
	}
	return report
}
