package pipeline

import (
	"fmt"
	"math"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
	"edgerefine/internal/morph"
	"edgerefine/internal/quantile"
)

type ForegroundOutput struct {
	Threshold model.ThresholdSnapshot `json:"threshold"`
	Pixels    []byte                  `json:"pixels"`
	Mask      []byte                  `json:"mask"`
}

type EdgeOutput struct {
	CleanMask    []byte `json:"clean_mask"`
	SkeletonMask []byte `json:"skeleton_mask"`
	Radius       int    `json:"radius"`
}

type RefineOutput struct {
	RefinedMask   []byte                  `json:"refined_mask"`
	DeviationMap  []float64               `json:"deviation_map"`
	EdgePoints    []model.EdgePoint       `json:"edge_points"`
	Iterations    []model.IterationRecord `json:"iterations"`
	FinalResidual float64                 `json:"final_residual"`
	MeanResidual  float64                 `json:"mean_residual"`
	Converged     bool                    `json:"converged"`
	ActualBudget  int                     `json:"actual_budget"`
	ElementStep   int                     `json:"element_step"`
	DecisionBasis string                  `json:"decision_basis"`
}

func TileRect(p model.Params, tile model.TileCoord) model.Rect {
	s := p.TileSize
	return model.Rect{X: tile.TX * s, Y: tile.TY * s, W: s, H: s}
}

func TilePixels(im *model.GrayImage, r model.Rect) []uint8 {
	out := make([]uint8, r.W*r.H)
	for y := 0; y < r.H; y++ {
		srcStart := (r.Y+y)*im.Width + r.X
		copy(out[y*r.W:(y+1)*r.W], im.Pix[srcStart:srcStart+r.W])
	}
	return out
}

func EstimateThreshold(sampleID string, im *model.GrayImage, tile model.TileCoord, r model.Rect, p model.Params, manual *float64) (model.ThresholdSnapshot, error) {
	pix := TilePixels(im, r)
	hist, err := quantile.BuildHistogram(pix, p.BitDepth)
	if err != nil {
		return model.ThresholdSnapshot{}, err
	}
	est, err := quantile.Estimate(hist, p.BGQuantile, p.FGQuantile)
	if err != nil {
		return model.ThresholdSnapshot{}, err
	}
	separation := est.FGLevel - est.BGLevel
	homogeneous := separation < 48
	threshold := est.Threshold
	maxLevel := 255
	if p.BitDepth == 8 {
		maxLevel = 255
	}
	if homogeneous {
		medianLevel, _ := quantile.Level(hist, 0.5)
		if medianLevel >= maxLevel/2 {
			threshold = 0
		} else {
			threshold = float64(maxLevel)
		}
	}
	snap := model.ThresholdSnapshot{
		SampleID: sampleID, Tile: tile, BitDepth: p.BitDepth,
		HistHash: model.CanonicalHash(hist), BGQuantile: p.BGQuantile, FGQuantile: p.FGQuantile,
		BGLevel: est.BGLevel, FGLevel: est.FGLevel, Threshold: threshold,
		ClassSeparation: separation, Homogeneous: homogeneous,
		ParamVersion: p.ParamVersion,
	}
	if manual != nil {
		v := *manual
		if v < 0 || v > float64(maxLevel) {
			return model.ThresholdSnapshot{}, errs.Para("threshold", v, [2]any{0, maxLevel})
		}
		snap.Manual = &v
		snap.Threshold = v
		snap.Homogeneous = false
	}
	return snap, nil
}

func ForegroundStage(snap model.ThresholdSnapshot, im *model.GrayImage, r model.Rect) ForegroundOutput {
	pix := TilePixels(im, r)
	return ForegroundOutput{Threshold: snap, Pixels: append([]byte(nil), pix...), Mask: morph.Threshold(pix, snap.Threshold)}
}

func EdgeStage(mask []byte, w, h, radius int) EdgeOutput {
	clean := morph.OpenClose(mask, w, h, radius)
	skeleton := morph.InternalBoundary(clean, w, h)
	return EdgeOutput{CleanMask: clean, SkeletonMask: skeleton, Radius: radius}
}

func EdgeStagePadded(globalMask []byte, globalW, globalH int, r model.Rect, radius int) EdgeOutput {
	pad := radius
	if pad < 1 {
		pad = 1
	}
	pw, ph := r.W+2*pad, r.H+2*pad
	src := make([]byte, pw*ph)
	for y := 0; y < ph; y++ {
		gy := r.Y - pad + y
		if gy < 0 {
			gy = 0
		} else if gy >= globalH {
			gy = globalH - 1
		}
		for x := 0; x < pw; x++ {
			gx := r.X - pad + x
			if gx < 0 {
				gx = 0
			} else if gx >= globalW {
				gx = globalW - 1
			}
			src[y*pw+x] = globalMask[gy*globalW+gx]
		}
	}
	cleanPadded := morph.OpenClose(src, pw, ph, radius)
	skelPadded := morph.InternalBoundary(cleanPadded, pw, ph)
	clean := make([]byte, r.W*r.H)
	skeleton := make([]byte, r.W*r.H)
	for y := 0; y < r.H; y++ {
		srcStart := (y+pad)*pw + pad
		dstStart := y * r.W
		copy(clean[dstStart:dstStart+r.W], cleanPadded[srcStart:srcStart+r.W])
		copy(skeleton[dstStart:dstStart+r.W], skelPadded[srcStart:srcStart+r.W])
	}
	return EdgeOutput{CleanMask: clean, SkeletonMask: skeleton, Radius: radius}
}

type edgeSite struct {
	a, b     int
	stepX    int
	stepY    int
	target   float64
	position float64
}

func buildSites(pix []uint8, mask []byte, w, h int, threshold float64) []edgeSite {
	var sites []edgeSite
	add := func(a, b, sx, sy int) {
		ga := float64(pix[a])
		gb := float64(pix[b])
		t := 0.5
		if math.Abs(gb-ga) > 1e-9 {
			t = (threshold - ga) / (gb - ga)
			t = math.Max(0, math.Min(1, t))
		}
		sites = append(sites, edgeSite{a: a, b: b, stepX: sx, stepY: sy, target: t, position: 0.5})
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if x+1 < w {
				j := i + 1
				if mask[i] == 0 && mask[j] != 0 {
					add(i, j, 1, 0)
				} else if mask[i] != 0 && mask[j] == 0 {
					add(j, i, -1, 0)
				}
			}
			if y+1 < h {
				j := i + w
				if mask[i] == 0 && mask[j] != 0 {
					add(i, j, 0, 1)
				} else if mask[i] != 0 && mask[j] == 0 {
					add(j, i, 0, -1)
				}
			}
		}
	}
	return sites
}

func rasterizeSites(clean []byte, sites []edgeSite, w, h int) []byte {
	out := append([]byte(nil), clean...)
	bgBest := make([]float64, w*h)
	fgBest := make([]float64, w*h)
	for i := range fgBest {
		fgBest[i] = 1
	}
	for _, s := range sites {
		t := s.position
		if t < 0.5 {
			covA := 0.5 - t
			if covA > bgBest[s.a] {
				bgBest[s.a] = covA
			}
		}
		if t > 0.5 {
			covB := 1.5 - t
			if covB < fgBest[s.b] {
				fgBest[s.b] = covB
			}
		}
	}
	for i := range out {
		if clean[i] == 0 {
			out[i] = byte(math.Round(255 * math.Max(0, math.Min(0.5, bgBest[i]))))
		} else {
			out[i] = byte(math.Round(255 * math.Max(0.5, math.Min(1, fgBest[i]))))
		}
	}
	return out
}

func extractPoints(mask []byte, w, h int, r model.Rect) []model.EdgePoint {
	var points []model.EdgePoint
	add := func(i, x, y, axis int, t, grad float64) {
		var gx, gy float64
		if axis == 0 {
			gx = t - 0.5
		} else {
			gy = t - 0.5
		}
		px := r.X + x
		py := r.Y + y
		p := model.EdgePoint{PixelX: px, PixelY: py, Axis: axis, Gradient: grad}
		if axis == 0 {
			p.X = float64(px) + 0.5 + t
			p.Y = float64(py) + 0.5
			p.Signed = gx
		} else {
			p.X = float64(px) + 0.5
			p.Y = float64(py) + 0.5 + t
			p.Signed = gy
		}
		points = append(points, p)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if x+1 < w {
				j := i + 1
				v0, v1 := float64(mask[i]), float64(mask[j])
				if (v0 < 128 && v1 >= 128) || (v0 >= 128 && v1 < 128) {
					den := v1 - v0
					t := 0.5
					if math.Abs(den) > 1e-9 {
						t = (128 - v0) / den
						t = math.Max(0, math.Min(1, t))
					}
					add(i, x, y, 0, t, math.Abs(den))
				}
			}
			if y+1 < h {
				j := i + w
				v0, v1 := float64(mask[i]), float64(mask[j])
				if (v0 < 128 && v1 >= 128) || (v0 >= 128 && v1 < 128) {
					den := v1 - v0
					t := 0.5
					if math.Abs(den) > 1e-9 {
						t = (128 - v0) / den
						t = math.Max(0, math.Min(1, t))
					}
					add(i, x, y, 1, t, math.Abs(den))
				}
			}
		}
	}
	return points
}

func ExtractGlobalPoints(mask []byte, w, h int) []model.EdgePoint {
	return extractPoints(mask, w, h, model.Rect{X: 0, Y: 0, W: w, H: h})
}

func RefineStage(pix []uint8, r model.Rect, snap model.ThresholdSnapshot, edge EdgeOutput, budget, elementStep int, tolerance float64, check func() error) (RefineOutput, error) {
	w, h := r.W, r.H
	if budget < 1 {
		return RefineOutput{}, errs.Para("iter_budget", budget, [2]any{1, 64})
	}
	if elementStep < 1 {
		elementStep = 1
	}
	sites := buildSites(pix, edge.CleanMask, w, h, snap.Threshold)
	current := append([]byte(nil), edge.CleanMask...)
	records := make([]model.IterationRecord, 0, budget)
	dev := make([]float64, w*h)
	maxResidual := 0.0
	meanResidual := 0.0
	converged := len(sites) == 0
	used := 0
	for iter := 1; iter <= budget; iter++ {
		used = iter
		if check != nil {
			if err := check(); err != nil {
				out := RefineOutput{
					RefinedMask: current, DeviationMap: dev, EdgePoints: extractPoints(current, w, h, r),
					Iterations: records, FinalResidual: maxResidual, MeanResidual: meanResidual,
					Converged: false, ActualBudget: used, ElementStep: elementStep, DecisionBasis: "interrupted at iteration boundary",
				}
				return out, err
			}
		}
		var sum, maxR float64
		for si := range sites {
			s := &sites[si]
			s.position += 0.5 * (s.target - s.position)
			res := math.Abs(s.target - s.position)
			sum += res
			if res > maxR {
				maxR = res
			}
		}
		next := rasterizeSites(edge.CleanMask, sites, w, h)
		changed := 0
		for i := range next {
			if next[i] != current[i] {
				changed++
			}
		}
		current = next
		n := math.Max(1, float64(len(sites)))
		mean := sum / n
		rec := model.IterationRecord{Iteration: iter, MaxResidual: maxR, MeanResidual: mean, ChangedPixels: changed, Converged: maxR <= tolerance}
		records = append(records, rec)
		maxResidual, meanResidual = maxR, mean
		if maxR <= tolerance {
			converged = true
			break
		}
	}
	for _, s := range sites {
		res := math.Abs(s.target - s.position)
		if res > dev[s.a] {
			dev[s.a] = res
		}
		if res > dev[s.b] {
			dev[s.b] = res
		}
	}
	points := extractPoints(current, w, h, r)
	basis := "damped threshold-crossing interpolation; deterministic row-major sites"
	if elementStep > 1 {
		basis = fmt.Sprintf("coarse preview: iter_budget=%d, element_step=%d; %s", used, elementStep, basis)
	}
	out := RefineOutput{
		RefinedMask: current, DeviationMap: dev, EdgePoints: points, Iterations: records,
		FinalResidual: maxResidual, MeanResidual: meanResidual, Converged: converged, ActualBudget: used,
		ElementStep: elementStep, DecisionBasis: basis,
	}
	if !converged {
		return out, errs.Iter(model.TileCoord{TX: r.X / w, TY: r.Y / h}.Array(), maxResidual, tolerance)
	}
	return out, nil
}
