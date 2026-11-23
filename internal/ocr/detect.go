package ocr

import (
	"fmt"
	"hash/fnv"
	"image"
	"math"
	"sort"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/imaging"
)

type Detector struct {
	Gray      *image.Gray
	Binary    imaging.BinaryView
	Gradient  []float64
	Width     int
	Height    int
	AvgStroke float64
}

func NewDetector(gray *image.Gray) *Detector {
	width, height := gray.Bounds().Dx(), gray.Bounds().Dy()
	return &Detector{
		Gray:      gray,
		Binary:    imaging.Binarize(gray),
		Gradient:  gradientMagnitude(gray),
		Width:     width,
		Height:    height,
		AvgStroke: imaging.EstimateAverageStrokeWidth(gray),
	}
}

func (d *Detector) ScanTile(tile domain.Tile, params domain.Params) ([]domain.Candidate, *domain.AppError) {
	components, appErr := d.extract(tile.Bounds(), tile.Core(), tile.Index, tile, params)
	if appErr != nil {
		return nil, appErr
	}
	if len(components) < params.MinCandidates {
		e := domain.Errorf(domain.CodeDet, "candidate count is below the configured lower bound")
		e.TileIndex = tile.Index
		e.TileX = tile.X
		e.TileY = tile.Y
		e.Field = "min_candidates"
		e.Expected = fmt.Sprintf(">=%d", params.MinCandidates)
		e.Actual = fmt.Sprint(len(components))
		return nil, e
	}
	return components, nil
}

func (d *Detector) ScanWhole(params domain.Params) ([]domain.Candidate, *domain.AppError) {
	all := domain.Rect{X: 0, Y: 0, W: d.Width, H: d.Height}
	candidates, appErr := d.extract(all, all, -1, domain.Tile{Index: -1}, params)
	if appErr != nil {
		return nil, appErr
	}
	return candidates, nil
}

func (d *Detector) extract(region, core domain.Rect, tileIndex int, tile domain.Tile, params domain.Params) ([]domain.Candidate, *domain.AppError) {
	region = clipRect(region, d.Width, d.Height)
	core = clipRect(core, d.Width, d.Height)
	if region.Empty() || core.Empty() {
		return nil, nil
	}
	visited := make([]bool, region.W*region.H)
	pixels := make([]image.Point, 0, 256)
	candidates := make([]domain.Candidate, 0, 64)
	for ly := 0; ly < region.H; ly++ {
		for lx := 0; lx < region.W; lx++ {
			localIndex := ly*region.W + lx
			if visited[localIndex] {
				continue
			}
			gx, gy := region.X+lx, region.Y+ly
			if !d.Binary.Mask[gy*d.Width+gx] {
				continue
			}
			pixels = pixels[:0]
			queue := []image.Point{{X: lx, Y: ly}}
			visited[localIndex] = true
			minX, minY, maxX, maxY := lx, ly, lx, ly
			for head := 0; head < len(queue); head++ {
				p := queue[head]
				pixels = append(pixels, image.Pt(region.X+p.X, region.Y+p.Y))
				if p.X < minX {
					minX = p.X
				}
				if p.Y < minY {
					minY = p.Y
				}
				if p.X > maxX {
					maxX = p.X
				}
				if p.Y > maxY {
					maxY = p.Y
				}
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if dx == 0 && dy == 0 {
							continue
						}
						nx, ny := p.X+dx, p.Y+dy
						if nx < 0 || ny < 0 || nx >= region.W || ny >= region.H {
							continue
						}
						ni := ny*region.W + nx
						if visited[ni] {
							continue
						}
						vx, vy := region.X+nx, region.Y+ny
						if !d.Binary.Mask[vy*d.Width+vx] {
							continue
						}
						visited[ni] = true
						queue = append(queue, image.Pt(nx, ny))
					}
				}
			}
			bbox := domain.Rect{
				X: region.X + minX,
				Y: region.Y + minY,
				W: maxX - minX + 1,
				H: maxY - minY + 1,
			}
			if bbox.W < 2 || bbox.H < 2 {
				continue
			}
			if len(pixels) < params.DetectorMinArea {
				continue
			}
			if bbox.W > region.W*3/4 && bbox.H > region.H*3/4 {
				continue
			}
			center := bbox.Center()
			if !pointInRect(center, core) {
				continue
			}
			if tileIndex >= 0 {
				if breakX, breakY, bad := truncationPoint(bbox, region, d.Width, d.Height); bad {
					e := domain.Errorf(domain.CodeBand, "candidate touches a tile boundary before a complete glyph is visible")
					e.TileIndex = tile.Index
					e.TileX = tile.X
					e.TileY = tile.Y
					e.BreakX = breakX
					e.BreakY = breakY
					e.Field = "band_width"
					e.Expected = "wider overlap band"
					e.Actual = fmt.Sprint(tile.BandWidth)
					e.Cause = fmt.Sprintf("component bbox=%+v region=%+v", bbox, region)
					return nil, e
				}
			}
			stroke := componentStroke(pixels, d.Binary.Mask, d.Width)
			gradient := componentGradient(pixels, d.Gradient, d.Width)
			candidates = append(candidates, domain.Candidate{
				ID:            candidateID(bbox),
				TileIndex:     tileIndex,
				BBox:          bbox,
				Area:          len(pixels),
				StrokeWidth:   domain.Round4(stroke),
				GradientScore: domain.Round4(gradient),
				PixelHash:     componentHash(pixels),
			})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.BBox.Y != b.BBox.Y {
			return a.BBox.Y < b.BBox.Y
		}
		if a.BBox.X != b.BBox.X {
			return a.BBox.X < b.BBox.X
		}
		return a.BBox.W < b.BBox.W
	})
	return candidates, nil
}

func MergeCandidates(candidates []domain.Candidate, params domain.Params) ([]domain.DetectedLine, *domain.AppError) {
	sorted := append([]domain.Candidate(nil), candidates...)
	domain.SortCandidates(sorted)
	if len(sorted) == 0 {
		return nil, nil
	}
	// Exact duplicates can occur only if a caller supplies candidates extracted
	// outside tile-core ownership. Remove them deterministically by bbox/pixels.
	unique := make([]domain.Candidate, 0, len(sorted))
	for _, candidate := range sorted {
		duplicate := false
		for _, kept := range unique {
			if candidate.PixelHash == kept.PixelHash && candidate.BBox == kept.BBox {
				duplicate = true
				break
			}
		}
		if !duplicate {
			unique = append(unique, candidate)
		}
	}
	parent := make([]int, len(unique))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if ra < rb {
			parent[rb] = ra
		} else {
			parent[ra] = rb
		}
	}
	for i := 0; i < len(unique); i++ {
		for j := i + 1; j < len(unique); j++ {
			if shouldMerge(unique[i], unique[j], params) {
				union(i, j)
			}
		}
	}
	groups := map[int][]domain.Candidate{}
	keys := make([]int, 0)
	for i, candidate := range unique {
		root := find(i)
		if _, ok := groups[root]; !ok {
			keys = append(keys, root)
		}
		groups[root] = append(groups[root], candidate)
	}
	sort.Ints(keys)
	lines := make([]domain.DetectedLine, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		domain.SortCandidates(group)
		bbox := group[0].BBox
		weightedStroke := 0.0
		totalArea := 0
		tileSet := map[int]struct{}{}
		ids := make([]string, 0, len(group))
		for _, candidate := range group {
			bbox = bbox.Union(candidate.BBox)
			weightedStroke += candidate.StrokeWidth * float64(maxInt(1, candidate.Area))
			totalArea += maxInt(1, candidate.Area)
			if candidate.TileIndex >= 0 {
				tileSet[candidate.TileIndex] = struct{}{}
			}
			ids = append(ids, candidate.ID)
		}
		if len(group) < 2 {
			continue
		}
		avgStroke := weightedStroke / float64(maxInt(1, totalArea))
		if float64(bbox.W) < avgStroke*2.5 {
			continue
		}
		span := make([]int, 0, len(tileSet))
		for tileIndex := range tileSet {
			span = append(span, tileIndex)
		}
		sort.Ints(span)
		primary := -1
		if len(span) > 0 {
			primary = group[0].TileIndex
			if primary < 0 {
				primary = span[0]
			}
		}
		lines = append(lines, domain.DetectedLine{
			BBox:              bbox,
			Quad:              rectQuad(bbox),
			CandidateIDs:      ids,
			CandidateCount:    len(group),
			AvgStrokeWidth:    domain.Round4(avgStroke),
			PrimaryTileIndex:  primary,
			TileSpan:          span,
			MergeIoU:          params.MergeIoU,
			StrokeConsistency: params.StrokeConsistency,
		})
	}
	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i].BBox, lines[j].BBox
		ac, bc := a.Center(), b.Center()
		rowTolerance := minInt(a.H, b.H) / 3
		if absInt(ac.Y-bc.Y) > rowTolerance {
			return ac.Y < bc.Y
		}
		if ac.X != bc.X {
			return ac.X < bc.X
		}
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		return a.X < b.X
	})
	for i := range lines {
		lines[i].ReadingIndex = i
	}
	if err := ValidateReadingOrder(lines); err != nil {
		return nil, err
	}
	return lines, nil
}

func ValidateReadingOrder(lines []domain.DetectedLine) *domain.AppError {
	seen := map[int]struct{}{}
	for _, line := range lines {
		if line.ReadingIndex < 0 || line.ReadingIndex >= len(lines) {
			e := domain.Errorf(domain.CodeRead, "reading-order index is out of range")
			e.LineIndex = line.ReadingIndex
			e.Field = "reading_index"
			e.Expected = fmt.Sprintf("0..%d", len(lines)-1)
			e.Actual = fmt.Sprint(line.ReadingIndex)
			return e
		}
		if _, exists := seen[line.ReadingIndex]; exists {
			e := domain.Errorf(domain.CodeRead, "duplicate reading-order index")
			e.LineIndex = line.ReadingIndex
			e.Field = "reading_index"
			e.Actual = fmt.Sprint(line.ReadingIndex)
			return e
		}
		seen[line.ReadingIndex] = struct{}{}
	}
	for i := range lines {
		if _, ok := seen[i]; !ok {
			e := domain.Errorf(domain.CodeRead, "reading-order sequence is not contiguous")
			e.LineIndex = i
			e.Field = "reading_index"
			e.Expected = fmt.Sprint(i)
			e.Actual = "missing"
			return e
		}
	}
	return nil
}

func shouldMerge(a, b domain.Candidate, params domain.Params) bool {
	maxStroke := math.Max(a.StrokeWidth, b.StrokeWidth)
	minStroke := math.Min(a.StrokeWidth, b.StrokeWidth)
	if maxStroke <= 0 {
		return false
	}
	if minStroke/maxStroke < params.StrokeConsistency {
		return false
	}
	verticalOverlap := overlapLen(a.BBox.Y, a.BBox.Bottom(), b.BBox.Y, b.BBox.Bottom())
	minHeight := float64(minInt(a.BBox.H, b.BBox.H))
	overlapRatio := 0.0
	if minHeight > 0 {
		overlapRatio = float64(verticalOverlap) / minHeight
	}
	verticalGap := gapLen(a.BBox.Y, a.BBox.Bottom(), b.BBox.Y, b.BBox.Bottom())
	horizontalGap := gapLen(a.BBox.X, a.BBox.Right(), b.BBox.X, b.BBox.Right())
	maxHeight := float64(maxInt(a.BBox.H, b.BBox.H))
	verticalOK := overlapRatio >= params.MergeIoU || float64(verticalGap) <= maxHeight*0.42
	horizontalOK := float64(horizontalGap) <= maxHeight*1.35
	return verticalOK && horizontalOK
}

func componentStroke(pixels []image.Point, mask []bool, width int) float64 {
	if len(pixels) == 0 {
		return 1
	}
	runs := make([]int, 0, len(pixels))
	for _, p := range pixels {
		if p.X > 0 && mask[p.Y*width+p.X-1] {
			continue
		}
		run := 0
		x := p.X
		for x < width && mask[p.Y*width+x] {
			run++
			x++
		}
		if run > 0 {
			runs = append(runs, run)
		}
	}
	if len(runs) == 0 {
		return 1
	}
	sort.Ints(runs)
	value := float64(runs[int(float64(len(runs)-1)*0.35)])
	if value < 1 {
		value = 1
	}
	return value
}

func componentGradient(pixels []image.Point, gradient []float64, width int) float64 {
	if len(pixels) == 0 || len(gradient) == 0 {
		return 0
	}
	sum := 0.0
	for _, p := range pixels {
		sum += gradient[p.Y*width+p.X]
	}
	return sum / float64(len(pixels))
}

func gradientMagnitude(gray *image.Gray) []float64 {
	b := gray.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]float64, w*h)
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			gx := float64(gray.Pix[(y-1)*w+x+1]+2*gray.Pix[y*w+x+1]+gray.Pix[(y+1)*w+x+1]) -
				float64(gray.Pix[(y-1)*w+x-1]+2*gray.Pix[y*w+x-1]+gray.Pix[(y+1)*w+x-1])
			gy := float64(gray.Pix[(y+1)*w+x-1]+2*gray.Pix[(y+1)*w+x]+gray.Pix[(y+1)*w+x+1]) -
				float64(gray.Pix[(y-1)*w+x-1]+2*gray.Pix[(y-1)*w+x]+gray.Pix[(y-1)*w+x+1])
			out[y*w+x] = math.Sqrt(gx*gx+gy*gy) / 4
		}
	}
	return out
}

func componentHash(pixels []image.Point) string {
	h := fnv.New64a()
	var buf [8]byte
	for _, p := range pixels {
		v := uint64(uint32(p.X))<<32 | uint64(uint32(p.Y))
		buf[0] = byte(v >> 56)
		buf[1] = byte(v >> 48)
		buf[2] = byte(v >> 40)
		buf[3] = byte(v >> 32)
		buf[4] = byte(v >> 24)
		buf[5] = byte(v >> 16)
		buf[6] = byte(v >> 8)
		buf[7] = byte(v)
		_, _ = h.Write(buf[:])
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

func candidateID(rect domain.Rect) string {
	return fmt.Sprintf("c-%d-%d-%d-%d", rect.X, rect.Y, rect.W, rect.H)
}

func rectQuad(rect domain.Rect) [4]domain.Point {
	return [4]domain.Point{
		{X: rect.X, Y: rect.Y},
		{X: rect.Right() - 1, Y: rect.Y},
		{X: rect.Right() - 1, Y: rect.Bottom() - 1},
		{X: rect.X, Y: rect.Bottom() - 1},
	}
}

func truncationPoint(bbox domain.Rect, region domain.Rect, imageW, imageH int) (int, int, bool) {
	if bbox.X <= region.X && region.X > 0 {
		return region.X, bbox.Y + bbox.H/2, true
	}
	if bbox.Y <= region.Y && region.Y > 0 {
		return bbox.X + bbox.W/2, region.Y, true
	}
	if bbox.Right() >= region.Right() && region.Right() < imageW {
		return region.Right() - 1, bbox.Y + bbox.H/2, true
	}
	if bbox.Bottom() >= region.Bottom() && region.Bottom() < imageH {
		return bbox.X + bbox.W/2, region.Bottom() - 1, true
	}
	return 0, 0, false
}

func clipRect(rect domain.Rect, width, height int) domain.Rect {
	x1 := clampInt(rect.X, 0, width)
	y1 := clampInt(rect.Y, 0, height)
	x2 := clampInt(rect.Right(), 0, width)
	y2 := clampInt(rect.Bottom(), 0, height)
	return domain.Rect{X: x1, Y: y1, W: maxInt(0, x2-x1), H: maxInt(0, y2-y1)}
}

func pointInRect(p domain.Point, rect domain.Rect) bool {
	return p.X >= rect.X && p.Y >= rect.Y && p.X < rect.Right() && p.Y < rect.Bottom()
}

func overlapLen(a1, a2, b1, b2 int) int {
	lo := maxInt(a1, b1)
	hi := minInt(a2, b2)
	if hi <= lo {
		return 0
	}
	return hi - lo
}

func gapLen(a1, a2, b1, b2 int) int {
	if a2 < b1 {
		return b1 - a2
	}
	if b2 < a1 {
		return a1 - b2
	}
	return 0
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
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
