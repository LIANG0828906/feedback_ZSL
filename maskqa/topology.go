package maskqa

import "math"
import "sort"

const (
	CentroidTolerancePx  = 0.05
	RingWidthTolerancePx = 2.0
)

// TopologyVerification contains measured domains and any tolerance issues.
type TopologyVerification struct {
	OK      bool           `json:"ok"`
	Domains []DomainMetric `json:"domains"`
	Issues  []DomainIssue  `json:"issues,omitempty"`
}

// AnalyzeTopology extracts 4-connected foreground domains and their metrics.
func AnalyzeTopology(m *Mask) []DomainMetric {
	if m == nil {
		return nil
	}
	labels := make([]int, m.Width*m.Height)
	var domains []DomainMetric
	nextLabel := 1

	for startY := 0; startY < m.Height; startY++ {
		for startX := 0; startX < m.Width; startX++ {
			idx := startY*m.Width + startX
			if m.Pix[idx] == 0 || labels[idx] != 0 {
				continue
			}
			label := nextLabel
			nextLabel++
			queue := []Point{{X: startX, Y: startY}}
			labels[idx] = label
			pixels := []Point{}
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				pixels = append(pixels, p)
				for _, d := range []Point{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					np := Point{X: p.X + d.X, Y: p.Y + d.Y}
					if !m.InBounds(np.X, np.Y) {
						continue
					}
					ni := np.Y*m.Width + np.X
					if m.Pix[ni] != 0 && labels[ni] == 0 {
						labels[ni] = label
						queue = append(queue, np)
					}
				}
			}
			domains = append(domains, measureDomain(m, label, pixels))
		}
	}
	return domains
}

func measureDomain(m *Mask, id int, pixels []Point) DomainMetric {
	d := DomainMetric{DomainID: id, Area: len(pixels)}
	d.MinX = m.Width
	d.MinY = m.Height
	var sumX, sumY int64
	for _, p := range pixels {
		sumX += int64(p.X)
		sumY += int64(p.Y)
		d.MinX = minInt(d.MinX, p.X)
		d.MinY = minInt(d.MinY, p.Y)
		d.MaxX = maxInt(d.MaxX, p.X)
		d.MaxY = maxInt(d.MaxY, p.Y)
	}
	d.CentroidX = float64(sumX) / float64(len(pixels))
	d.CentroidY = float64(sumY) / float64(len(pixels))
	d.Holes = countHoles(pixels, d)
	d.RingWidthPx = measureBandWidth(m, pixels)
	return d
}

func countHoles(pixels []Point, d DomainMetric) int {
	w := d.MaxX - d.MinX + 3
	h := d.MaxY - d.MinY + 3
	loc := make([]byte, w*h)
	for _, p := range pixels {
		x := p.X - d.MinX + 1
		y := p.Y - d.MinY + 1
		loc[y*w+x] = 1
	}
	visited := make([]byte, w*h)
	queue := []Point{{0, 0}}
	visited[0] = 1
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, delta := range []Point{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			np := Point{X: p.X + delta.X, Y: p.Y + delta.Y}
			if np.X < 0 || np.Y < 0 || np.X >= w || np.Y >= h {
				continue
			}
			i := np.Y*w + np.X
			if visited[i] != 0 || loc[i] != 0 {
				continue
			}
			visited[i] = 1
			queue = append(queue, np)
		}
	}
	holes := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if loc[i] != 0 || visited[i] != 0 {
				continue
			}
			holes++
			queue = []Point{{X: x, Y: y}}
			visited[i] = 1
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				for _, delta := range []Point{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					np := Point{X: p.X + delta.X, Y: p.Y + delta.Y}
					if np.X < 0 || np.Y < 0 || np.X >= w || np.Y >= h {
						continue
					}
					ni := np.Y*w + np.X
					if visited[ni] == 0 && loc[ni] == 0 {
						visited[ni] = 1
						queue = append(queue, np)
					}
				}
			}
		}
	}
	return holes
}

func measureBandWidth(m *Mask, pixels []Point) float64 {
	distances := make([]float64, 0, len(pixels))
	for _, p := range pixels {
		best := -1
		search := 18
		for radius := 1; radius <= search; radius++ {
			found := false
			for dy := -radius; dy <= radius; dy++ {
				for dx := -radius; dx <= radius; dx++ {
					if maxInt(absInt(dx), absInt(dy)) != radius {
						continue
					}
					x, y := p.X+dx, p.Y+dy
					if !m.InBounds(x, y) || m.Get(x, y) == 0 {
						d2 := dx*dx + dy*dy
						if best < 0 || d2 < best {
							best = d2
						}
						found = true
					}
				}
			}
			if found {
				break
			}
		}
		if best < 0 {
			// Fallback for broad filled regions; samples are stroke bands and normally do not reach here.
			best = nearestBackgroundSquaredFull(m, p)
		}
		distances = append(distances, math.Sqrt(float64(best)))
	}
	sort.Float64s(distances)
	idx := int(0.90 * float64(len(distances)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(distances) {
		idx = len(distances) - 1
	}
	return 2 * distances[idx]
}

func nearestBackgroundSquaredFull(m *Mask, p Point) int {
	best := (m.Width + m.Height) * (m.Width + m.Height)
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			if m.Get(x, y) == 0 {
				dx := x - p.X
				dy := y - p.Y
				d2 := dx*dx + dy*dy
				if d2 < best {
					best = d2
				}
			}
		}
	}
	return best
}

// VerifyTopology compares extracted domains against the analytic truth manifest.
func VerifyTopology(actual *Mask, truth *TruthManifest) (*TopologyVerification, error) {
	measured := AnalyzeTopology(actual)
	rep := &TopologyVerification{OK: true, Domains: measured}
	if len(measured) != len(truth.Domains) {
		rep.OK = false
		issue := DomainIssue{DomainID: 0, Field: "domain_count", Expected: float64(len(truth.Domains)), Actual: float64(len(measured)), Message: "connected domain count mismatch"}
		rep.Issues = append(rep.Issues, issue)
	}
	for _, exp := range truth.Domains {
		var act *DomainMetric
		for i := range measured {
			if measured[i].DomainID == exp.DomainID {
				act = &measured[i]
				break
			}
		}
		if act == nil {
			rep.OK = false
			rep.Issues = append(rep.Issues, DomainIssue{DomainID: exp.DomainID, Field: "domain", Expected: 1, Actual: 0, Message: "truth domain missing"})
			continue
		}
		if act.Area != exp.Area {
			rep.Issues = append(rep.Issues, DomainIssue{DomainID: exp.DomainID, Field: "area", Expected: float64(exp.Area), Actual: float64(act.Area), Tolerance: 0, Message: "area mismatch"})
		}
		if act.MinX != exp.MinX || act.MinY != exp.MinY || act.MaxX != exp.MaxX || act.MaxY != exp.MaxY {
			rep.Issues = append(rep.Issues, DomainIssue{DomainID: exp.DomainID, Field: "bounding_box", Expected: float64(exp.MinX*1000000 + exp.MinY*10000 + exp.MaxX*100 + exp.MaxY), Actual: float64(act.MinX*1000000 + act.MinY*10000 + act.MaxX*100 + act.MaxY), Message: "bounding box mismatch"})
		}
		if math.Abs(act.CentroidX-exp.CentroidX) > CentroidTolerancePx || math.Abs(act.CentroidY-exp.CentroidY) > CentroidTolerancePx {
			rep.Issues = append(rep.Issues, DomainIssue{DomainID: exp.DomainID, Field: "centroid", Expected: exp.CentroidX, Actual: act.CentroidX, Tolerance: CentroidTolerancePx, Message: "centroid mismatch"})
		}
		if act.Holes != exp.Holes {
			rep.Issues = append(rep.Issues, DomainIssue{DomainID: exp.DomainID, Field: "holes", Expected: float64(exp.Holes), Actual: float64(act.Holes), Tolerance: 0, Message: "hole count mismatch"})
		}
		widthDiff := math.Abs(act.RingWidthPx - exp.RingWidthPx)
		if widthDiff > RingWidthTolerancePx {
			rep.Issues = append(rep.Issues, DomainIssue{DomainID: exp.DomainID, Field: "ring_width_px", Expected: exp.RingWidthPx, Actual: act.RingWidthPx, Tolerance: RingWidthTolerancePx, Message: "ring/stroke band width outside tolerance"})
		}
	}
	if len(rep.Issues) > 0 {
		rep.OK = false
		return rep, &Error{Code: CodeTOP, Message: "topology verification failed", Domains: rep.Issues}
	}
	return rep, nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
