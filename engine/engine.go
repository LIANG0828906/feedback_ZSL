// Package engine advances column-batch updates, resolves dependencies,
// invalidates scan caches and supports deterministic incremental replay.
package engine

import (
	"fmt"
	"sort"

	"example.com/columnar/domain"
)

const EngineVersion = "columnar-engine-v1"

type CommitHook func(domain.CheckpointData) error

type Options struct {
	OnCommit CommitHook
}

type stepReport struct {
	changed    []int
	before     domain.Batch
	checkBatch domain.Batch
	overflow   []string
	saturated  []string
}

// Run executes a project without mutating it.
func Run(project domain.Project, opts Options) (*domain.RunResult, error) {
	p := project.Clone()
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	state := make(map[int]domain.Batch, len(p.Batches))
	for _, b := range p.Batches {
		state[b.ID] = b.Clone()
	}
	graph, dependents, err := buildGraph(state, p)
	if err != nil {
		return nil, err
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
	runScans := func(after int) error {
		for _, scan := range scansByAfter[after] {
			sr, err := evaluateScan(scan, state, cache, p.Budget)
			if err != nil {
				var de *domain.Error
				if errorAs(err, &de) {
					if de.Op == 0 {
						de.Op = after
					}
					if de.Context == "" {
						de.Context = "scan." + scan.ID
					}
				}
				return err
			}
			result.Scans = append(result.Scans, sr)
			result.ScanCache[scan.ID] = scanCacheEntry(sr)
		}
		return nil
	}
	if err := runScans(0); err != nil {
		return nil, err
	}
	for i, op := range p.Operations {
		if op.Index != i+1 {
			return nil, &domain.Error{Code: domain.CodeOperationOrder, Op: op.Index, Context: "operations", Message: fmt.Sprintf("expected contiguous index %d", i+1)}
		}
		report, err := applyOperation(state, op, p)
		if err != nil {
			return nil, err
		}
		graph, dependents, err = buildGraph(state, p)
		if err != nil {
			return nil, err
		}
		invalidateCache(cache, report.changed, dependents, op.Index)
		check := makeCheck(report, op, p)
		obs := domain.BudgetObservation{}
		target := state[op.Target]
		if op.Kind == domain.OpCompact {
			target = state[op.NewBatchID]
		}
		obs, err = checkOperationBudget(target, p.Budget)
		if err != nil {
			return nil, err
		}
		or := domain.OperationResult{
			Index: op.Index, Kind: op.Kind,
			Changed:  append([]int(nil), report.changed...),
			Affected: closureFrom(report.changed, dependents),
			Batches:  cloneBatches(domain.SortedBatches(state)),
			Graph:    cloneGraph(graph), Check: check, Budget: obs, Recomputed: true,
		}
		result.Operations = append(result.Operations, or)
		if opts.OnCommit != nil {
			if err := opts.OnCommit(checkpointData(p, op.Index, state, cache, result.ScanCache)); err != nil {
				return nil, domain.WrapError(domain.CodeCheckpointWrite, "checkpoint.after_op", err)
			}
		}
		if err := runScans(op.Index); err != nil {
			return nil, err
		}
		if opts.OnCommit != nil {
			if err := opts.OnCommit(checkpointData(p, op.Index, state, cache, result.ScanCache)); err != nil {
				return nil, domain.WrapError(domain.CodeCheckpointWrite, "checkpoint.after_scan", err)
			}
		}
	}
	for after := range scansByAfter {
		if after < 0 || after > len(p.Operations) {
			return nil, domain.Errorf(domain.CodeScanInvalid, "scans.after_op", "after_op %d outside [0,%d]", after, len(p.Operations))
		}
	}
	graph, _, err = buildGraph(state, p)
	if err != nil {
		return nil, err
	}
	result.FinalBatches = cloneBatches(domain.SortedBatches(state))
	result.Graph = cloneGraph(graph)
	return result, nil
}

func ValidateProject(p domain.Project) error {
	if p.DigestBits == 0 || p.DigestBits > 64 {
		return domain.Errorf(domain.CodeDigestInvalid, "digest_bits", "digest width must be in [1,64]")
	}
	if p.Budget.Workers <= 0 {
		return domain.Errorf(domain.CodeBudgetWorkers, "budget.workers", "workers must be positive, got %d", p.Budget.Workers)
	}
	if p.Budget.TotalDecodeBufferBytes <= 0 || p.Budget.PerBatchDecodeBufferBytes <= 0 {
		return domain.Errorf(domain.CodeBudgetBuffer, "budget", "decode buffer limits must be positive")
	}
	seenBatches := map[int]bool{}
	for _, b := range p.Batches {
		if b.ID <= 0 {
			return &domain.Error{Code: domain.CodeSchemaConflict, Context: "batches.id", Message: fmt.Sprintf("batch id must be positive, got %d", b.ID)}
		}
		if seenBatches[b.ID] {
			return &domain.Error{Code: domain.CodeBatchDuplicate, Context: "batches", BatchID: b.ID}
		}
		seenBatches[b.ID] = true
		if b.Rows < 0 || b.CompressedBytes < 0 || b.DecodeBufferBytes < 0 {
			return &domain.Error{Code: domain.CodeSchemaConflict, Context: "batches", BatchID: b.ID}
		}
	}
	seenOps := map[int]bool{}
	for i, op := range p.Operations {
		if op.Index != i+1 {
			return &domain.Error{Code: domain.CodeOperationOrder, Op: op.Index, Context: "operations", Message: fmt.Sprintf("expected index %d", i+1)}
		}
		if seenOps[op.Index] {
			return &domain.Error{Code: domain.CodeOperationOrder, Op: op.Index, Context: "operations", Message: "duplicate operation"}
		}
		seenOps[op.Index] = true
		if _, ok := seenBatches[op.Target]; !ok {
			return &domain.Error{Code: domain.CodeBatchUnknown, Op: op.Index, BatchID: op.Target, Context: "operations.target"}
		}
		if op.Kind == domain.OpCompact {
			if op.NewBatchID <= 0 {
				return &domain.Error{Code: domain.CodeSchemaConflict, Op: op.Index, Context: "operations.new_batch_id", Message: "compact new batch id is required"}
			}
			if seenBatches[op.NewBatchID] {
				return &domain.Error{Code: domain.CodeBatchDuplicate, Op: op.Index, BatchID: op.NewBatchID, Context: "operations.new_batch_id"}
			}
			seenBatches[op.NewBatchID] = true
		}
		if op.AtMillis < 0 {
			return &domain.Error{Code: domain.CodeSchemaConflict, Op: op.Index, Context: "operations.at_millis", Message: "negative timestamp"}
		}
	}
	seenScans := map[string]bool{}
	for _, scan := range p.Scans {
		if scan.ID == "" {
			return &domain.Error{Code: domain.CodeScanInvalid, Context: "scans", Message: "scan id is required"}
		}
		if seenScans[scan.ID] {
			return &domain.Error{Code: domain.CodeScanInvalid, Context: "scans." + scan.ID, Message: "duplicate scan id"}
		}
		seenScans[scan.ID] = true
		if scan.AfterOp < 0 || scan.AfterOp > len(p.Operations) {
			return &domain.Error{Code: domain.CodeScanInvalid, Context: "scans." + scan.ID, Message: fmt.Sprintf("after_op %d outside [0,%d]", scan.AfterOp, len(p.Operations))}
		}
		if scan.FromRow < 0 || scan.ToRow < scan.FromRow {
			return &domain.Error{Code: domain.CodeScanInvalid, Context: "scans." + scan.ID, Message: "invalid predicate range"}
		}
		if len(scan.Columns) == 0 {
			return &domain.Error{Code: domain.CodeScanInvalid, Context: "scans." + scan.ID, Message: "at least one projected column is required"}
		}
		if scan.AtMillis < 0 {
			return &domain.Error{Code: domain.CodeScanInvalid, Context: "scans." + scan.ID, Message: "negative timestamp"}
		}
	}
	return nil
}

func applyOperation(state map[int]domain.Batch, op domain.Operation, p domain.Project) (stepReport, error) {
	before, ok := state[op.Target]
	if !ok {
		return stepReport{}, &domain.Error{Code: domain.CodeBatchUnknown, Op: op.Index, BatchID: op.Target, Context: "operation.target"}
	}
	report := stepReport{before: before.Clone()}
	switch op.Kind {
	case domain.OpPatch:
		if before.Status == domain.BatchDeleted {
			return report, &domain.Error{Code: domain.CodeBatchDeleted, Op: op.Index, BatchID: op.Target, Context: "operation.patch"}
		}
		if before.Status == domain.BatchSuperseded {
			return report, &domain.Error{Code: domain.CodeBatchSuperseded, Op: op.Index, BatchID: op.Target, Context: "operation.patch"}
		}
		if op.Range == nil {
			return report, &domain.Error{Code: domain.CodePatchRange, Op: op.Index, BatchID: op.Target, Message: "patch range is required"}
		}
		if op.Range.From < 0 || op.Range.To < op.Range.From || op.Range.To > before.Rows {
			return report, &domain.Error{
				Code: domain.CodePatchRange, Op: op.Index, BatchID: op.Target,
				Context: "operation.patch.range",
				Message: fmt.Sprintf("range [%d,%d) outside current row count %d", op.Range.From, op.Range.To, before.Rows),
			}
		}
		rows, under, overflow := domain.SatAddDelta(before.Rows, op.RowDelta)
		if under {
			return report, &domain.Error{Code: domain.CodePatchResult, Op: op.Index, BatchID: op.Target, Message: "row delta makes row count negative"}
		}
		if overflow {
			return report, &domain.Error{Code: domain.CodeOverflow, Op: op.Index, BatchID: op.Target, Message: "row count overflow"}
		}
		bytes, bytesUnder, bytesOverflow := domain.SatAddDelta(before.CompressedBytes, op.BytesDelta)
		if bytesUnder {
			return report, &domain.Error{Code: domain.CodePatchResult, Op: op.Index, BatchID: op.Target, Message: "compressed byte delta underflows"}
		}
		if bytesOverflow {
			return report, &domain.Error{Code: domain.CodeOverflow, Op: op.Index, BatchID: op.Target, Message: "compressed byte count overflow"}
		}
		rowEnd, endOverflow := domain.SatAddInt64(before.RowStart, rows)
		if endOverflow {
			return report, &domain.Error{Code: domain.CodeOverflow, Op: op.Index, BatchID: op.Target, Message: "row end overflow"}
		}
		updated := before.Clone()
		updated.Rows = rows
		updated.RowEnd = rowEnd
		updated.CompressedBytes = bytes
		updated.Digest = patchDigest(before.Digest, *op.Range, op.RowDelta, op.Digest, op.Encoding, p.DigestBits)
		if op.Encoding != "" {
			updated.Encoding = op.Encoding
		}
		if len(op.DependsOn) > 0 {
			updated.DependsOn = domain.CanonicalIntSet(op.DependsOn)
		}
		if op.SourceID != 0 {
			updated.SourceID = op.SourceID
		}
		state[op.Target] = updated
		report.changed = []int{op.Target}
		report.checkBatch = updated
		return report, nil
	case domain.OpReplace:
		if before.Status == domain.BatchDeleted {
			return report, &domain.Error{Code: domain.CodeBatchDeleted, Op: op.Index, BatchID: op.Target, Context: "operation.replace"}
		}
		if before.Status == domain.BatchSuperseded {
			return report, &domain.Error{Code: domain.CodeBatchSuperseded, Op: op.Index, BatchID: op.Target, Context: "operation.replace"}
		}
		if op.Rows < 0 || op.CompressedBytes < 0 {
			return report, &domain.Error{Code: domain.CodeOverflow, Op: op.Index, BatchID: op.Target, Message: "replacement has negative rows/bytes"}
		}
		rowEnd, overflow := domain.SatAddInt64(before.RowStart, op.Rows)
		if overflow {
			return report, &domain.Error{Code: domain.CodeOverflow, Op: op.Index, BatchID: op.Target, Message: "replacement row end overflow"}
		}
		updated := before.Clone()
		updated.Rows = op.Rows
		updated.RowEnd = rowEnd
		updated.CompressedBytes = op.CompressedBytes
		updated.Digest = domain.MaskWidth(op.Digest, p.DigestBits)
		if op.Encoding != "" {
			updated.Encoding = op.Encoding
		}
		if len(op.DependsOn) > 0 {
			updated.DependsOn = domain.CanonicalIntSet(op.DependsOn)
		}
		if op.SourceID != 0 {
			updated.SourceID = op.SourceID
		}
		state[op.Target] = updated
		report.changed = []int{op.Target}
		report.checkBatch = updated
		return report, nil
	case domain.OpDelete:
		if before.Status == domain.BatchDeleted {
			return report, &domain.Error{Code: domain.CodeBatchDeleted, Op: op.Index, BatchID: op.Target, Context: "operation.delete"}
		}
		updated := before.Clone()
		updated.Status = domain.BatchDeleted
		updated.Rows = 0
		updated.RowEnd = updated.RowStart
		updated.CompressedBytes = 0
		updated.Digest = 0
		state[op.Target] = updated
		report.changed = []int{op.Target}
		report.checkBatch = updated
		return report, nil
	case domain.OpCompact:
		if before.Status == domain.BatchDeleted {
			return report, &domain.Error{Code: domain.CodeBatchDeleted, Op: op.Index, BatchID: op.Target, Context: "operation.compact"}
		}
		if before.Status != domain.BatchActive {
			return report, &domain.Error{Code: domain.CodeBatchSuperseded, Op: op.Index, BatchID: op.Target, Context: "operation.compact"}
		}
		if op.NewBatchID <= 0 {
			return report, &domain.Error{Code: domain.CodeSchemaConflict, Op: op.Index, BatchID: op.Target, Message: "compact new_batch_id is required"}
		}
		if _, exists := state[op.NewBatchID]; exists {
			return report, &domain.Error{Code: domain.CodeBatchDuplicate, Op: op.Index, BatchID: op.NewBatchID, Context: "operation.compact.new_batch_id"}
		}
		if op.CompressedBytes < 0 {
			return report, &domain.Error{Code: domain.CodeOverflow, Op: op.Index, BatchID: op.Target, Message: "compact bytes are negative"}
		}
		expected := compactDigest(before, op.NewBatchID, op.CompressedBytes, op.Encoding, p.DigestBits)
		actual := domain.MaskWidth(op.Digest, p.DigestBits)
		if actual != expected {
			return report, &domain.Error{
				Code: domain.CodeCompactDrift, Op: op.Index, BatchID: op.Target,
				Context: "operation.compact.digest",
				Message: fmt.Sprintf("superseded digest drift: expected %s got %s", domain.FormatDigest(expected), domain.FormatDigest(actual)),
			}
		}
		old := before.Clone()
		old.Status = domain.BatchSuperseded
		state[op.Target] = old
		deps := append([]int(nil), before.DependsOn...)
		deps = append(deps, before.ID)
		deps = append(deps, op.DependsOn...)
		deps = domain.CanonicalIntSet(deps)
		family := op.Family
		if family == "" {
			family = before.Family
		}
		rowEnd, overflow := domain.SatAddInt64(before.RowStart, before.Rows)
		if overflow {
			return report, &domain.Error{Code: domain.CodeOverflow, Op: op.Index, BatchID: op.Target}
		}
		created := domain.Batch{
			ID: op.NewBatchID, Family: family, Column: before.Column,
			RowStart: before.RowStart, RowEnd: rowEnd, Rows: before.Rows,
			Encoding: op.Encoding, CompressedBytes: op.CompressedBytes,
			Digest: expected, DecodeBufferBytes: before.DecodeBufferBytes,
			Status: domain.BatchActive, DependsOn: deps, SourceID: before.ID,
		}
		state[op.NewBatchID] = created
		report.changed = []int{op.Target, op.NewBatchID}
		report.checkBatch = created
		return report, nil
	default:
		return report, &domain.Error{Code: domain.CodeOperationKind, Op: op.Index, Context: "operation.kind", Message: string(op.Kind)}
	}
}

func makeCheck(report stepReport, op domain.Operation, p domain.Project) domain.DigestCheck {
	expected := op.Digest
	expectedRows := report.before.Rows
	switch op.Kind {
	case domain.OpPatch:
		expected = patchDigest(report.before.Digest, *op.Range, op.RowDelta, op.Digest, op.Encoding, p.DigestBits)
		expectedRows = report.before.Rows + op.RowDelta
	case domain.OpReplace:
		expected = domain.MaskWidth(op.Digest, p.DigestBits)
		expectedRows = op.Rows
	case domain.OpCompact:
		expected = compactDigest(report.before, op.NewBatchID, op.CompressedBytes, op.Encoding, p.DigestBits)
		expectedRows = report.before.Rows
	case domain.OpDelete:
		expected = 0
		expectedRows = 0
	}
	hamming := domain.HammingDistance(expected, report.checkBatch.Digest, p.DigestBits)
	deviation := report.checkBatch.Rows - expectedRows
	if deviation < 0 {
		deviation = -deviation
	}
	var attribution []string
	if hamming > p.DigestToleranceBits {
		attribution = append(attribution, fmt.Sprintf("digest_hamming_distance=%d>tolerance=%d", hamming, p.DigestToleranceBits))
	}
	if deviation > p.RowTolerance {
		attribution = append(attribution, fmt.Sprintf("row_deviation=%d>tolerance=%d", deviation, p.RowTolerance))
	}
	if len(report.overflow) > 0 {
		attribution = append(attribution, "overflow:"+fmt.Sprint(report.overflow))
	}
	if len(report.saturated) > 0 {
		attribution = append(attribution, "saturation:"+fmt.Sprint(report.saturated))
	}
	return domain.DigestCheck{
		Before: report.before.Digest, Expected: expected, Actual: report.checkBatch.Digest,
		Bits: p.DigestBits, Match: expected == report.checkBatch.Digest,
		HammingDistance: hamming, ExpectedRows: expectedRows,
		ActualRows: report.checkBatch.Rows, RowDeviation: deviation,
		WithinTolerance: hamming <= p.DigestToleranceBits && deviation <= p.RowTolerance,
		OverflowFields:  append([]string(nil), report.overflow...),
		SaturatedFields: append([]string(nil), report.saturated...),
		Attribution:     attribution,
	}
}

func patchDigest(prev uint64, r domain.RowRange, rowDelta int64, payload uint64, encoding string, bits uint) uint64 {
	return domain.MaskWidth(domain.Mix64(prev, uint64(r.From), uint64(r.To), uint64(rowDelta), payload, domain.StringDigest(encoding)), bits)
}

func compactDigest(source domain.Batch, newID int, bytes int64, encoding string, bits uint) uint64 {
	return domain.MaskWidth(domain.Mix64(source.Digest, uint64(source.ID), uint64(newID),
		uint64(source.Rows), uint64(bytes), domain.StringDigest(encoding)), bits)
}

func cloneBatches(in []domain.Batch) []domain.Batch {
	out := make([]domain.Batch, len(in))
	for i, b := range in {
		out[i] = b.Clone()
	}
	return out
}

func cloneGraph(in []domain.DependencyNode) []domain.DependencyNode {
	out := make([]domain.DependencyNode, len(in))
	for i, n := range in {
		out[i] = n
		out[i].Dependencies = append([]int(nil), n.Dependencies...)
		out[i].Dependents = append([]int(nil), n.Dependents...)
	}
	return out
}

func scanCacheEntry(sr domain.ScanResult) domain.ScanCacheEntry {
	return domain.ScanCacheEntry{
		ScanID: sr.ScanID, AfterOp: sr.AfterOp,
		ReadBatches: append([]int(nil), sr.ReadBatches...), DecodedRows: sr.DecodedRows,
		InvalidSources: append([]int(nil), sr.InvalidSources...),
	}
}

func checkpointData(p domain.Project, op int, state map[int]domain.Batch, cache map[int]*cacheSlot, scans map[string]domain.ScanCacheEntry) domain.CheckpointData {
	entries := make([]domain.ScanCacheEntry, 0, len(scans))
	for _, v := range scans {
		v.ReadBatches = append([]int(nil), v.ReadBatches...)
		v.InvalidSources = append([]int(nil), v.InvalidSources...)
		entries = append(entries, v)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ScanID < entries[j].ScanID })
	return domain.CheckpointData{
		SchemaVersion: p.SchemaVersion, EngineVersion: EngineVersion, CommittedOp: op,
		Batches: cloneBatches(domain.SortedBatches(state)), ScanCache: entries,
		ContentDigest: ContentDigest(p),
	}
}

// ContentDigest fingerprints the immutable logical project, not a specific
// schema encoding.
func ContentDigest(p domain.Project) uint64 {
	values := []uint64{
		domain.StringDigest(p.Table), uint64(p.DigestBits), uint64(p.DigestToleranceBits),
		uint64(p.RowTolerance), uint64(p.Budget.Workers),
		uint64(p.Budget.TotalDecodeBufferBytes), uint64(p.Budget.PerBatchDecodeBufferBytes),
	}
	for _, f := range p.Families {
		values = append(values, domain.StringDigest(f.ID))
		for _, id := range f.BatchIDs {
			values = append(values, uint64(id))
		}
	}
	for _, b := range p.Batches {
		values = append(values, uint64(b.ID), domain.StringDigest(b.Family), domain.StringDigest(b.Column),
			uint64(b.RowStart), uint64(b.RowEnd), uint64(b.Rows), domain.StringDigest(b.Encoding),
			uint64(b.CompressedBytes), b.Digest, uint64(b.DecodeBufferBytes),
			uint64(b.SourceID), boolValue(b.Invalidated))
		for _, dep := range b.DependsOn {
			values = append(values, uint64(dep))
		}
	}
	for _, op := range p.Operations {
		values = append(values, uint64(op.Index), domain.StringDigest(string(op.Kind)), uint64(op.Target),
			uint64(op.NewBatchID), uint64(op.Rows), uint64(op.RowDelta), op.Digest,
			uint64(op.CompressedBytes), uint64(op.BytesDelta), domain.StringDigest(op.Encoding),
			domain.StringDigest(op.Family), uint64(op.SourceID), uint64(op.AtMillis))
		if op.Range != nil {
			values = append(values, uint64(op.Range.From), uint64(op.Range.To))
		}
		for _, dep := range op.DependsOn {
			values = append(values, uint64(dep))
		}
	}
	for _, s := range p.Scans {
		values = append(values, domain.StringDigest(s.ID), uint64(s.AfterOp), domain.StringDigest(s.Family),
			uint64(s.FromRow), uint64(s.ToRow), uint64(s.AtMillis))
		for _, c := range s.Columns {
			values = append(values, domain.StringDigest(c))
		}
	}
	return domain.Mix64(values...)
}

func boolValue(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

func errorAs(err error, target **domain.Error) bool {
	for err != nil {
		if de, ok := err.(*domain.Error); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
