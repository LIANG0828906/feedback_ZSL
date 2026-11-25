// Package generator builds the deterministic local fixture used by the demo
// and tests. It intentionally depends only on the domain package.
package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"example.com/columnar/domain"
)

const (
	Seed       int64 = 20261126
	TickMillis int64 = 10
)

type OperationTruth struct {
	Index   int                     `json:"index"`
	Kind    domain.OperationKind    `json:"kind"`
	Changed []int                   `json:"changed"`
	Batches []domain.Batch          `json:"batches"`
	Graph   []domain.DependencyNode `json:"graph"`
	Check   domain.DigestCheck      `json:"check"`
}

type ScanTruth struct {
	ScanID         string `json:"scan_id"`
	AfterOp        int    `json:"after_op"`
	ReadBatches    []int  `json:"read_batches"`
	DecodedRows    int64  `json:"decoded_rows"`
	RebuiltBatches []int  `json:"rebuilt_batches"`
	ReusedBatches  []int  `json:"reused_batches"`
	InvalidSources []int  `json:"invalid_sources"`
	ColdCache      bool   `json:"cold_cache"`
	CacheHit       bool   `json:"cache_hit"`
}

type Truth struct {
	InitialBatches []domain.Batch          `json:"initial_batches"`
	Operations     []OperationTruth        `json:"operations"`
	Scans          []ScanTruth             `json:"scans"`
	FinalBatches   []domain.Batch          `json:"final_batches"`
	FinalGraph     []domain.DependencyNode `json:"final_graph"`
}

type Sample struct {
	Seed         int64
	Canonical    domain.Project
	Truth        Truth
	VersionJSON  map[string][]byte
	VersionOrder []string
}

type logicalOp struct {
	index      int
	kind       domain.OperationKind
	target     int
	newBatchID int
	family     string
	rowRange   *domain.RowRange
	rows       int64
	rowDelta   int64
	digest     uint64
	bytes      int64
	bytesDelta int64
	encoding   string
	setDeps    bool
	deps       []int
	sourceID   int
	atMillis   int64
}

type refScanner struct {
	cacheValid  map[int]bool
	cacheSource map[int]int
	cacheSeen   map[int]bool
}

// Generate constructs the canonical sample, an independent operation-by-
// operation reference trace, and the three schema encodings.
func Generate(seed int64) (*Sample, error) {
	if seed == 0 {
		seed = Seed
	}
	project, err := canonicalProject(seed)
	if err != nil {
		return nil, err
	}
	ops, scans, truth, err := buildTrace(seed, project)
	if err != nil {
		return nil, err
	}
	project.Operations = make([]domain.Operation, len(ops))
	for i, op := range ops {
		project.Operations[i] = domain.Operation{
			Index: op.index, Kind: op.kind, Target: op.target,
			NewBatchID: op.newBatchID, Family: op.family,
			Range: cloneRange(op.rowRange), Rows: op.rows, RowDelta: op.rowDelta,
			Digest: op.digest, CompressedBytes: op.bytes, BytesDelta: op.bytesDelta,
			Encoding: op.encoding, DependsOn: append([]int(nil), op.deps...),
			SourceID: op.sourceID, AtMillis: op.atMillis,
		}
		if !op.setDeps {
			project.Operations[i].DependsOn = nil
		}
	}
	project.Scans = make([]domain.Scan, len(scans))
	for i, scan := range scans {
		project.Scans[i] = scan.Clone()
	}

	sample := &Sample{
		Seed: seed, Canonical: project, Truth: truth,
		VersionOrder: []string{"v1", "v2", "v3"},
		VersionJSON:  make(map[string][]byte, 3),
	}
	encoders := []struct {
		name string
		fn   func(int64, domain.Project, []logicalOp, []domain.Scan, Truth) ([]byte, error)
	}{
		{"v1", encodeV1},
		{"v2", encodeV2},
		{"v3", encodeV3},
	}
	for _, enc := range encoders {
		blob, err := enc.fn(seed, project, ops, scans, truth)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", enc.name, err)
		}
		sample.VersionJSON[enc.name] = blob
	}
	return sample, nil
}

func canonicalProject(seed int64) (domain.Project, error) {
	p := domain.Project{
		SchemaVersion: "canonical", Table: "orders", TimeUnit: "ms", SizeUnit: "bytes",
		TickMillis: TickMillis, DigestBits: 64,
		DigestToleranceBits: 0, RowTolerance: 0,
		Budget: domain.Budget{
			Workers:                   3,
			TotalDecodeBufferBytes:    8192,
			PerBatchDecodeBufferBytes: 4096,
		},
	}
	families := []struct {
		name  string
		specs []batchSpec
	}{
		{"core", []batchSpec{
			{100, "id", 0, 120, 120, "rle", 1024, 2048, nil},
			{101, "id", 120, 240, 120, "rle", 1024, 2048, []int{100}},
			{102, "id", 240, 360, 120, "rle", 2048, 2048, []int{101}},
		}},
		{"attrs", []batchSpec{
			{200, "name", 0, 120, 120, "dict", 1024, 2048, []int{100}},
			{201, "name", 120, 240, 120, "dict", 1024, 2048, []int{101}},
		}},
		{"metrics", []batchSpec{
			{300, "value", 0, 120, 120, "delta", 2048, 2048, []int{100}},
			{301, "value", 120, 240, 120, "delta", 2048, 2048, []int{101}},
			{302, "value", 240, 300, 60, "delta", 1024, 2048, []int{102}},
		}},
	}
	for order, family := range families {
		f := domain.Family{ID: family.name, Order: order}
		for _, spec := range family.specs {
			deps := domain.CanonicalIntSet(spec.deps)
			if len(f.BatchIDs) > 0 {
				prev := f.BatchIDs[len(f.BatchIDs)-1]
				deps = domain.CanonicalIntSet(append(deps, prev))
			}
			digest := domain.MaskWidth(domain.Mix64(
				uint64(seed), domain.StringDigest(p.Table), domain.StringDigest(family.name),
				domain.StringDigest(spec.column), uint64(spec.id), uint64(spec.rowStart),
				uint64(spec.rows), uint64(spec.bytes), domain.StringDigest(spec.encoding),
			), p.DigestBits)
			b := domain.Batch{
				ID: spec.id, Family: family.name, Column: spec.column,
				RowStart: spec.rowStart, RowEnd: spec.rowEnd, Rows: spec.rows,
				Encoding: spec.encoding, CompressedBytes: spec.bytes, Digest: digest,
				DecodeBufferBytes: spec.decodeBytes, Status: domain.BatchActive,
				DependsOn: deps,
			}
			p.Batches = append(p.Batches, b)
			f.BatchIDs = append(f.BatchIDs, spec.id)
		}
		p.Families = append(p.Families, f)
	}
	return p, nil
}

type batchSpec struct {
	id          int
	column      string
	rowStart    int64
	rowEnd      int64
	rows        int64
	encoding    string
	bytes       int64
	decodeBytes int64
	deps        []int
}

func buildTrace(seed int64, project domain.Project) ([]logicalOp, []domain.Scan, Truth, error) {
	state := make(map[int]domain.Batch, len(project.Batches))
	for _, b := range project.Batches {
		state[b.ID] = b.Clone()
	}
	initial := domain.SortedBatches(state)
	truth := Truth{InitialBatches: cloneBatches(initial)}
	scanner := &refScanner{cacheValid: map[int]bool{}, cacheSource: map[int]int{}, cacheSeen: map[int]bool{}}

	var ops []logicalOp
	var scans []domain.Scan
	addOp := func(op logicalOp) error {
		before := cloneBatchMap(state)
		changed, err := applyReference(state, op, project.DigestBits)
		if err != nil {
			return err
		}
		graph, err := buildGraph(state, 0)
		if err != nil {
			return err
		}
		for _, id := range changed {
			invalidateClosure(scanner, state, graph, id, op.index)
		}
		check := referenceCheck(before, state, op, project)
		truth.Operations = append(truth.Operations, OperationTruth{
			Index: op.index, Kind: op.kind, Changed: append([]int(nil), changed...),
			Batches: cloneBatches(domain.SortedBatches(state)),
			Graph:   cloneGraph(graph), Check: check,
		})
		ops = append(ops, op)
		return nil
	}
	addScan := func(scan domain.Scan) error {
		result, err := referenceScan(scan, state, scanner)
		if err != nil {
			return err
		}
		truth.Scans = append(truth.Scans, result)
		scans = append(scans, scan.Clone())
		return nil
	}

	nextDigest := func(kind string, id int, extra ...int64) uint64 {
		values := []uint64{uint64(seed), domain.StringDigest(kind), uint64(id)}
		for _, v := range extra {
			values = append(values, uint64(v))
		}
		return domain.MaskWidth(domain.Mix64(values...), project.DigestBits)
	}

	if err := addOp(logicalOp{
		index: 1, kind: domain.OpPatch, target: 101,
		rowRange: &domain.RowRange{From: 10, To: 23}, rowDelta: 2,
		digest: nextDigest("patch", 101, 10, 23, 2), bytesDelta: 1024,
		encoding: "delta-rle", atMillis: 20,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addScan(domain.Scan{
		ID: "scan-1", Order: 0, AfterOp: 1, Columns: []string{"id", "name"},
		FromRow: 100, ToRow: 300, AtMillis: 30,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	newDigest200 := nextDigest("replace", 200, 116, 2048)
	if err := addOp(logicalOp{
		index: 2, kind: domain.OpReplace, target: 200, rows: 116,
		digest: newDigest200, bytes: 2048, encoding: "dict",
		setDeps: true, deps: []int{100}, atMillis: 40,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addOp(logicalOp{
		index: 3, kind: domain.OpPatch, target: 201,
		rowRange: &domain.RowRange{From: 0, To: 10}, rowDelta: -2,
		digest: nextDigest("patch", 201, 0, 10, -2), bytesDelta: 1024,
		encoding: "delta-rle", atMillis: 60,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addScan(domain.Scan{
		ID: "scan-2", Order: 1, AfterOp: 3, Columns: []string{"id", "name"},
		FromRow: 0, ToRow: 240, AtMillis: 70,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	source101 := state[101]
	compact901Digest := compactDigest(source101, 901, 2048, "zstd", project.DigestBits)
	if err := addOp(logicalOp{
		index: 4, kind: domain.OpCompact, target: 101, newBatchID: 901,
		digest: compact901Digest, bytes: 2048, encoding: "zstd", sourceID: 101,
		atMillis: 80,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addScan(domain.Scan{
		ID: "scan-3", Order: 2, AfterOp: 4, Columns: []string{"id", "name"},
		FromRow: 120, ToRow: 242, AtMillis: 90,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addOp(logicalOp{
		index: 5, kind: domain.OpPatch, target: 901,
		rowRange: &domain.RowRange{From: 0, To: 5}, rowDelta: 1,
		digest: nextDigest("patch", 901, 0, 5, 1), bytesDelta: 1024,
		encoding: "zstd-delta", atMillis: 100,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addOp(logicalOp{
		index: 6, kind: domain.OpDelete, target: 302, atMillis: 120,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addScan(domain.Scan{
		ID: "scan-4", Order: 3, AfterOp: 6, Columns: []string{"id", "value"},
		FromRow: 230, ToRow: 310, AtMillis: 130,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	source300 := state[300]
	compact902Digest := compactDigest(source300, 902, 2048, "zstd", project.DigestBits)
	if err := addOp(logicalOp{
		index: 7, kind: domain.OpCompact, target: 300, newBatchID: 902,
		digest: compact902Digest, bytes: 2048, encoding: "zstd", sourceID: 300,
		atMillis: 140,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addOp(logicalOp{
		index: 8, kind: domain.OpReplace, target: 902, rows: 96,
		digest: nextDigest("replace", 902, 96, 2048), bytes: 2048, encoding: "dict",
		setDeps: true, deps: []int{100, 300}, atMillis: 160,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addOp(logicalOp{
		index: 9, kind: domain.OpPatch, target: 902,
		rowRange: &domain.RowRange{From: 0, To: 4}, rowDelta: 4,
		digest: nextDigest("patch", 902, 0, 4, 4), bytesDelta: 1024,
		encoding: "dict-delta", atMillis: 180,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addScan(domain.Scan{
		ID: "scan-5", Order: 4, AfterOp: 9, Columns: []string{"id", "value"},
		FromRow: 0, ToRow: 250, AtMillis: 190,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addScan(domain.Scan{
		ID: "scan-6", Order: 5, AfterOp: 9, Columns: []string{"name"},
		FromRow: 0, ToRow: 100, AtMillis: 190,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	source201 := state[201]
	compact903Digest := compactDigest(source201, 903, 2048, "zstd", project.DigestBits)
	if err := addOp(logicalOp{
		index: 10, kind: domain.OpCompact, target: 201, newBatchID: 903,
		digest: compact903Digest, bytes: 2048, encoding: "zstd", sourceID: 201,
		atMillis: 200,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addScan(domain.Scan{
		ID: "scan-7", Order: 6, AfterOp: 10, Columns: []string{"name"},
		FromRow: 120, ToRow: 230, AtMillis: 210,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addOp(logicalOp{
		index: 11, kind: domain.OpPatch, target: 901,
		rowRange: &domain.RowRange{From: 2, To: 4}, rowDelta: 0,
		digest: nextDigest("patch", 901, 2, 4, 0), bytesDelta: 1024,
		encoding: "zstd-delta", atMillis: 220,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	if err := addScan(domain.Scan{
		ID: "scan-8", Order: 7, AfterOp: 11, Columns: []string{"id", "name"},
		FromRow: 120, ToRow: 242, AtMillis: 230,
	}); err != nil {
		return nil, nil, Truth{}, err
	}
	truth.FinalBatches = cloneBatches(domain.SortedBatches(state))
	graph, err := buildGraph(state, 0)
	if err != nil {
		return nil, nil, Truth{}, err
	}
	truth.FinalGraph = cloneGraph(graph)
	return ops, scans, truth, nil
}

func applyReference(state map[int]domain.Batch, op logicalOp, bits uint) ([]int, error) {
	b, ok := state[op.target]
	if !ok {
		return nil, &domain.Error{Code: domain.CodeBatchUnknown, Context: "operation.reference", Op: op.index, BatchID: op.target}
	}
	switch op.kind {
	case domain.OpPatch:
		if b.Status == domain.BatchDeleted {
			return nil, &domain.Error{Code: domain.CodeBatchDeleted, Op: op.index, BatchID: op.target}
		}
		if b.Status == domain.BatchSuperseded {
			return nil, &domain.Error{Code: domain.CodeBatchSuperseded, Op: op.index, BatchID: op.target}
		}
		if op.rowRange == nil || op.rowRange.From < 0 || op.rowRange.To < op.rowRange.From || op.rowRange.To > b.Rows {
			return nil, &domain.Error{Code: domain.CodePatchRange, Op: op.index, BatchID: op.target, Message: fmt.Sprintf("range %+v outside rows %d", op.rowRange, b.Rows)}
		}
		rows, under, over := domain.SatAddDelta(b.Rows, op.rowDelta)
		if under || over {
			return nil, &domain.Error{Code: domain.CodeOverflow, Op: op.index, BatchID: op.target, Message: "row delta saturated"}
		}
		bytes, _, under := domain.SatAddDelta(b.CompressedBytes, op.bytesDelta)
		if under {
			return nil, &domain.Error{Code: domain.CodePatchResult, Op: op.index, BatchID: op.target, Message: "compressed size became negative"}
		}
		b.Rows = rows
		b.RowEnd = b.RowStart + rows
		b.CompressedBytes = bytes
		b.Digest = patchDigest(b.Digest, *op.rowRange, op.rowDelta, op.digest, op.encoding, bits)
		if op.encoding != "" {
			b.Encoding = op.encoding
		}
		state[op.target] = b
		return []int{op.target}, nil
	case domain.OpReplace:
		if b.Status == domain.BatchDeleted {
			return nil, &domain.Error{Code: domain.CodeBatchDeleted, Op: op.index, BatchID: op.target}
		}
		if b.Status == domain.BatchSuperseded {
			return nil, &domain.Error{Code: domain.CodeBatchSuperseded, Op: op.index, BatchID: op.target}
		}
		if op.rows < 0 || op.bytes < 0 {
			return nil, &domain.Error{Code: domain.CodeOverflow, Op: op.index, BatchID: op.target}
		}
		b.Rows = op.rows
		b.RowEnd = b.RowStart + op.rows
		b.CompressedBytes = op.bytes
		b.Digest = domain.MaskWidth(op.digest, bits)
		if op.encoding != "" {
			b.Encoding = op.encoding
		}
		if op.setDeps {
			b.DependsOn = domain.CanonicalIntSet(op.deps)
		}
		state[op.target] = b
		return []int{op.target}, nil
	case domain.OpDelete:
		if b.Status == domain.BatchDeleted {
			return nil, &domain.Error{Code: domain.CodeBatchDeleted, Op: op.index, BatchID: op.target}
		}
		b.Status = domain.BatchDeleted
		b.Rows = 0
		b.RowEnd = b.RowStart
		b.CompressedBytes = 0
		b.Digest = 0
		state[op.target] = b
		return []int{op.target}, nil
	case domain.OpCompact:
		if b.Status != domain.BatchActive {
			return nil, &domain.Error{Code: domain.CodeBatchSuperseded, Op: op.index, BatchID: op.target}
		}
		if op.newBatchID == 0 {
			return nil, &domain.Error{Code: domain.CodeSchemaConflict, Op: op.index, BatchID: op.target, Message: "compact new batch id is zero"}
		}
		if _, exists := state[op.newBatchID]; exists {
			return nil, &domain.Error{Code: domain.CodeBatchDuplicate, Op: op.index, BatchID: op.newBatchID}
		}
		expected := compactDigest(b, op.newBatchID, op.bytes, op.encoding, bits)
		if expected != domain.MaskWidth(op.digest, bits) {
			return nil, &domain.Error{Code: domain.CodeCompactDrift, Op: op.index, BatchID: op.target,
				Message: fmt.Sprintf("expected digest %s, payload %s", domain.FormatDigest(expected), domain.FormatDigest(op.digest))}
		}
		b.Status = domain.BatchSuperseded
		state[op.target] = b
		deps := append([]int(nil), b.DependsOn...)
		deps = domain.CanonicalIntSet(append(deps, b.ID))
		if op.setDeps {
			deps = domain.CanonicalIntSet(append(deps, op.deps...))
		}
		family := op.family
		if family == "" {
			family = b.Family
		}
		state[op.newBatchID] = domain.Batch{
			ID: op.newBatchID, Family: family, Column: b.Column,
			RowStart: b.RowStart, RowEnd: b.RowStart + b.Rows, Rows: b.Rows,
			Encoding: op.encoding, CompressedBytes: op.bytes,
			Digest: expected, DecodeBufferBytes: b.DecodeBufferBytes,
			Status: domain.BatchActive, DependsOn: deps, SourceID: b.ID,
		}
		return []int{op.target, op.newBatchID}, nil
	default:
		return nil, &domain.Error{Code: domain.CodeOperationKind, Op: op.index, Message: string(op.kind)}
	}
}

func referenceCheck(before, after map[int]domain.Batch, op logicalOp, p domain.Project) domain.DigestCheck {
	b := after[op.target]
	beforeBatch := before[op.target]
	expected := op.digest
	expectedRows := beforeBatch.Rows
	switch op.kind {
	case domain.OpPatch:
		expected = patchDigest(beforeBatch.Digest, *op.rowRange, op.rowDelta, op.digest, op.encoding, p.DigestBits)
		expectedRows = beforeBatch.Rows + op.rowDelta
	case domain.OpReplace:
		expected = domain.MaskWidth(op.digest, p.DigestBits)
		expectedRows = op.rows
	case domain.OpCompact:
		expected = compactDigest(beforeBatch, op.newBatchID, op.bytes, op.encoding, p.DigestBits)
		if n, ok := after[op.newBatchID]; ok {
			b = n
		}
		expectedRows = beforeBatch.Rows
	case domain.OpDelete:
		expected = 0
		expectedRows = 0
	}
	hamming := domain.HammingDistance(expected, b.Digest, p.DigestBits)
	dev := b.Rows - expectedRows
	if dev < 0 {
		dev = -dev
	}
	var attribution []string
	if hamming > p.DigestToleranceBits {
		attribution = append(attribution, fmt.Sprintf("digest_hamming_distance=%d>tolerance=%d", hamming, p.DigestToleranceBits))
	}
	if dev > p.RowTolerance {
		attribution = append(attribution, fmt.Sprintf("row_deviation=%d>tolerance=%d", dev, p.RowTolerance))
	}
	return domain.DigestCheck{
		Before: beforeBatch.Digest, Expected: expected, Actual: b.Digest,
		Bits: p.DigestBits, Match: expected == b.Digest, HammingDistance: hamming,
		ExpectedRows: expectedRows, ActualRows: b.Rows, RowDeviation: dev,
		WithinTolerance: hamming <= p.DigestToleranceBits && dev <= p.RowTolerance,
		Attribution:     attribution,
	}
}

func referenceScan(scan domain.Scan, state map[int]domain.Batch, s *refScanner) (ScanTruth, error) {
	result := ScanTruth{ScanID: scan.ID, AfterOp: scan.AfterOp}
	invalid := map[int]struct{}{}
	for _, b := range domain.SortedBatches(state) {
		if !scanSelects(scan, b) {
			continue
		}
		rows := overlapRows(scan, b)
		if rows <= 0 {
			continue
		}
		result.ReadBatches = append(result.ReadBatches, b.ID)
		result.DecodedRows += rows
		if s.cacheValid[b.ID] {
			result.ReusedBatches = append(result.ReusedBatches, b.ID)
		} else {
			result.RebuiltBatches = append(result.RebuiltBatches, b.ID)
			src := s.cacheSource[b.ID]
			invalid[src] = struct{}{}
			if !s.cacheSeen[b.ID] {
				result.ColdCache = true
			}
		}
		s.cacheValid[b.ID] = true
		s.cacheSeen[b.ID] = true
	}
	result.InvalidSources = sortedSet(invalid)
	result.CacheHit = len(result.RebuiltBatches) == 0
	return result, nil
}

func invalidateClosure(s *refScanner, state map[int]domain.Batch, graph []domain.DependencyNode, root, source int) {
	dependents := make(map[int][]int, len(graph))
	for _, node := range graph {
		for _, dep := range node.Dependencies {
			dependents[dep] = append(dependents[dep], node.BatchID)
		}
	}
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		s.cacheValid[id] = false
		s.cacheSource[id] = source
		for _, child := range dependents[id] {
			if !seen[child] {
				seen[child] = true
				queue = append(queue, child)
			}
		}
	}
}

func scanSelects(scan domain.Scan, b domain.Batch) bool {
	if b.Status != domain.BatchActive {
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

func overlapRows(scan domain.Scan, b domain.Batch) int64 {
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

func buildGraph(state map[int]domain.Batch, _ int) ([]domain.DependencyNode, error) {
	nodes := make([]domain.DependencyNode, 0, len(state))
	byID := make(map[int]domain.Batch, len(state))
	for id, b := range state {
		byID[id] = b
	}
	dependents := map[int][]int{}
	for _, b := range byID {
		for _, dep := range b.DependsOn {
			dependents[dep] = append(dependents[dep], b.ID)
		}
	}
	for _, b := range domain.SortedBatches(state) {
		nodes = append(nodes, domain.DependencyNode{
			BatchID: b.ID, Family: b.Family, Status: b.Status,
			Dependencies: domain.CanonicalIntSet(b.DependsOn),
			SourceID:     b.SourceID, Dependents: domain.CanonicalIntSet(dependents[b.ID]),
		})
	}
	if err := validateGraphNodes(nodes, byID); err != nil {
		return nil, err
	}
	return nodes, nil
}

func validateGraphNodes(nodes []domain.DependencyNode, byID map[int]domain.Batch) error {
	adj := make(map[int][]int, len(nodes))
	for _, n := range nodes {
		adj[n.BatchID] = n.Dependencies
		for _, dep := range n.Dependencies {
			d, ok := byID[dep]
			if !ok {
				return &domain.Error{Code: domain.CodeBatchUnknown, BatchID: n.BatchID, Context: "dependency", Message: fmt.Sprintf("missing predecessor %d", dep)}
			}
			if d.Status == domain.BatchDeleted && n.Status != domain.BatchDeleted {
				return &domain.Error{Code: domain.CodeDependencyDeleted, BatchID: n.BatchID, Context: "dependency", Message: fmt.Sprintf("depends on deleted batch %d", dep)}
			}
		}
	}
	color := map[int]int{}
	var visit func(int, []int) error
	visit = func(id int, path []int) error {
		if color[id] == 1 {
			return &domain.Error{Code: domain.CodeDependencyCycle, BatchID: id, Context: "dependency", Message: fmt.Sprintf("cycle: %v", append(path, id))}
		}
		if color[id] == 2 {
			return nil
		}
		color[id] = 1
		for _, dep := range adj[id] {
			if err := visit(dep, append(path, id)); err != nil {
				return err
			}
		}
		color[id] = 2
		return nil
	}
	ids := make([]int, 0, len(nodes))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		if err := visit(id, nil); err != nil {
			return err
		}
	}
	return nil
}

func patchDigest(prev uint64, r domain.RowRange, rowDelta int64, payload uint64, encoding string, bits uint) uint64 {
	return domain.MaskWidth(domain.Mix64(prev, uint64(r.From), uint64(r.To), uint64(rowDelta), payload, domain.StringDigest(encoding)), bits)
}

func compactDigest(source domain.Batch, newID int, bytes int64, encoding string, bits uint) uint64 {
	return domain.MaskWidth(domain.Mix64(source.Digest, uint64(source.ID), uint64(newID),
		uint64(source.Rows), uint64(bytes), domain.StringDigest(encoding)), bits)
}

func sortedSet(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func cloneBatchMap(in map[int]domain.Batch) map[int]domain.Batch {
	out := make(map[int]domain.Batch, len(in))
	for id, b := range in {
		out[id] = b.Clone()
	}
	return out
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

func cloneRange(r *domain.RowRange) *domain.RowRange {
	if r == nil {
		return nil
	}
	v := *r
	return &v
}

// WriteSample writes the three generated schema documents and a manifest into
// dir. It never rewrites the in-memory canonical source.
func WriteSample(dir string, sample *Sample) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range sample.VersionOrder {
		if err := os.WriteFile(filepath.Join(dir, name+".json"), sample.VersionJSON[name], 0o644); err != nil {
			return err
		}
	}
	truth, err := json.MarshalIndent(sample.Truth, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), truth, 0o644); err != nil {
		return err
	}
	return nil
}
