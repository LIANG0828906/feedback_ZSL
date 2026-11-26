package strokelab

import (
	"bytes"
	"fmt"
	"sort"
)

// DomainStats is the normalized topology record stored in truth and recomputed
// from the accumulated mask.
type DomainStats struct {
	ID             int  `json:"id"`
	Area           int  `json:"area"`
	CentroidXMilli int  `json:"centroid_x_milli"`
	CentroidYMilli int  `json:"centroid_y_milli"`
	BBox           Rect `json:"bbox"`
	Holes          int  `json:"holes"`
	RingWidthMilli int  `json:"ring_width_milli"`
	BoundaryPixels int  `json:"boundary_pixels"`
}

type domainPixels struct {
	DomainStats
	pixels []int
}

type ExtractResult struct {
	Domains []domainPixels
	Labels  []int
}

// ExtractDomains uses deterministic row-major 4-connectivity. Ring width is
// represented as milli-pixels: 2000*area/boundary-pixels.
func ExtractDomains(mask []uint8, width, height, threshold int) ExtractResult {
	if width <= 0 || height <= 0 || len(mask) != width*height {
		return ExtractResult{Labels: make([]int, max(0, width*height))}
	}
	if threshold <= 0 {
		threshold = DefaultMaskThreshold
	}
	labels := make([]int, len(mask))
	var domains []domainPixels
	queue := make([]int, 0, 64)
	for start := 0; start < len(mask); start++ {
		if labels[start] != 0 || int(mask[start]) < threshold {
			continue
		}
		id := len(domains) + 1
		labels[start] = id
		queue = queue[:0]
		queue = append(queue, start)
		pixels := make([]int, 0, 32)
		minX, minY := width, height
		maxX, maxY := -1, -1
		var sumX, sumY int64
		for head := 0; head < len(queue); head++ {
			idx := queue[head]
			pixels = append(pixels, idx)
			x, y := idx%width, idx/width
			sumX += int64(x)
			sumY += int64(y)
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
			if x > 0 {
				ni := idx - 1
				if labels[ni] == 0 && int(mask[ni]) >= threshold {
					labels[ni] = id
					queue = append(queue, ni)
				}
			}
			if x+1 < width {
				ni := idx + 1
				if labels[ni] == 0 && int(mask[ni]) >= threshold {
					labels[ni] = id
					queue = append(queue, ni)
				}
			}
			if y > 0 {
				ni := idx - width
				if labels[ni] == 0 && int(mask[ni]) >= threshold {
					labels[ni] = id
					queue = append(queue, ni)
				}
			}
			if y+1 < height {
				ni := idx + width
				if labels[ni] == 0 && int(mask[ni]) >= threshold {
					labels[ni] = id
					queue = append(queue, ni)
				}
			}
		}
		area := len(pixels)
		boundary := 0
		for _, idx := range pixels {
			x, y := idx%width, idx/width
			if x == 0 || y == 0 || x+1 == width || y+1 == height ||
				int(mask[idx-1]) < threshold || int(mask[idx+1]) < threshold ||
				int(mask[idx-width]) < threshold || int(mask[idx+width]) < threshold {
				boundary++
			}
		}
		ring := 0
		if boundary > 0 {
			ring = int((int64(area)*2000 + int64(boundary)/2) / int64(boundary))
		}
		holes := countHoles(mask, labels, id, width, height, threshold,
			Rect{X: minX, Y: minY, W: maxX - minX + 1, H: maxY - minY + 1})
		domains = append(domains, domainPixels{
			DomainStats: DomainStats{
				ID:             id,
				Area:           area,
				CentroidXMilli: int((sumX*1000 + int64(area)/2) / int64(area)),
				CentroidYMilli: int((sumY*1000 + int64(area)/2) / int64(area)),
				BBox:           Rect{X: minX, Y: minY, W: maxX - minX + 1, H: maxY - minY + 1},
				Holes:          holes,
				RingWidthMilli: ring,
				BoundaryPixels: boundary,
			},
			pixels: pixels,
		})
	}
	return ExtractResult{Domains: domains, Labels: labels}
}

func countHoles(mask []uint8, labels []int, domainID, width, height, threshold int, bbox Rect) int {
	if bbox.W <= 2 || bbox.H <= 2 {
		return 0
	}
	visited := make([]bool, bbox.W*bbox.H)
	queue := make([]int, 0, 16)
	mark := func(x, y int) {
		if x < bbox.X || x >= bbox.X+bbox.W || y < bbox.Y || y >= bbox.Y+bbox.H {
			return
		}
		lx, ly := x-bbox.X, y-bbox.Y
		li := ly*bbox.W + lx
		if visited[li] {
			return
		}
		gi := y*width + x
		if labels[gi] == domainID || int(mask[gi]) >= threshold {
			visited[li] = true
			return
		}
		visited[li] = true
		queue = append(queue, li)
	}
	for x := bbox.X; x < bbox.X+bbox.W; x++ {
		mark(x, bbox.Y)
		mark(x, bbox.Y+bbox.H-1)
	}
	for y := bbox.Y; y < bbox.Y+bbox.H; y++ {
		mark(bbox.X, y)
		mark(bbox.X+bbox.W-1, y)
	}
	for head := 0; head < len(queue); head++ {
		li := queue[head]
		lx, ly := li%bbox.W, li/bbox.W
		x, y := bbox.X+lx, bbox.Y+ly
		mark(x-1, y)
		mark(x+1, y)
		mark(x, y-1)
		mark(x, y+1)
	}
	holes := 0
	for y := bbox.Y; y < bbox.Y+bbox.H; y++ {
		for x := bbox.X; x < bbox.X+bbox.W; x++ {
			li := (y-bbox.Y)*bbox.W + (x - bbox.X)
			gi := y*width + x
			if !visited[li] && labels[gi] != domainID && int(mask[gi]) < threshold {
				holes++
				floodBackground(mask, labels, visited, domainID, width, height, threshold, bbox, x, y)
			}
		}
	}
	// The loop above counts the first unvisited pixel of every enclosed
	// background component exactly once because floodBackground marks it.
	return holes
}

func floodBackground(mask []uint8, labels []int, visited []bool, domainID, width, height, threshold int, bbox Rect, sx, sy int) {
	seed := (sy-bbox.Y)*bbox.W + (sx - bbox.X)
	visited[seed] = true
	queue := []int{seed}
	for head := 0; head < len(queue); head++ {
		li := queue[head]
		lx, ly := li%bbox.W, li/bbox.W
		x, y := bbox.X+lx, bbox.Y+ly
		for _, p := range [][2]int{{x - 1, y}, {x + 1, y}, {x, y - 1}, {x, y + 1}} {
			nx, ny := p[0], p[1]
			if nx < bbox.X || nx >= bbox.X+bbox.W || ny < bbox.Y || ny >= bbox.Y+bbox.H {
				continue
			}
			nli := (ny-bbox.Y)*bbox.W + (nx - bbox.X)
			if visited[nli] {
				continue
			}
			gi := ny*width + nx
			visited[nli] = true
			if labels[gi] == domainID || int(mask[gi]) >= threshold {
				continue
			}
			queue = append(queue, nli)
		}
	}
}

type TopologyTolerance struct {
	MinIoU             int `json:"min_iou_permille"`
	MaxAreaDelta       int `json:"max_area_delta"`
	MaxCentroidMilli   int `json:"max_centroid_delta_milli"`
	MaxBBoxDelta       int `json:"max_bbox_delta"`
	MaxHoleDelta       int `json:"max_hole_delta"`
	MaxRingWidthMilli  int `json:"max_ring_width_delta_milli"`
	MaxPixelDifference int `json:"max_pixel_difference"`
}

func DefaultTopologyTolerance() TopologyTolerance {
	return TopologyTolerance{
		MinIoU:             800,
		MaxAreaDelta:       4,
		MaxCentroidMilli:   1500,
		MaxBBoxDelta:       1,
		MaxHoleDelta:       0,
		MaxRingWidthMilli:  120,
		MaxPixelDifference: 16,
	}
}

type DomainMatch struct {
	ExpectedID int `json:"expected_id"`
	ActualID   int `json:"actual_id"`
	IoU        int `json:"iou_permille"`
	Inter      int `json:"inter"`
	Union      int `json:"union"`
}

type TopologyIssue struct {
	DomainID int    `json:"domain_id"`
	ActualID int    `json:"actual_id"`
	Metric   string `json:"metric"`
	Expected int    `json:"expected"`
	Actual   int    `json:"actual"`
	Limit    int    `json:"limit"`
	Message  string `json:"message"`
}

type TopologyReport struct {
	Threshold        int             `json:"threshold"`
	Expected         []DomainStats   `json:"expected"`
	Actual           []DomainStats   `json:"actual"`
	Matches          []DomainMatch   `json:"matches"`
	Issues           []TopologyIssue `json:"issues"`
	DifferencePixels int             `json:"difference_pixels"`
	Passed           bool            `json:"passed"`
}

func CompareTopology(actualMask []uint8, truth *Truth, width, height int, tol TopologyTolerance) (*TopologyReport, error) {
	if truth == nil {
		return nil, coded(CodeVersion, "truth is required for topology verification")
	}
	if tol.MinIoU <= 0 {
		tol.MinIoU = 800
	}
	threshold := DefaultMaskThreshold
	actualEx := ExtractDomains(actualMask, width, height, threshold)
	expectedEx := ExtractDomains(truth.ReferenceMask, width, height, threshold)
	actualStats := make([]DomainStats, len(actualEx.Domains))
	for i := range actualStats {
		actualStats[i] = actualEx.Domains[i].DomainStats
	}
	expectedStats := truth.Domains
	if expectedStats == nil {
		expectedStats = make([]TruthDomain, len(expectedEx.Domains))
		for i := range expectedEx.Domains {
			expectedStats[i] = domainTruth(expectedEx.Domains[i].DomainStats)
		}
	}
	report := &TopologyReport{
		Threshold: threshold,
		Expected:  make([]DomainStats, len(expectedStats)),
		Actual:    actualStats,
	}
	for i, d := range expectedStats {
		report.Expected[i] = DomainStats{
			ID: d.ID, Area: d.Area, CentroidXMilli: d.CentroidXMilli,
			CentroidYMilli: d.CentroidYMilli, BBox: d.BBox, Holes: d.Holes,
			RingWidthMilli: d.RingWidthMilli, BoundaryPixels: d.BoundaryPixels,
		}
	}
	diff := 0
	for i := range actualMask {
		if int(actualMask[i]) != int(truth.ReferenceMask[i]) {
			diff++
		}
	}
	report.DifferencePixels = diff

	type pair struct {
		expected, actual int
		iou              int
		inter, union     int
		areaDelta        int
	}
	var pairs []pair
	actualByID := make(map[int]domainPixels, len(actualEx.Domains))
	for _, d := range actualEx.Domains {
		actualByID[d.ID] = d
	}
	for _, ed := range expectedEx.Domains {
		for _, ad := range actualEx.Domains {
			inter := actualInter(ed.pixels, actualEx.Labels, ad.ID)
			union := ed.Area + ad.Area - inter
			iou := 0
			if union > 0 {
				iou = int((int64(inter)*1000 + int64(union)/2) / int64(union))
			}
			pairs = append(pairs, pair{
				expected: ed.ID, actual: ad.ID, iou: iou, inter: inter, union: union,
				areaDelta: absInt(ed.Area - ad.Area),
			})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].iou != pairs[j].iou {
			return pairs[i].iou > pairs[j].iou
		}
		if pairs[i].areaDelta != pairs[j].areaDelta {
			return pairs[i].areaDelta < pairs[j].areaDelta
		}
		if pairs[i].expected != pairs[j].expected {
			return pairs[i].expected < pairs[j].expected
		}
		return pairs[i].actual < pairs[j].actual
	})
	usedExpected := make(map[int]bool)
	usedActual := make(map[int]bool)
	expectedByID := make(map[int]TruthDomain, len(expectedStats))
	for _, d := range expectedStats {
		expectedByID[d.ID] = d
	}
	for _, p := range pairs {
		if usedExpected[p.expected] || usedActual[p.actual] {
			continue
		}
		usedExpected[p.expected] = true
		usedActual[p.actual] = true
		report.Matches = append(report.Matches, DomainMatch{
			ExpectedID: p.expected, ActualID: p.actual, IoU: p.iou,
			Inter: p.inter, Union: p.union,
		})
		ed := expectedByID[p.expected]
		ad := actualByID[p.actual].DomainStats
		addIssue := func(metric string, expected, actual, limit int) {
			if absInt(expected-actual) <= limit {
				return
			}
			report.Issues = append(report.Issues, TopologyIssue{
				DomainID: p.expected, ActualID: p.actual, Metric: metric,
				Expected: expected, Actual: actual, Limit: limit,
				Message: fmt.Sprintf("domain %d %s expected %d, actual %d, allowed delta %d",
					p.expected, metric, expected, actual, limit),
			})
		}
		if p.iou < tol.MinIoU {
			report.Issues = append(report.Issues, TopologyIssue{
				DomainID: p.expected, ActualID: p.actual, Metric: "iou_permille",
				Expected: tol.MinIoU, Actual: p.iou, Limit: 0,
				Message: fmt.Sprintf("domain %d IoU %d below %d", p.expected, p.iou, tol.MinIoU),
			})
		}
		addIssue("area", ed.Area, ad.Area, tol.MaxAreaDelta)
		addIssue("centroid_x_milli", ed.CentroidXMilli, ad.CentroidXMilli, tol.MaxCentroidMilli)
		addIssue("centroid_y_milli", ed.CentroidYMilli, ad.CentroidYMilli, tol.MaxCentroidMilli)
		addIssue("bbox_x", ed.BBox.X, ad.BBox.X, tol.MaxBBoxDelta)
		addIssue("bbox_y", ed.BBox.Y, ad.BBox.Y, tol.MaxBBoxDelta)
		addIssue("bbox_w", ed.BBox.W, ad.BBox.W, tol.MaxBBoxDelta)
		addIssue("bbox_h", ed.BBox.H, ad.BBox.H, tol.MaxBBoxDelta)
		addIssue("holes", ed.Holes, ad.Holes, tol.MaxHoleDelta)
		addIssue("ring_width_milli", ed.RingWidthMilli, ad.RingWidthMilli, tol.MaxRingWidthMilli)
	}
	for _, ed := range expectedStats {
		if !usedExpected[ed.ID] {
			report.Issues = append(report.Issues, TopologyIssue{
				DomainID: ed.ID, Metric: "missing_domain", Expected: ed.Area, Actual: 0,
				Message: fmt.Sprintf("expected domain %d (area %d) has no one-to-one match", ed.ID, ed.Area),
			})
		}
	}
	for _, ad := range actualEx.Domains {
		if !usedActual[ad.ID] {
			report.Issues = append(report.Issues, TopologyIssue{
				DomainID: -1, ActualID: ad.ID, Metric: "extra_domain", Expected: 0, Actual: ad.Area,
				Message: fmt.Sprintf("actual domain %d (area %d) has no expected match", ad.ID, ad.Area),
			})
		}
	}
	report.Passed = len(report.Issues) == 0 &&
		report.DifferencePixels <= tol.MaxPixelDifference
	if !report.Passed {
		msg := fmt.Sprintf("topology failed: %d differences, %d issues", report.DifferencePixels, len(report.Issues))
		if len(report.Issues) > 0 {
			msg += "; first: " + report.Issues[0].Message
		}
		return report, withDetails(CodeTopology, "%s", report, msg)
	}
	return report, nil
}

func domainTruth(d DomainStats) TruthDomain {
	return TruthDomain{
		ID: d.ID, Area: d.Area, CentroidXMilli: d.CentroidXMilli,
		CentroidYMilli: d.CentroidYMilli, BBox: d.BBox, Holes: d.Holes,
		RingWidthMilli: d.RingWidthMilli, BoundaryPixels: d.BoundaryPixels,
	}
}

func actualInter(expectedPixels []int, actualLabels []int, actualID int) int {
	n := 0
	for _, idx := range expectedPixels {
		if idx >= 0 && idx < len(actualLabels) && actualLabels[idx] == actualID {
			n++
		}
	}
	return n
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func masksEqual(a, b []uint8) bool {
	return bytes.Equal(a, b)
}
