package kernel

import "sort"

type Accumulator struct {
	Count       int     `json:"count"`
	Sum         float64 `json:"sum"`
	Min         float64 `json:"min"`
	Max         float64 `json:"max"`
	HasMinMax   bool    `json:"has_min_max"`
	WeightedSum float64 `json:"weighted_sum"`
	WeightMass  float64 `json:"weight_mass"`
}

func NewAccumulator() *Accumulator {
	return &Accumulator{}
}

func (a *Accumulator) Add(value float64, weightPPM int64) {
	a.Count++
	a.Sum += value
	if !a.HasMinMax {
		a.Min = value
		a.Max = value
		a.HasMinMax = true
	} else {
		if value < a.Min {
			a.Min = value
		}
		if value > a.Max {
			a.Max = value
		}
	}
	a.WeightedSum += value * float64(weightPPM)
	a.WeightMass += float64(weightPPM)
}

func (a Accumulator) Value(fn AggFunc) float64 {
	switch fn {
	case AggCount:
		return float64(a.Count)
	case AggSum:
		return a.Sum
	case AggAvg:
		if a.Count == 0 {
			return 0
		}
		return a.Sum / float64(a.Count)
	case AggMin:
		if !a.HasMinMax {
			return 0
		}
		return a.Min
	case AggMax:
		if !a.HasMinMax {
			return 0
		}
		return a.Max
	case AggWeightedAvg:
		if a.WeightMass == 0 {
			return 0
		}
		return a.WeightedSum / a.WeightMass
	default:
		return 0
	}
}

func (a Accumulator) Results(funcs []AggFunc) []AggregationValue {
	results := make([]AggregationValue, 0, len(funcs))
	for _, fn := range funcs {
		results = append(results, AggregationValue{Func: fn, Value: a.Value(fn)})
	}
	return results
}

func SortedWindowResults(windows []WindowResult) {
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].FeatureID != windows[j].FeatureID {
			return windows[i].FeatureID < windows[j].FeatureID
		}
		return windows[i].WindowIndex < windows[j].WindowIndex
	})
}

func UniqueSortedStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
