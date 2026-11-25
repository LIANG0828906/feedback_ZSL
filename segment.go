package iconcut

import (
	"math"
	"sort"
)

const (
	MinSeedArea = 16
)

func ValidateSegmentParams(p SegmentParams) error {
	if !(p.ColorTolerance >= 0 && p.ColorTolerance <= 220) {
		return WithDetails(NewError(EPara, "declared.segment.color_tolerance", "color_tolerance must be in [0,220]"), map[string]any{
			"min": 0, "max": 220, "actual": p.ColorTolerance,
		})
	}
	if !(p.GradientThreshold >= 0 && p.GradientThreshold <= 255) {
		return WithDetails(NewError(EPara, "declared.segment.gradient_threshold", "gradient_threshold must be in [0,255]"), map[string]any{
			"min": 0, "max": 255, "actual": p.GradientThreshold,
		})
	}
	if p.MorphSize < 1 || p.MorphSize > 31 || p.MorphSize%2 == 0 {
		return WithDetails(NewError(EPara, "declared.segment.morph_size", "morph_size must be an odd integer in [1,31]"), map[string]any{
			"min": 1, "max": 31, "actual": p.MorphSize, "odd": true,
		})
	}
	if len(p.SeedPoints) == 0 {
		return NewError(EPara, "declared.segment.seed_points", "at least one seed point is required")
	}
	return nil
}

func ValidateBudget(b Budget) error {
	if b.Workers < 1 || b.Workers > 64 {
		return NewError(EPara, "declared.budget.workers", "workers must be in [1,64]")
	}
	if b.MaxThreadPixels < 4096 {
		return NewError(EPara, "declared.budget.max_thread_pixels", "max_thread_pixels must be >= 4096")
	}
	if b.MaxQueuedTasks < 1 {
		return NewError(EPara, "declared.budget.max_queued_tasks", "max_queued_tasks must be >= 1")
	}
	if b.PreviewDelayMaxMS < 1 {
		return NewError(EPara, "declared.budget.preview_delay_max_ms", "preview_delay_max_ms must be >= 1")
	}
	return nil
}

func colorDist(a, b [3]byte) float64 {
	dr := float64(int(a[0]) - int(b[0]))
	dg := float64(int(a[1]) - int(b[1]))
	db := float64(int(a[2]) - int(b[2]))
	return math.Sqrt(dr*dr + dg*dg + db*db)
}

func grayAt(im *Image, x, y int) float64 {
	c := im.Pixel(clampInt(x, 0, im.W-1), clampInt(y, 0, im.H-1))
	return 0.299*float64(c[0]) + 0.587*float64(c[1]) + 0.114*float64(c[2])
}

func GradientMap(im *Image) []float64 {
	out := make([]float64, im.W*im.H)
	for y := 0; y < im.H; y++ {
		for x := 0; x < im.W; x++ {
			gx := grayAt(im, x+1, y) - grayAt(im, x-1, y)
			gy := grayAt(im, x, y+1) - grayAt(im, x, y-1)
			out[y*im.W+x] = math.Hypot(gx, gy) / 2
		}
	}
	return out
}

type GrowTrace struct {
	Iterations int    `json:"iterations"`
	IterBudget int    `json:"iteration_budget"`
	Downsample int    `json:"downsample"`
	Basis      string `json:"basis"`
}

type SegmentResult struct {
	Labels *LabelMask
	Raw    *LabelMask
	Trace  GrowTrace
}

func RegionGrow(im *Image, p SegmentParams, maxIterations int) (*LabelMask, GrowTrace, error) {
	if im == nil || im.W == 0 || im.H == 0 {
		return nil, GrowTrace{}, NewError(EEmpty, "corrected", "corrected canvas is empty")
	}
	if err := ValidateSegmentParams(p); err != nil {
		return nil, GrowTrace{}, err
	}
	for i, s := range p.SeedPoints {
		if s.X < 0 || s.Y < 0 || s.X >= im.W || s.Y >= im.H {
			return nil, GrowTrace{}, WithDetails(NewError(ESeed, "declared.segment.seed_points", "seed %d lies outside corrected canvas", i), map[string]any{
				"index": i, "seed": s, "canvas": Size{W: im.W, H: im.H},
			})
		}
	}
	for i, s := range p.SeedPoints {
		sc := im.Pixel(s.X, s.Y)
		if seedOnBorderBackground(im, s, p.ColorTolerance) {
			return nil, GrowTrace{}, WithDetails(NewError(ESeed, "declared.segment.seed_points", "seed %d is on background", i), map[string]any{
				"index": i, "seed": s, "color": sc,
				"background_distance": nearestBorderDistance(im, s),
			})
		}
	}

	grad := GradientMap(im)
	labels := NewLabelMask(im.W, im.H)
	seedColors := make([][3]byte, len(p.SeedPoints))
	iterations := 0
	truncated := false
	budget := maxIterations
	if budget <= 0 {
		budget = im.W * im.H * len(p.SeedPoints)
	}

	type item struct{ x, y int }
	neighbors := []IPoint{{X: -1, Y: 0}, {X: 1, Y: 0}, {X: 0, Y: -1}, {X: 0, Y: 1}}
	for id1, seed := range p.SeedPoints {
		label := uint8(id1 + 1)
		seedColor := im.Pixel(seed.X, seed.Y)
		seedColors[id1] = seedColor
		if labels.At(seed.X, seed.Y) != 0 {
			continue
		}
		queue := []item{{seed.X, seed.Y}}
		labels.Set(seed.X, seed.Y, label)
		processed := 0
		for len(queue) > 0 {
			if maxIterations > 0 && iterations >= maxIterations {
				truncated = true
				break
			}
			cur := queue[0]
			queue = queue[1:]
			iterations++
			processed++
			curColor := im.Pixel(cur.x, cur.y)
			for _, d := range neighbors {
				nx, ny := cur.x+d.X, cur.y+d.Y
				if nx < 0 || ny < 0 || nx >= im.W || ny >= im.H {
					continue
				}
				idx := ny*im.W + nx
				if labels.Labels[idx] != 0 {
					continue
				}
				nextColor := im.Pixel(nx, ny)
				if colorDist(nextColor, seedColor) > p.ColorTolerance {
					continue
				}
				edgeGrad := math.Max(grad[cur.y*im.W+cur.x], grad[idx])
				edgeColor := colorDist(curColor, nextColor)
				if edgeGrad > p.GradientThreshold && edgeColor > p.ColorTolerance*0.75 {
					continue
				}
				labels.Labels[idx] = label
				queue = append(queue, item{nx, ny})
			}
		}
		if processed < MinSeedArea && iterations < budget {
			return nil, GrowTrace{Iterations: iterations, IterBudget: budget}, WithDetails(NewError(ESeed, "declared.segment.seed_points", "seed %d produced only %d pixels", id1, processed), map[string]any{
				"index": id1, "seed": seed, "area": processed, "min_area": MinSeedArea,
			})
		}
	}
	if !truncated {
		recoverBoundary(im, labels, seedColors)
	}
	return labels, GrowTrace{Iterations: iterations, IterBudget: budget, Basis: "color_distance+gradient_barrier"}, nil
}

func recoverBoundary(im *Image, labels *LabelMask, seedColors [][3]byte) {
	bg := borderColor(im)
	neighbors := []IPoint{{X: -1, Y: 0}, {X: 1, Y: 0}, {X: 0, Y: -1}, {X: 0, Y: 1}}
	for iteration := 0; iteration < maxInt(im.W, im.H); iteration++ {
		bestLabel := make([]uint8, im.W*im.H)
		bestScore := make([]float64, im.W*im.H)
		for i := range bestScore {
			bestScore[i] = math.Inf(1)
		}
		changed := false
		for y := 0; y < im.H; y++ {
			for x := 0; x < im.W; x++ {
				id := labels.At(x, y)
				if id == 0 || int(id) > len(seedColors) {
					continue
				}
				for _, d := range neighbors {
					nx, ny := x+d.X, y+d.Y
					if nx < 0 || ny < 0 || nx >= im.W || ny >= im.H {
						continue
					}
					idx := ny*im.W + nx
					if labels.Labels[idx] != 0 {
						continue
					}
					c := im.Pixel(nx, ny)
					dSeed := colorDist(c, seedColors[id-1])
					dBG := colorDist(c, bg)
					score := dSeed - dBG
					if dSeed <= dBG+2 && score < bestScore[idx] {
						bestScore[idx] = score
						bestLabel[idx] = id
						changed = true
					}
				}
			}
		}
		for i, id := range bestLabel {
			if id != 0 {
				labels.Labels[i] = id
			}
		}
		if !changed {
			return
		}
	}
}

func borderColor(im *Image) [3]byte {
	var sum [3]int
	n := 0
	for x := 0; x < im.W; x++ {
		for _, y := range []int{0, im.H - 1} {
			c := im.Pixel(x, y)
			sum[0] += int(c[0])
			sum[1] += int(c[1])
			sum[2] += int(c[2])
			n++
		}
	}
	for y := 1; y < im.H-1; y++ {
		for _, x := range []int{0, im.W - 1} {
			c := im.Pixel(x, y)
			sum[0] += int(c[0])
			sum[1] += int(c[1])
			sum[2] += int(c[2])
			n++
		}
	}
	return [3]byte{byte(sum[0] / n), byte(sum[1] / n), byte(sum[2] / n)}
}

func nearestBorderDistance(im *Image, p IPoint) float64 {
	c := im.Pixel(p.X, p.Y)
	best := math.Inf(1)
	for x := 0; x < im.W; x++ {
		best = math.Min(best, colorDist(c, im.Pixel(x, 0)))
		best = math.Min(best, colorDist(c, im.Pixel(x, im.H-1)))
	}
	for y := 0; y < im.H; y++ {
		best = math.Min(best, colorDist(c, im.Pixel(0, y)))
		best = math.Min(best, colorDist(c, im.Pixel(im.W-1, y)))
	}
	return best
}

func seedOnBorderBackground(im *Image, p IPoint, tol float64) bool {
	return nearestBorderDistance(im, p) <= math.Max(tol, 10)
}

func CleanLabels(in *LabelMask, morphSize int) *LabelMask {
	radius := morphSize / 2
	if radius <= 0 {
		return cloneLabels(in)
	}
	ids := make([]int, 0)
	seen := map[uint8]bool{}
	for _, v := range in.Labels {
		if v != 0 && !seen[v] {
			seen[v] = true
			ids = append(ids, int(v))
		}
	}
	sort.Ints(ids)
	out := NewLabelMask(in.W, in.H)
	for _, id := range ids {
		bin := make([]bool, in.W*in.H)
		for i, v := range in.Labels {
			bin[i] = int(v) == id
		}
		dilated := morphOp(bin, in.W, in.H, radius, false)
		closed := morphOp(dilated, in.W, in.H, radius, true)
		eroded := morphOp(closed, in.W, in.H, radius, true)
		opened := morphOp(eroded, in.W, in.H, radius, false)
		opened = protectLargeHoles(in, id, opened, radius)
		for i, v := range opened {
			if v {
				out.Labels[i] = uint8(id)
			}
		}
	}
	return out
}

func protectLargeHoles(in *LabelMask, id int, cleaned []bool, radius int) []bool {
	bbox := bboxForID(in, uint8(id))
	if bbox.W == 0 {
		return cleaned
	}
	visited := make([]bool, in.W*in.H)
	for y := bbox.Y; y < bbox.MaxY(); y++ {
		for x := bbox.X; x < bbox.MaxX(); x++ {
			idx := y*in.W + x
			if visited[idx] || in.Labels[idx] == uint8(id) {
				continue
			}
			var pixels []IPoint
			queue := []IPoint{{X: x, Y: y}}
			visited[idx] = true
			enclosed := true
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				pixels = append(pixels, p)
				if p.X == bbox.X || p.Y == bbox.Y || p.X == bbox.MaxX()-1 || p.Y == bbox.MaxY()-1 {
					enclosed = false
				}
				for _, d := range []IPoint{{X: 1, Y: 0}, {X: -1, Y: 0}, {X: 0, Y: 1}, {X: 0, Y: -1}} {
					nx, ny := p.X+d.X, p.Y+d.Y
					if nx < bbox.X || ny < bbox.Y || nx >= bbox.MaxX() || ny >= bbox.MaxY() {
						enclosed = false
						continue
					}
					nidx := ny*in.W + nx
					if visited[nidx] || in.Labels[nidx] == uint8(id) {
						continue
					}
					visited[nidx] = true
					queue = append(queue, IPoint{X: nx, Y: ny})
				}
			}
			if enclosed && len(pixels) >= 24 {
				hx0, hy0 := bbox.MaxX(), bbox.MaxY()
				hx1, hy1 := bbox.X, bbox.Y
				for _, p := range pixels {
					hx0 = minInt(hx0, p.X)
					hy0 = minInt(hy0, p.Y)
					hx1 = maxInt(hx1, p.X)
					hy1 = maxInt(hy1, p.Y)
				}
				pad := radius + 1
				r := Rect{X: hx0 - pad, Y: hy0 - pad, W: hx1 - hx0 + 1 + 2*pad, H: hy1 - hy0 + 1 + 2*pad}
				r = r.Intersect(Rect{W: in.W, H: in.H})
				for yy := r.Y; yy < r.MaxY(); yy++ {
					for xx := r.X; xx < r.MaxX(); xx++ {
						cleaned[yy*in.W+xx] = in.Labels[yy*in.W+xx] == uint8(id)
					}
				}
			}
		}
	}
	return cleaned
}

func bboxForID(m *LabelMask, id uint8) Rect {
	r := Rect{}
	found := false
	for y := 0; y < m.H; y++ {
		for x := 0; x < m.W; x++ {
			if m.At(x, y) != id {
				continue
			}
			if !found {
				r = Rect{X: x, Y: y, W: 1, H: 1}
				found = true
			} else {
				if x < r.X {
					r.W += r.X - x
					r.X = x
				}
				if y < r.Y {
					r.H += r.Y - y
					r.Y = y
				}
				if x >= r.MaxX() {
					r.W = x - r.X + 1
				}
				if y >= r.MaxY() {
					r.H = y - r.Y + 1
				}
			}
		}
	}
	return r
}

func cloneLabels(in *LabelMask) *LabelMask {
	out := NewLabelMask(in.W, in.H)
	copy(out.Labels, in.Labels)
	return out
}

func morphOp(src []bool, w, h, r int, erode bool) []bool {
	tmp := make([]bool, w*h)
	out := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			ok := erode
			for k := -r; k <= r; k++ {
				xx := clampInt(x+k, 0, w-1)
				v := src[y*w+xx]
				if erode && !v {
					ok = false
					break
				}
				if !erode && v {
					ok = true
					break
				}
			}
			tmp[y*w+x] = ok
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			ok := erode
			for k := -r; k <= r; k++ {
				yy := clampInt(y+k, 0, h-1)
				v := tmp[yy*w+x]
				if erode && !v {
					ok = false
					break
				}
				if !erode && v {
					ok = true
					break
				}
			}
			out[y*w+x] = ok
		}
	}
	return out
}

type RegionStats struct {
	ID       int   `json:"id"`
	Area     int   `json:"area"`
	Centroid Point `json:"centroid"`
	BBox     Rect  `json:"bbox"`
	Holes    int   `json:"holes"`
}

func AnalyzeLabels(mask *LabelMask) []RegionStats {
	byID := map[int]*RegionStats{}
	type sum struct{ sx, sy int }
	sums := map[int]*sum{}
	for y := 0; y < mask.H; y++ {
		for x := 0; x < mask.W; x++ {
			id := int(mask.At(x, y))
			if id == 0 {
				continue
			}
			st := byID[id]
			if st == nil {
				st = &RegionStats{ID: id, BBox: Rect{X: x, Y: y, W: 1, H: 1}}
				byID[id] = st
				sums[id] = &sum{}
			}
			st.Area++
			s := sums[id]
			s.sx += x
			s.sy += y
			if x < st.BBox.X {
				st.BBox.W += st.BBox.X - x
				st.BBox.X = x
			}
			if y < st.BBox.Y {
				st.BBox.H += st.BBox.Y - y
				st.BBox.Y = y
			}
			if x >= st.BBox.MaxX() {
				st.BBox.W = x - st.BBox.X + 1
			}
			if y >= st.BBox.MaxY() {
				st.BBox.H = y - st.BBox.Y + 1
			}
		}
	}
	ids := make([]int, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]RegionStats, 0, len(ids))
	for _, id := range ids {
		st := byID[id]
		s := sums[id]
		st.Centroid = Point{X: float64(s.sx) / float64(st.Area), Y: float64(s.sy) / float64(st.Area)}
		st.Holes = countHoles(mask, uint8(id), st.BBox)
		out = append(out, *st)
	}
	return out
}

func countHoles(mask *LabelMask, id uint8, bbox Rect) int {
	visited := make([]bool, mask.W*mask.H)
	holes := 0
	for y := bbox.Y; y < bbox.MaxY(); y++ {
		for x := bbox.X; x < bbox.MaxX(); x++ {
			idx := y*mask.W + x
			if visited[idx] || mask.Labels[idx] == id {
				continue
			}
			queue := []IPoint{{X: x, Y: y}}
			visited[idx] = true
			touchesBoundary := false
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				if p.X == bbox.X || p.Y == bbox.Y || p.X == bbox.MaxX()-1 || p.Y == bbox.MaxY()-1 {
					touchesBoundary = true
				}
				for _, d := range []IPoint{{X: 1, Y: 0}, {X: -1, Y: 0}, {X: 0, Y: 1}, {X: 0, Y: -1}} {
					nx, ny := p.X+d.X, p.Y+d.Y
					if nx < bbox.X || ny < bbox.Y || nx >= bbox.MaxX() || ny >= bbox.MaxY() {
						touchesBoundary = true
						continue
					}
					nidx := ny*mask.W + nx
					if visited[nidx] || mask.Labels[nidx] == id {
						continue
					}
					visited[nidx] = true
					queue = append(queue, IPoint{X: nx, Y: ny})
				}
			}
			if !touchesBoundary {
				holes++
			}
		}
	}
	return holes
}

type RegionMismatch struct {
	ID    int `json:"id"`
	Area  int `json:"area"`
	Truth int `json:"truth_area,omitempty"`
}

func CompareToTruth(stats []RegionStats, truth []RegionTruth, tol Tolerances) *Error {
	det := map[int]RegionStats{}
	for _, s := range stats {
		det[s.ID] = s
	}
	trueMap := map[int]RegionTruth{}
	for _, t := range truth {
		trueMap[t.ID] = t
	}
	var extra, missing []RegionMismatch
	for id, s := range det {
		if _, ok := trueMap[id]; !ok {
			extra = append(extra, RegionMismatch{ID: id, Area: s.Area})
		}
	}
	for id, t := range trueMap {
		s, ok := det[id]
		if !ok || s.Area == 0 {
			missing = append(missing, RegionMismatch{ID: id, Area: 0, Truth: t.Area})
		}
	}
	if len(det) != len(truth) || len(extra)+len(missing) > 0 {
		sortMismatch(extra)
		sortMismatch(missing)
		return WithDetails(NewError(ERegion, "regions", "region count mismatch: got %d want %d", len(det), len(truth)), map[string]any{
			"detected_count": len(det), "truth_count": len(truth), "extra": extra, "missing": missing,
		})
	}
	var centroidMax, areaMax float64
	var holeDeltas []map[string]int
	for _, t := range truth {
		s := det[t.ID]
		cd := math.Hypot(s.Centroid.X-t.CentroidX, s.Centroid.Y-t.CentroidY)
		ar := math.Abs(float64(s.Area-t.Area)) / math.Max(1, float64(t.Area))
		centroidMax = math.Max(centroidMax, cd)
		areaMax = math.Max(areaMax, ar)
		hd := s.Holes - t.Holes
		if absInt(hd) > tol.HoleCountDelta {
			holeDeltas = append(holeDeltas, map[string]int{"id": t.ID, "detected": s.Holes, "truth": t.Holes})
		}
		if cd > tol.RegionCentroidPX || ar > tol.RegionAreaRel {
			return WithDetails(NewError(ERegion, "regions", "region %d exceeds geometry tolerance", t.ID), map[string]any{
				"region_id": t.ID, "centroid_px": cd, "centroid_limit": tol.RegionCentroidPX,
				"area_rel": ar, "area_limit": tol.RegionAreaRel,
				"detected": s, "truth": t,
			})
		}
	}
	if len(holeDeltas) > 0 {
		return WithDetails(NewError(ERegion, "regions.holes", "hole count deviation exceeds tolerance"), map[string]any{
			"hole_deltas": holeDeltas, "allowed_delta": tol.HoleCountDelta,
		})
	}
	return nil
}

func sortMismatch(x []RegionMismatch) {
	sort.Slice(x, func(i, j int) bool { return x[i].ID < x[j].ID })
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
