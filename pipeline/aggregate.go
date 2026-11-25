package pipeline

import (
	"sort"
)

type eventRef struct {
	Seq          int64
	ObservedAtMs int64
	Value        int64
}

type aggAccumulator struct {
	kind      AggKind
	weight    int64
	weightSum int64
	weighted  int64
	minValue  int64
	maxValue  int64
	hasValues bool
}

type windowAccumulator struct {
	count int
	sum   int64
	aggs  []aggAccumulator
}

func newWindowAccumulator(aggs []Aggregation) *windowAccumulator {
	a := &windowAccumulator{aggs: make([]aggAccumulator, len(aggs))}
	for i, agg := range aggs {
		a.aggs[i] = aggAccumulator{kind: agg.Kind, weight: agg.Weight}
	}
	return a
}

func (a *windowAccumulator) add(value int64) {
	a.count++
	for i := range a.aggs {
		state := &a.aggs[i]
		switch state.kind {
		case AggSum:
			a.sum += value
		case AggCount:
			// Count is produced from a.count.
		case AggAvg:
			state.weighted += value * state.weight
			state.weightSum += state.weight
		case AggWeightedSum:
			state.weighted += value * state.weight
		case AggMin:
			if !state.hasValues || value < state.minValue {
				state.minValue = value
			}
			state.hasValues = true
		case AggMax:
			if !state.hasValues || value > state.maxValue {
				state.maxValue = value
			}
			state.hasValues = true
		}
	}
}

func (a *windowAccumulator) results() []AggregateResult {
	out := make([]AggregateResult, 0, len(a.aggs))
	for i := range a.aggs {
		state := &a.aggs[i]
		value := int64(0)
		switch state.kind {
		case AggSum:
			value = a.sum
		case AggCount:
			value = int64(a.count)
		case AggAvg:
			if state.weightSum != 0 {
				value = state.weighted / state.weightSum
			}
		case AggWeightedSum:
			value = state.weighted / NormalizedWeightScale
		case AggMin:
			if state.hasValues {
				value = state.minValue
			}
		case AggMax:
			if state.hasValues {
				value = state.maxValue
			}
		}
		out = append(out, AggregateResult{Kind: state.kind, Weight: state.weight, Value: value})
	}
	return out
}

func aggregateRefs(refs []eventRef, aggs []Aggregation) []AggregateResult {
	a := newWindowAccumulator(aggs)
	for _, ref := range refs {
		a.add(ref.Value)
	}
	return a.results()
}

func sortRefs(refs []eventRef) {
	sort.Slice(refs, func(i, j int) bool { return refs[i].Seq < refs[j].Seq })
}

func refsToSeqs(refs []eventRef) []int64 {
	sortRefs(refs)
	out := make([]int64, len(refs))
	for i := range refs {
		out[i] = refs[i].Seq
	}
	return out
}
