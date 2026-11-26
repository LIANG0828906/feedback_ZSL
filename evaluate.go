package quantforensics

import "math/bits"

type Violation struct {
	BlockID    string          `json:"blockId"`
	Coordinate BlockCoordinate `json:"coordinate"`
	Component  string          `json:"component"`
	Deviation  int64           `json:"deviation"`
	Tolerance  int64           `json:"tolerance"`
	Kind       BlockKind       `json:"kind"`
	Message    string          `json:"message"`
}

type BlockEvaluation struct {
	BlockID            string          `json:"blockId"`
	ImageID            string          `json:"imageId"`
	Coordinate         BlockCoordinate `json:"coordinate"`
	Kind               BlockKind       `json:"kind"`
	MaxDeviation       [3]int          `json:"maxDeviation"`
	WeightedColorDiff  int64           `json:"weightedColorDiff"`
	Tolerance          Tolerance       `json:"tolerance"`
	Passed             bool            `json:"passed"`
	Violations         []Violation     `json:"violations"`
	GradientConclusion string          `json:"gradientConclusion,omitempty"`
	StrokeConclusion   string          `json:"strokeConclusion,omitempty"`
}

func IntegerSqrtCeil(n int64) int64 {
	if n <= 0 {
		return 0
	}
	// bits.Len64 is used only to seed Newton's method; all state is integral.
	x := int64(1) << uint((bits.Len64(uint64(n))+1)/2)
	for {
		nx := (x + n/x) / 2
		if nx >= x {
			break
		}
		x = nx
	}
	if x*x < n {
		x++
	}
	return x
}

func ReconstructBlock(indices []int, palette Palette, w, h int) ([]RGB, error) {
	if w <= 0 || h <= 0 || len(indices) != w*h {
		return nil, schemaError("$.indices", "index count %d does not match %dx%d", len(indices), w, h)
	}
	out := make([]RGB, w*h)
	for i, idx := range indices {
		entry, ok := palette.ByIndex(idx)
		if !ok {
			return nil, schemaError("$.indices["+itoa(i)+"]", "palette index %d out of range", idx)
		}
		out[i] = entry.EffectiveColor()
	}
	return out, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// EvaluateBlock compares reconstructed pixels with the source screenshot. The
// measured deviations do not depend on tolerance; tolerance only classifies.
func EvaluateBlock(image Image, block Block, reconstructed []RGB, metric Metric, tolerance Tolerance) BlockEvaluation {
	ev := BlockEvaluation{
		BlockID:    block.ID,
		ImageID:    block.ImageID,
		Coordinate: block.Coordinate(),
		Kind:       block.Kind,
		Tolerance:  tolerance,
		Passed:     true,
	}
	if metric == "" {
		metric = MetricWeightedRGB
	}
	var maxSq int64
	for y := 0; y < block.H; y++ {
		for x := 0; x < block.W; x++ {
			local := y*block.W + x
			want := image.At(block.X+x, block.Y+y)
			got := reconstructed[local]
			wi, gi := want.Ints(), got.Ints()
			for c := 0; c < 3; c++ {
				d := wi[c] - gi[c]
				if d < 0 {
					d = -d
				}
				if d > ev.MaxDeviation[c] {
					ev.MaxDeviation[c] = d
				}
			}
			d2 := ColorDistanceSq(want, got, metric)
			if d2 > maxSq {
				maxSq = d2
			}
		}
	}
	ev.WeightedColorDiff = IntegerSqrtCeil(maxSq)

	components := [3]struct {
		name string
		max  int
	}{{"R", ev.MaxDeviation[0]}, {"G", ev.MaxDeviation[1]}, {"B", ev.MaxDeviation[2]}}
	for _, c := range components {
		if int64(c.max) > int64(tolerance.MaxComponentDeviation) {
			ev.Passed = false
			ev.Violations = append(ev.Violations, Violation{
				BlockID: block.ID, Coordinate: block.Coordinate(), Component: c.name,
				Deviation: int64(c.max), Tolerance: int64(tolerance.MaxComponentDeviation), Kind: block.Kind,
				Message: "component maximum deviation exceeds tolerance",
			})
		}
	}
	if ev.WeightedColorDiff > tolerance.MaxWeightedColorDiff {
		ev.Passed = false
		ev.Violations = append(ev.Violations, Violation{
			BlockID: block.ID, Coordinate: block.Coordinate(), Component: "weighted_color",
			Deviation: ev.WeightedColorDiff, Tolerance: tolerance.MaxWeightedColorDiff, Kind: block.Kind,
			Message: "weighted color difference exceeds tolerance",
		})
	}

	conclusion := "pass"
	if !ev.Passed {
		conclusion = "fail"
	}
	switch block.Kind {
	case KindGradient:
		ev.GradientConclusion = conclusion
	case KindStroke:
		ev.StrokeConclusion = conclusion
	}
	return ev
}

func EvaluateIndices(image Image, block Block, indices []int, palette Palette, metric Metric, tolerance Tolerance) (BlockEvaluation, []RGB, error) {
	recon, err := ReconstructBlock(indices, palette, block.W, block.H)
	if err != nil {
		return BlockEvaluation{}, nil, err
	}
	return EvaluateBlock(image, block, recon, metric, tolerance), recon, nil
}
