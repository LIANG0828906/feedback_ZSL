package maskqc

import (
	"fmt"
	"sort"
)

type DomainAnalysis struct {
	DomainID        int     `json:"domainId"`
	CentroidX       float64 `json:"centroidX"`
	CentroidY       float64 `json:"centroidY"`
	CentroidX1000   int64   `json:"centroidX1000"`
	CentroidY1000   int64   `json:"centroidY1000"`
	Area            int     `json:"area"`
	BBox            BBox    `json:"bbox"`
	Holes           int     `json:"holes"`
	BandWidthPixels int     `json:"bandWidthPixels"`
	MaxDistance     int     `json:"maxDistance"`
}

type DomainReport struct {
	DomainAnalysis
	Expected DomainTruth `json:"expected"`
	Pass     bool        `json:"pass"`
	Issues   []string    `json:"issues,omitempty"`
}

type TopologyReport struct {
	OK      bool           `json:"ok"`
	Domains []DomainReport `json:"domains"`
	Error   *QCError       `json:"error,omitempty"`
}

func binaryAlpha(m *Mask) []bool {
	out := make([]bool, m.Width*m.Height)
	for i, v := range m.Alpha {
		out[i] = v >= ForegroundThreshold
	}
	return out
}

// AnalyzeTopology uses 4-connectivity for foreground and 4-connectivity for
// background holes. Samples are thick enough that diagonal conventions do not
// change their truth.
func AnalyzeTopology(m *Mask) ([]DomainAnalysis, []int, error) {
	if m == nil {
		return nil, nil, schemaError("mask", "nil mask")
	}
	w, h := m.Width, m.Height
	fg := binaryAlpha(m)
	labels := make([]int, w*h)
	var domains []DomainAnalysis
	type accum struct {
		area, sumX, sumY int64
		bbox             BBox
		pixels           []int
	}
	var accs []*accum
	neighbors4 := [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}

	for start := 0; start < w*h; start++ {
		if !fg[start] || labels[start] != 0 {
			continue
		}
		id := len(domains) + 1
		a := &accum{bbox: BBox{MinX: w, MinY: h, MaxX: -1, MaxY: -1}}
		accs = append(accs, a)
		queue := []int{start}
		labels[start] = id
		for qi := 0; qi < len(queue); qi++ {
			idx := queue[qi]
			x, y := idx%w, idx/w
			a.area++
			a.sumX += int64(x)
			a.sumY += int64(y)
			a.pixels = append(a.pixels, idx)
			if x < a.bbox.MinX {
				a.bbox.MinX = x
			}
			if x > a.bbox.MaxX {
				a.bbox.MaxX = x
			}
			if y < a.bbox.MinY {
				a.bbox.MinY = y
			}
			if y > a.bbox.MaxY {
				a.bbox.MaxY = y
			}
			for _, d := range neighbors4 {
				nx, ny := x+d[0], y+d[1]
				if nx < 0 || ny < 0 || nx >= w || ny >= h {
					continue
				}
				ni := ny*w + nx
				if fg[ni] && labels[ni] == 0 {
					labels[ni] = id
					queue = append(queue, ni)
				}
			}
		}
		cx1000 := (a.sumX*1000 + a.area/2) / a.area
		cy1000 := (a.sumY*1000 + a.area/2) / a.area
		domains = append(domains, DomainAnalysis{
			DomainID: id, Area: int(a.area), BBox: a.bbox,
			CentroidX1000: cx1000, CentroidY1000: cy1000,
			CentroidX: float64(cx1000) / 1000, CentroidY: float64(cy1000) / 1000,
		})
	}

	holeCountByDomain := make([]int, len(domains)+1)
	visitedBg := make([]bool, w*h)
	for start := 0; start < w*h; start++ {
		if fg[start] || visitedBg[start] {
			continue
		}
		touchesBorder := false
		adjacentDomain := 0
		queue := []int{start}
		visitedBg[start] = true
		for qi := 0; qi < len(queue); qi++ {
			idx := queue[qi]
			x, y := idx%w, idx/w
			if x == 0 || y == 0 || x == w-1 || y == h-1 {
				touchesBorder = true
			}
			for _, d := range neighbors4 {
				nx, ny := x+d[0], y+d[1]
				if nx < 0 || ny < 0 || nx >= w || ny >= h {
					continue
				}
				ni := ny*w + nx
				if fg[ni] {
					if labels[ni] > 0 {
						adjacentDomain = labels[ni]
					}
					continue
				}
				if !visitedBg[ni] {
					visitedBg[ni] = true
					queue = append(queue, ni)
				}
			}
		}
		if !touchesBorder && adjacentDomain > 0 {
			holeCountByDomain[adjacentDomain]++
		}
	}

	dist := manhattanBackgroundDistance(w, h, fg)
	for i := range domains {
		id := i + 1
		domains[i].Holes = holeCountByDomain[id]
		maxDist := 0
		for idx, label := range labels {
			if label == id && dist[idx] > maxDist {
				maxDist = dist[idx]
			}
		}
		domains[i].MaxDistance = maxDist
		domains[i].BandWidthPixels = 2*maxDist + 1
	}
	return domains, labels, nil
}

func manhattanBackgroundDistance(w, h int, fg []bool) []int {
	const inf = 1 << 20
	dist := make([]int, w*h)
	queue := make([]int, 0, w*h)
	for i, isFG := range fg {
		if !isFG {
			dist[i] = 0
			queue = append(queue, i)
		} else {
			dist[i] = inf
		}
	}
	neighbors4 := [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
	for qi := 0; qi < len(queue); qi++ {
		idx := queue[qi]
		x, y := idx%w, idx/w
		for _, d := range neighbors4 {
			nx, ny := x+d[0], y+d[1]
			if nx < 0 || ny < 0 || nx >= w || ny >= h {
				continue
			}
			ni := ny*w + nx
			if dist[ni] > dist[idx]+1 {
				dist[ni] = dist[idx] + 1
				queue = append(queue, ni)
			}
		}
	}
	return dist
}

func VerifyTopology(p *Project, m *Mask) TopologyReport {
	analyses, _, err := AnalyzeTopology(m)
	if err != nil {
		return TopologyReport{OK: false, Error: err.(*QCError)}
	}
	report := TopologyReport{OK: true}
	truthByID := make(map[int]DomainTruth, len(p.Truth.Domains))
	for _, d := range p.Truth.Domains {
		truthByID[d.DomainID] = d
	}
	usedTruth := make(map[int]bool)
	for _, got := range analyses {
		expected := matchDomainTruth(got, p.Truth.Domains, usedTruth)
		dr := DomainReport{DomainAnalysis: got, Expected: expected, Pass: true}
		if expected.DomainID == 0 {
			dr.Pass = false
			dr.Issues = append(dr.Issues, "no matching truth domain")
		} else {
			usedTruth[expected.DomainID] = true
			if got.Area != expected.Area {
				dr.Pass = false
				dr.Issues = append(dr.Issues, fmt.Sprintf("area measured=%d truth=%d", got.Area, expected.Area))
			}
			if got.BBox != expected.BBox {
				dr.Pass = false
				dr.Issues = append(dr.Issues, fmt.Sprintf("bbox measured=%+v truth=%+v", got.BBox, expected.BBox))
			}
			if abs64(got.CentroidX1000-expected.CentroidX1000) > 50 || abs64(got.CentroidY1000-expected.CentroidY1000) > 50 {
				dr.Pass = false
				dr.Issues = append(dr.Issues, fmt.Sprintf("centroid measured=(%.3f,%.3f) truth=(%.3f,%.3f)", got.CentroidX, got.CentroidY, float64(expected.CentroidX1000)/1000, float64(expected.CentroidY1000)/1000))
			}
			holeTol := expected.HoleTolerance
			if holeTol < 0 {
				holeTol = 0
			}
			if absInt(got.Holes-expected.Holes) > holeTol {
				dr.Pass = false
				dr.Issues = append(dr.Issues, fmt.Sprintf("holes measured=%d truth=%d tolerance=%d", got.Holes, expected.Holes, holeTol))
			}
			bandTolPixels := (expected.BandWidthPixels*expected.BandTolerancePM + 999) / 1000
			if expected.BandTolerancePM > 0 && bandTolPixels < 1 {
				bandTolPixels = 1
			}
			if absInt(got.BandWidthPixels-expected.BandWidthPixels) > bandTolPixels {
				dr.Pass = false
				dr.Issues = append(dr.Issues, fmt.Sprintf("bandWidth measured=%d truth=%d tolerancePixels=%d", got.BandWidthPixels, expected.BandWidthPixels, bandTolPixels))
			}
		}
		if !dr.Pass {
			report.OK = false
		}
		report.Domains = append(report.Domains, dr)
	}
	for _, td := range p.Truth.Domains {
		if usedTruth[td.DomainID] {
			continue
		}
		report.OK = false
		report.Domains = append(report.Domains, DomainReport{
			Pass: false, Expected: td,
			Issues: []string{fmt.Sprintf("truth domain %d missing in measured mask", td.DomainID)},
		})
	}
	if !report.OK {
		failures := map[string]any{}
		measured := []map[string]any{}
		for _, d := range report.Domains {
			if d.Pass {
				continue
			}
			measured = append(measured, map[string]any{
				"domainId": d.DomainID, "holes": d.Holes,
				"bandWidthPixels": d.BandWidthPixels, "issues": d.Issues,
			})
		}
		failures["domains"] = measured
		report.Error = &QCError{
			Code: CodeTopology, Field: "truth.domains", Path: "$.truth.domains",
			Message: "topology or ring tolerance check failed", Details: failures,
		}
	}
	return report
}

func matchDomainTruth(got DomainAnalysis, truth []DomainTruth, used map[int]bool) DomainTruth {
	best := DomainTruth{}
	bestDist := int64(1 << 62)
	for _, td := range truth {
		if used[td.DomainID] {
			continue
		}
		dx := got.CentroidX1000 - td.CentroidX1000
		dy := got.CentroidY1000 - td.CentroidY1000
		d := dx*dx + dy*dy
		if d < bestDist {
			bestDist = d
			best = td
		}
	}
	if best.DomainID == 0 && len(truth) > 0 {
		return truth[0]
	}
	return best
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

type ObservedSegment struct {
	SegmentTruth
}

type DirectionCheck struct {
	SegmentID  string         `json:"segmentId"`
	DomainID   int            `json:"domainId"`
	StrokeID   string         `json:"strokeId"`
	Reference  DirectionClass `json:"reference"`
	Observed   DirectionClass `json:"observed"`
	Verdict    string         `json:"verdict"`
	PointCount int            `json:"pointCount"`
	BBox       BBox           `json:"bbox"`
}

type DirectionReport struct {
	OK       bool             `json:"ok"`
	Segments []DirectionCheck `json:"segments"`
}

type segmentAccum struct {
	truth SegmentTruth
	bbox  BBox
	nxSum int
	nySum int
}

func ExtractDirectionSegments(m *Mask, strokes []Stroke) []SegmentTruth {
	domains, labels, err := AnalyzeTopology(m)
	if err != nil {
		return nil
	}
	_ = domains
	w, h := m.Width, m.Height
	fg := binaryAlpha(m)
	accMap := make(map[string]*segmentAccum)
	neighbors4 := [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if !fg[idx] {
				continue
			}
			isBoundary := false
			nx, ny := 0, 0
			for _, d := range neighbors4 {
				nxx, nyy := x+d[0], y+d[1]
				if !m.InBounds(nxx, nyy) || !fg[nyy*w+nxx] {
					isBoundary = true
					nx += d[0]
					ny += d[1]
				}
			}
			if !isBoundary {
				continue
			}
			if nx == 0 && ny == 0 {
				nx, ny = sobelOutwardNormal(m, x, y)
			}
			strokeID := NearestStrokeID(x, y, strokes)
			class := ClassifyNormal(nx, ny, strokeID, strokes)
			domainID := labels[idx]
			key := fmt.Sprintf("d%d|%s|%s", domainID, strokeID, class)
			acc := accMap[key]
			if acc == nil {
				acc = &segmentAccum{bbox: BBox{MinX: x, MinY: y, MaxX: x, MaxY: y}}
				acc.truth = SegmentTruth{
					SegmentID: fmt.Sprintf("seg-d%d-%s-%s", domainID, sanitizeStroke(strokeID), class),
					DomainID:  domainID, StrokeID: strokeID, Class: class,
					NormalX: nx, NormalY: ny,
				}
				accMap[key] = acc
			}
			acc.truth.PointCount++
			acc.nxSum += nx
			acc.nySum += ny
			if x < acc.bbox.MinX {
				acc.bbox.MinX = x
			}
			if x > acc.bbox.MaxX {
				acc.bbox.MaxX = x
			}
			if y < acc.bbox.MinY {
				acc.bbox.MinY = y
			}
			if y > acc.bbox.MaxY {
				acc.bbox.MaxY = y
			}
		}
	}

	out := make([]SegmentTruth, 0, len(accMap))
	for _, acc := range accMap {
		acc.truth.BBox = acc.bbox
		if acc.truth.PointCount > 0 {
			acc.truth.NormalX = signOr(acc.nxSum, acc.truth.NormalX)
			acc.truth.NormalY = signOr(acc.nySum, acc.truth.NormalY)
		}
		out = append(out, acc.truth)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DomainID != out[j].DomainID {
			return out[i].DomainID < out[j].DomainID
		}
		if out[i].StrokeID != out[j].StrokeID {
			return out[i].StrokeID < out[j].StrokeID
		}
		return out[i].Class < out[j].Class
	})
	_ = labels
	return out
}

func signOr(sum, fallback int) int {
	if sum > 0 {
		return 1
	}
	if sum < 0 {
		return -1
	}
	return fallback
}

func sanitizeStroke(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}

func sobelOutwardNormal(m *Mask, x, y int) (int, int) {
	at := func(dx, dy int) int { return int(m.Get(x+dx, y+dy)) }
	gx := -at(-1, -1) - 2*at(-1, 0) - at(-1, 1) + at(1, -1) + 2*at(1, 0) + at(1, 1)
	gy := -at(-1, -1) - 2*at(0, -1) - at(1, -1) + at(-1, 1) + 2*at(0, 1) + at(1, 1)
	// Gradient points toward larger alpha; outward is its negation.
	nx, ny := -gx/255, -gy/255
	if nx == 0 && ny == 0 {
		return 1, 0
	}
	return signOr(nx, 1), signOr(ny, 0)
}

func NearestStrokeID(x, y int, strokes []Stroke) string {
	id, _ := nearestStroke(x, y, strokes)
	return id
}

func nearestStroke(x, y int, strokes []Stroke) (string, Stroke) {
	var best Stroke
	bestID := "none"
	bestNum := int64(1 << 62)
	bestDen := int64(1)
	for _, s := range strokes {
		num, den := pointSegmentDistanceRational(x, y, s)
		if num*bestDen < bestNum*den {
			best = s
			bestID = s.ID
			bestNum = num
			bestDen = den
		}
	}
	return bestID, best
}

func pointSegmentDistanceRational(x, y int, s Stroke) (num int64, den int64) {
	ax, ay := int64(s.Start.X), int64(s.Start.Y)
	bx, by := int64(s.End.X), int64(s.End.Y)
	px, py := int64(x), int64(y)
	dx, dy := bx-ax, by-ay
	vx, vy := px-ax, py-ay
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return vx*vx + vy*vy, 1
	}
	dot := vx*dx + vy*dy
	if dot <= 0 {
		return vx*vx + vy*vy, 1
	}
	if dot >= l2 {
		ex, ey := px-bx, py-by
		return ex*ex + ey*ey, 1
	}
	cross := dx*vy - dy*vx
	return cross * cross, l2
}

func ClassifyNormal(nx, ny int, strokeID string, strokes []Stroke) DirectionClass {
	var stroke Stroke
	found := false
	for _, s := range strokes {
		if s.ID == strokeID {
			stroke = s
			found = true
			break
		}
	}
	if !found || (nx == 0 && ny == 0) {
		return DirUnknown
	}
	dx := int64(stroke.End.X - stroke.Start.X)
	dy := int64(stroke.End.Y - stroke.Start.Y)
	if dx == 0 && dy == 0 {
		return DirUnknown
	}
	nxi, nyi := int64(nx), int64(ny)
	dot := dx*nxi + dy*nyi
	cross := dx*nyi - dy*nxi
	if abs64(dot) >= abs64(cross) {
		if dot > 0 {
			return DirForward
		}
		if dot < 0 {
			return DirReverse
		}
	}
	if cross > 0 {
		return DirRight
	}
	if cross < 0 {
		return DirLeft
	}
	return DirUnknown
}

func VerifyDirections(p *Project, m *Mask) DirectionReport {
	observed := ExtractDirectionSegments(m, p.Strokes)
	truth := make(map[string]SegmentTruth)
	for _, s := range p.Truth.Segments {
		truth[segmentKey(s.DomainID, s.StrokeID, s.Class)] = s
	}
	obsMap := make(map[string]SegmentTruth)
	for _, s := range observed {
		obsMap[segmentKey(s.DomainID, s.StrokeID, s.Class)] = s
	}
	report := DirectionReport{OK: true}
	keys := map[string]bool{}
	for k := range truth {
		keys[k] = true
	}
	for k := range obsMap {
		keys[k] = true
	}
	orderedKeys := make([]string, 0, len(keys))
	for k := range keys {
		orderedKeys = append(orderedKeys, k)
	}
	sort.Strings(orderedKeys)
	for _, k := range orderedKeys {
		t, hasT := truth[k]
		o, hasO := obsMap[k]
		check := DirectionCheck{}
		if hasT {
			check.SegmentID = t.SegmentID
			check.DomainID = t.DomainID
			check.StrokeID = t.StrokeID
			check.Reference = t.Class
		}
		if hasO {
			check.Observed = o.Class
			check.PointCount = o.PointCount
			check.BBox = o.BBox
			if check.SegmentID == "" {
				check.SegmentID = o.SegmentID
				check.DomainID = o.DomainID
				check.StrokeID = o.StrokeID
			}
		}
		switch {
		case hasT && hasO && t.Class == o.Class:
			check.Verdict = "consistent"
		case hasT && hasO:
			check.Verdict = mismatchVerdict(t.Class, o.Class)
			report.OK = false
		case hasT && !hasO:
			check.Observed = DirUnknown
			check.Verdict = "missing_observed_segment"
			report.OK = false
		default:
			check.Reference = DirUnknown
			check.Verdict = "unexpected_observed_segment"
			report.OK = false
		}
		report.Segments = append(report.Segments, check)
	}
	return report
}

func segmentKey(domain int, stroke string, class DirectionClass) string {
	return fmt.Sprintf("%d|%s|%s", domain, stroke, class)
}

func mismatchVerdict(a, b DirectionClass) string {
	if (a == DirForward && b == DirReverse) || (a == DirReverse && b == DirForward) ||
		(a == DirLeft && b == DirRight) || (a == DirRight && b == DirLeft) {
		return "reversed"
	}
	parallel := map[DirectionClass]bool{DirForward: true, DirReverse: true}
	perp := map[DirectionClass]bool{DirLeft: true, DirRight: true}
	if parallel[a] && perp[b] || perp[a] && parallel[b] {
		return "crosswise"
	}
	return "inconsistent"
}
