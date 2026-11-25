package edgeeval

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
)

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.Inf(1)
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

func quantileSorted(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 999
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

func Evaluate(points []model.EdgePoint, truth []model.TruthEdge, medianTol, upperTol float64) model.AccuracyReport {
	rep := model.AccuracyReport{Pass: true}
	type metric struct {
		d, signed float64
		p         model.EdgePoint
		tp        model.Point
	}
	assigned := make([][]metric, len(truth))

	for _, p := range points {
		bestEdge := -1
		bestD := 4.0
		var bestSigned, bestT float64
		var bestTP model.Point
		for ei, e := range truth {
			dx, dy := e.X2-e.X1, e.Y2-e.Y1
			l2 := dx*dx + dy*dy
			l := math.Sqrt(l2)
			wantAxis := -1
			if math.Abs(dy) > 2*math.Abs(dx) {
				wantAxis = 0
			} else if math.Abs(dx) > 2*math.Abs(dy) {
				wantAxis = 1
			}
			if wantAxis >= 0 && p.Axis != wantAxis {
				continue
			}
			t := ((p.X-e.X1)*dx + (p.Y-e.Y1)*dy) / l2
			if t < 0 || t > 1 {
				continue
			}
			s := ((p.X-e.X1)*dy - (p.Y-e.Y1)*dx) / l
			d := math.Abs(s)
			if d < bestD {
				bestD = d
				bestEdge = ei
				bestSigned = s
				bestT = t
				bestTP = model.Point{X: e.X1 + t*dx, Y: e.Y1 + t*dy}
			}
		}
		if bestEdge >= 0 {
			_ = bestT
			assigned[bestEdge] = append(assigned[bestEdge], metric{d: bestD, signed: bestSigned, p: p, tp: bestTP})
		}
	}

	var allAbs, allSigned []float64
	for _, e := range truth {
		var abs, signed []float64
		stat := model.EdgeStat{EdgeID: e.ID}
		worst := -1.0
		var worstP model.EdgePoint
		var worstT model.Point
		for _, m := range assigned[len(rep.Edges)] {
			abs = append(abs, m.d)
			signed = append(signed, m.signed)
			allAbs = append(allAbs, m.d)
			allSigned = append(allSigned, m.signed)
			if m.d > worst {
				worst = m.d
				worstP = m.p
				worstT = m.tp
			}
		}
		if len(abs) == 0 {
			stat.Count = 0
			stat.Median = 999
			stat.Upper = 999
			stat.SignedMedian = 999
			stat.Pass = false
			stat.TruthPoint = model.Point{X: (e.X1 + e.X2) / 2, Y: (e.Y1 + e.Y2) / 2}
			stat.ObservedPoint = stat.TruthPoint
		} else {
			sortedAbs := append([]float64(nil), abs...)
			sort.Float64s(sortedAbs)
			md := median(abs)
			sm := median(signed)
			up := quantileSorted(sortedAbs, 0.95)
			maxV := 0.0
			for _, v := range abs {
				if v > maxV {
					maxV = v
				}
			}
			stat.Count = len(abs)
			stat.Median = md
			stat.P95 = up
			stat.Upper = maxV
			stat.Max = maxV
			stat.SignedMedian = sm
			stat.Pass = md <= medianTol && maxV <= upperTol
			stat.ObservedPoint = model.Point{X: worstP.X, Y: worstP.Y}
			stat.TruthPoint = worstT
		}
		if !stat.Pass {
			rep.Pass = false
			rep.OutOfTolerance = append(rep.OutOfTolerance, e.ID)
		}
		rep.Edges = append(rep.Edges, stat)
	}
	rep.Median = median(allAbs)
	rep.SignedMedian = median(allSigned)
	sortAll := append([]float64(nil), allAbs...)
	sort.Float64s(sortAll)
	rep.P95 = quantileSorted(sortAll, 0.95)
	rep.Max = 0
	for _, v := range allAbs {
		if v > rep.Max {
			rep.Max = v
		}
	}
	rep.Upper = rep.Max
	if math.IsInf(rep.Median, 1) {
		rep.Pass = false
	}
	return rep
}

func AsEdgeError(rep model.AccuracyReport) error {
	if rep.Pass {
		return nil
	}
	first := model.EdgeStat{}
	for _, st := range rep.Edges {
		if !st.Pass {
			first = st
			break
		}
	}
	ids := fmt.Sprint(rep.OutOfTolerance)
	coords := make([]string, 0, len(rep.OutOfTolerance))
	for _, st := range rep.Edges {
		if !st.Pass {
			coords = append(coords, fmt.Sprintf("edge=%d observed=(%.3f,%.3f) truth=(%.3f,%.3f)",
				st.EdgeID, st.ObservedPoint.X, st.ObservedPoint.Y, st.TruthPoint.X, st.TruthPoint.Y))
		}
	}
	return &errs.Error{
		Code:    errs.EEdge,
		Message: fmt.Sprintf("edge localization exceeds tolerance; edge_ids=%s median=%.4f upper=%.4f; %s", ids, rep.Median, rep.Upper, strings.Join(coords, "; ")),
		Violation: errs.Violation{
			EdgeID:     first.EdgeID,
			LeftCoord:  []float64{first.ObservedPoint.X, first.ObservedPoint.Y},
			RightCoord: []float64{first.TruthPoint.X, first.TruthPoint.Y},
			Measured:   []float64{first.Median, first.Upper},
			LegalRange: [2]any{"median<=", "upper-bound"},
		},
	}
}
