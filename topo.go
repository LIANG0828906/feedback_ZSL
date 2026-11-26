package maskqc

import (
	"fmt"
	"math"
	"sort"
)

const MaskThreshold = 128

type Tolerance struct {
	CentroidMilli        int64 `json:"centroid_milli"`
	RingWidthMilli       int64 `json:"ring_width_milli"`
	HoleCount            int   `json:"hole_count"`
	AreaRelativePermille int64 `json:"area_relative_permille"`
	BBoxPixels           int   `json:"bbox_pixels"`
	DirectionCount       int   `json:"direction_count"`
}

func DefaultTolerance() Tolerance {
	return Tolerance{
		CentroidMilli:        1500, // 1.5 px in thousandths
		RingWidthMilli:       2500, // 2.5 px
		HoleCount:            0,
		AreaRelativePermille: 250, // 25%
		BBoxPixels:           2,
		DirectionCount:       2,
	}
}

func BuildTruth(m *GrayMask, strokes []Stroke) *Truth {
	report := AnalyzeMask(m, AnalyzeOptions{Tolerance: DefaultTolerance()})
	for i := range report.Domains {
		report.Domains[i].Boundary = ExtractBoundarySegments(m, report, report.Domains[i].DomainID, strokes)
	}
	domains := make([]DomainTruth, 0, len(report.Domains))
	for _, d := range report.Domains {
		domains = append(domains, DomainTruth{
			DomainID: d.DomainID, Area: d.Area,
			CentroidX1000: d.CentroidX1000, CentroidY1000: d.CentroidY1000,
			BBox: d.BBox, Holes: d.Holes, RingWidth1000: d.RingWidth1000,
			Boundary: d.Boundary,
		})
	}
	return &Truth{Mask: m, Domains: domains}
}

type AnalyzeOptions struct {
	Tolerance Tolerance `json:"tolerance"`
}

type DomainStats struct {
	DomainID      int               `json:"domain_id"`
	Area          int               `json:"area"`
	CentroidX1000 int64             `json:"centroid_x1000"`
	CentroidY1000 int64             `json:"centroid_y1000"`
	BBox          BBox              `json:"bbox"`
	Holes         int               `json:"holes"`
	RingWidth1000 int64             `json:"ring_width1000"`
	Boundary      []BoundarySegment `json:"boundary,omitempty"`
}

type TopologyReport struct {
	Width   int           `json:"width"`
	Height  int           `json:"height"`
	Domains []DomainStats `json:"domains"`
	Labels  []int         `json:"-"`
}

func isForeground(m *GrayMask, i int) bool {
	return m.Pix[i] >= MaskThreshold
}

// AnalyzeMask labels 8-connected foreground domains. Holes are 4-connected
// background components that do not touch the canvas border.
func AnalyzeMask(m *GrayMask, opts AnalyzeOptions) TopologyReport {
	if m == nil {
		return TopologyReport{}
	}
	w, h := m.Width, m.Height
	n := w * h
	labels := make([]int, n)
	var domains []DomainStats
	type accum struct {
		area, sumX, sumY, boundary int
		minX, minY, maxX, maxY     int
		pixels                     []int
	}
	var accs []*accum
	neighbors8 := [8][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}, {1, 1}, {1, -1}, {-1, 1}, {-1, -1}}

	for start := 0; start < n; start++ {
		if labels[start] != 0 || !isForeground(m, start) {
			continue
		}
		id := len(domains) + 1
		acc := &accum{minX: w, minY: h, maxX: -1, maxY: -1}
		accs = append(accs, acc)
		stack := []int{start}
		labels[start] = id
		for len(stack) > 0 {
			idx := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := idx%w, idx/w
			acc.area++
			acc.sumX += x
			acc.sumY += y
			acc.pixels = append(acc.pixels, idx)
			if x < acc.minX {
				acc.minX = x
			}
			if x > acc.maxX {
				acc.maxX = x
			}
			if y < acc.minY {
				acc.minY = y
			}
			if y > acc.maxY {
				acc.maxY = y
			}
			boundary := false
			for _, d := range neighbors8[:4] {
				nx, ny := x+d[0], y+d[1]
				if nx < 0 || ny < 0 || nx >= w || ny >= h || m.Pix[ny*w+nx] < MaskThreshold {
					boundary = true
					break
				}
			}
			if boundary {
				acc.boundary++
			}
			for _, d := range neighbors8 {
				nx, ny := x+d[0], y+d[1]
				if nx < 0 || ny < 0 || nx >= w || ny >= h {
					continue
				}
				ni := ny*w + nx
				if labels[ni] == 0 && isForeground(m, ni) {
					labels[ni] = id
					stack = append(stack, ni)
				}
			}
		}
		d := DomainStats{DomainID: id, Area: acc.area, BBox: BBox{acc.minX, acc.minY, acc.maxX, acc.maxY}}
		d.CentroidX1000 = int64((acc.sumX*1000 + acc.area/2) / acc.area)
		d.CentroidY1000 = int64((acc.sumY*1000 + acc.area/2) / acc.area)
		if acc.boundary > 0 {
			d.RingWidth1000 = int64(2 * acc.area * 1000 / acc.boundary)
		}
		domains = append(domains, d)
	}

	// Background 4-connectivity hole labeling.
	bgLabels := make([]int, n)
	holeCounts := make(map[int]int)
	neighbors4 := neighbors8[:4]
	for start := 0; start < n; start++ {
		if bgLabels[start] != 0 || isForeground(m, start) {
			continue
		}
		bgID := start + 1
		stack := []int{start}
		bgLabels[start] = bgID
		touchesBorder := false
		adjacent := map[int]int{}
		for len(stack) > 0 {
			idx := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
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
				if isForeground(m, ni) {
					adjacent[labels[ni]]++
					continue
				}
				if bgLabels[ni] == 0 {
					bgLabels[ni] = bgID
					stack = append(stack, ni)
				}
			}
		}
		if !touchesBorder && len(adjacent) > 0 {
			bestLabel, bestCount := 0, -1
			for label, count := range adjacent {
				if count > bestCount {
					bestLabel, bestCount = label, count
				}
			}
			if bestLabel > 0 {
				holeCounts[bestLabel]++
			}
		}
	}
	for i := range domains {
		domains[i].Holes = holeCounts[domains[i].DomainID]
	}

	report := TopologyReport{Width: w, Height: h, Domains: domains, Labels: labels}
	for i := range report.Domains {
		report.Domains[i].Boundary = ExtractBoundarySegments(m, report, report.Domains[i].DomainID, nil)
	}
	return report
}

// ClassifyDirection compares an integer normal vector with the stroke travel
// vector using squared cosine thresholds; no floating point is used.
func ClassifyDirection(normal Point, stroke Stroke) DirClass {
	vx := stroke.End.X - stroke.Start.X
	vy := stroke.End.Y - stroke.Start.Y
	if vx == 0 && vy == 0 {
		return DirMixed
	}
	nx, ny := normal.X, normal.Y
	n2 := nx*nx + ny*ny
	v2 := vx*vx + vy*vy
	if n2 == 0 || v2 == 0 {
		return DirMixed
	}
	dot := nx*vx + ny*vy
	dot2 := dot * dot
	// |cos| <= 1/4.
	if dot2*16 <= n2*v2 {
		return DirTangent
	}
	// |cos| >= 3/4.
	if dot2*16 >= 9*n2*v2 {
		if dot >= 0 {
			return DirNormalAlign
		}
		return DirNormalOppose
	}
	return DirDiagonal
}

func distancePointSegment2(px, py int, s Stroke) int64 {
	ax, ay := s.Start.X, s.Start.Y
	bx, by := s.End.X, s.End.Y
	vx, vy := bx-ax, by-ay
	wx, wy := px-ax, py-ay
	l2 := int64(vx*vx + vy*vy)
	if l2 == 0 {
		dx, dy := px-ax, py-ay
		return int64(dx*dx + dy*dy)
	}
	dot := int64(wx*vx + wy*vy)
	if dot <= 0 {
		return int64(wx*wx + wy*wy)
	}
	if dot >= l2 {
		dx, dy := px-bx, py-by
		return int64(dx*dx + dy*dy)
	}
	cross := int64(wx*vy - wy*vx)
	return cross * cross / l2
}

func nearestStrokeID(p Point, strokes []Stroke) string {
	best := ""
	var bestDist int64 = math.MaxInt64
	for _, s := range strokes {
		d := distancePointSegment2(p.X, p.Y, s)
		if d < bestDist {
			bestDist = d
			best = s.ID
		}
	}
	return best
}

// ExtractBoundarySegments groups horizontal/vertical exposed edges into
// deterministic segments. It is intentionally scan-order stable.
func ExtractBoundarySegments(m *GrayMask, report TopologyReport, domainID int, strokes []Stroke) []BoundarySegment {
	if m == nil || len(report.Labels) != m.Width*m.Height {
		return nil
	}
	w, h := m.Width, m.Height
	var segments []BoundarySegment
	segIndex := 0
	addRun := func(points []Point, tangent, normal Point) {
		if len(points) < 2 {
			return
		}
		mid := points[len(points)/2]
		b := BoundarySegment{
			DomainID: domainID, Index: segIndex, Points: points,
			Tangent: tangent, Normal: normal,
		}
		if len(strokes) > 0 {
			b.StrokeID = nearestStrokeID(mid, strokes)
			if st, ok := findStroke(strokes, b.StrokeID); ok {
				b.Category = ClassifyDirection(normal, st)
			}
		}
		segIndex++
		segments = append(segments, b)
	}

	// Horizontal top and bottom edge runs.
	for y := 0; y < h; y++ {
		for _, edge := range []struct{ dy, ny int }{{-1, -1}, {1, 1}} {
			var run []Point
			for x := 0; x <= w; x++ {
				exposed := false
				if x < w && report.Labels[y*w+x] == domainID {
					ny := y + edge.dy
					exposed = ny < 0 || ny >= h || report.Labels[ny*w+x] != domainID
				}
				if exposed {
					run = append(run, Point{X: x, Y: y})
				} else if len(run) > 0 {
					addRun(run, Point{X: 1, Y: 0}, Point{X: 0, Y: edge.ny})
					run = nil
				}
			}
		}
	}
	// Vertical left and right edge runs.
	for x := 0; x < w; x++ {
		for _, edge := range []struct{ dx, nx int }{{-1, -1}, {1, 1}} {
			var run []Point
			for y := 0; y <= h; y++ {
				exposed := false
				if y < h && report.Labels[y*w+x] == domainID {
					nx := x + edge.dx
					exposed = nx < 0 || nx >= w || report.Labels[y*w+nx] != domainID
				}
				if exposed {
					run = append(run, Point{X: x, Y: y})
				} else if len(run) > 0 {
					addRun(run, Point{X: 0, Y: 1}, Point{X: edge.nx, Y: 0})
					run = nil
				}
			}
		}
	}
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].Index < segments[j].Index })
	return segments
}

func findStroke(strokes []Stroke, id string) (Stroke, bool) {
	for _, s := range strokes {
		if s.ID == id {
			return s, true
		}
	}
	return Stroke{}, false
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// VerifyTopology compares accumulated mask statistics with generator truth.
func VerifyTopology(accumulated *GrayMask, truth *Truth, strokes []Stroke, tol Tolerance) (*TopologyReport, *BizError) {
	report := AnalyzeMask(accumulated, AnalyzeOptions{Tolerance: tol})
	if truth == nil {
		return &report, NewError(CodeTop, "missing truth")
	}
	var mismatches []Mismatch
	if len(report.Domains) != len(truth.Domains) {
		mismatches = append(mismatches, Mismatch{
			Domain: 0, Item: "domain_count",
			Want: fmt.Sprint(len(truth.Domains)), Got: fmt.Sprint(len(report.Domains)),
		})
	}
	n := min(len(report.Domains), len(truth.Domains))
	for i := 0; i < n; i++ {
		got := report.Domains[i]
		want := truth.Domains[i]
		domainID := want.DomainID
		if absInt64(got.CentroidX1000-want.CentroidX1000) > tol.CentroidMilli {
			mismatches = append(mismatches, Mismatch{domainID, "centroid_x", fmt.Sprint(want.CentroidX1000), fmt.Sprint(got.CentroidX1000)})
		}
		if absInt64(got.CentroidY1000-want.CentroidY1000) > tol.CentroidMilli {
			mismatches = append(mismatches, Mismatch{domainID, "centroid_y", fmt.Sprint(want.CentroidY1000), fmt.Sprint(got.CentroidY1000)})
		}
		if want.Area > 0 && absInt64(int64(got.Area-want.Area))*1000 > int64(want.Area)*tol.AreaRelativePermille {
			mismatches = append(mismatches, Mismatch{domainID, "area", fmt.Sprint(want.Area), fmt.Sprint(got.Area)})
		}
		if absInt64(int64(got.Holes-want.Holes)) > int64(tol.HoleCount) {
			mismatches = append(mismatches, Mismatch{domainID, "holes", fmt.Sprint(want.Holes), fmt.Sprint(got.Holes)})
		}
		if absInt64(got.RingWidth1000-want.RingWidth1000) > tol.RingWidthMilli {
			mismatches = append(mismatches, Mismatch{domainID, "ring_width", fmt.Sprint(want.RingWidth1000), fmt.Sprint(got.RingWidth1000)})
		}
		if absInt64(int64(got.BBox.MinX-want.BBox.MinX)) > int64(tol.BBoxPixels) ||
			absInt64(int64(got.BBox.MinY-want.BBox.MinY)) > int64(tol.BBoxPixels) ||
			absInt64(int64(got.BBox.MaxX-want.BBox.MaxX)) > int64(tol.BBoxPixels) ||
			absInt64(int64(got.BBox.MaxY-want.BBox.MaxY)) > int64(tol.BBoxPixels) {
			mismatches = append(mismatches, Mismatch{domainID, "bbox", fmt.Sprintf("%+v", want.BBox), fmt.Sprintf("%+v", got.BBox)})
		}
		wantHist := directionHistogram(want.Boundary)
		gotSegments := ExtractBoundarySegments(accumulated, report, got.DomainID, strokes)
		gotHist := directionHistogram(gotSegments)
		for _, cat := range []DirClass{DirTangent, DirNormalAlign, DirNormalOppose, DirDiagonal, DirMixed} {
			if absInt64(int64(gotHist[cat]-wantHist[cat])) > int64(tol.DirectionCount) {
				mismatches = append(mismatches, Mismatch{
					domainID, "direction:" + string(cat),
					fmt.Sprint(wantHist[cat]), fmt.Sprint(gotHist[cat]),
				})
			}
		}
	}
	if len(mismatches) > 0 {
		return &report, &BizError{
			Code: CodeTop, Message: "topology or direction verification failed",
			Details: mismatches, ChunkID: -1,
		}
	}
	return &report, nil
}

func directionHistogram(segments []BoundarySegment) map[DirClass]int {
	out := map[DirClass]int{}
	for _, s := range segments {
		out[s.Category]++
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
