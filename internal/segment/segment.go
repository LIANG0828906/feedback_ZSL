package segment

import (
	"fmt"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/model"
)

type Options struct {
	Params            model.Params
	TruthLabels       []byte
	TruthAreas        map[int]int
	TruthRects        map[int]model.Rect
	IterationBudget   int
	MaxAreaRatio      float64
	OutsideTruthRatio float64
}

type Result struct {
	Labels       []byte
	GrowthLabels []byte
	Stats        model.Stats
	StatsHash    string
	MaskHash     string
	Truncated    bool
	Iterations   []int
	IterationCap int
}

func Run(im *model.Image, opt Options) (*Result, error) {
	if im == nil || im.Width <= 0 || im.Height <= 0 || im.Channels != 3 {
		return nil, apperr.New(apperr.CodeMeta, "segment", "invalid image dimensions or channels")
	}
	params, err := model.ValidateParams(opt.Params, im.Width, im.Height)
	if err != nil {
		return nil, err
	}
	if opt.MaxAreaRatio <= 0 {
		opt.MaxAreaRatio = model.DefaultSpillAreaRate
	}
	if opt.OutsideTruthRatio <= 0 {
		opt.OutsideTruthRatio = 0.25
	}
	feat := BuildFeatures(im)
	w, h := im.Width, im.Height
	seenTruth := make(map[int]bool, len(params.Seeds))
	growthCombined := make([]byte, w*h)
	cleanCombined := make([]byte, w*h)
	iterations := make([]int, 0, len(params.Seeds))
	truncated := false

	for regionIdx, seed := range params.Seeds {
		label := regionIdx + 1
		truthID := 0
		if len(opt.TruthLabels) == w*h {
			truthID = int(opt.TruthLabels[seed.Y*w+seed.X])
		}
		if truthID == 0 {
			return nil, apperr.New(apperr.CodeSeed, "segment", "seed is outside every declared foreground region").
				WithField(fmt.Sprintf("seeds[%d]", regionIdx)).
				WithDetail("seed", seed)
		}
		if seenTruth[truthID] {
			return nil, apperr.New(apperr.CodeSeed, "segment", "multiple seeds resolve to the same truth region").
				WithField(fmt.Sprintf("seeds[%d]", regionIdx)).
				WithDetail("seed", seed).
				WithDetail("truth_region", truthID)
		}
		seenTruth[truthID] = true

		seedColor := im.RGBAt(seed.X, seed.Y)
		candidate := thresholdCandidate(im, feat, seedColor, params.ColorTolerance, params.GradientThreshold)
		grown, used, cut := grow(candidate, w, h, seed, opt.IterationBudget)
		iterations = append(iterations, used)
		if cut {
			truncated = true
		}
		for i, v := range grown {
			if v {
				growthCombined[i] = byte(label)
			}
		}
		cleaned := OpenClose(grown, w, h, params.StructureElementSize)
		cleaned = largestComponentTouchingSeed(cleaned, w, h, seed.X, seed.Y)
		area := countTrue(cleaned)
		if area == 0 && cut {
			cleaned = largestComponentTouchingSeed(grown, w, h, seed.X, seed.Y)
			area = countTrue(cleaned)
		}
		if area == 0 {
			return nil, apperr.New(apperr.CodeSeed, "segment", "seed region disappeared after cleanup").
				WithField(fmt.Sprintf("seeds[%d]", regionIdx)).
				WithDetail("seed", seed)
		}
		if err := detectSpill(cleaned, opt.TruthLabels, w, h, truthID, area, opt, regionIdx, seed); err != nil {
			return nil, err
		}
		for i, v := range cleaned {
			if !v {
				continue
			}
			if cleanCombined[i] != 0 {
				return nil, apperr.New(apperr.CodeSpill, "segment", "cleaned regions overlap").
					WithDetail("region", label).
					WithDetail("first_pixel", indexPoint(i, w)).
					WithDetail("seed", seed)
			}
			cleanCombined[i] = byte(label)
		}
	}
	stats := model.StatsFromLabels(cleanCombined, w, h)
	for i := range stats.Regions {
		if i < len(params.Seeds) {
			stats.Regions[i].Seed = params.Seeds[i]
		}
	}
	return &Result{
		Labels:       cleanCombined,
		GrowthLabels: growthCombined,
		Stats:        stats,
		StatsHash:    model.StatsHash(stats),
		MaskHash:     model.HashBytes(cleanCombined),
		Truncated:    truncated,
		Iterations:   iterations,
		IterationCap: opt.IterationBudget,
	}, nil
}

func thresholdCandidate(im *model.Image, feat Features, seed model.RGB, colorTol, gradTol float64) []bool {
	w, h := im.Width, im.Height
	out := make([]bool, w*h)
	colorTol2 := colorTol*colorTol + 1e-9
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if feat.Grad[idx] > gradTol+1e-9 {
				continue
			}
			if colorDistanceSquared(im.RGBAt(x, y), seed) <= colorTol2 {
				out[idx] = true
			}
		}
	}
	return out
}

func grow(candidate []bool, width, height int, seed model.Point, budget int) ([]bool, int, bool) {
	out := make([]bool, len(candidate))
	seedIdx := seed.Y*width + seed.X
	if seedIdx < 0 || seedIdx >= len(candidate) {
		return out, 0, false
	}
	// The seed is always admitted so a noisy center pixel cannot collapse the region.
	candidate[seedIdx] = true
	queue := make([]int, 1, 4096)
	queue[0] = seedIdx
	out[seedIdx] = true
	used := 0
	for head := 0; head < len(queue); head++ {
		if budget > 0 && used >= budget {
			return out, used, true
		}
		idx := queue[head]
		used++
		x, y := idx%width, idx/width
		if y > 0 {
			n := idx - width
			if candidate[n] && !out[n] {
				out[n] = true
				queue = append(queue, n)
			}
		}
		if x > 0 {
			n := idx - 1
			if candidate[n] && !out[n] {
				out[n] = true
				queue = append(queue, n)
			}
		}
		if x+1 < width {
			n := idx + 1
			if candidate[n] && !out[n] {
				out[n] = true
				queue = append(queue, n)
			}
		}
		if y+1 < height {
			n := idx + width
			if candidate[n] && !out[n] {
				out[n] = true
				queue = append(queue, n)
			}
		}
	}
	return out, used, false
}

func detectSpill(mask []bool, truth []byte, width, height, truthID, area int, opt Options, regionIdx int, seed model.Point) error {
	if touchesBorder(mask, width, height) {
		rect := opt.TruthRects[truthID]
		touchesDeclaredBorder := rect.X <= 0 || rect.Y <= 0 || rect.X+rect.W >= width || rect.Y+rect.H >= height
		if !touchesDeclaredBorder {
			return spillError(regionIdx, seed, "region touches image border", area, opt.TruthAreas[truthID], firstBorderPixel(mask, width, height))
		}
	}
	if declaredArea := opt.TruthAreas[truthID]; declaredArea > 0 && float64(area) > float64(declaredArea)*opt.MaxAreaRatio {
		return spillError(regionIdx, seed, "region area exceeds declared spill limit", area, declaredArea, firstTrue(mask, width))
	}
	if len(truth) == width*height {
		outside := 0
		first := model.Point{}
		found := false
		label := byte(truthID)
		for i, v := range mask {
			if !v || truth[i] == label {
				continue
			}
			outside++
			if !found {
				first = indexPoint(i, width)
				found = true
			}
		}
		if area > 0 && float64(outside)/float64(area) > opt.OutsideTruthRatio {
			return spillError(regionIdx, seed, "region extends outside declared truth region", area, opt.TruthAreas[truthID], first)
		}
	}
	return nil
}

func spillError(regionIdx int, seed model.Point, message string, area, truthArea int, first model.Point) error {
	return apperr.New(apperr.CodeSpill, "segment", message).
		WithField(fmt.Sprintf("seeds[%d]", regionIdx)).
		WithDetail("seed", seed).
		WithDetail("area", area).
		WithDetail("truth_area", truthArea).
		WithDetail("first_pixel", first)
}

func touchesBorder(mask []bool, width, height int) bool {
	for x := 0; x < width; x++ {
		if mask[x] || mask[(height-1)*width+x] {
			return true
		}
	}
	for y := 0; y < height; y++ {
		if mask[y*width] || mask[y*width+width-1] {
			return true
		}
	}
	return false
}

func firstBorderPixel(mask []bool, width, height int) model.Point {
	for x := 0; x < width; x++ {
		if mask[x] {
			return model.Point{X: x, Y: 0}
		}
	}
	for x := 0; x < width; x++ {
		if mask[(height-1)*width+x] {
			return model.Point{X: x, Y: height - 1}
		}
	}
	for y := 0; y < height; y++ {
		if mask[y*width] {
			return model.Point{X: 0, Y: y}
		}
		if mask[y*width+width-1] {
			return model.Point{X: width - 1, Y: y}
		}
	}
	return model.Point{}
}

func firstTrue(mask []bool, width int) model.Point {
	for i, v := range mask {
		if v {
			return indexPoint(i, width)
		}
	}
	return model.Point{}
}

func indexPoint(idx, width int) model.Point {
	return model.Point{X: idx % width, Y: idx / width}
}

func countTrue(mask []bool) int {
	n := 0
	for _, v := range mask {
		if v {
			n++
		}
	}
	return n
}
