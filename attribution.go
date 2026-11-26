package strokelab

import "sort"

type PixelAttribution struct {
	Pixel             int    `json:"pixel"`
	X                 int    `json:"x"`
	Y                 int    `json:"y"`
	BlockID           int    `json:"block_id"`
	ActualAlpha       uint8  `json:"actual_alpha"`
	ReferenceAlpha    uint8  `json:"reference_alpha"`
	CandidateAlpha    uint8  `json:"candidate_alpha"`
	FirstStrokeSeq    int    `json:"first_stroke_seq"`
	FirstStrokeID     string `json:"first_stroke_id"`
	PrimaryStrokeSeq  int    `json:"primary_stroke_seq"`
	PrimaryStrokeID   string `json:"primary_stroke_id"`
	PrimaryAlpha      uint8  `json:"primary_alpha"`
	ContributionShare int    `json:"contribution_share_pp10000"`
	Reason            string `json:"reason"`
}

type CulpritSummary struct {
	StrokeID   string `json:"stroke_id"`
	StrokeSeq  int    `json:"stroke_seq"`
	Pixels     int    `json:"pixels"`
	ShareTotal int    `json:"share_total_pp10000"`
}

type AttributionReport struct {
	Differences []PixelAttribution `json:"differences"`
	Culprits    []CulpritSummary   `json:"culprits"`
}

// AttributeDifferences assigns each differing pixel to the stroke with the
// largest recorded alpha component. All inputs are full-canvas canonical
// components, so worker order cannot affect attribution.
func AttributeDifferences(actual, reference, candidate []uint8, width, blockSize int,
	ordered []OrderedStroke, components [][]uint8) *AttributionReport {
	report := &AttributionReport{}
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}
	culpritIndex := make(map[string]int)
	for i := range actual {
		if actual[i] == reference[i] {
			continue
		}
		pa := PixelAttribution{
			Pixel: i, X: i % width, Y: i / width,
			ActualAlpha: actual[i], ReferenceAlpha: reference[i],
			CandidateAlpha: candidate[i],
		}
		x, y := pa.X, pa.Y
		blocksPerRow := (width + blockSize - 1) / blockSize
		if blocksPerRow <= 0 {
			blocksPerRow = 1
		}
		pa.BlockID = (y/blockSize)*blocksPerRow + x/blockSize
		var firstID string
		var primaryID string
		primaryAlpha := -1
		firstAlpha := -1
		sum := 0
		for ci, comp := range components {
			a := int(comp[i])
			if a <= 0 {
				continue
			}
			sum += a
			if firstAlpha < 0 {
				firstAlpha = a
				firstID = ordered[ci].Stroke.ID
				pa.FirstStrokeSeq = ordered[ci].Position
			}
			if a > primaryAlpha {
				primaryAlpha = a
				primaryID = ordered[ci].Stroke.ID
				pa.PrimaryStrokeSeq = ordered[ci].Position
			}
		}
		pa.FirstStrokeID = firstID
		pa.PrimaryStrokeID = primaryID
		if primaryAlpha >= 0 {
			pa.PrimaryAlpha = uint8(primaryAlpha)
			pa.ContributionShare = int((int64(primaryAlpha)*WeightMax + int64(sum)/2) / int64(sum))
			pa.Reason = "largest_stroke_component"
		} else {
			pa.PrimaryStrokeSeq = -1
			pa.FirstStrokeSeq = -1
			pa.Reason = "candidate_base_only"
		}
		report.Differences = append(report.Differences, pa)
		if primaryID != "" {
			key := primaryID
			idx, ok := culpritIndex[key]
			if !ok {
				idx = len(report.Culprits)
				culpritIndex[key] = idx
				report.Culprits = append(report.Culprits, CulpritSummary{
					StrokeID: primaryID, StrokeSeq: pa.PrimaryStrokeSeq,
				})
			}
			report.Culprits[idx].Pixels++
			report.Culprits[idx].ShareTotal += pa.ContributionShare
		}
	}
	sort.SliceStable(report.Culprits, func(i, j int) bool {
		if report.Culprits[i].Pixels != report.Culprits[j].Pixels {
			return report.Culprits[i].Pixels > report.Culprits[j].Pixels
		}
		return report.Culprits[i].PrimaryOrder() < report.Culprits[j].PrimaryOrder()
	})
	return report
}

func (c CulpritSummary) PrimaryOrder() int {
	if c.StrokeSeq <= 0 {
		return 1 << 30
	}
	return c.StrokeSeq
}

type TruthDomain struct {
	ID             int  `json:"id"`
	Area           int  `json:"area"`
	CentroidXMilli int  `json:"centroid_x_milli"`
	CentroidYMilli int  `json:"centroid_y_milli"`
	BBox           Rect `json:"bbox"`
	Holes          int  `json:"holes"`
	RingWidthMilli int  `json:"ring_width_milli"`
	BoundaryPixels int  `json:"boundary_pixels"`
}

type TruthContribution struct {
	StrokeID  string  `json:"stroke_id"`
	StrokeSeq int     `json:"stroke_seq"`
	Mask      []uint8 `json:"mask"`
}

type Truth struct {
	ReferenceMask []uint8             `json:"reference_mask"`
	Domains       []TruthDomain       `json:"domains"`
	Contributions []TruthContribution `json:"contributions"`
}

func truthFromEvaluation(mask []uint8, width, height int, ordered []OrderedStroke, components [][]uint8) *Truth {
	ex := ExtractDomains(mask, width, height, DefaultMaskThreshold)
	t := &Truth{ReferenceMask: append([]uint8(nil), mask...)}
	for _, d := range ex.Domains {
		t.Domains = append(t.Domains, domainTruth(d.DomainStats))
	}
	for i, c := range components {
		t.Contributions = append(t.Contributions, TruthContribution{
			StrokeID: ordered[i].Stroke.ID, StrokeSeq: ordered[i].Position,
			Mask: append([]uint8(nil), c...),
		})
	}
	return t
}

func diffMaskPNG(actual, reference []uint8, width, height int) []uint8 {
	out := make([]uint8, width*height*4)
	for i := 0; i < width*height; i++ {
		d := int(actual[i]) - int(reference[i])
		base := uint8(32)
		r, g, b, a := base, base, base, uint8(255)
		if d > 0 {
			r = clampU8(int64(64 + d))
			g = 32
			b = 32
		} else if d < 0 {
			r = 32
			g = 64
			b = clampU8(int64(64 - d))
		}
		o := i * 4
		out[o], out[o+1], out[o+2], out[o+3] = r, g, b, a
	}
	return out
}
