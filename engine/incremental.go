package engine

import (
	"fmt"
	"sort"

	"example.com/columnar/domain"
)

// RunIncremental replays an edited project while reusing operation and scan
// results outside the dependency invalidation closure. Tests assert that the
// result is identical to a full Run.
func RunIncremental(base *domain.RunResult, edited domain.Project, opts Options) (*domain.RunResult, *domain.IncrementalStats, error) {
	if base == nil {
		return nil, nil, fmt.Errorf("nil base run result")
	}
	p := edited.Clone()
	if err := ValidateProject(p); err != nil {
		return nil, nil, err
	}
	if len(base.Project.Operations) != len(p.Operations) {
		full, err := Run(p, opts)
		if err != nil {
			return nil, nil, err
		}
		stats := &domain.IncrementalStats{ChangedFields: []string{"operations.added_or_removed"}}
		for _, op := range p.Operations {
			stats.RecomputedOps = append(stats.RecomputedOps, op.Index)
		}
		for _, sr := range full.Scans {
			stats.RecomputedScans = append(stats.RecomputedScans, sr.ScanID)
		}
		return full, stats, nil
	}

	stats := &domain.IncrementalStats{}
	impact := map[int]int{}
	changedOps := map[int]bool{}
	if base.Project.DigestBits != p.DigestBits ||
		base.Project.DigestToleranceBits != p.DigestToleranceBits ||
		base.Project.RowTolerance != p.RowTolerance ||
		base.Project.Budget != p.Budget {
		for _, b := range p.Batches {
			impact[b.ID] = 0
		}
		for _, b := range base.FinalBatches {
			impact[b.ID] = 0
		}
		for _, op := range p.Operations {
			changedOps[op.Index] = true
		}
		stats.ChangedFields = append(stats.ChangedFields, "global.fixed_width_or_budget")
	}
	for _, b := range p.Batches {
		old, ok := base.Project.FindBatch(b.ID)
		if !ok || batchSignature(old) != batchSignature(b) {
			impact[b.ID] = 0
			stats.ChangedFields = append(stats.ChangedFields, fmt.Sprintf("batch.%d", b.ID))
		}
	}
	for i, op := range p.Operations {
		old := base.Project.Operations[i]
		if operationSignature(old) != operationSignature(op) {
			changedOps[op.Index] = true
			impact[op.Target] = minImpact(impact[op.Target], op.Index)
			if old.Target != op.Target {
				impact[old.Target] = minImpact(impact[old.Target], op.Index)
			}
			if op.NewBatchID != 0 {
				impact[op.NewBatchID] = minImpact(impact[op.NewBatchID], op.Index)
			}
			stats.ChangedFields = append(stats.ChangedFields, fmt.Sprintf("operation.%d", op.Index))
		}
	}
	roots := make([]int, 0, len(impact))
	for id := range impact {
		roots = append(roots, id)
	}
	sort.Ints(roots)
	dependents := graphDependents(base.Graph)
	for _, root := range roots {
		for _, id := range closureFrom([]int{root}, dependents) {
			impact[id] = minImpact(impact[id], impact[root])
		}
	}
	for id := range impact {
		stats.ClosureBatchIDs = append(stats.ClosureBatchIDs, id)
	}
	sort.Ints(stats.ClosureBatchIDs)

	baseScans := map[string]domain.Scan{}
	baseScanResult := map[string]domain.ScanResult{}
	for _, s := range base.Project.Scans {
		baseScans[s.ID] = s
	}
	for _, sr := range base.Scans {
		baseScanResult[sr.ScanID] = sr
	}
	editedScans := map[string]domain.Scan{}
	for _, s := range p.Scans {
		editedScans[s.ID] = s
	}
	for id, old := range baseScans {
		cur, ok := editedScans[id]
		if !ok {
			stats.RemovedScans = append(stats.RemovedScans, id)
			stats.ChangedFields = append(stats.ChangedFields, "scan.removed."+id)
			continue
		}
		if scanSignature(old) != scanSignature(cur) {
			stats.ChangedFields = append(stats.ChangedFields, "scan.changed."+id)
		}
	}
	for id := range editedScans {
		if _, ok := baseScans[id]; !ok {
			stats.AddedScans = append(stats.AddedScans, id)
			stats.ChangedFields = append(stats.ChangedFields, "scan.added."+id)
		}
	}
	sort.Strings(stats.AddedScans)
	sort.Strings(stats.RemovedScans)

	state := make(map[int]domain.Batch, len(p.Batches))
	for _, b := range p.Batches {
		state[b.ID] = b.Clone()
	}
	cache := map[int]*cacheSlot{}
	for id, b := range state {
		if b.Invalidated {
			cache[id] = &cacheSlot{seen: true, valid: false}
		}
	}
	result := &domain.RunResult{
		Version: p.SchemaVersion, Table: p.Table, Project: p,
		InitialBatches: cloneBatches(domain.SortedBatches(state)),
		ScanCache:      map[string]domain.ScanCacheEntry{},
	}
	scansByAfter := map[int][]domain.Scan{}
	for _, scan := range p.Scans {
		scansByAfter[scan.AfterOp] = append(scansByAfter[scan.AfterOp], scan.Clone())
	}
	for after := range scansByAfter {
		sort.Slice(scansByAfter[after], func(i, j int) bool {
			return scansByAfter[after][i].Order < scansByAfter[after][j].Order
		})
	}

	replayScan := func(scan domain.Scan) error {
		old, existed := baseScans[scan.ID]
		changed := !existed || scanSignature(old) != scanSignature(scan)
		selected := selectScanBatches(scan, state)
		selectedIDs := batchIDs(selected)
		if !changed {
			for _, id := range selectedIDs {
				if at, ok := impact[id]; ok && at <= scan.AfterOp {
					changed = true
					break
				}
			}
		}
		oldResult, hadResult := baseScanResult[scan.ID]
		if !changed && hadResult {
			if fmt.Sprint(oldResult.ReadBatches) != fmt.Sprint(selectedIDs) {
				changed = true
			}
			if !changed {
				rebuilt := intSet(oldResult.RebuiltBatches)
				for _, id := range selectedIDs {
					slot := cache[id]
					valid := slot != nil && slot.valid
					expectedValid := !rebuilt[id]
					if valid != expectedValid {
						changed = true
						break
					}
				}
			}
		}
		if changed || !hadResult {
			sr, err := evaluateScan(scan, state, cache, p.Budget)
			if err != nil {
				return err
			}
			result.Scans = append(result.Scans, sr)
			stats.RecomputedScans = append(stats.RecomputedScans, scan.ID)
			if !existed {
				// Added scans are reported in AddedScans as a more specific
				// subset than RecomputedScans.
				stats.RecomputedScans = stats.RecomputedScans[:len(stats.RecomputedScans)-1]
			}
		} else {
			reused := oldResult
			reused.Recomputed = false
			for _, id := range selectedIDs {
				slot := cache[id]
				if slot == nil {
					slot = &cacheSlot{}
					cache[id] = slot
				}
				slot.seen = true
				slot.valid = true
			}
			result.Scans = append(result.Scans, reused)
			stats.ReusedScans = append(stats.ReusedScans, scan.ID)
		}
		result.ScanCache[scan.ID] = scanCacheEntry(result.Scans[len(result.Scans)-1])
		return nil
	}
	runScans := func(after int) error {
		for _, scan := range scansByAfter[after] {
			if err := replayScan(scan); err != nil {
				return err
			}
		}
		return nil
	}
	if err := runScans(0); err != nil {
		return nil, nil, err
	}
	for i, op := range p.Operations {
		recompute := changedOps[op.Index]
		if at, ok := impact[op.Target]; ok && at <= op.Index {
			recompute = true
		}
		if op.NewBatchID != 0 {
			if at, ok := impact[op.NewBatchID]; ok && at <= op.Index {
				recompute = true
			}
		}
		baseOp := base.Operations[i]
		if recompute {
			report, err := applyOperation(state, op, p)
			if err != nil {
				return nil, nil, err
			}
			graph, deps, err := buildGraph(state, p)
			if err != nil {
				return nil, nil, err
			}
			invalidateCache(cache, report.changed, deps, op.Index)
			check := makeCheck(report, op, p)
			target := state[op.Target]
			if op.Kind == domain.OpCompact {
				target = state[op.NewBatchID]
			}
			obs, err := checkOperationBudget(target, p.Budget)
			if err != nil {
				return nil, nil, err
			}
			result.Operations = append(result.Operations, domain.OperationResult{
				Index: op.Index, Kind: op.Kind, Changed: append([]int(nil), report.changed...),
				Affected: closureFrom(report.changed, deps),
				Batches:  cloneBatches(domain.SortedBatches(state)), Graph: cloneGraph(graph),
				Check: check, Budget: obs, Recomputed: true,
			})
			stats.RecomputedOps = append(stats.RecomputedOps, op.Index)
		} else {
			for _, id := range append([]int{op.Target}, op.NewBatchID) {
				if id == 0 {
					continue
				}
				if b, ok := findBatch(baseOp.Batches, id); ok {
					state[id] = b.Clone()
				}
			}
			graph, deps, err := buildGraph(state, p)
			if err != nil {
				return nil, nil, err
			}
			invalidateCache(cache, baseOp.Changed, deps, op.Index)
			reused := baseOp
			reused.Recomputed = false
			reused.Batches = cloneBatches(domain.SortedBatches(state))
			reused.Graph = cloneGraph(graph)
			reused.Affected = closureFrom(reused.Changed, deps)
			result.Operations = append(result.Operations, reused)
			stats.ReusedOps = append(stats.ReusedOps, op.Index)
		}
		if opts.OnCommit != nil {
			if err := opts.OnCommit(checkpointData(p, op.Index, state, cache, result.ScanCache)); err != nil {
				return nil, nil, domain.WrapError(domain.CodeCheckpointWrite, "incremental.checkpoint", err)
			}
		}
		if err := runScans(op.Index); err != nil {
			return nil, nil, err
		}
	}
	graph, _, err := buildGraph(state, p)
	if err != nil {
		return nil, nil, err
	}
	result.FinalBatches = cloneBatches(domain.SortedBatches(state))
	result.Graph = cloneGraph(graph)
	stats.ReusedOperationCache = len(stats.ReusedOps) > 0
	return result, stats, nil
}

func operationSignature(op domain.Operation) string {
	r := ""
	if op.Range != nil {
		r = fmt.Sprintf("%d:%d", op.Range.From, op.Range.To)
	}
	return fmt.Sprintf("%d|%s|%d|%d|%s|%s|%d|%d|%d|%d|%d|%s|%v|%d|%d",
		op.Index, op.Kind, op.Target, op.NewBatchID, op.Family, r, op.Rows,
		op.RowDelta, op.Digest, op.CompressedBytes, op.BytesDelta, op.Encoding,
		op.DependsOn, op.SourceID, op.AtMillis)
}

func scanSignature(scan domain.Scan) string {
	return fmt.Sprintf("%s|%d|%d|%s|%v|%d|%d|%d", scan.ID, scan.Order, scan.AfterOp, scan.Family, scan.Columns, scan.FromRow, scan.ToRow, scan.AtMillis)
}

func batchSignature(b domain.Batch) string {
	return fmt.Sprintf("%d|%s|%s|%d|%d|%d|%s|%d|%d|%v|%d|%t|%s",
		b.ID, b.Family, b.Column, b.RowStart, b.RowEnd, b.Rows, b.Encoding,
		b.CompressedBytes, b.Digest, b.DependsOn, b.SourceID, b.Invalidated, b.Status)
}

func graphDependents(graph []domain.DependencyNode) map[int][]int {
	out := map[int][]int{}
	for _, n := range graph {
		for _, dep := range n.Dependencies {
			out[dep] = append(out[dep], n.BatchID)
		}
	}
	return out
}

func minImpact(cur, next int) int {
	if cur == 0 && next != 0 {
		return next
	}
	if cur == 0 {
		return 0
	}
	if next == 0 {
		return 0
	}
	if next < cur {
		return next
	}
	return cur
}

func batchIDs(batches []domain.Batch) []int {
	out := make([]int, len(batches))
	for i, b := range batches {
		out[i] = b.ID
	}
	return out
}

func intSet(in []int) map[int]bool {
	out := make(map[int]bool, len(in))
	for _, v := range in {
		out[v] = true
	}
	return out
}

func findBatch(in []domain.Batch, id int) (domain.Batch, bool) {
	for _, b := range in {
		if b.ID == id {
			return b, true
		}
	}
	return domain.Batch{}, false
}
