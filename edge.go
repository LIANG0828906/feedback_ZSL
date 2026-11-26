package maskqa

import (
	"sort"
	"sync"
)

type IntField struct {
	Width  int   `json:"width"`
	Height int   `json:"height"`
	Values []int `json:"values"`
}

func NewIntField(w, h int) *IntField {
	return &IntField{Width: w, Height: h, Values: make([]int, w*h)}
}

func (f *IntField) At(x, y int) int {
	if f == nil || x < 0 || y < 0 || x >= f.Width || y >= f.Height {
		return 0
	}
	return f.Values[y*f.Width+x]
}

func (f *IntField) Set(x, y, v int) {
	if f != nil && x >= 0 && y >= 0 && x < f.Width && y < f.Height {
		f.Values[y*f.Width+x] = v
	}
}

type ThresholdEvidence struct {
	Threshold              int   `json:"threshold"`
	QuantileIndex          int   `json:"quantile_index"`
	QuantileFraction       int64 `json:"quantile_fraction"`
	InterpolationRemainder int64 `json:"interpolation_remainder"`
	ScaleRemainder         int64 `json:"scale_remainder"`
	ContributionMicro      int64 `json:"contribution_micro"`
}

func ComputeThreshold(magnitudes []int, p Params) (ThresholdEvidence, error) {
	if err := p.ValidateGradient(); err != nil {
		return ThresholdEvidence{}, err
	}
	if p.QuantilePermille < 1 || p.QuantilePermille > 999 {
		return ThresholdEvidence{}, paraError("quantile_permille", 1, 999, p.QuantilePermille, "quantile must be declared in permille")
	}
	if len(magnitudes) == 0 {
		return ThresholdEvidence{}, schemaError("gradient_magnitudes", "threshold", "empty magnitude distribution")
	}
	sorted := append([]int(nil), magnitudes...)
	sort.Ints(sorted)
	n := len(sorted)
	posNum := int64(n-1) * int64(p.QuantilePermille)
	idx := int(posNum / 1000)
	frac := posNum % 1000
	lo := sorted[idx]
	hi := sorted[minInt(n-1, idx+1)]
	deltaNum := int64(hi-lo) * p.FixedScale * frac
	deltaQ := deltaNum / 1000
	deltaRem := deltaNum % 1000
	thresholdQ := int64(lo)*p.FixedScale + deltaQ
	threshold := int(thresholdQ / p.FixedScale)
	scaleRem := thresholdQ % p.FixedScale
	micro := deltaRem*p.TraceScale/1000 + scaleRem*p.TraceScale/p.FixedScale
	return ThresholdEvidence{
		Threshold: threshold, QuantileIndex: idx, QuantileFraction: frac,
		InterpolationRemainder: deltaRem, ScaleRemainder: scaleRem, ContributionMicro: micro,
	}, nil
}

func ComputeEdgeBlock(mag *IntField, evidence ThresholdEvidence, p Params, block BlockCoord) (*EdgeOutput, error) {
	if p.EdgeBandWidth < 1 || p.EdgeBandWidth > 12 {
		return nil, paraError("edge_band_width", 1, 12, p.EdgeBandWidth, "edge band width outside interval")
	}
	out := &EdgeOutput{Block: block, Threshold: evidence.Threshold, Band: NewMask(block.Width, block.Height)}
	offsets := DiskOffsets(p.EdgeBandWidth)
	for ly := 0; ly < block.Height; ly++ {
		for lx := 0; lx < block.Width; lx++ {
			x, y := block.OriginX+lx, block.OriginY+ly
			edge := false
			for _, off := range offsets {
				if mag.At(x+off.dx, y+off.dy) >= evidence.Threshold {
					edge = true
					break
				}
			}
			if edge {
				out.Band.Set(lx, ly, 1)
			}
			trace := PixelTrace{
				X: x, Y: y, BlockX: block.IndexX, BlockY: block.IndexY,
				Threshold: evidence.Threshold, ThresholdMicro: evidence.ContributionMicro,
				Truncations: []Truncation{{
					Level: "threshold", X: x, Y: y, Operation: "quantile_interpolation_and_scale_floor",
					Quotient: int64(evidence.Threshold), Remainder: evidence.ScaleRemainder + evidence.InterpolationRemainder,
					Denominator: p.FixedScale, ContributionMicro: evidence.ContributionMicro,
					TruncatedAt: "percentile_linear_interpolation_q_floor",
				}},
			}
			out.Traces = append(out.Traces, trace)
		}
	}
	return out, nil
}

type offset struct{ dx, dy int }

var diskCache = map[int][]offset{}
var diskMu sync.Mutex

func DiskOffsets(radius int) []offset {
	diskMu.Lock()
	defer diskMu.Unlock()
	if v, ok := diskCache[radius]; ok {
		return v
	}
	var out []offset
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			if dx*dx+dy*dy <= radius*radius {
				out = append(out, offset{dx, dy})
			}
		}
	}
	diskCache[radius] = out
	return out
}
