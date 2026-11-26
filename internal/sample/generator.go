// Package sample contains the deterministic local sample generator.
package sample

import (
	"fmt"

	"msworkbench/internal/conv"
	"msworkbench/internal/domain"
)

const FixedSeed int64 = 0x51A7C0DE

type Plane struct {
	BandID string  `json:"band_id"`
	Values []int64 `json:"values"`
}

type ExposureFrame struct {
	ExposureID string  `json:"exposure_id"`
	ExposureUS int64   `json:"exposure_us"`
	Counts     []Plane `json:"counts"`
}

type PixelQuantTruth struct {
	Values     []int64 `json:"values"`
	Remainders []int64 `json:"remainders"`
	Saturated  []bool  `json:"saturated"`
}

type BlockTruth struct {
	Coord     domain.BlockCoord          `json:"coord"`
	InputHash string                     `json:"input_hash"`
	BandQuant map[string]PixelQuantTruth `json:"band_quant"`
}

type BlockSummary struct {
	Scale            domain.SampleScale `json:"scale"`
	Block            string             `json:"block"`
	SelectedPixels   []int              `json:"selected_local_pixels"`
	PixelCount       int                `json:"pixel_count"`
	ObservationCount int                `json:"observation_count"`
	RadianceSum      int64              `json:"radiance_sum"`
	RadianceMin      int64              `json:"radiance_min"`
	RadianceMax      int64              `json:"radiance_max"`
	SummaryHash      string             `json:"summary_hash"`
}

type SampleSet struct {
	Seed          int64                                  `json:"seed"`
	Radiance      []Plane                                `json:"radiance"`
	ExposureStack []ExposureFrame                        `json:"exposure_stack"`
	BlockHashes   map[string]string                      `json:"block_hashes"`
	Truth         map[string]*BlockTruth                 `json:"truth"`
	Summaries     map[domain.SampleScale][]*BlockSummary `json:"summaries"`
	GlobalObs     map[domain.SampleScale]int             `json:"global_observations"`
}

func NewCanonicalProject() *domain.Project {
	p := &domain.Project{
		ProjectID:      "PRJ-MS-HDR-042",
		Name:           "Wulin Ruins - multispectral verification plate",
		SourceSchema:   domain.SchemaV3,
		Seed:           FixedSeed,
		Width:          16,
		Height:         12,
		BlockWidth:     4,
		BlockHeight:    4,
		QuantScale:     domain.InternalQuantScale,
		ReferenceExpUS: 8_000,
		FullWellDose:   384_000_000,
		WhitePointBP:   [3]int64{9500, 10000, 10900},
		Bands: []domain.Band{
			{ID: "B01", Name: "coastal-blue", GroupPath: []string{"Visible", "Blue"}, CenterNM: 460, GainPPM: conv.GainPPMUnity, GainExplicit: true, Response: [3]int64{3500, 900, 8500}},
			{ID: "B02", Name: "green", GroupPath: []string{"Visible", "Green"}, CenterNM: 540, GainPPM: 1_125_000, GainExplicit: true, Response: [3]int64{1200, 7200, 1800}},
			{ID: "B03", Name: "red", GroupPath: []string{"Visible", "Red"}, CenterNM: 620, GainPPM: conv.GainPPMUnity, GainExplicit: true, Response: [3]int64{6800, 2600, 500}},
			{ID: "B04", Name: "deep-red", GroupPath: []string{"Extended", "RedEdge"}, CenterNM: 700, GainPPM: conv.GainPPMUnity, GainExplicit: true, Response: [3]int64{5200, 1200, 100}},
			{ID: "B05", Name: "nir", GroupPath: []string{"Extended", "NIR"}, CenterNM: 850, GainPPM: 950_000, GainExplicit: true, Response: [3]int64{1800, 300, 0}},
		},
		Exposures: []domain.Exposure{
			{ID: "exp-lo", ExposureUS: 2_000},
			{ID: "exp-mid", ExposureUS: 8_000, Reference: true},
			{ID: "exp-hi", ExposureUS: 32_000},
		},
	}
	p.Tree = []*domain.Group{
		{ID: "g-vis", Name: "Visible", Path: []string{"Visible"}, Children: []*domain.Group{
			{ID: "g-blue", Name: "Blue", Path: []string{"Visible", "Blue"}, BandIDs: []string{"B01"}},
			{ID: "g-green", Name: "Green", Path: []string{"Visible", "Green"}, BandIDs: []string{"B02"}},
			{ID: "g-red", Name: "Red", Path: []string{"Visible", "Red"}, BandIDs: []string{"B03"}},
		}},
		{ID: "g-ext", Name: "Extended", Path: []string{"Extended"}, Children: []*domain.Group{
			{ID: "g-edge", Name: "RedEdge", Path: []string{"Extended", "RedEdge"}, BandIDs: []string{"B04"}},
			{ID: "g-nir", Name: "NIR", Path: []string{"Extended", "NIR"}, BandIDs: []string{"B05"}},
		}},
	}
	return p
}

func splitMix64(x int64) int64 {
	z := uint64(x) + 0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	z = z ^ (z >> 31)
	return int64(z & 0x7fffffffffffffff)
}

func Generate(p *domain.Project) (*SampleSet, error) {
	if p == nil {
		p = NewCanonicalProject()
	}
	n := p.Width * p.Height
	set := &SampleSet{
		Seed:          p.Seed,
		Radiance:      make([]Plane, 0, len(p.Bands)),
		ExposureStack: make([]ExposureFrame, 0, len(p.Exposures)),
		BlockHashes:   map[string]string{},
		Truth:         map[string]*BlockTruth{},
		Summaries: map[domain.SampleScale][]*BlockSummary{
			domain.ScaleSparse: {}, domain.ScaleMedium: {}, domain.ScaleDense: {},
		},
		GlobalObs: map[domain.SampleScale]int{},
	}

	for bi, band := range p.Bands {
		plane := Plane{BandID: band.ID, Values: make([]int64, n)}
		for y := 0; y < p.Height; y++ {
			for x := 0; x < p.Width; x++ {
				cx := x / p.BlockWidth
				cy := y / p.BlockHeight
				idx := y*p.Width + x
				if (domain.BlockCoord{BX: 3, BY: 2} == domain.BlockCoord{BX: cx, BY: cy} && band.ID == "B05") ||
					((cx*11+cy*7+bi*13+3)%19 == 0 && ((x+y)%5 == 0)) {
					plane.Values[idx] = 0
					continue
				}
				noise := splitMix64(p.Seed+int64(idx*31+bi*1009)) % 900
				gradient := int64(x*38 + y*57)
				wave := int64((bi+1)*260 + (x*(bi+3)*17)%620 + (y*(bi+2)*23)%540)
				v := int64(420) + gradient + wave + noise
				if (cx+cy+bi)%7 == 0 {
					v += 900
				}
				plane.Values[idx] = v
			}
		}
		set.Radiance = append(set.Radiance, plane)
	}

	for _, exp := range p.Exposures {
		frame := ExposureFrame{ExposureID: exp.ID, ExposureUS: exp.ExposureUS, Counts: make([]Plane, 0, len(p.Bands))}
		for bi, band := range p.Bands {
			counts := Plane{BandID: band.ID, Values: make([]int64, n)}
			for i, radiance := range set.Radiance[bi].Values {
				dose := radiance * exp.ExposureUS
				q, _, err := conv.MulDivFloor(dose, 60_000, p.FullWellDose, "exposure_stack.count", nil, "exposure_count")
				if err != nil {
					return nil, err
				}
				if q > 60_000 {
					q = 60_000
				}
				counts.Values[i] = q
			}
			frame.Counts = append(frame.Counts, counts)
		}
		set.ExposureStack = append(set.ExposureStack, frame)
	}

	for _, coord := range p.Coords() {
		indices, _, _ := p.BlockPixels(coord)
		truth := &BlockTruth{Coord: coord, BandQuant: map[string]PixelQuantTruth{}}
		hashInput := map[string]any{"block": coord.String(), "bands": map[string][]int64{}}
		for bi, band := range p.Bands {
			values := make([]int64, len(indices))
			rems := make([]int64, len(indices))
			sat := make([]bool, len(indices))
			blockVals := make([]int64, len(indices))
			for li, gi := range indices {
				r := set.Radiance[bi].Values[gi]
				blockVals[li] = r
				adjusted, _, err := conv.MulDivFloor(r, band.GainPPM, conv.GainPPMUnity, "bands["+band.ID+"].gain_ppm", nil, "truth_gain")
				if err != nil {
					return nil, err
				}
				dose := adjusted * p.ReferenceExpUS
				q, rem, err := conv.MulDivFloor(dose, p.QuantScale, p.FullWellDose, "truth.quant", nil, "truth_quant")
				if err != nil {
					return nil, err
				}
				if q > p.QuantScale {
					q = p.QuantScale
					sat[li] = true
				}
				values[li] = q
				rems[li] = rem
			}
			truth.BandQuant[band.ID] = PixelQuantTruth{Values: values, Remainders: rems, Saturated: sat}
			hashInput["bands"].(map[string][]int64)[band.ID] = blockVals
		}
		truth.InputHash = domain.HashJSON(hashInput)
		key := coord.String()
		set.Truth[key] = truth
		set.BlockHashes[key] = truth.InputHash

		for _, scale := range []domain.SampleScale{domain.ScaleSparse, domain.ScaleMedium, domain.ScaleDense} {
			sel := selectLocalPixels(len(indices), scale)
			s := &BlockSummary{Scale: scale, Block: key, SelectedPixels: sel, PixelCount: len(sel)}
			minV := int64(1 << 62)
			for _, li := range sel {
				for bi := range p.Bands {
					v := set.Radiance[bi].Values[indices[li]]
					s.RadianceSum += v
					if v < minV {
						minV = v
					}
					if v > s.RadianceMax {
						s.RadianceMax = v
					}
				}
			}
			if len(sel) > 0 {
				s.RadianceMin = minV
			}
			s.ObservationCount = len(sel) * len(p.Bands)
			s.SummaryHash = domain.HashJSON(map[string]any{"block": key, "scale": scale, "selected": sel, "sum": s.RadianceSum, "min": s.RadianceMin, "max": s.RadianceMax})
			set.Summaries[scale] = append(set.Summaries[scale], s)
			set.GlobalObs[scale] += s.ObservationCount
		}
	}
	return set, nil
}

func selectLocalPixels(n int, scale domain.SampleScale) []int {
	stride := 1
	switch scale {
	case domain.ScaleSparse:
		stride = 4
	case domain.ScaleMedium:
		stride = 2
	case domain.ScaleDense:
		stride = 1
	}
	out := make([]int, 0, n/stride+1)
	for i := 0; i < n; i += stride {
		out = append(out, i)
	}
	return out
}

func MustGenerate() *SampleSet {
	s, err := Generate(nil)
	if err != nil {
		panic(fmt.Sprintf("generate deterministic sample: %v", err))
	}
	return s
}
