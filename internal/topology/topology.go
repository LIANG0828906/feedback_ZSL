package topology

import (
	"math"
	"sort"

	"maskqa/internal/model"
)

type Tolerances struct {
	CentroidPx      float64
	RingWidthPx     float64
	HoleCount       int
	DirectionCosine float64
	AreaRelative    float64
}

func DefaultTolerances() Tolerances {
	return Tolerances{CentroidPx: 0.75, RingWidthPx: 0.75, HoleCount: 0, DirectionCosine: 0.94, AreaRelative: 0.01}
}

type labeledMask struct {
	labels []int
	next   int
}

func Analyze(coverage model.Mask) model.TopologyReport {
	binary := coverage.Binary(model.AlphaThreshold)
	lm := labelForeground(binary)

	domains := buildDomains(coverage, binary, lm)
	holeCounts := countHoles(binary, lm)
	for i := range domains {
		domains[i].Holes = holeCounts[domains[i].Index]
		domains[i].DirectionBins, domains[i].DirectionDegrees, domains[i].DirectionConfidence = directionSpectrum(coverage, lm.labels, domains[i].Index)
	}
	sort.Slice(domains, func(i, j int) bool { return domains[i].Index < domains[j].Index })
	return model.TopologyReport{
		Version:     model.TopologyVersion,
		Algorithm:   "fg8-bfs,background4-hole-bfs,manhattan-depth-ring,sobel-tangent-spectrum",
		Threshold:   model.AlphaThreshold,
		DomainCount: len(domains),
		Domains:     domains,
	}
}

func labelForeground(binary model.Mask) labeledMask {
	w, h := binary.Width, binary.Height
	labels := make([]int, w*h)
	next := 0
	type qp struct{ x, y int }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if binary.Pix[idx] == 0 || labels[idx] != 0 {
				continue
			}
			next++
			labels[idx] = next
			q := []qp{{x, y}}
			for head := 0; head < len(q); head++ {
				p := q[head]
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if dx == 0 && dy == 0 {
							continue
						}
						nx, ny := p.x+dx, p.y+dy
						if nx < 0 || ny < 0 || nx >= w || ny >= h {
							continue
						}
						ni := ny*w + nx
						if binary.Pix[ni] == 0 || labels[ni] != 0 {
							continue
						}
						labels[ni] = next
						q = append(q, qp{nx, ny})
					}
				}
			}
		}
	}
	return labeledMask{labels: labels, next: next}
}

func buildDomains(coverage, binary model.Mask, lm labeledMask) []model.DomainReport {
	type accum struct {
		index                  int
		area                   int
		sumX, sumY             int64
		minX, minY, maxX, maxY int
		dist                   []int
		maxDist                int
	}
	accs := map[int]*accum{}
	for label := 1; label <= lm.next; label++ {
		accs[label] = &accum{index: label, minX: coverage.Width, minY: coverage.Height, maxX: -1, maxY: -1}
	}
	dist := foregroundDistance(binary)
	for y := 0; y < coverage.Height; y++ {
		for x := 0; x < coverage.Width; x++ {
			idx := y*coverage.Width + x
			label := lm.labels[idx]
			if label == 0 {
				continue
			}
			a := accs[label]
			a.area++
			a.sumX += int64(x)
			a.sumY += int64(y)
			a.minX = min(a.minX, x)
			a.minY = min(a.minY, y)
			a.maxX = max(a.maxX, x)
			a.maxY = max(a.maxY, y)
			d := dist[idx]
			if d > a.maxDist {
				a.maxDist = d
			}
		}
	}
	out := make([]model.DomainReport, 0, len(accs))
	for label := 1; label <= lm.next; label++ {
		a := accs[label]
		if a == nil || a.area == 0 {
			continue
		}
		cx := round64(float64(a.sumX) / float64(a.area))
		cy := round64(float64(a.sumY) / float64(a.area))
		out = append(out, model.DomainReport{
			Index:       label,
			ID:          domainID(label),
			Area:        a.area,
			CentroidX:   cx,
			CentroidY:   cy,
			BBox:        model.BBox{MinX: a.minX, MinY: a.minY, MaxX: a.maxX, MaxY: a.maxY},
			RingWidthPx: round64(float64(2 * a.maxDist)),
		})
	}
	return out
}

func foregroundDistance(binary model.Mask) []int {
	w, h := binary.Width, binary.Height
	dist := make([]int, w*h)
	type p struct{ x, y int }
	var q []p
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if binary.Pix[idx] == 0 {
				continue
			}
			border := false
			for _, d := range [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				nx, ny := x+d[0], y+d[1]
				if nx < 0 || ny < 0 || nx >= w || ny >= h || binary.Pix[ny*w+nx] == 0 {
					border = true
					break
				}
			}
			if border {
				dist[idx] = 1
				q = append(q, p{x, y})
			}
		}
	}
	for head := 0; head < len(q); head++ {
		cur := q[head]
		nd := dist[cur.y*w+cur.x] + 1
		for _, d := range [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			nx, ny := cur.x+d[0], cur.y+d[1]
			if nx < 0 || ny < 0 || nx >= w || ny >= h {
				continue
			}
			ni := ny*w + nx
			if binary.Pix[ni] == 0 || dist[ni] != 0 {
				continue
			}
			dist[ni] = nd
			q = append(q, p{nx, ny})
		}
	}
	return dist
}

func countHoles(binary model.Mask, lm labeledMask) map[int]int {
	w, h := binary.Width, binary.Height
	visited := make([]bool, w*h)
	holes := map[int]int{}
	type p struct{ x, y int }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			start := y*w + x
			if binary.Pix[start] != 0 || visited[start] {
				continue
			}
			touchesBorder := false
			adjacentLabels := map[int]struct{}{}
			q := []p{{x, y}}
			visited[start] = true
			for head := 0; head < len(q); head++ {
				cur := q[head]
				if cur.x == 0 || cur.y == 0 || cur.x == w-1 || cur.y == h-1 {
					touchesBorder = true
				}
				for _, d := range [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					nx, ny := cur.x+d[0], cur.y+d[1]
					if nx < 0 || ny < 0 || nx >= w || ny >= h {
						continue
					}
					ni := ny*w + nx
					if binary.Pix[ni] != 0 {
						if label := lm.labels[ni]; label > 0 {
							adjacentLabels[label] = struct{}{}
						}
						continue
					}
					if !visited[ni] {
						visited[ni] = true
						q = append(q, p{nx, ny})
					}
				}
			}
			if !touchesBorder {
				owner := smallestKey(adjacentLabels)
				holes[owner]++
			}
		}
	}
	return holes
}

func directionSpectrum(coverage model.Mask, labels []int, domain int) ([]int, []float64, float64) {
	bins := make([]int, model.DirectionBins)
	w, h := coverage.Width, coverage.Height
	at := func(x, y int) int {
		if x < 0 || y < 0 || x >= w || y >= h {
			return 0
		}
		return int(coverage.Pix[y*w+x])
	}
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			idx := y*w + x
			if labels[idx] != domain {
				continue
			}
			gx := -at(x-1, y-1) - 2*at(x-1, y) - at(x-1, y+1) + at(x+1, y-1) + 2*at(x+1, y) + at(x+1, y+1)
			gy := -at(x-1, y-1) - 2*at(x, y-1) - at(x+1, y-1) + at(x-1, y+1) + 2*at(x, y+1) + at(x+1, y+1)
			mag := abs(gx) + abs(gy)
			if mag < 20 {
				continue
			}
			gradAngle := math.Atan2(float64(gy), float64(gx)) * 180 / math.Pi
			tangent := math.Mod(gradAngle+90, 180)
			if tangent < 0 {
				tangent += 180
			}
			bin := int(math.Floor(tangent / (180 / float64(model.DirectionBins))))
			if bin >= model.DirectionBins {
				bin = model.DirectionBins - 1
			}
			bins[bin] += mag
		}
	}
	degrees := make([]float64, model.DirectionBins)
	total := 0
	maxBin := 0
	for i, v := range bins {
		degrees[i] = round64((float64(i) + 0.5) * 180 / float64(model.DirectionBins))
		total += v
		if v > bins[maxBin] {
			maxBin = i
		}
	}
	confidence := 0.0
	if total > 0 {
		confidence = round64(float64(bins[maxBin]) / float64(total))
	}
	return bins, degrees, confidence
}

func VerifyAgainstTruth(got, truth model.TopologyReport, tol Tolerances) *model.Error {
	if got.DomainCount != truth.DomainCount || len(got.Domains) != len(truth.Domains) {
		return &model.Error{Code: model.CodeTop, Field: "domains", Message: "domain count mismatch", Original: truth.DomainCount, Residual: got.DomainCount, Details: map[string]any{"measured_domains": got.DomainCount, "truth_domains": truth.DomainCount}}
	}
	for i := range truth.Domains {
		g := got.Domains[i]
		t := truth.Domains[i]
		if g.ID != t.ID {
			return topError("domain_id", i+1, "domain label/order mismatch", t.ID, g.ID)
		}
		if abs(g.Holes-t.Holes) > tol.HoleCount {
			return &model.Error{Code: model.CodeTop, Field: "holes", StrokeID: g.ID, Message: "hole count outside tolerance", Details: map[string]any{"domain": i + 1, "measured_holes": g.Holes, "truth_holes": t.Holes, "tolerance": tol.HoleCount}}
		}
		if math.Abs(g.RingWidthPx-t.RingWidthPx) > tol.RingWidthPx {
			return &model.Error{Code: model.CodeTop, Field: "ring_width_px", StrokeID: g.ID, Message: "ring width outside tolerance", Details: map[string]any{"domain": i + 1, "measured_ring_width_px": g.RingWidthPx, "truth_ring_width_px": t.RingWidthPx, "tolerance_px": tol.RingWidthPx}}
		}
		if math.Abs(g.CentroidX-t.CentroidX) > tol.CentroidPx || math.Abs(g.CentroidY-t.CentroidY) > tol.CentroidPx {
			return topError("centroid", i+1, "centroid outside tolerance", []float64{t.CentroidX, t.CentroidY}, []float64{g.CentroidX, g.CentroidY})
		}
		if g.BBox != t.BBox {
			return topError("bbox", i+1, "bounding box mismatch", t.BBox, g.BBox)
		}
		if t.Area > 0 && math.Abs(float64(g.Area-t.Area))/float64(t.Area) > tol.AreaRelative {
			return topError("area", i+1, "area outside tolerance", t.Area, g.Area)
		}
		if cosine := SpectrumCosine(g.DirectionBins, t.DirectionBins); cosine < tol.DirectionCosine {
			return &model.Error{Code: model.CodeTop, Field: "direction_bins", StrokeID: g.ID, Message: "stroke-ring direction spectrum consistency below tolerance", Details: map[string]any{"domain": i + 1, "cosine_similarity": round64(cosine), "required": tol.DirectionCosine}}
		}
	}
	return nil
}

func SpectrumCosine(a, b []int) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		if na == nb {
			return 1
		}
		return 0
	}
	return round64(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

func FirstDifference(a, b model.TopologyReport) int {
	if a.DomainCount != b.DomainCount {
		return minInt(a.DomainCount, b.DomainCount) + 1
	}
	for i := range a.Domains {
		if i >= len(b.Domains) {
			return i + 1
		}
		aj := model.HashCanonical(a.Domains[i])
		bj := model.HashCanonical(b.Domains[i])
		if aj != bj {
			return i + 1
		}
	}
	return -1
}

func topError(field string, domain int, msg string, truth, measured any) *model.Error {
	return &model.Error{Code: model.CodeTop, Field: field, Message: msg, Original: truth, Residual: measured, Details: map[string]any{"domain": domain, "measured": measured, "truth": truth}}
}

func domainID(label int) string {
	return "D" + string(rune('0'+label/10)) + string(rune('0'+label%10))
}

func smallestKey(m map[int]struct{}) int {
	best := 0
	for k := range m {
		if best == 0 || k < best {
			best = k
		}
	}
	return best
}

func round64(v float64) float64 { return math.Round(v*1e6) / 1e6 }
func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int { return min(a, b) }
