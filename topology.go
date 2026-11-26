package maskqa

import (
	"fmt"
	"image"
)

const DefaultThreshold = 128

// ComponentStat is one extracted foreground connected domain.
type ComponentStat struct {
	DomainNo       int `json:"domain_no"`
	Area           int `json:"area"`
	CentroidXMilli int `json:"centroid_x_milli"`
	CentroidYMilli int `json:"centroid_y_milli"`
	MinX           int `json:"min_x"`
	MinY           int `json:"min_y"`
	MaxX           int `json:"max_x"`
	MaxY           int `json:"max_y"`
	Holes          int `json:"holes"`
	BandWidthMilli int `json:"band_width_milli"`

	labels []int
}

// TopologyReport contains deterministic topology extraction output.
type TopologyReport struct {
	Width      int             `json:"width"`
	Height     int             `json:"height"`
	Threshold  byte            `json:"threshold"`
	Components []ComponentStat `json:"components"`
	Binary     []byte          `json:"-"`
	Labels     []int           `json:"-"`
}

// ExtractTopology extracts 8-connected foreground components.
func ExtractTopology(mask *image.Gray, threshold byte) *TopologyReport {
	bounds := mask.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	report := &TopologyReport{Width: w, Height: h, Threshold: threshold}
	if w <= 0 || h <= 0 {
		return report
	}
	n := w * h
	report.Binary = make([]byte, n)
	report.Labels = make([]int, n)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if mask.GrayAt(bounds.Min.X+x, bounds.Min.Y+y).Y >= threshold {
				report.Binary[y*w+x] = 1
			}
		}
	}

	type point struct{ x, y int }
	neighbors := []point{{1, 0}, {-1, 0}, {0, 1}, {0, -1}, {1, 1}, {1, -1}, {-1, 1}, {-1, -1}}
	nextLabel := 0
	for start := 0; start < n; start++ {
		if report.Binary[start] == 0 || report.Labels[start] != 0 {
			continue
		}
		nextLabel++
		stat := ComponentStat{DomainNo: nextLabel, MinX: w, MinY: h}
		queue := []int{start}
		report.Labels[start] = nextLabel
		for len(queue) > 0 {
			idx := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			x, y := idx%w, idx/w
			stat.Area++
			stat.CentroidXMilli += x
			stat.CentroidYMilli += y
			if x < stat.MinX {
				stat.MinX = x
			}
			if x+1 > stat.MaxX {
				stat.MaxX = x + 1
			}
			if y < stat.MinY {
				stat.MinY = y
			}
			if y+1 > stat.MaxY {
				stat.MaxY = y + 1
			}
			for _, dn := range neighbors {
				nx, ny := x+dn.x, y+dn.y
				if nx < 0 || ny < 0 || nx >= w || ny >= h {
					continue
				}
				nidx := ny*w + nx
				if report.Binary[nidx] != 0 && report.Labels[nidx] == 0 {
					report.Labels[nidx] = nextLabel
					queue = append(queue, nidx)
				}
			}
		}
		if stat.Area > 0 {
			stat.CentroidXMilli = stat.CentroidXMilli * 1000 / stat.Area
			stat.CentroidYMilli = stat.CentroidYMilli * 1000 / stat.Area
		}
		stat.Holes = countHoles(report.Binary, w, h, stat)
		stat.BandWidthMilli = bandWidth(report.Binary, report.Labels, w, h, nextLabel)
		stat.labels = report.Labels
		report.Components = append(report.Components, stat)
	}
	return report
}

func countHoles(binary []byte, w, h int, comp ComponentStat) int {
	x0 := maxInt(0, comp.MinX-1)
	y0 := maxInt(0, comp.MinY-1)
	x1 := minInt(w, comp.MaxX+1)
	y1 := minInt(h, comp.MaxY+1)
	rw, rh := x1-x0, y1-y0
	if rw <= 2 || rh <= 2 {
		return 0
	}
	visited := make([]byte, rw*rh)
	holeCount := 0
	type point struct{ x, y int }
	for sy := 0; sy < rh; sy++ {
		for sx := 0; sx < rw; sx++ {
			ridx := sy*rw + sx
			gx, gy := x0+sx, y0+sy
			if visited[ridx] != 0 || binary[gy*w+gx] != 0 {
				continue
			}
			touchesBorder := sx == 0 || sy == 0 || sx == rw-1 || sy == rh-1
			queue := []point{{sx, sy}}
			visited[ridx] = 1
			isHole := true
			for len(queue) > 0 {
				p := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				if p.x == 0 || p.y == 0 || p.x == rw-1 || p.y == rh-1 {
					isHole = false
				}
				for _, d := range []point{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					nx, ny := p.x+d.x, p.y+d.y
					if nx < 0 || ny < 0 || nx >= rw || ny >= rh {
						isHole = false
						continue
					}
					nidx := ny*rw + nx
					gidx := (y0+ny)*w + (x0 + nx)
					if visited[nidx] == 0 && binary[gidx] == 0 {
						visited[nidx] = 1
						queue = append(queue, point{nx, ny})
						if nx == 0 || ny == 0 || nx == rw-1 || ny == rh-1 {
							touchesBorder = true
						}
					}
				}
			}
			_ = touchesBorder
			if isHole {
				holeCount++
			}
		}
	}
	return holeCount
}

func bandWidth(binary []byte, labels []int, w, h, label int) int {
	const inf = 1 << 24
	dist := make([]int, w*h)
	for i, v := range binary {
		if v == 0 {
			dist[i] = 0
		} else {
			dist[i] = inf
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if dist[idx] == 0 {
				continue
			}
			best := dist[idx]
			if x > 0 {
				best = minInt(best, dist[idx-1]+3)
			}
			if y > 0 {
				best = minInt(best, dist[idx-w]+3)
				if x > 0 {
					best = minInt(best, dist[idx-w-1]+4)
				}
				if x < w-1 {
					best = minInt(best, dist[idx-w+1]+4)
				}
			}
			dist[idx] = best
		}
	}
	maxDist := 0
	for y := h - 1; y >= 0; y-- {
		for x := w - 1; x >= 0; x-- {
			idx := y*w + x
			if dist[idx] == 0 {
				continue
			}
			best := dist[idx]
			if x < w-1 {
				best = minInt(best, dist[idx+1]+3)
			}
			if y < h-1 {
				best = minInt(best, dist[idx+w]+3)
				if x > 0 {
					best = minInt(best, dist[idx+w-1]+4)
				}
				if x < w-1 {
					best = minInt(best, dist[idx+w+1]+4)
				}
			}
			dist[idx] = best
			if labels[idx] == label && dist[idx] > maxDist && dist[idx] < inf {
				maxDist = dist[idx]
			}
		}
	}
	if maxDist == 0 {
		return 0
	}
	return 2000 * maxDist / 3
}

// Deviation describes one mismatched statistic.
type Deviation struct {
	Field    string `json:"field"`
	Expected any    `json:"expected"`
	Actual   any    `json:"actual"`
	Message  string `json:"message"`
}

// DomainReport compares one extracted domain to one truth record.
type DomainReport struct {
	DomainNo   int              `json:"domain_no"`
	TruthNo    int              `json:"truth_no"`
	Matched    bool             `json:"matched"`
	Component  ComponentStat    `json:"component"`
	Truth      DomainTruth      `json:"truth"`
	Deviations []Deviation      `json:"deviations,omitempty"`
	Direction  *DirectionReport `json:"direction,omitempty"`
}

// VerificationReport is the complete QC output.
type VerificationReport struct {
	Width       int            `json:"width"`
	Height      int            `json:"height"`
	TopologyOK  bool           `json:"topology_ok"`
	DirectionOK bool           `json:"direction_ok"`
	Domains     []DomainReport `json:"domains"`
	Issues      []Issue        `json:"issues,omitempty"`
}

// Issue is a serializable business issue.
type Issue struct {
	Code     string `json:"code"`
	Field    string `json:"field,omitempty"`
	DomainNo int    `json:"domain_no,omitempty"`
	Message  string `json:"message"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
}

func matchTruth(components []ComponentStat, truth []DomainTruth) map[int]int {
	used := map[int]bool{}
	mapping := map[int]int{}
	for _, comp := range components {
		best := -1
		bestDistance := int(^uint(0) >> 1)
		for ti, td := range truth {
			if used[ti] {
				continue
			}
			dx := comp.CentroidXMilli - td.CentroidXMilli
			dy := comp.CentroidYMilli - td.CentroidYMilli
			dist := dx*dx + dy*dy
			if dist < bestDistance {
				bestDistance = dist
				best = ti
			}
		}
		if best >= 0 {
			used[best] = true
			mapping[comp.DomainNo] = truth[best].DomainNo
		}
	}
	return mapping
}

func compareDomain(comp ComponentStat, truth DomainTruth, tol Tolerances) []Deviation {
	var deviations []Deviation
	areaDiff := absInt(comp.Area - truth.Area)
	if int64(areaDiff)*10000 > int64(maxInt(truth.Area, 1))*int64(tol.AreaRelPermyriad) {
		deviations = append(deviations, Deviation{
			Field: "area", Expected: truth.Area, Actual: comp.Area,
			Message: fmt.Sprintf("area diff %d exceeds relative tolerance %d/10000", areaDiff, tol.AreaRelPermyriad),
		})
	}
	cdx := absInt(comp.CentroidXMilli - truth.CentroidXMilli)
	cdy := absInt(comp.CentroidYMilli - truth.CentroidYMilli)
	if cdx > tol.CentroidMilliPx || cdy > tol.CentroidMilliPx {
		deviations = append(deviations, Deviation{
			Field: "centroid", Expected: [2]int{truth.CentroidXMilli, truth.CentroidYMilli},
			Actual:  [2]int{comp.CentroidXMilli, comp.CentroidYMilli},
			Message: fmt.Sprintf("centroid milli-px diff (%d,%d) exceeds %d", cdx, cdy, tol.CentroidMilliPx),
		})
	}
	for item, pair := range map[string][2]int{
		"bbox.min_x": {comp.MinX - truth.MinX, truth.MinX},
		"bbox.min_y": {comp.MinY - truth.MinY, truth.MinY},
		"bbox.max_x": {comp.MaxX - truth.MaxX, truth.MaxX},
		"bbox.max_y": {comp.MaxY - truth.MaxY, truth.MaxY},
	} {
		if absInt(pair[0]) > tol.BBoxPx {
			deviations = append(deviations, Deviation{
				Field: item, Expected: pair[1], Actual: pair[1] + pair[0],
				Message: fmt.Sprintf("bbox edge diff %d exceeds %d px", pair[0], tol.BBoxPx),
			})
		}
	}
	if absInt(comp.Holes-truth.Holes) > tol.HoleCount {
		deviations = append(deviations, Deviation{
			Field: "holes", Expected: truth.Holes, Actual: comp.Holes,
			Message: fmt.Sprintf("hole count measured=%d truth=%d", comp.Holes, truth.Holes),
		})
	}
	if absInt(comp.BandWidthMilli-truth.BandWidthMilli) > tol.BandWidthMilliPx {
		deviations = append(deviations, Deviation{
			Field: "band_width_milli", Expected: truth.BandWidthMilli, Actual: comp.BandWidthMilli,
			Message: fmt.Sprintf("band width measured=%d truth=%d tolerance=%d milli-px",
				comp.BandWidthMilli, truth.BandWidthMilli, tol.BandWidthMilliPx),
		})
	}
	return deviations
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
