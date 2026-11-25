package maskqc

import (
	"fmt"
	"image"
	"math"
)

type TopologyTolerance struct {
	AreaAbs     int     `json:"area_abs"`
	CentroidPx  float64 `json:"centroid_px"`
	BandWidthPx float64 `json:"band_width_px"`
	AngleDeg    float64 `json:"angle_deg"`
	Holes       int     `json:"holes"`
	BoundsPx    int     `json:"bounds_px"`
}

func DefaultTopologyTolerance() TopologyTolerance {
	return TopologyTolerance{
		AreaAbs:     8,
		CentroidPx:  1.5,
		BandWidthPx: 1.75,
		AngleDeg:    2.5,
		Holes:       0,
		BoundsPx:    2,
	}
}

// AnalyzeGrayMask extracts deterministic 4-connected foreground domains.
func AnalyzeGrayMask(mask *image.Gray, threshold uint8) ([]DomainMetric, error) {
	if mask == nil {
		return nil, fmt.Errorf("nil mask")
	}
	b := mask.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return []DomainMetric{}, nil
	}
	foreground := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			foreground[y*w+x] = mask.GrayAt(b.Min.X+x, b.Min.Y+y).Y >= threshold
		}
	}
	return analyzeBinary(foreground, w, h), nil
}

func analyzeBinary(fg []bool, w, h int) []DomainMetric {
	labels := make([]int, w*h)
	var domains []DomainMetric
	neighbors := [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}

	for start := 0; start < w*h; start++ {
		if !fg[start] || labels[start] != 0 {
			continue
		}
		id := len(domains) + 1
		queue := []int{start}
		labels[start] = id
		metric := DomainMetric{DomainID: id, Bounds: Rect{MinX: w, MinY: h, MaxX: -1, MaxY: -1}}
		sumX, sumY := 0.0, 0.0
		m20, m02, m11 := 0.0, 0.0, 0.0

		for qi := 0; qi < len(queue); qi++ {
			idx := queue[qi]
			x, y := idx%w, idx/w
			metric.Area++
			sumX += float64(x)
			sumY += float64(y)
			if x < metric.Bounds.MinX {
				metric.Bounds.MinX = x
			}
			if y < metric.Bounds.MinY {
				metric.Bounds.MinY = y
			}
			if x+1 > metric.Bounds.MaxX {
				metric.Bounds.MaxX = x + 1
			}
			if y+1 > metric.Bounds.MaxY {
				metric.Bounds.MaxY = y + 1
			}
			for _, n := range neighbors {
				nx, ny := x+n[0], y+n[1]
				if nx < 0 || nx >= w || ny < 0 || ny >= h {
					continue
				}
				ni := ny*w + nx
				if fg[ni] && labels[ni] == 0 {
					labels[ni] = id
					queue = append(queue, ni)
				}
			}
		}

		cx := sumX / float64(metric.Area)
		cy := sumY / float64(metric.Area)
		for _, idx := range queue {
			x, y := idx%w, idx/w
			dx := float64(x) - cx
			dy := float64(y) - cy
			m20 += dx * dx
			m02 += dy * dy
			m11 += dx * dy
		}
		metric.CentroidX = cx
		metric.CentroidY = cy
		metric.PixelCount = metric.Area
		metric.Holes = countHoles(fg, metric.Bounds, w, h)
		metric.BandWidth = estimateBandWidth(fg, labels, id, queue, w, h)
		metric.AngleDeg = pcaAngle(m20, m02, m11)
		domains = append(domains, metric)
	}
	return domains
}

func countHoles(fg []bool, bounds Rect, w, h int) int {
	bw, bh := bounds.Width(), bounds.Height()
	if bw <= 2 || bh <= 2 {
		return 0
	}
	visited := make([]bool, bw*bh)
	queue := make([]int, 0, bw+bh)

	localIndex := func(x, y int) int { return (y-bounds.MinY)*bw + (x - bounds.MinX) }
	mark := func(x, y int) {
		if x < bounds.MinX || x >= bounds.MaxX || y < bounds.MinY || y >= bounds.MaxY {
			return
		}
		li := localIndex(x, y)
		if visited[li] || fg[y*w+x] {
			return
		}
		visited[li] = true
		queue = append(queue, li)
	}

	for x := bounds.MinX; x < bounds.MaxX; x++ {
		mark(x, bounds.MinY)
		mark(x, bounds.MaxY-1)
	}
	for y := bounds.MinY; y < bounds.MaxY; y++ {
		mark(bounds.MinX, y)
		mark(bounds.MaxX-1, y)
	}

	neighbors := [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
	for qi := 0; qi < len(queue); qi++ {
		li := queue[qi]
		lx, ly := li%bw, li/bw
		x, y := bounds.MinX+lx, bounds.MinY+ly
		for _, n := range neighbors {
			nx, ny := x+n[0], y+n[1]
			if nx < bounds.MinX || nx >= bounds.MaxX || ny < bounds.MinY || ny >= bounds.MaxY {
				continue
			}
			nli := localIndex(nx, ny)
			if visited[nli] || fg[ny*w+nx] {
				continue
			}
			visited[nli] = true
			queue = append(queue, nli)
		}
	}

	holes := 0
	for y := bounds.MinY; y < bounds.MaxY; y++ {
		for x := bounds.MinX; x < bounds.MaxX; x++ {
			li := localIndex(x, y)
			if visited[li] || fg[y*w+x] {
				continue
			}
			holes++
			visited[li] = true
			queue = queue[:0]
			queue = append(queue, li)
			for qi := 0; qi < len(queue); qi++ {
				cur := queue[qi]
				lx, ly := cur%bw, cur/bw
				cx, cy := bounds.MinX+lx, bounds.MinY+ly
				for _, n := range neighbors {
					nx, ny := cx+n[0], cy+n[1]
					if nx < bounds.MinX || nx >= bounds.MaxX || ny < bounds.MinY || ny >= bounds.MaxY {
						continue
					}
					ni := localIndex(nx, ny)
					if visited[ni] || fg[ny*w+nx] {
						continue
					}
					visited[ni] = true
					queue = append(queue, ni)
				}
			}
		}
	}
	return holes
}

func estimateBandWidth(fg []bool, labels []int, domainID int, pixels []int, w, h int) float64 {
	dist := make([]int, w*h)
	queue := make([]int, 0, w*h)
	for i, isFG := range fg {
		if !isFG {
			dist[i] = 0
			queue = append(queue, i)
		} else {
			dist[i] = -1
		}
	}
	neighbors := [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
	for qi := 0; qi < len(queue); qi++ {
		idx := queue[qi]
		x, y := idx%w, idx/w
		for _, n := range neighbors {
			nx, ny := x+n[0], y+n[1]
			if nx < 0 || nx >= w || ny < 0 || ny >= h {
				continue
			}
			ni := ny*w + nx
			if dist[ni] != -1 {
				continue
			}
			dist[ni] = dist[idx] + 1
			queue = append(queue, ni)
		}
	}
	maxDistance := 0
	for _, idx := range pixels {
		if labels[idx] == domainID && dist[idx] > maxDistance {
			maxDistance = dist[idx]
		}
	}
	if maxDistance <= 0 {
		return 0
	}
	return float64(2 * maxDistance)
}

func pcaAngle(m20, m02, m11 float64) float64 {
	angle := 0.5 * math.Atan2(2*m11, m20-m02) * 180 / math.Pi
	angle = math.Mod(angle, 180)
	if angle < 0 {
		angle += 180
	}
	return angle
}

// AngleDifferenceDeg returns the smallest unsigned difference on an unoriented axis.
func AngleDifferenceDeg(a, b float64) float64 {
	d := math.Abs(math.Mod(a-b, 180))
	if d > 90 {
		d = 180 - d
	}
	return d
}

// VerifyTopology compares measured domains against generator truth.
func VerifyTopology(measured, truth []DomainMetric, tol TopologyTolerance) *Error {
	if len(measured) != len(truth) {
		return &Error{
			Code:    CodeTop,
			Message: "foreground domain count does not match truth",
			Field:   "domains",
			Details: map[string]any{"measured_count": len(measured), "truth_count": len(truth), "measured": measured},
		}
	}
	var bad []map[string]any
	for i := range truth {
		got := measured[i]
		want := truth[i]
		problems := map[string]any{}
		if got.Holes-want.Holes > tol.Holes || want.Holes-got.Holes > tol.Holes {
			problems["holes"] = map[string]int{"measured": got.Holes, "truth": want.Holes}
		}
		if math.Abs(got.BandWidth-want.BandWidth) > tol.BandWidthPx {
			problems["band_width"] = map[string]float64{"measured": got.BandWidth, "truth": want.BandWidth}
		}
		if d := AngleDifferenceDeg(got.AngleDeg, want.AngleDeg); d > tol.AngleDeg {
			problems["angle_deg"] = map[string]float64{"measured": got.AngleDeg, "truth": want.AngleDeg, "difference_deg": d}
		}
		if absInt(got.Area-want.Area) > tol.AreaAbs {
			problems["area"] = map[string]int{"measured": got.Area, "truth": want.Area}
		}
		if math.Hypot(got.CentroidX-want.CentroidX, got.CentroidY-want.CentroidY) > tol.CentroidPx {
			problems["centroid"] = map[string]float64{"measured_x": got.CentroidX, "measured_y": got.CentroidY, "truth_x": want.CentroidX, "truth_y": want.CentroidY}
		}
		if absInt(got.Bounds.MinX-want.Bounds.MinX) > tol.BoundsPx ||
			absInt(got.Bounds.MinY-want.Bounds.MinY) > tol.BoundsPx ||
			absInt(got.Bounds.MaxX-want.Bounds.MaxX) > tol.BoundsPx ||
			absInt(got.Bounds.MaxY-want.Bounds.MaxY) > tol.BoundsPx {
			problems["bounds"] = map[string]Rect{"measured": got.Bounds, "truth": want.Bounds}
		}
		if len(problems) > 0 {
			problems["domain_id"] = i + 1
			problems["measured"] = got
			bad = append(bad, problems)
		}
	}
	if len(bad) > 0 {
		return &Error{
			Code:    CodeTop,
			Message: fmt.Sprintf("%d topology domain(s) exceed tolerance", len(bad)),
			Field:   "domains",
			Details: map[string]any{"domains": bad},
		}
	}
	return nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
