package ocrflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"sort"
)

type Component struct {
	ID                 int  `json:"id"`
	Rect               Rect `json:"rect"`
	PixelCount         int  `json:"pixel_count"`
	StrokeWidthPx      int  `json:"stroke_width_px"`
	StrokeWidthMilliPx int  `json:"stroke_width_milli_px"`
	ConfidenceBPS      int  `json:"confidence_bps"`
	GradientBPS        int  `json:"gradient_bps"`
}

type DetectedLine struct {
	LineNo             int    `json:"line_no"`
	LineID             string `json:"line_id"`
	Box                Rect   `json:"box"`
	Quad               Quad   `json:"quad"`
	ComponentIDs       []int  `json:"component_ids"`
	StrokeWidthPx      int    `json:"stroke_width_px"`
	StrokeWidthMilliPx int    `json:"stroke_width_milli_px"`
	ConfidenceBPS      int    `json:"confidence_bps"`
}

type DetectionResult struct {
	ShotID           string         `json:"shot_id"`
	ContentHash      string         `json:"content_hash"`
	RecipeVersion    string         `json:"recipe_version"`
	ParamVersion     string         `json:"param_version"`
	AlgorithmVersion string         `json:"algorithm_version"`
	Lines            []DetectedLine `json:"lines"`
	Components       []Component    `json:"components"`
	Mask             *Mask          `json:"-"`
	BoxSummary       string         `json:"box_summary"`
}

// DetectionLineCountTolerance is the declared acceptance tolerance versus
// generator truth line count. The deterministic sample currently has delta 0.
const DetectionLineCountTolerance = 1

type PreparedImage struct {
	Gray     *GrayImage
	Mask     *Mask
	Gradient *Mask
}

func PreprocessImage(img *image.RGBA, cfg PreprocessConfig) (*PreparedImage, error) {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	gray := NewGrayImage(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, _ := img.At(x+bounds.Min.X, y+bounds.Min.Y).RGBA()
			r8, g8, b8 := byte(r>>8), byte(g>>8), byte(b>>8)
			lum := (299*int(r8) + 587*int(g8) + 114*int(b8)) / 1000
			gray.Pix[y*w+x] = byte(clampInt(lum, 0, 255))
		}
	}

	hist := [256]int{}
	for _, v := range gray.Pix {
		hist[v]++
	}
	total := w * h
	lowCount := total * cfg.NormalizeLowBPS / 10000
	highCount := total * cfg.NormalizeHighBPS / 10000
	low, high, cum := 0, 255, 0
	for i, n := range hist {
		cum += n
		if cum >= lowCount {
			low = i
			break
		}
	}
	cum = 0
	for i, n := range hist {
		cum += n
		if cum >= highCount {
			high = i
			break
		}
	}
	if high <= low {
		low, high = 0, 255
	}
	span := high - low
	for i, v := range gray.Pix {
		nv := (int(v) - low) * 255 / span
		gray.Pix[i] = byte(clampInt(nv, 0, 255))
	}

	gradient := NewMask(w, h)
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			gx := absInt(int(gray.At(x+1, y)) - int(gray.At(x-1, y)))
			gy := absInt(int(gray.At(x, y+1)) - int(gray.At(x, y-1)))
			if gx+gy >= 55 {
				gradient.Set(x, y, 1)
			}
		}
	}

	thresholdBPS := 5000
	if cfg.BinarizationEnabled {
		thresholdBPS = cfg.BinarizationThreshBPS
	}
	cut := 255 * thresholdBPS / 10000
	mask := NewMask(w, h)
	for i, v := range gray.Pix {
		if int(v) >= cut {
			mask.Pix[i] = 1
		}
	}
	return &PreparedImage{Gray: gray, Mask: mask, Gradient: gradient}, nil
}

func DetectLines(shot *Screenshot, recipe *Recipe, prepared *PreparedImage) (*DetectionResult, error) {
	if err := recipe.ValidateRuntimeParameters(); err != nil {
		return nil, err
	}
	if prepared == nil || prepared.Mask == nil {
		return nil, fmt.Errorf("prepared image is required")
	}
	components := extractComponents(prepared, recipe.Detection)
	lines := mergeComponents(components, recipe.Detection)
	for i := range lines {
		lines[i].LineNo = i + 1
		lines[i].LineID = fmt.Sprintf("L%02d", i+1)
		lines[i].Quad = RectQuad(lines[i].Box)
	}
	summary := BoxSummary(lines)
	return &DetectionResult{
		ShotID:           shot.ShotID,
		ContentHash:      shot.ContentHash,
		RecipeVersion:    recipe.RecipeVersion,
		ParamVersion:     recipe.ParamVersion,
		AlgorithmVersion: RecipeAlgorithmVersion,
		Lines:            lines,
		Components:       components,
		Mask:             prepared.Mask,
		BoxSummary:       summary,
	}, nil
}

func RectQuad(r Rect) Quad {
	return Quad{
		{X: r.MinX, Y: r.MinY},
		{X: r.MaxX - 1, Y: r.MinY},
		{X: r.MaxX - 1, Y: r.MaxY - 1},
		{X: r.MinX, Y: r.MaxY - 1},
	}
}

func BoxSummary(lines []DetectedLine) string {
	h := sha256.New()
	for _, line := range lines {
		_, _ = fmt.Fprintf(h, "%d:%d,%d,%d,%d:%d;", line.LineNo, line.Box.MinX, line.Box.MinY, line.Box.MaxX, line.Box.MaxY, line.ConfidenceBPS)
	}
	return "boxsha256:" + hex.EncodeToString(h.Sum(nil))
}

func extractComponents(prep *PreparedImage, cfg DetectConfig) []Component {
	w, h := prep.Mask.Width, prep.Mask.Height
	visited := make([]bool, w*h)
	components := make([]Component, 0, 64)
	nextID := 1

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if prep.Mask.Pix[idx] == 0 || visited[idx] {
				continue
			}
			pixels := make([]Point, 0, 128)
			stack := []Point{{X: x, Y: y}}
			visited[idx] = true
			minX, minY, maxX, maxY := x, y, x, y
			for len(stack) > 0 {
				p := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				pixels = append(pixels, p)
				if p.X < minX {
					minX = p.X
				}
				if p.X > maxX {
					maxX = p.X
				}
				if p.Y < minY {
					minY = p.Y
				}
				if p.Y > maxY {
					maxY = p.Y
				}
				for _, d := range []Point{{1, 0}, {-1, 0}, {0, 1}, {0, -1}, {1, 1}, {1, -1}, {-1, 1}, {-1, -1}} {
					nx, ny := p.X+d.X, p.Y+d.Y
					if nx < 0 || ny < 0 || nx >= w || ny >= h {
						continue
					}
					ni := ny*w + nx
					if visited[ni] || prep.Mask.Pix[ni] == 0 {
						continue
					}
					visited[ni] = true
					stack = append(stack, Point{X: nx, Y: ny})
				}
			}
			rect := Rect{MinX: minX, MinY: minY, MaxX: maxX + 1, MaxY: maxY + 1}
			sw := estimateStrokeWidth(prep.Mask, rect, cfg)
			gradientBPS := componentGradientScore(prep.Gradient, rect)
			comp := Component{
				ID:                 nextID,
				Rect:               rect,
				PixelCount:         len(pixels),
				StrokeWidthPx:      sw,
				StrokeWidthMilliPx: sw * 1000,
				GradientBPS:        gradientBPS,
				ConfidenceBPS:      componentConfidence(rect, len(pixels), sw, gradientBPS),
			}
			nextID++
			if componentLooksLikeGlyph(comp, cfg) {
				components = append(components, comp)
			}
		}
	}
	sort.Slice(components, func(i, j int) bool {
		if components[i].Rect.MinY != components[j].Rect.MinY {
			return components[i].Rect.MinY < components[j].Rect.MinY
		}
		if components[i].Rect.MinX != components[j].Rect.MinX {
			return components[i].Rect.MinX < components[j].Rect.MinX
		}
		return components[i].ID < components[j].ID
	})
	for i := range components {
		components[i].ID = i + 1
	}
	return components
}

func estimateStrokeWidth(mask *Mask, r Rect, cfg DetectConfig) int {
	runs := make([]int, 0, 64)
	for y := r.MinY; y < r.MaxY; y++ {
		run := 0
		for x := r.MinX; x < r.MaxX; x++ {
			if mask.At(x, y) != 0 {
				run++
				continue
			}
			if run > 0 {
				runs = append(runs, run)
				run = 0
			}
		}
		if run > 0 {
			runs = append(runs, run)
		}
	}
	for x := r.MinX; x < r.MaxX; x++ {
		run := 0
		for y := r.MinY; y < r.MaxY; y++ {
			if mask.At(x, y) != 0 {
				run++
				continue
			}
			if run > 0 {
				runs = append(runs, run)
				run = 0
			}
		}
		if run > 0 {
			runs = append(runs, run)
		}
	}
	if len(runs) == 0 {
		return cfg.MinStrokeWidthPx
	}
	sort.Ints(runs)
	median := runs[len(runs)/2]
	return clampInt(median, cfg.MinStrokeWidthPx, cfg.MaxStrokeWidthPx)
}

func componentGradientScore(gradient *Mask, r Rect) int {
	if gradient == nil {
		return 0
	}
	hits, probes := 0, 0
	for y := r.MinY - 1; y <= r.MaxY; y++ {
		for x := r.MinX - 1; x <= r.MaxX; x++ {
			probes++
			if gradient.At(x, y) != 0 {
				hits++
			}
		}
	}
	if probes == 0 {
		return 0
	}
	return hits * 10000 / probes
}

func componentConfidence(r Rect, pixels, sw, gradientBPS int) int {
	area := r.Area()
	densityBPS := 0
	if area > 0 {
		densityBPS = pixels * 10000 / area
	}
	densityScore := 10000 - absInt(densityBPS-4500)
	if densityScore < 0 {
		densityScore = 0
	}
	swScore := 10000
	if sw <= 0 {
		swScore = 0
	}
	score := (densityScore*45 + minInt(gradientBPS*4, 10000)*25 + swScore*30) / 100
	return clampInt(score, 0, 10000)
}

func componentLooksLikeGlyph(c Component, cfg DetectConfig) bool {
	r := c.Rect
	if r.Width() < 3 || r.Height() < 7 {
		return false
	}
	if c.StrokeWidthPx < cfg.MinStrokeWidthPx || c.StrokeWidthPx > cfg.MaxStrokeWidthPx {
		return false
	}
	if r.Height() < 4*c.StrokeWidthPx || r.Height() > 11*c.StrokeWidthPx+8 {
		return false
	}
	if r.Width() > 9*c.StrokeWidthPx+10 {
		return false
	}
	density := c.PixelCount * 10000 / r.Area()
	if density < 1200 || density > 9200 {
		return false
	}
	if c.GradientBPS < 800 {
		return false
	}
	return c.ConfidenceBPS >= cfg.CandidateScoreBPS
}

func mergeComponents(components []Component, cfg DetectConfig) []DetectedLine {
	n := len(components)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			if ra < rb {
				parent[rb] = ra
			} else {
				parent[ra] = rb
			}
		}
	}

	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			a, b := components[i], components[j]
			if !sameTextLine(a, b, cfg) {
				continue
			}
			union(i, j)
		}
	}

	groups := map[int][]int{}
	roots := make([]int, 0, n)
	for i := range components {
		root := find(i)
		if _, ok := groups[root]; !ok {
			roots = append(roots, root)
		}
		groups[root] = append(groups[root], i)
	}
	sort.Ints(roots)

	lines := make([]DetectedLine, 0, len(roots))
	for _, root := range roots {
		idxs := groups[root]
		sort.Slice(idxs, func(i, j int) bool {
			return components[idxs[i]].Rect.MinX < components[idxs[j]].Rect.MinX
		})
		box := components[idxs[0]].Rect
		swSum, confSum := 0, 0
		ids := make([]int, 0, len(idxs))
		for _, idx := range idxs {
			box = unionRect(box, components[idx].Rect)
			swSum += components[idx].StrokeWidthPx
			confSum += components[idx].ConfidenceBPS
			ids = append(ids, components[idx].ID)
		}
		sw := swSum / len(idxs)
		lines = append(lines, DetectedLine{
			Box:                box,
			ComponentIDs:       ids,
			StrokeWidthPx:      sw,
			StrokeWidthMilliPx: sw * 1000,
			ConfidenceBPS:      confSum / len(idxs),
		})
	}
	sort.Slice(lines, func(i, j int) bool {
		ay, by := lines[i].Box.MinY, lines[j].Box.MinY
		if absInt(ay-by) > 8 {
			return ay < by
		}
		return lines[i].Box.MinX < lines[j].Box.MinX
	})
	return lines
}

func sameTextLine(a, b Component, cfg DetectConfig) bool {
	vertical := VerticalOverlapBPS(a.Rect, b.Rect)
	heightRatio := minInt(a.Rect.Height(), b.Rect.Height()) * 10000 / maxInt(a.Rect.Height(), b.Rect.Height())
	if vertical < cfg.MergeOverlapBPS || heightRatio < 6000 {
		return false
	}
	swRatio := minInt(a.StrokeWidthPx, b.StrokeWidthPx) * 10000 / maxInt(a.StrokeWidthPx, b.StrokeWidthPx)
	if swRatio < cfg.StrokeConsistencyBPS {
		return false
	}

	left, right := a.Rect, b.Rect
	if left.MinX > right.MinX {
		left, right = right, left
	}
	gap := right.MinX - left.MaxX
	maxGap := 10*maxInt(a.StrokeWidthPx, b.StrokeWidthPx) + 8
	return gap <= maxGap
}

func unionRect(a, b Rect) Rect {
	return Rect{
		MinX: minInt(a.MinX, b.MinX),
		MinY: minInt(a.MinY, b.MinY),
		MaxX: maxInt(a.MaxX, b.MaxX),
		MaxY: maxInt(a.MaxY, b.MaxY),
	}
}
