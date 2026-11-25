package engine

import (
	"sort"

	"example.com/columnar/domain"
)

type cacheSlot struct {
	seen   bool
	valid  bool
	source int
}

func selectScanBatches(scan domain.Scan, state map[int]domain.Batch) []domain.Batch {
	var out []domain.Batch
	for _, b := range state {
		if !scanSelects(scan, b) {
			continue
		}
		if scanOverlapRows(scan, b) <= 0 {
			continue
		}
		out = append(out, b.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func scanSelects(scan domain.Scan, b domain.Batch) bool {
	if b.Status != domain.BatchActive || b.Rows <= 0 {
		return false
	}
	if scan.Family != "" && scan.Family != b.Family {
		return false
	}
	if scan.FromRow >= b.RowEnd || scan.ToRow <= b.RowStart {
		return false
	}
	for _, col := range scan.Columns {
		if col == b.Column {
			return true
		}
	}
	return false
}

func scanOverlapRows(scan domain.Scan, b domain.Batch) int64 {
	from := scan.FromRow
	if from < b.RowStart {
		from = b.RowStart
	}
	to := scan.ToRow
	if to > b.RowEnd {
		to = b.RowEnd
	}
	if to <= from {
		return 0
	}
	return to - from
}

func evaluateScan(scan domain.Scan, state map[int]domain.Batch, cache map[int]*cacheSlot, budget domain.Budget) (domain.ScanResult, error) {
	selected := selectScanBatches(scan, state)
	result := domain.ScanResult{ScanID: scan.ID, AfterOp: scan.AfterOp, Recomputed: true}
	invalid := map[int]struct{}{}
	var rebuilt []int
	for _, b := range selected {
		rows := scanOverlapRows(scan, b)
		result.ReadBatches = append(result.ReadBatches, b.ID)
		result.DecodedRows += rows
		slot := cache[b.ID]
		if slot == nil {
			slot = &cacheSlot{}
			cache[b.ID] = slot
		}
		if slot.valid {
			result.ReusedBatches = append(result.ReusedBatches, b.ID)
			result.ReusedRows += rows
		} else {
			result.RebuiltBatches = append(result.RebuiltBatches, b.ID)
			result.RebuiltRows += rows
			rebuilt = append(rebuilt, b.ID)
			invalid[slot.source] = struct{}{}
			if !slot.seen {
				result.ColdCache = true
			}
		}
		slot.valid = true
		slot.seen = true
	}
	result.InvalidSources = sortedSet(invalid)
	result.CacheHit = len(result.RebuiltBatches) == 0
	obs, err := runDecodeWaves(state, rebuilt, budget)
	if err != nil {
		return result, err
	}
	result.Budget = obs
	return result, nil
}

func invalidateCache(cache map[int]*cacheSlot, changed []int, dependents map[int][]int, source int) {
	for _, id := range closureFrom(changed, dependents) {
		slot := cache[id]
		if slot == nil {
			slot = &cacheSlot{}
			cache[id] = slot
		}
		slot.valid = false
		slot.source = source
	}
}

func sortedSet(in map[int]struct{}) []int {
	out := make([]int, 0, len(in))
	for v := range in {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}
