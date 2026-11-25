package verify

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"edgerefine/internal/model"
	"edgerefine/internal/state"
	"edgerefine/internal/store"
)

type fgDTO struct {
	Threshold model.ThresholdSnapshot `json:"threshold"`
	Pixels    []byte                  `json:"pixels"`
	Mask      []byte                  `json:"mask"`
}

type edgeDTO struct {
	CleanMask    []byte `json:"clean_mask"`
	SkeletonMask []byte `json:"skeleton_mask"`
	Radius       int    `json:"radius"`
}

type refineDTO struct {
	RefinedMask   []byte            `json:"refined_mask"`
	DeviationMap  []float64         `json:"deviation_map"`
	EdgePoints    []model.EdgePoint `json:"edge_points"`
	FinalResidual float64           `json:"final_residual"`
	Converged     bool              `json:"converged"`
	ActualBudget  int               `json:"actual_budget"`
}

type offset struct{ x, y int }

func disk(r int) []offset {
	if r <= 0 {
		return []offset{{0, 0}}
	}
	var out []offset
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			if x*x+y*y <= r*r+r {
				out = append(out, offset{x, y})
			}
		}
	}
	return out
}

func at(mask []byte, w, h, x, y int) byte {
	x = maxInt(0, minInt(w-1, x))
	y = maxInt(0, minInt(h-1, y))
	return mask[y*w+x]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func erode(mask []byte, w, h, r int) []byte {
	offs := disk(r)
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := byte(255)
			for _, o := range offs {
				if at(mask, w, h, x+o.x, y+o.y) == 0 {
					v = 0
					break
				}
			}
			out[y*w+x] = v
		}
	}
	return out
}

func dilate(mask []byte, w, h, r int) []byte {
	offs := disk(r)
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := byte(0)
			for _, o := range offs {
				if at(mask, w, h, x+o.x, y+o.y) != 0 {
					v = 255
					break
				}
			}
			out[y*w+x] = v
		}
	}
	return out
}

func openClose(mask []byte, w, h, r int) []byte {
	opened := dilate(erode(mask, w, h, r), w, h, r)
	return erode(dilate(opened, w, h, r), w, h, r)
}

func boundary(mask []byte, w, h int) []byte {
	er := erode(mask, w, h, 1)
	out := make([]byte, w*h)
	for i := range mask {
		if mask[i] != 0 && er[i] == 0 {
			out[i] = 255
		}
	}
	return out
}

func edgePadded(globalMask []byte, gw, gh, tx, ty, tile, radius int) ([]byte, []byte) {
	pad := radius
	if pad < 1 {
		pad = 1
	}
	x0, y0 := tx*tile, ty*tile
	pw, ph := tile+2*pad, tile+2*pad
	src := make([]byte, pw*ph)
	for y := 0; y < ph; y++ {
		gy := y0 - pad + y
		gy = maxInt(0, minInt(gh-1, gy))
		for x := 0; x < pw; x++ {
			gx := x0 - pad + x
			gx = maxInt(0, minInt(gw-1, gx))
			src[y*pw+x] = globalMask[gy*gw+gx]
		}
	}
	cleanPadded := openClose(src, pw, ph, radius)
	skelPadded := boundary(cleanPadded, pw, ph)
	clean := make([]byte, tile*tile)
	skel := make([]byte, tile*tile)
	for y := 0; y < tile; y++ {
		start := (y+pad)*pw + pad
		copy(clean[y*tile:(y+1)*tile], cleanPadded[start:start+tile])
		copy(skel[y*tile:(y+1)*tile], skelPadded[start:start+tile])
	}
	return clean, skel
}

type site struct {
	a, b     int
	target   float64
	position float64
}

func sites(pix, mask []byte, w, h int, threshold float64) []site {
	var out []site
	add := func(a, b int) {
		ga, gb := float64(pix[a]), float64(pix[b])
		t := 0.5
		if math.Abs(gb-ga) > 1e-9 {
			t = (threshold - ga) / (gb - ga)
			t = math.Max(0, math.Min(1, t))
		}
		out = append(out, site{a: a, b: b, target: t, position: .5})
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if x+1 < w {
				j := i + 1
				if mask[i] == 0 && mask[j] != 0 {
					add(i, j)
				} else if mask[i] != 0 && mask[j] == 0 {
					add(j, i)
				}
			}
			if y+1 < h {
				j := i + w
				if mask[i] == 0 && mask[j] != 0 {
					add(i, j)
				} else if mask[i] != 0 && mask[j] == 0 {
					add(j, i)
				}
			}
		}
	}
	return out
}

func raster(clean []byte, ss []site, w, h int) []byte {
	out := append([]byte(nil), clean...)
	bg := make([]float64, w*h)
	fg := make([]float64, w*h)
	for i := range fg {
		fg[i] = 1
	}
	for _, s := range ss {
		if s.position < .5 {
			c := .5 - s.position
			if c > bg[s.a] {
				bg[s.a] = c
			}
		}
		if s.position > .5 {
			c := 1.5 - s.position
			if c < fg[s.b] {
				fg[s.b] = c
			}
		}
	}
	for i := range out {
		if clean[i] == 0 {
			out[i] = byte(math.Round(255 * math.Max(0, math.Min(.5, bg[i]))))
		} else {
			out[i] = byte(math.Round(255 * math.Max(.5, math.Min(1, fg[i]))))
		}
	}
	return out
}

func refine(pix []byte, threshold float64, clean []byte, w, h, budget int, tol float64) ([]byte, []float64, []model.EdgePoint) {
	ss := sites(pix, clean, w, h, threshold)
	dev := make([]float64, w*h)
	for iter := 0; iter < maxInt(1, budget); iter++ {
		maxR := 0.0
		for i := range ss {
			ss[i].position += .5 * (ss[i].target - ss[i].position)
			r := math.Abs(ss[i].target - ss[i].position)
			if r > maxR {
				maxR = r
			}
		}
		if maxR <= tol {
			break
		}
	}
	for _, s := range ss {
		r := math.Abs(s.target - s.position)
		if r > dev[s.a] {
			dev[s.a] = r
		}
		if r > dev[s.b] {
			dev[s.b] = r
		}
	}
	mask := raster(clean, ss, w, h)
	points := extract(mask, w, h)
	return mask, dev, points
}

func extract(mask []byte, w, h int) []model.EdgePoint {
	var pts []model.EdgePoint
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if x+1 < w {
				j := i + 1
				v0, v1 := float64(mask[i]), float64(mask[j])
				if (v0 < 128 && v1 >= 128) || (v0 >= 128 && v1 < 128) {
					t := .5
					if math.Abs(v1-v0) > 1e-9 {
						t = math.Max(0, math.Min(1, (128-v0)/(v1-v0)))
					}
					pts = append(pts, model.EdgePoint{X: float64(x) + .5 + t, Y: float64(y) + .5, PixelX: x, PixelY: y, Axis: 0})
				}
			}
			if y+1 < h {
				j := i + w
				v0, v1 := float64(mask[i]), float64(mask[j])
				if (v0 < 128 && v1 >= 128) || (v0 >= 128 && v1 < 128) {
					t := .5
					if math.Abs(v1-v0) > 1e-9 {
						t = math.Max(0, math.Min(1, (128-v0)/(v1-v0)))
					}
					pts = append(pts, model.EdgePoint{X: float64(x) + .5, Y: float64(y) + .5 + t, PixelX: x, PixelY: y, Axis: 1})
				}
			}
		}
	}
	return pts
}

func med(xs []float64) float64 {
	if len(xs) == 0 {
		return 999
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 0 {
		return (cp[n/2-1] + cp[n/2]) / 2
	}
	return cp[n/2]
}

func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 999
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	pos := q * float64(len(cp)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return cp[lo]
	}
	return cp[lo] + (cp[hi]-cp[lo])*(pos-float64(lo))
}

func accuracy(points []model.EdgePoint, edges []model.TruthEdge, medTol, upperTol float64) *model.AccuracyReport {
	rep := &model.AccuracyReport{Pass: true}
	var all []float64
	var signedAll []float64
	for _, e := range edges {
		dx, dy := e.X2-e.X1, e.Y2-e.Y1
		l2 := dx*dx + dy*dy
		l := math.Sqrt(l2)
		wantAxis := -1
		if math.Abs(dy) > 2*math.Abs(dx) {
			wantAxis = 0
		} else if math.Abs(dx) > 2*math.Abs(dy) {
			wantAxis = 1
		}
		var ds, ss []float64
		st := model.EdgeStat{EdgeID: e.ID, Pass: true}
		for _, p := range points {
			if wantAxis >= 0 && p.Axis != wantAxis {
				continue
			}
			t := ((p.X-e.X1)*dx + (p.Y-e.Y1)*dy) / l2
			if t < 0 || t > 1 {
				continue
			}
			s := ((p.X-e.X1)*dy - (p.Y-e.Y1)*dx) / l
			if math.Abs(s) > 4.0 {
				continue
			}
			ds = append(ds, math.Abs(s))
			ss = append(ss, s)
			all = append(all, math.Abs(s))
			signedAll = append(signedAll, s)
		}
		st.Count = len(ds)
		st.Median = med(ds)
		st.SignedMedian = med(ss)
		st.Upper = quantile(ds, .95)
		for _, v := range ds {
			if v > st.Max {
				st.Max = v
			}
		}
		st.Pass = st.Count > 0 && st.Median <= medTol && st.Upper <= upperTol
		if !st.Pass {
			rep.Pass = false
			rep.OutOfTolerance = append(rep.OutOfTolerance, e.ID)
		}
		rep.Edges = append(rep.Edges, st)
	}
	rep.Median = med(all)
	rep.SignedMedian = med(signedAll)
	rep.Upper = quantile(all, .95)
	for _, v := range all {
		if v > rep.Max {
			rep.Max = v
		}
	}
	return rep
}

func violation(tile [2]int, stage state.Stage, field, expected, actual string) *model.VerificationReport {
	return &model.VerificationReport{
		OK: false, CheckedRows: 0,
		FirstViolation: &model.Violation{Tile: tile, Stage: string(stage), Field: field, Expected: expected, Actual: actual},
		Message:        fmt.Sprintf("first inconsistency at tile=%v stage=%s field=%s", tile, stage, field),
	}
}

func Verify(st *store.Store, truth model.Manifest, truthFP string) (*model.VerificationReport, error) {
	rows, err := st.LoadStages(truth.SampleID)
	if err != nil {
		return nil, err
	}
	rowMap := map[string]store.StoredStage{}
	for _, r := range rows {
		rowMap[fmt.Sprintf("%d,%d:%s", r.Tile.TX, r.Tile.TY, r.Stage)] = r
	}
	nx, ny := truth.Width/truth.Params.TileSize, truth.Height/truth.Params.TileSize
	checked := 0
	globalMask := make([]byte, truth.Width*truth.Height)
	globalFG := make([]byte, truth.Width*truth.Height)
	for ty := 0; ty < ny; ty++ {
		for tx := 0; tx < nx; tx++ {
			row, ok := rowMap[fmt.Sprintf("%d,%d:%s", tx, ty, state.StageForeground)]
			if !ok {
				continue
			}
			var fg fgDTO
			if json.Unmarshal(row.OutputJSON, &fg) != nil {
				continue
			}
			for y := 0; y < truth.Params.TileSize; y++ {
				dst := (ty*truth.Params.TileSize+y)*truth.Width + tx*truth.Params.TileSize
				copy(globalFG[dst:dst+truth.Params.TileSize], fg.Mask[y*truth.Params.TileSize:(y+1)*truth.Params.TileSize])
			}
		}
	}

	for ty := 0; ty < ny; ty++ {
		for tx := 0; tx < nx; tx++ {
			tile := [2]int{tx, ty}
			for _, stageName := range state.Stages() {
				key := fmt.Sprintf("%d,%d:%s", tx, ty, stageName)
				row, ok := rowMap[key]
				if !ok || row.Status != state.StatusCommitted {
					return violation(tile, stageName, "committed_row", "committed", "missing_or_uncommitted"), nil
				}
				checked++
				if row.AlgorithmVersion != model.AlgorithmVersion {
					return violation(tile, stageName, "algorithm_version", model.AlgorithmVersion, row.AlgorithmVersion), nil
				}
				if truthFP != "" && row.TruthFingerprint != truthFP {
					return violation(tile, stageName, "truth_fingerprint", truthFP, row.TruthFingerprint), nil
				}
				if h := model.HashBytes("stage-output-v1:", row.OutputJSON); h != row.ContentHash {
					return violation(tile, stageName, "content_hash", h[:16], row.ContentHash[:minInt(16, len(row.ContentHash))]), nil
				}

				if stageName == state.StageForeground {
					var fg fgDTO
					if err := json.Unmarshal(row.OutputJSON, &fg); err != nil {
						return violation(tile, stageName, "output_json", "valid fg dto", err.Error()), nil
					}
					w, h := truth.Params.TileSize, truth.Params.TileSize
					if len(fg.Pixels) != w*h || len(fg.Mask) != w*h {
						return violation(tile, stageName, "mask_or_pixels_length", fmt.Sprint(w*h), fmt.Sprintf("%d,%d", len(fg.Pixels), len(fg.Mask))), nil
					}
					edgeRow, ok := rowMap[fmt.Sprintf("%d,%d:%s", tx, ty, state.StageEdgeTrace)]
					if !ok {
						continue
					}
					var ed edgeDTO
					if err := json.Unmarshal(edgeRow.OutputJSON, &ed); err != nil {
						return violation(tile, state.StageEdgeTrace, "output_json", "valid edge dto", err.Error()), nil
					}
					radius := ed.Radius
					if radius <= 0 {
						radius = truth.Params.Radius
					}
					expectedClean, expectedSkeleton := edgePadded(globalFG, truth.Width, truth.Height, tx, ty, w, radius)
					if !bytesEqual(expectedClean, ed.CleanMask) {
						return violation(tile, state.StageEdgeTrace, "clean_mask", model.HashBytes("verify-clean:", expectedClean)[:16], model.HashBytes("stored-clean:", ed.CleanMask)[:16]), nil
					}
					if !bytesEqual(expectedSkeleton, ed.SkeletonMask) {
						return violation(tile, state.StageEdgeTrace, "skeleton_mask", "", ""), nil
					}

					refineRow, ok := rowMap[fmt.Sprintf("%d,%d:%s", tx, ty, state.StageRefine)]
					if !ok {
						continue
					}
					var rf refineDTO
					if err := json.Unmarshal(refineRow.OutputJSON, &rf); err != nil {
						return violation(tile, state.StageRefine, "output_json", "valid refine dto", err.Error()), nil
					}
					budget := refineRow.IterBudget
					if budget <= 0 {
						budget = truth.Params.IterBudget
					}
					mask, dev, _ := refine(fg.Pixels, fg.Threshold.Threshold, expectedClean, w, h, budget, truth.Params.ConvergeTolerance)
					if !bytesEqual(mask, rf.RefinedMask) {
						return violation(tile, state.StageRefine, "refined_mask", model.HashBytes("verify-mask:", mask)[:16], model.HashBytes("stored-mask:", rf.RefinedMask)[:16]), nil
					}
					if len(dev) != len(rf.DeviationMap) {
						return violation(tile, state.StageRefine, "deviation_map.length", fmt.Sprint(len(dev)), fmt.Sprint(len(rf.DeviationMap))), nil
					}
					for i := range dev {
						if math.Abs(dev[i]-rf.DeviationMap[i]) > 1e-12 {
							return violation(tile, state.StageRefine, fmt.Sprintf("deviation_map[%d]", i), fmt.Sprint(dev[i]), fmt.Sprint(rf.DeviationMap[i])), nil
						}
					}
					rectX, rectY := tx*w, ty*h
					for y := 0; y < h; y++ {
						copy(globalMask[(rectY+y)*truth.Width+rectX:(rectY+y)*truth.Width+rectX+w], rf.RefinedMask[y*w:(y+1)*w])
					}
				}
			}
		}
	}

	allPoints := extract(globalMask, truth.Width, truth.Height)
	acc := accuracyAssigned(allPoints, truth.Edges, truth.Params.EdgeTolerance, truth.Params.EdgeUpperBound)
	return &model.VerificationReport{
		OK: true, CheckedRows: checked, TruthFingerprint: truthFP,
		Message:  "all committed stage hashes and independently recomputed masks match",
		Accuracy: acc,
	}, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func accuracyAssigned(points []model.EdgePoint, edges []model.TruthEdge, medTol, upperTol float64) *model.AccuracyReport {
	type metric struct{ d, signed float64 }
	assigned := make([][]metric, len(edges))
	for _, p := range points {
		best := -1
		bestD := 4.0
		bestSigned := 0.0
		for i, e := range edges {
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
			if d := math.Abs(s); d < bestD {
				bestD, best, bestSigned = d, i, s
			}
		}
		if best >= 0 {
			assigned[best] = append(assigned[best], metric{d: bestD, signed: bestSigned})
		}
	}
	rep := &model.AccuracyReport{Pass: true}
	var all, signedAll []float64
	for i, e := range edges {
		var ds, ss []float64
		for _, m := range assigned[i] {
			ds = append(ds, m.d)
			ss = append(ss, m.signed)
			all = append(all, m.d)
			signedAll = append(signedAll, m.signed)
		}
		st := model.EdgeStat{EdgeID: e.ID, Count: len(ds)}
		st.Median = med(ds)
		st.P95 = quantile(ds, .95)
		st.SignedMedian = med(ss)
		for _, v := range ds {
			st.Max = math.Max(st.Max, v)
		}
		st.Upper = st.Max
		st.Pass = st.Count > 0 && st.Median <= medTol && st.Max <= upperTol
		if !st.Pass {
			rep.Pass = false
			rep.OutOfTolerance = append(rep.OutOfTolerance, e.ID)
		}
		rep.Edges = append(rep.Edges, st)
	}
	rep.Median = med(all)
	rep.P95 = quantile(all, .95)
	rep.SignedMedian = med(signedAll)
	for _, v := range all {
		rep.Max = math.Max(rep.Max, v)
	}
	rep.Upper = rep.Max
	return rep
}
