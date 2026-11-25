package app

import (
	"multiplexgw/internal/fixedpoint"
	"multiplexgw/internal/model"
	"multiplexgw/internal/sample"
)

func buildAllocationReport(actual *model.RunResult, truth *sample.Truth) ([]AllocationRow, bool) {
	if actual == nil || truth == nil || len(truth.EventTruth) == 0 || len(actual.FinalStreams) == 0 {
		return nil, false
	}
	actualBytes := map[int]int64{}
	truthBytes := map[int]int64{}
	var totalWeight, actualTotal, truthTotal int64
	for _, s := range actual.FinalStreams {
		totalWeight += s.WeightPermyriad
	}
	for _, rec := range actual.Sends {
		actualBytes[rec.StreamID] += rec.BytesSent
		actualTotal += rec.BytesSent
	}
	for _, rec := range truth.ScheduleTruth {
		truthBytes[rec.StreamID] += rec.BytesSent
		truthTotal += rec.BytesSent
	}
	truthFinal := truth.EventTruth[len(truth.EventTruth)-1]
	truthWindow := map[int]int64{}
	truthWeight := map[int]int64{}
	for _, s := range truthFinal.Streams {
		truthWindow[s.StreamID] = s.WindowRemaining
		truthWeight[s.StreamID] = s.WeightPermyriad
	}

	rows := make([]AllocationRow, 0, len(actual.FinalStreams))
	allOK := true
	for _, s := range actual.FinalStreams {
		weightShare, _ := fixedpoint.Share(s.WeightPermyriad, totalWeight)
		allocated := actualBytes[s.StreamID]
		truthAllocated := truthBytes[s.StreamID]
		allocShare := shareOrZero(allocated, actualTotal)
		truthAllocShare := shareOrZero(truthAllocated, truthTotal)
		diff := allocShare - truthAllocShare
		if diff < 0 {
			diff = -diff
		}
		truthRemaining := truthWindow[s.StreamID]
		windowMatch := s.WindowRemaining == truthRemaining
		allocationMatch := allocated == truthAllocated
		within := diff <= fixedpoint.DefaultTolerance && windowMatch && allocationMatch
		if !within || truthWeight[s.StreamID] != s.WeightPermyriad {
			allOK = false
		}
		rows = append(rows, AllocationRow{
			StreamID:                s.StreamID,
			WeightPermyriad:         s.WeightPermyriad,
			WeightShareQ:            weightShare,
			AllocatedBytes:          allocated,
			TruthAllocatedBytes:     truthAllocated,
			AllocationShareQ:        allocShare,
			TruthAllocationShareQ:   truthAllocShare,
			ShareDiffQ:              diff,
			RemainingWindow:         s.WindowRemaining,
			TruthRemainingWindow:    truthRemaining,
			WindowExactMatch:        windowMatch,
			AllocationExactMatch:    allocationMatch,
			WithinDeclaredTolerance: within,
			ToleranceQ:              fixedpoint.DefaultTolerance,
		})
	}
	return rows, allOK
}

func shareOrZero(part, total int64) fixedpoint.Q {
	if total <= 0 {
		return 0
	}
	q, err := fixedpoint.Share(part, total)
	if err != nil {
		return 0
	}
	return q
}
