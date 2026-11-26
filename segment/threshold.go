package segment

import (
	"sort"

	"iconflow/errs"
	"iconflow/model"
)

type ThresholdResult struct {
	QuantilePermyriad int              `json:"quantile_permyriad"`
	Rank              int              `json:"rank"`
	RankRemainder     int64            `json:"rank_remainder"`
	QuantileValue     int              `json:"quantile_value"`
	MinContrast       int              `json:"min_contrast"`
	MaximumMagnitude  int              `json:"maximum_magnitude"`
	Threshold         int              `json:"threshold"`
	Bounds            [2]int           `json:"bounds"`
	Histogram         []int            `json:"histogram,omitempty"`
	Coord             model.BlockCoord `json:"coord"`
}

func SelectThreshold(gradient *GradientResult, rules model.Rules) (*ThresholdResult, *errs.Error) {
	if gradient == nil {
		return nil, errs.New(errs.EInternal, "", "gradient result is nil")
	}
	permille := rules.QuantilePermyriad
	if permille == 0 {
		permille = 8500
	}
	if permille < 0 || permille > 10000 {
		return nil, errs.Schema("$.rules.quantile_permyriad", "quantile must be in [0,10000], got %d", permille)
	}
	n := len(gradient.Magnitude)
	if n == 0 {
		return nil, errs.New(errs.EInternal, gradient.Coord.Key(), "empty gradient buffer")
	}

	rank64 := int64(permille) * int64(n)
	rank := rank64 / 10000
	rem := rank64 % 10000
	if rem != 0 {
		rank++
	}
	if rank < 1 {
		rank = 1
	}
	if rank > int64(n) {
		rank = int64(n)
	}

	sorted := append([]int(nil), gradient.Magnitude...)
	sort.Ints(sorted)
	quantileValue := sorted[rank-1]
	maximum := sorted[n-1]

	histogram := make([]int, maximum+1)
	for _, magnitude := range gradient.Magnitude {
		histogram[magnitude]++
	}

	threshold := rules.MinContrast
	if quantileValue > threshold {
		threshold = quantileValue
	}
	minBound := rules.ThresholdMin
	maxBound := rules.ThresholdMax
	if minBound == 0 {
		minBound = 1
	}
	if maxBound == 0 {
		maxBound = 2000
	}
	result := &ThresholdResult{
		QuantilePermyriad: permille,
		Rank:              int(rank),
		RankRemainder:     rem,
		QuantileValue:     quantileValue,
		MinContrast:       rules.MinContrast,
		MaximumMagnitude:  maximum,
		Threshold:         threshold,
		Bounds:            [2]int{minBound, maxBound},
		Histogram:         histogram,
		Coord:             gradient.Coord,
	}
	if threshold < minBound || threshold > maxBound {
		e := errs.New(errs.EThr, gradient.Coord.Key(),
			"dynamic threshold %d outside allowed interval [%d,%d]", threshold, minBound, maxBound)
		e.Details = map[string]any{
			"coord": gradient.Coord.Key(), "measured_quantile": quantileValue,
			"rank": rank, "min_contrast": rules.MinContrast,
			"allowed_interval": []int{minBound, maxBound},
		}
		return result, e
	}
	return result, nil
}

func MarkForeground(gradient *GradientResult, threshold *ThresholdResult) []bool {
	mask := make([]bool, len(gradient.Magnitude))
	for i, magnitude := range gradient.Magnitude {
		mask[i] = magnitude >= threshold.Threshold
	}
	return mask
}
