package eval

import (
	"sort"

	"semanticshading/internal/colormetric"
	"semanticshading/internal/model"
)

func MergeAccumulators(records []model.BlockRecord) map[int]model.Accumulator {
	merged := make(map[int]model.Accumulator)
	for _, record := range records {
		for _, acc := range record.Accumulators {
			current := merged[acc.Label]
			current.Label = acc.Label
			current.Area += acc.Area
			current.SumDistanceQ8 += acc.SumDistanceQ8
			current.SumSquaredQ16 += acc.SumSquaredQ16
			current.BoundaryEdges += acc.BoundaryEdges
			current.BoundarySumQ8 += acc.BoundarySumQ8
			merged[acc.Label] = current
		}
	}
	return merged
}

func RegionMetricsFromRecords(m model.Manifest, records []model.BlockRecord) []model.RegionMetrics {
	merged := MergeAccumulators(records)
	blockSets := make(map[int][]int)
	for _, record := range records {
		for _, label := range record.RegionLabels {
			blockSets[label] = append(blockSets[label], record.Block.Index)
		}
	}
	labels := make([]int, 0, len(merged))
	for label := range merged {
		labels = append(labels, label)
	}
	sort.Ints(labels)

	result := make([]model.RegionMetrics, 0, len(labels))
	for _, label := range labels {
		acc := merged[label]
		mean := int64(0)
		if acc.Area > 0 {
			mean = acc.SumDistanceQ8 / acc.Area
		}
		boundary := int64(0)
		if acc.BoundaryEdges > 0 {
			boundary = acc.BoundarySumQ8 / acc.BoundaryEdges
		}
		std, truncations := colormetric.StdFromSums(acc.SumDistanceQ8, acc.SumSquaredQ16, acc.Area)
		name := ""
		if label >= 0 && label < len(m.Categories) {
			name = m.Categories[label]
		}
		indices := append([]int(nil), blockSets[label]...)
		sort.Ints(indices)
		result = append(result, model.RegionMetrics{
			Label:          label,
			Name:           name,
			Area:           acc.Area,
			MeanDistanceQ8: mean,
			StdDistanceQ8:  std,
			BoundaryJumpQ8: boundary,
			BoundaryEdges:  acc.BoundaryEdges,
			BlockIndices:   indices,
			Truncations:    truncations,
		})
	}
	return result
}

func PropagationPaths(m model.Manifest, records []model.BlockRecord) []model.PropagationPath {
	byIndex := make(map[int]model.BlockRecord, len(records))
	for _, record := range records {
		byIndex[record.Block.Index] = record
	}
	paths := make([]model.PropagationPath, 0)
	addPair := func(a, b model.BlockRecord, direction string) {
		common := intersectSorted(a.RegionLabels, b.RegionLabels)
		for _, label := range common {
			var edges int64
			var jumpSum int64
			var regionSeam *model.RegionSeamRecord
			for i := range a.RegionSeams {
				if a.RegionSeams[i].Label == label {
					regionSeam = &a.RegionSeams[i]
					break
				}
			}
			if regionSeam == nil {
				continue
			}
			if direction == "vertical" {
				edges = regionSeam.VerticalEdges
				jumpSum = regionSeam.VerticalJumpSumQ8
			} else {
				edges = regionSeam.HorizontalEdges
				jumpSum = regionSeam.HorizontalJumpSumQ8
			}
			if edges == 0 {
				continue
			}
			avg := int64(0)
			if edges > 0 {
				avg = jumpSum / edges
			}
			paths = append(paths, model.PropagationPath{
				RegionLabel: label,
				FromBlock:   a.Block.Index,
				ToBlock:     b.Block.Index,
				Direction:   direction,
				EdgeCount:   edges,
				JumpAvgQ8:   avg,
			})
		}
	}
	for _, record := range records {
		b := record.Block
		if right, ok := byIndex[b.Index+1]; ok && right.Block.BX == b.BX+1 && right.Block.BY == b.BY {
			addPair(record, right, "vertical")
		}
		belowIndex := b.Index + m.BlockCols()
		if below, ok := byIndex[belowIndex]; ok && below.Block.BY == b.BY+1 && below.Block.BX == b.BX {
			addPair(record, below, "horizontal")
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		if paths[i].RegionLabel != paths[j].RegionLabel {
			return paths[i].RegionLabel < paths[j].RegionLabel
		}
		if paths[i].FromBlock != paths[j].FromBlock {
			return paths[i].FromBlock < paths[j].FromBlock
		}
		return paths[i].ToBlock < paths[j].ToBlock
	})
	return paths
}

func intersectSorted(a, b []int) []int {
	result := make([]int, 0)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			result = append(result, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return result
}
