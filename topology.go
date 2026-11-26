package maskqc

import (
	"fmt"
	"sort"
)

type StrokeDirectionCheck struct {
	StrokeID      string         `json:"stroke_id"`
	Class         DirectionClass `json:"class"`
	SameVotes     int            `json:"same_votes"`
	OppositeVotes int            `json:"opposite_votes"`
	CrossVotes    int            `json:"cross_votes"`
	MixedVotes    int            `json:"mixed_votes"`
	Segments      int            `json:"segments"`
}

type Domain struct {
	No             int                    `json:"no"`
	PixelCount     int                    `json:"pixel_count"`
	CentroidX1000  int                    `json:"centroid_x1000"`
	CentroidY1000  int                    `json:"centroid_y1000"`
	Bounds         Rect                   `json:"bounds"`
	HoleCount      int                    `json:"hole_count"`
	BandWidth      int                    `json:"band_width_px"`
	BandWidthMin   int                    `json:"band_width_min_px"`
	BandWidthMax   int                    `json:"band_width_max_px"`
	Direction      DirectionClass         `json:"direction"`
	DirectionVotes map[DirectionClass]int `json:"direction_votes"`
	StrokeChecks   []StrokeDirectionCheck `json:"stroke_checks,omitempty"`

	pixels []int
}

func (d Domain) CentroidX() float64 { return float64(d.CentroidX1000) / 1000 }
func (d Domain) CentroidY() float64 { return float64(d.CentroidY1000) / 1000 }

type Analysis struct {
	Width   int      `json:"width"`
	Height  int      `json:"height"`
	Domains []Domain `json:"domains"`
	Labels  []int32  `json:"-"`
}

type TopologyTolerance struct {
	AreaPermille    int `json:"area_permille"`
	CentroidPixels  int `json:"centroid_pixels"`
	BBoxPixels      int `json:"bbox_pixels"`
	BandWidthPixels int `json:"band_width_pixels"`
	HoleCountDelta  int `json:"hole_count_delta"`
}

func DefaultTopologyTolerance() TopologyTolerance {
	return TopologyTolerance{AreaPermille: 180, CentroidPixels: 3, BBoxPixels: 2, BandWidthPixels: 2, HoleCountDelta: 0}
}

type TopologyIssue struct {
	Code     ErrorCode `json:"code"`
	DomainNo int       `json:"domain_no"`
	Field    string    `json:"field"`
	Expected any       `json:"expected"`
	Actual   any       `json:"actual"`
	Message  string    `json:"message"`
}

type DomainComparison struct {
	AccumDomainNo int             `json:"accum_domain_no"`
	TruthDomainNo int             `json:"truth_domain_no"`
	OverlapPixels int             `json:"overlap_pixels"`
	Issues        []TopologyIssue `json:"issues,omitempty"`
}

type TopologyReport struct {
	Tolerance TopologyTolerance  `json:"tolerance"`
	Accum     *Analysis          `json:"accum"`
	Truth     *Analysis          `json:"truth"`
	Matches   []DomainComparison `json:"matches"`
	Issues    []TopologyIssue    `json:"issues,omitempty"`
}

func (r *TopologyReport) Error() error {
	if r == nil || len(r.Issues) == 0 {
		return nil
	}
	return &Error{Code: ETop, FieldPath: "domains", Message: fmt.Sprintf("%d topology/direction issue(s)", len(r.Issues)), Details: map[string]any{"issues": r.Issues}}
}

func AnalyzeMask(m *Mask, strokes []*Stroke) (*Analysis, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	w, h := m.Width, m.Height
	fg := make([]bool, w*h)
	labels := make([]int32, w*h)
	for i, a := range m.Alpha {
		fg[i] = a >= 128
	}

	var domains []Domain
	nextLabel := int32(1)
	for start := 0; start < w*h; start++ {
		if !fg[start] || labels[start] != 0 {
			continue
		}
		label := nextLabel
		nextLabel++
		pixels := floodForeground(fg, labels, w, h, start, label)
		d := buildDomain(len(domains)+1, pixels, label, fg, w, h)
		domains = append(domains, d)
	}

	assignHoles(&domains, fg, w, h)
	assignBandWidth(&domains, fg, w, h)
	for i := range domains {
		checkDirections(&domains[i], m, strokes)
	}
	// Stable output ordering is scan order; explicitly sort by domain number.
	sort.SliceStable(domains, func(i, j int) bool { return domains[i].No < domains[j].No })
	return &Analysis{Width: w, Height: h, Domains: domains, Labels: labels}, nil
}

func floodForeground(fg []bool, labels []int32, w, h, start int, label int32) []int {
	stack := []int{start}
	labels[start] = label
	var pixels []int
	for len(stack) > 0 {
		idx := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		pixels = append(pixels, idx)
		x, y := idx%w, idx/w
		neighbors := [4]int{}
		count := 0
		if x > 0 {
			neighbors[count] = idx - 1
			count++
		}
		if x+1 < w {
			neighbors[count] = idx + 1
			count++
		}
		if y > 0 {
			neighbors[count] = idx - w
			count++
		}
		if y+1 < h {
			neighbors[count] = idx + w
			count++
		}
		for _, ni := range neighbors[:count] {
			if fg[ni] && labels[ni] == 0 {
				labels[ni] = label
				stack = append(stack, ni)
			}
		}
	}
	return pixels
}

func buildDomain(no int, pixels []int, label int32, fg []bool, w, h int) Domain {
	d := Domain{No: no, PixelCount: len(pixels), pixels: pixels, DirectionVotes: map[DirectionClass]int{}}
	sumX, sumY := 0, 0
	minX, minY, maxX, maxY := w, h, -1, -1
	for _, idx := range pixels {
		x, y := idx%w, idx/w
		sumX += x
		sumY += y
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
	}
	d.CentroidX1000 = sumX * 1000 / len(pixels)
	d.CentroidY1000 = sumY * 1000 / len(pixels)
	d.Bounds = Rect{minX, minY, maxX + 1, maxY + 1}
	_ = label
	return d
}

func assignHoles(domains *[]Domain, fg []bool, w, h int) {
	visited := make([]bool, w*h)
	for start := 0; start < w*h; start++ {
		if fg[start] || visited[start] {
			continue
		}
		stack := []int{start}
		visited[start] = true
		touchesBorder := false
		votes := map[int]int{}
		for len(stack) > 0 {
			idx := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := idx%w, idx/w
			if x == 0 || y == 0 || x == w-1 || y == h-1 {
				touchesBorder = true
			}
			neighbors := neighbors4(idx, x, y, w, h)
			for _, ni := range neighbors {
				if fg[ni] {
					// Domain numbers are label order in this implementation.
					votes[int(domainNoByPixel(domains, ni))]++
				} else if !visited[ni] {
					visited[ni] = true
					stack = append(stack, ni)
				}
			}
		}
		if !touchesBorder {
			bestNo, bestVotes := 0, -1
			for no, votesCount := range votes {
				if votesCount > bestVotes {
					bestNo, bestVotes = no, votesCount
				}
			}
			if bestNo > 0 {
				for i := range *domains {
					if (*domains)[i].No == bestNo {
						(*domains)[i].HoleCount++
						break
					}
				}
			}
		}
	}
}

func domainNoByPixel(domains *[]Domain, pixel int) int {
	// Linear lookup is acceptable for deterministic diagnostic masks; labels are
	// scan-ordered and domain pixel lists are small in the bundled samples.
	for _, d := range *domains {
		for _, p := range d.pixels {
			if p == pixel {
				return d.No
			}
		}
	}
	return 0
}

func neighbors4(idx, x, y, w, h int) []int {
	out := make([]int, 0, 4)
	if x > 0 {
		out = append(out, idx-1)
	}
	if x+1 < w {
		out = append(out, idx+1)
	}
	if y > 0 {
		out = append(out, idx-w)
	}
	if y+1 < h {
		out = append(out, idx+w)
	}
	return out
}

func assignBandWidth(domains *[]Domain, fg []bool, w, h int) {
	const big = 1 << 20
	dist := make([]int, w*h)
	for i, v := range fg {
		if v {
			dist[i] = big
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if !fg[idx] {
				continue
			}
			if x > 0 && dist[idx-1]+1 < dist[idx] {
				dist[idx] = dist[idx-1] + 1
			}
			if y > 0 && dist[idx-w]+1 < dist[idx] {
				dist[idx] = dist[idx-w] + 1
			}
		}
	}
	for y := h - 1; y >= 0; y-- {
		for x := w - 1; x >= 0; x-- {
			idx := y*w + x
			if !fg[idx] {
				continue
			}
			if x+1 < w && dist[idx+1]+1 < dist[idx] {
				dist[idx] = dist[idx+1] + 1
			}
			if y+1 < h && dist[idx+w]+1 < dist[idx] {
				dist[idx] = dist[idx+w] + 1
			}
		}
	}
	for di := range *domains {
		minD, maxD := big, 0
		for _, p := range (*domains)[di].pixels {
			d := dist[p]
			if d < minD {
				minD = d
			}
			if d > maxD {
				maxD = d
			}
		}
		(*domains)[di].BandWidthMax = 2*maxD + 1
		(*domains)[di].BandWidthMin = 2*minD + 1
		(*domains)[di].BandWidth = (*domains)[di].BandWidthMax
	}
}

func checkDirections(domain *Domain, m *Mask, strokes []*Stroke) {
	for _, s := range strokes {
		check := StrokeDirectionCheck{StrokeID: s.ID}
		dx := s.End.X - s.Start.X
		dy := s.End.Y - s.Start.Y
		if dx == 0 && dy == 0 {
			check.Class = DirUnknown
			domain.StrokeChecks = append(domain.StrokeChecks, check)
			continue
		}
		b := StrokeBounds(*s, m.Width, m.Height)
		for _, idx := range domain.pixels {
			x, y := idx%m.Width, idx/m.Width
			if x < b.MinX-1 || x >= b.MaxX+1 || y < b.MinY-1 || y >= b.MaxY+1 {
				continue
			}
			if !isBoundaryPixel(m, x, y) {
				continue
			}
			gx := foregroundAt(m, x+1, y) - foregroundAt(m, x-1, y)
			gy := foregroundAt(m, x, y+1) - foregroundAt(m, x, y-1)
			if gx == 0 && gy == 0 {
				continue
			}
			check.Segments++
			c := gx*dy - gy*dx
			dot := gx*dx + gy*dy
			c2, dot2 := c*c, dot*dot
			switch {
			case c2 >= 4*dot2:
				if c > 0 {
					check.SameVotes++
				} else {
					check.OppositeVotes++
				}
			case dot2 >= 4*c2:
				check.CrossVotes++
			default:
				check.MixedVotes++
			}
		}
		total := check.SameVotes + check.OppositeVotes + check.CrossVotes + check.MixedVotes
		switch {
		case total == 0:
			check.Class = DirUnknown
		case check.CrossVotes > check.SameVotes+check.OppositeVotes && check.CrossVotes > check.MixedVotes:
			check.Class = DirCross
		case check.MixedVotes > check.SameVotes+check.OppositeVotes:
			check.Class = DirMixed
		default:
			check.Class = DirAlong
		}
		domain.StrokeChecks = append(domain.StrokeChecks, check)
		if check.Segments > 0 && check.Class != DirUnknown {
			domain.DirectionVotes[check.Class]++
		}
	}

	best := DirUnknown
	bestCount := 0
	for class, count := range domain.DirectionVotes {
		if count > bestCount {
			best, bestCount = class, count
		}
	}
	domain.Direction = best
}

func isBoundaryPixel(m *Mask, x, y int) bool {
	if m.At(x, y) < 128 {
		return false
	}
	return foregroundAt(m, x+1, y) == 0 || foregroundAt(m, x-1, y) == 0 ||
		foregroundAt(m, x, y+1) == 0 || foregroundAt(m, x, y-1) == 0
}

func foregroundAt(m *Mask, x, y int) int {
	if x < 0 || y < 0 || x >= m.Width || y >= m.Height || m.At(x, y) < 128 {
		return 0
	}
	return 1
}

func CompareTopology(accum, truth *Mask, strokes []*Stroke, tol TopologyTolerance) (*TopologyReport, error) {
	accA, err := AnalyzeMask(accum, strokes)
	if err != nil {
		return nil, err
	}
	truthA, err := AnalyzeMask(truth, strokes)
	if err != nil {
		return nil, err
	}
	rep := &TopologyReport{Tolerance: tol, Accum: accA, Truth: truthA}
	matchedTruth := map[int]bool{}

	for ai := range accA.Domains {
		ad := &accA.Domains[ai]
		bestTruth, bestOverlap := 0, -1
		overlapByTruth := map[int]int{}
		for _, p := range ad.pixels {
			tl := truthA.Labels[p]
			if tl > 0 {
				no := truthDomainNoByLabel(truthA, tl)
				overlapByTruth[no]++
			}
		}
		for no, n := range overlapByTruth {
			if n > bestOverlap {
				bestTruth, bestOverlap = no, n
			}
		}
		cmp := DomainComparison{AccumDomainNo: ad.No, TruthDomainNo: bestTruth, OverlapPixels: bestOverlap}
		if bestTruth == 0 {
			issue := TopologyIssue{Code: ETop, DomainNo: ad.No, Field: "domain", Expected: "no extra foreground domain", Actual: map[string]any{"area": ad.PixelCount, "bounds": ad.Bounds}, Message: "accumulated domain does not overlap truth"}
			cmp.Issues = append(cmp.Issues, issue)
			rep.Issues = append(rep.Issues, issue)
			rep.Matches = append(rep.Matches, cmp)
			continue
		}
		matchedTruth[bestTruth] = true
		td := truthDomainByNo(truthA, bestTruth)
		if td == nil {
			return nil, newError(ETop, "truth", "matched truth domain %d not found", bestTruth)
		}
		addIssue := func(field string, expected, actual any, msg string) {
			issue := TopologyIssue{Code: ETop, DomainNo: ad.No, Field: field, Expected: expected, Actual: actual, Message: msg}
			cmp.Issues = append(cmp.Issues, issue)
			rep.Issues = append(rep.Issues, issue)
		}
		areaDiff := absInt(ad.PixelCount - td.PixelCount)
		if areaDiff*1000 > td.PixelCount*tol.AreaPermille {
			addIssue("area", td.PixelCount, ad.PixelCount, "area exceeds relative tolerance")
		}
		if absInt(ad.CentroidX1000-td.CentroidX1000) > tol.CentroidPixels*1000 || absInt(ad.CentroidY1000-td.CentroidY1000) > tol.CentroidPixels*1000 {
			addIssue("centroid", [2]int{td.CentroidX1000, td.CentroidY1000}, [2]int{ad.CentroidX1000, ad.CentroidY1000}, "centroid exceeds tolerance")
		}
		for _, bc := range []struct {
			field string
			a, t  int
		}{
			{"bounds.min_x", ad.Bounds.MinX, td.Bounds.MinX},
			{"bounds.min_y", ad.Bounds.MinY, td.Bounds.MinY},
			{"bounds.max_x", ad.Bounds.MaxX, td.Bounds.MaxX},
			{"bounds.max_y", ad.Bounds.MaxY, td.Bounds.MaxY},
		} {
			if absInt(bc.a-bc.t) > tol.BBoxPixels {
				addIssue("bounds", td.Bounds, ad.Bounds, "bounding box exceeds tolerance")
				break
			}
		}
		if absInt(ad.HoleCount-td.HoleCount) > tol.HoleCountDelta {
			addIssue("hole_count", td.HoleCount, ad.HoleCount, "hole count exceeds tolerance")
		}
		if absInt(ad.BandWidth-td.BandWidth) > tol.BandWidthPixels {
			addIssue("band_width_px", td.BandWidth, ad.BandWidth, "annular/stroke band width exceeds tolerance")
		}
		if ad.Direction != DirUnknown && td.Direction != DirUnknown && ad.Direction != td.Direction {
			addIssue("direction", td.Direction, ad.Direction, "boundary-normal direction class differs from truth")
		}
		rep.Matches = append(rep.Matches, cmp)
	}
	for _, td := range truthA.Domains {
		if !matchedTruth[td.No] {
			issue := TopologyIssue{Code: ETop, DomainNo: td.No, Field: "missing_domain", Expected: td.PixelCount, Actual: 0, Message: "truth domain missing from accumulated mask"}
			rep.Issues = append(rep.Issues, issue)
		}
	}
	return rep, nil
}

func truthDomainNoByLabel(a *Analysis, label int32) int {
	// Labels are assigned in scan order and Domain.No matches that order.
	return int(label)
}

func truthDomainByNo(a *Analysis, no int) *Domain {
	for i := range a.Domains {
		if a.Domains[i].No == no {
			return &a.Domains[i]
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
