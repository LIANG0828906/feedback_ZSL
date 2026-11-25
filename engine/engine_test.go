package engine_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"example.com/columnar/anomaly"
	"example.com/columnar/domain"
	"example.com/columnar/engine"
	"example.com/columnar/generator"
	"example.com/columnar/loader"
)

func fixture(t *testing.T) *generator.Sample {
	t.Helper()
	s, err := generator.Generate(generator.Seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func loadVersion(t *testing.T, s *generator.Sample, version string) domain.Project {
	t.Helper()
	p, err := loader.Load(s.VersionJSON[version])
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestThreeVersionsMatchReferenceTruth(t *testing.T) {
	s := fixture(t)
	var results []*domain.RunResult
	for _, name := range s.VersionOrder {
		p := loadVersion(t, s, name)
		r, err := engine.Run(p, engine.Options{})
		if err != nil {
			t.Fatalf("%s run: %v", name, err)
		}
		results = append(results, r)
		if !reflect.DeepEqual(r.InitialBatches, s.Truth.InitialBatches) {
			t.Fatalf("%s initial batches differ from truth", name)
		}
		if len(r.Operations) != len(s.Truth.Operations) {
			t.Fatalf("%s op count", name)
		}
		for i, op := range r.Operations {
			truth := s.Truth.Operations[i]
			if !reflect.DeepEqual(op.Batches, truth.Batches) {
				for j := range op.Batches {
					if !reflect.DeepEqual(op.Batches[j], truth.Batches[j]) {
						t.Fatalf("%s op %d batch %d:\n got %+v\nwant %+v", name, op.Index, op.Batches[j].ID, op.Batches[j], truth.Batches[j])
					}
				}
				t.Fatalf("%s op %d batch snapshot mismatch", name, op.Index)
			}
			if !reflect.DeepEqual(op.Graph, truth.Graph) {
				t.Fatalf("%s op %d dependency graph differs", name, op.Index)
			}
			if !op.Check.Match || op.Check.HammingDistance != 0 || op.Check.RowDeviation != 0 || !op.Check.WithinTolerance {
				t.Fatalf("%s op %d fixed-width check failed: %+v", name, op.Index, op.Check)
			}
		}
		if len(r.Scans) != len(s.Truth.Scans) {
			t.Fatalf("%s scan count", name)
		}
		for i, scan := range r.Scans {
			truth := s.Truth.Scans[i]
			if scan.ScanID != truth.ScanID || scan.AfterOp != truth.AfterOp ||
				fmt.Sprint(scan.ReadBatches) != fmt.Sprint(truth.ReadBatches) ||
				scan.DecodedRows != truth.DecodedRows ||
				fmt.Sprint(scan.RebuiltBatches) != fmt.Sprint(truth.RebuiltBatches) ||
				fmt.Sprint(scan.ReusedBatches) != fmt.Sprint(truth.ReusedBatches) ||
				fmt.Sprint(scan.InvalidSources) != fmt.Sprint(truth.InvalidSources) ||
				scan.ColdCache != truth.ColdCache || scan.CacheHit != truth.CacheHit {
				t.Fatalf("%s scan %s differs:\n got %+v\nwant %+v", name, scan.ScanID, scan, truth)
			}
		}
		if !reflect.DeepEqual(r.FinalBatches, s.Truth.FinalBatches) {
			t.Fatalf("%s final batches differ", name)
		}
	}
	if err := engine.VerifyVersionEquivalence(results); err != nil {
		t.Fatal(err)
	}
}

func TestCompactionLineageAndDependencyAudit(t *testing.T) {
	s := fixture(t)
	p := loadVersion(t, s, "v2")
	r, err := engine.Run(p, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	op4 := opResult(t, r, 4)
	b901 := mustBatch(t, op4.Batches, 901)
	if b901.Status != domain.BatchActive || b901.SourceID != 101 || fmt.Sprint(b901.DependsOn) != "[100 101]" {
		t.Fatalf("compact lineage wrong: %+v", b901)
	}
	b101 := mustBatch(t, op4.Batches, 101)
	if b101.Status != domain.BatchSuperseded {
		t.Fatalf("source batch should be superseded: %+v", b101)
	}
	op10 := opResult(t, r, 10)
	b903 := mustBatch(t, op10.Batches, 903)
	if b903.SourceID != 201 || fmt.Sprint(b903.DependsOn) != "[101 200 201]" {
		t.Fatalf("inherited dependencies wrong: %+v", b903)
	}
	derived, err := engine.DeriveFamilyPredecessors(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{101, 102, 201, 301, 302} {
		got := mustBatch(t, p.Batches, id).DependsOn
		if fmt.Sprint(got) != fmt.Sprint(derived[id]) {
			t.Fatalf("family derivation batch %d got %v want %v", id, derived[id], got)
		}
	}
	for _, op := range r.Operations {
		if len(op.Graph) == 0 {
			t.Fatalf("op %d has empty dependency report", op.Index)
		}
	}
}

func TestOperationErrorClassification(t *testing.T) {
	s := fixture(t)
	cases := []struct {
		name string
		want domain.Code
		edit func(*domain.Project)
	}{
		{"patch_out_of_range", domain.CodePatchRange, func(p *domain.Project) {
			p.Operations[0].Range = &domain.RowRange{From: 500, To: 510}
		}},
		{"patch_superseded", domain.CodeBatchSuperseded, func(p *domain.Project) {
			p.Operations[4].Target = 101
		}},
		{"patch_deleted", domain.CodeBatchDeleted, func(p *domain.Project) {
			p.Operations[8].Target = 302
		}},
		{"compact_drift", domain.CodeCompactDrift, func(p *domain.Project) {
			p.Operations[3].Digest ^= 1
		}},
		{"compact_duplicate_batch", domain.CodeBatchDuplicate, func(p *domain.Project) {
			p.Operations[3].NewBatchID = 100
		}},
		{"deleted_dependency", domain.CodeDependencyDeleted, func(p *domain.Project) {
			p.Operations[7].DependsOn = []int{302}
		}},
		{"dependency_cycle", domain.CodeDependencyCycle, func(p *domain.Project) {
			for i := range p.Batches {
				if p.Batches[i].ID == 100 {
					p.Batches[i].DependsOn = []int{102}
				}
			}
		}},
		{"missing_dependency", domain.CodeBatchUnknown, func(p *domain.Project) {
			for i := range p.Batches {
				if p.Batches[i].ID == 100 {
					p.Batches[i].DependsOn = []int{9999}
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := loadVersion(t, s, "v2")
			tc.edit(&p)
			_, err := engine.Run(p, engine.Options{})
			if !domain.HasCode(err, tc.want) {
				t.Fatalf("got %v (%s), want %s", err, domain.CodeOf(err), tc.want)
			}
		})
	}
}

func TestAnomalyInjectionCodes(t *testing.T) {
	s := fixture(t)
	for _, c := range anomaly.Cases {
		t.Run(c.Name, func(t *testing.T) {
			err := anomaly.Inject(c.Name, s)
			if !domain.HasCode(err, c.Expected) {
				t.Fatalf("got %v (%s), want %s", err, domain.CodeOf(err), c.Expected)
			}
		})
	}
}

func TestScanCacheReuseAndDeterministicBackpressure(t *testing.T) {
	s := fixture(t)
	p := loadVersion(t, s, "v2")
	r, err := engine.Run(p, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	scan6 := scanResult(t, r, "scan-6")
	if !scan6.CacheHit || len(scan6.RebuiltBatches) != 0 || len(scan6.ReusedBatches) != 1 {
		t.Fatalf("scan-6 should be a pure cache hit: %+v", scan6)
	}
	scan8 := scanResult(t, r, "scan-8")
	if fmt.Sprint(scan8.RebuiltBatches) != "[901]" || fmt.Sprint(scan8.ReusedBatches) != "[102 903]" {
		t.Fatalf("scan-8 should rebuild only 901: %+v", scan8)
	}
	if scanResult(t, r, "scan-1").Budget.BackpressureEvents == 0 {
		t.Fatal("expected observable initial backpressure")
	}

	small := loadVersion(t, s, "v2")
	small.Budget.TotalDecodeBufferBytes = 2048
	small.Budget.PerBatchDecodeBufferBytes = 4096
	r2, err := engine.Run(small, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	first := scanResult(t, r2, "scan-1")
	if first.Budget.Waves != 5 || first.Budget.MaxInFlight != 1 || first.Budget.BackpressureEvents != 4 {
		t.Fatalf("deterministic wave schedule mismatch: %+v", first.Budget)
	}
	if fmt.Sprint(first.Budget.BackpressuredBatches) != "[101 102 200 201]" {
		t.Fatalf("backpressure batch set mismatch: %v", first.Budget.BackpressuredBatches)
	}

	zeroWorkers := loadVersion(t, s, "v2")
	zeroWorkers.Budget.Workers = 0
	if _, err := engine.Run(zeroWorkers, engine.Options{}); !domain.HasCode(err, domain.CodeBudgetWorkers) {
		t.Fatalf("worker budget error: %v", err)
	}
	tooSmall := loadVersion(t, s, "v2")
	tooSmall.Budget.PerBatchDecodeBufferBytes = 1024
	if _, err := engine.Run(tooSmall, engine.Options{}); !domain.HasCode(err, domain.CodeBudgetBuffer) {
		t.Fatalf("buffer budget error: %v", err)
	}
}

func TestParallelPatchSolverUsesSharedBudget(t *testing.T) {
	s := fixture(t)
	p := loadVersion(t, s, "v2")
	b100 := mustBatch(t, p.Batches, 100)
	b102 := mustBatch(t, p.Batches, 102)
	reqs := []engine.PatchRequest{
		{Batch: b102, Operation: domain.Operation{Index: 1, Kind: domain.OpPatch, Target: 102, Range: &domain.RowRange{From: 0, To: 4}, RowDelta: 1, BytesDelta: 128, Digest: 1, Encoding: "wave-a"}},
		{Batch: b100, Operation: domain.Operation{Index: 2, Kind: domain.OpPatch, Target: 100, Range: &domain.RowRange{From: 1, To: 5}, RowDelta: 2, BytesDelta: 256, Digest: 2, Encoding: "wave-b"}},
	}
	small := p.Budget
	small.TotalDecodeBufferBytes = 2048
	small.PerBatchDecodeBufferBytes = 4096
	solutions, obs, err := engine.SolvePatches(reqs, p.DigestBits, small)
	if err != nil {
		t.Fatal(err)
	}
	if len(solutions) != 2 || solutions[0].BatchID != 100 || solutions[1].BatchID != 102 {
		t.Fatalf("non-deterministic solution order: %+v", solutions)
	}
	if obs.Waves != 2 || obs.MaxInFlight != 1 || obs.BackpressureEvents != 1 || fmt.Sprint(obs.BackpressuredBatches) != "[102]" {
		t.Fatalf("patch solver budget schedule wrong: %+v", obs)
	}
	if solutions[0].Rows != b100.Rows+2 || solutions[0].CompressedBytes != b100.CompressedBytes+256 || solutions[0].Encoding != "wave-b" {
		t.Fatalf("batch 100 solution wrong: %+v", solutions[0])
	}
	if solutions[1].Rows != b102.Rows+1 || solutions[1].CompressedBytes != b102.CompressedBytes+128 {
		t.Fatalf("batch 102 solution wrong: %+v", solutions[1])
	}
	if _, _, err := engine.SolvePatches([]engine.PatchRequest{reqs[0], reqs[0]}, p.DigestBits, p.Budget); !domain.HasCode(err, domain.CodeOperationOrder) {
		t.Fatalf("duplicate patch target must be rejected: %v", err)
	}
}

func TestRepeatedPatchesStayInDeclaredWidthAndDoNotAmplify(t *testing.T) {
	bits := uint(12)
	p := domain.Project{
		SchemaVersion: "synthetic", Table: "width-check",
		DigestBits: bits, Budget: domain.Budget{Workers: 2, TotalDecodeBufferBytes: 4096, PerBatchDecodeBufferBytes: 4096},
		Families: []domain.Family{{ID: "f", Order: 0, BatchIDs: []int{1}}},
		Batches: []domain.Batch{{
			ID: 1, Family: "f", Column: "v", RowStart: 0, RowEnd: 100, Rows: 100,
			Encoding: "rle", CompressedBytes: 1000, Digest: 0xabc, DecodeBufferBytes: 1024,
			Status: domain.BatchActive,
		}},
	}
	for i := 1; i <= 60; i++ {
		p.Operations = append(p.Operations, domain.Operation{
			Index: i, Kind: domain.OpPatch, Target: 1,
			Range: &domain.RowRange{From: 0, To: 1}, BytesDelta: 1,
			Digest: uint64(i), Encoding: "rle",
		})
		if i%10 == 0 {
			p.Scans = append(p.Scans, domain.Scan{
				ID: fmt.Sprintf("s-%02d", i), Order: len(p.Scans), AfterOp: i,
				Columns: []string{"v"}, FromRow: 0, ToRow: 100,
			})
		}
	}
	r, err := engine.Run(p, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range r.Operations {
		if !op.Check.Match || op.Check.HammingDistance != 0 || op.Check.RowDeviation != 0 || len(op.Check.Attribution) != 0 {
			t.Fatalf("op %d accumulated drift: %+v", op.Index, op.Check)
		}
		b := mustBatch(t, op.Batches, 1)
		if b.Digest > 0xfff {
			t.Fatalf("digest escaped %d-bit width: %#x", bits, b.Digest)
		}
	}
	if len(r.Scans) != 6 {
		t.Fatalf("scan count: %d", len(r.Scans))
	}
	for i, sr := range r.Scans {
		wantSource := (i + 1) * 10
		if fmt.Sprint(sr.InvalidSources) != fmt.Sprintf("[%d]", wantSource) || sr.DecodedRows != 100 {
			t.Fatalf("scan %d invalidation changed after repeated patches: %+v", i, sr)
		}
	}
}

func TestIncrementalEditsEqualFullRecompute(t *testing.T) {
	s := fixture(t)
	p := loadVersion(t, s, "v2")
	base, err := engine.Run(p, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("patch_encoding_and_scan_add", func(t *testing.T) {
		ed := engine.NewEditor(p)
		if err := ed.SetPatchRange(11, 2, 5); err != nil {
			t.Fatal(err)
		}
		if err := ed.SetOperationEncoding(11, "zstd-delta-v2"); err != nil {
			t.Fatal(err)
		}
		if err := ed.AddScan(domain.Scan{
			ID: "scan-9", AfterOp: 9, Columns: []string{"id"}, FromRow: 0, ToRow: 220, AtMillis: 235,
		}); err != nil {
			t.Fatal(err)
		}
		edited := ed.Project()
		full, err := engine.Run(edited, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		inc, stats, err := engine.RunIncremental(base, edited, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.CompareResults(full, inc); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(stats.RecomputedOps) != "[11]" || len(stats.ReusedOps) != 10 {
			t.Fatalf("expected only op 11 recomputed: %+v", stats)
		}
		if fmt.Sprint(stats.ClosureBatchIDs) != "[901]" || fmt.Sprint(stats.RecomputedScans) != "[scan-8]" {
			t.Fatalf("unexpected closure: %+v", stats)
		}
		if fmt.Sprint(stats.AddedScans) != "[scan-9]" || !stats.ReusedOperationCache {
			t.Fatalf("add-scan stats wrong: %+v", stats)
		}
	})

	t.Run("family_move", func(t *testing.T) {
		ed := engine.NewEditor(p)
		if err := ed.MoveBatchFamily(200, "core", 3); err != nil {
			t.Fatal(err)
		}
		edited := ed.Project()
		full, err := engine.Run(edited, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		inc, stats, err := engine.RunIncremental(base, edited, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.CompareResults(full, inc); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(stats.ClosureBatchIDs) != "[200 201 903]" {
			t.Fatalf("family closure wrong: %v", stats.ClosureBatchIDs)
		}
		if fmt.Sprint(stats.RecomputedOps) != "[2 3 10]" {
			t.Fatalf("family move recompute ops wrong: %v", stats.RecomputedOps)
		}
	})

	t.Run("batch_encoding", func(t *testing.T) {
		ed := engine.NewEditor(p)
		if err := ed.SetBatchEncoding(200, "dict-v2"); err != nil {
			t.Fatal(err)
		}
		edited := ed.Project()
		full, err := engine.Run(edited, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		inc, stats, err := engine.RunIncremental(base, edited, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.CompareResults(full, inc); err != nil {
			t.Fatal(err)
		}
		if !containsInt(stats.ClosureBatchIDs, 200) || len(stats.ReusedOps) == 0 {
			t.Fatalf("batch encoding propagation wrong: %+v", stats)
		}
	})

	t.Run("scan_remove", func(t *testing.T) {
		ed := engine.NewEditor(p)
		if err := ed.RemoveScan("scan-1"); err != nil {
			t.Fatal(err)
		}
		edited := ed.Project()
		full, err := engine.Run(edited, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		inc, stats, err := engine.RunIncremental(base, edited, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.CompareResults(full, inc); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(stats.RemovedScans) != "[scan-1]" {
			t.Fatalf("removed scan stats wrong: %+v", stats)
		}
		if len(stats.ReusedScans) == len(edited.Scans) {
			t.Fatalf("removing the cold scan must invalidate later cache assumptions: %+v", stats)
		}
	})
}

func TestRunAndIncrementalNeverMutateSource(t *testing.T) {
	s := fixture(t)
	p := loadVersion(t, s, "v2")
	before := p.Clone()
	base, err := engine.Run(p, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if projectJSON(t, before) != projectJSON(t, p) {
		t.Fatal("Run mutated its source project")
	}
	ed := engine.NewEditor(p)
	if err := ed.SetOperationEncoding(11, "changed"); err != nil {
		t.Fatal(err)
	}
	edited := ed.Project()
	if projectJSON(t, before) != projectJSON(t, p) {
		t.Fatal("Editor mutated its source project")
	}
	if _, _, err := engine.RunIncremental(base, edited, engine.Options{}); err != nil {
		t.Fatal(err)
	}
	if projectJSON(t, before) != projectJSON(t, p) {
		t.Fatal("incremental replay mutated its source project")
	}
}

func projectJSON(t *testing.T, p domain.Project) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func containsInt(in []int, target int) bool {
	for _, v := range in {
		if v == target {
			return true
		}
	}
	return false
}

func opResult(t *testing.T, r *domain.RunResult, index int) domain.OperationResult {
	t.Helper()
	for _, op := range r.Operations {
		if op.Index == index {
			return op
		}
	}
	t.Fatalf("operation %d not found", index)
	return domain.OperationResult{}
}

func scanResult(t *testing.T, r *domain.RunResult, id string) domain.ScanResult {
	t.Helper()
	for _, s := range r.Scans {
		if s.ScanID == id {
			return s
		}
	}
	t.Fatalf("scan %s not found", id)
	return domain.ScanResult{}
}

func mustBatch(t *testing.T, batches []domain.Batch, id int) domain.Batch {
	t.Helper()
	for _, b := range batches {
		if b.ID == id {
			return b
		}
	}
	t.Fatalf("batch %d not found", id)
	return domain.Batch{}
}
